package usecase

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/sentiae/platform-kit/logger"
	"github.com/sentiae/runtime-service/internal/domain"
)

// ─────────────────────────────────────────────────────────────────────
// The snapshot cadence — the worker whose PASSES are the `cadence` fact (D-202).
//
// Before this, SnapshotAppVolumes had exactly two callers: a manual RPC and
// decommission. A provisioned durable database received zero automatic snapshots,
// ever. This is the scheduled caller.
//
// ⚠ IT IS PINNED TO ONE HOST, AND CONSTRUCTION FAILS WITHOUT THAT PIN (J4). Both
// halves of the work are host-local: the snapshot freezes a guest running HERE,
// and the heartbeat it writes claims that THIS host's resources are being
// captured. A worker that could not name its own host would either snapshot
// another host's volumes (it cannot — the backing file is not on this filesystem)
// or, far worse, beat a global row that greens every host's accepts from one
// host's liveness. Migration 0025's scope-shape CHECK forbids that row at the
// storage layer; this constructor forbids building the worker that would want it.
// ─────────────────────────────────────────────────────────────────────

const (
	// ProtectionCadenceTickEvery is how often the worker LOOKS for due resources.
	// The poll rate, not the protection cadence — that one is per-row, stamped at
	// accept, and can differ between resources.
	ProtectionCadenceTickEvery = time.Minute
	// protectionCadenceBatch bounds snapshots per tick. Each one FIFREEZEs a
	// customer's guest, so a tick must never freeze the whole fleet at once; the
	// backlog drains oldest-due-first across ticks.
	protectionCadenceBatch = 3
	// protectionCadenceRetryAfter is how long a resource whose last snapshot
	// ATTEMPT failed is left alone. The failure-streak machinery already records
	// and alarms on it (migration 0018), and hammering a broken snapshot path every
	// tick helps nobody while consuming a batch slot the healthy rows need.
	protectionCadenceRetryAfter = 10 * time.Minute
)

// ProtectionCadenceLedger is the narrow ledger slice the worker needs: its work
// list and its own liveness fact. It can reach nothing else.
type ProtectionCadenceLedger interface {
	// ListResourcesDueSnapshot returns durable, cadence-enrolled, live dedicated
	// resources WHOSE CLAIM-OWNED VOLUMES ALL LIVE ON selfHost and whose newest
	// successful snapshot is older than their OWN cadence, oldest-due first.
	ListResourcesDueSnapshot(ctx context.Context, selfHost uuid.UUID, now time.Time, retryCooldown time.Duration, limit int) ([]domain.FleetResource, error)
	// UpsertProtectionHeartbeat records that a component completed the start of a
	// pass in this ledger.
	UpsertProtectionHeartbeat(ctx context.Context, component, scope string, at time.Time, detail string) error
}

// ProtectionCapture is the narrow capture port the cadence worker drives: take
// this resource's recovery point, now.
//
// Narrow on purpose — the worker must not be able to reach the snapshotter's
// restore, catalog or health surface. The D-202 adapter below wraps the existing
// VolumeSnapshotter (the freeze path), which is TRANSITIONAL BY DESIGN: when
// D-212's capture plane lands, the port stays and the adapter is replaced.
type ProtectionCapture interface {
	Capture(ctx context.Context, resourceID, appID uuid.UUID) error
}

// volumeSnapshotCapture adapts the existing VolumeSnapshotter to ProtectionCapture.
//
// It discards the returned recovery points deliberately: the snapshotter already
// records success and failure streaks on the resource row and stamps each point's
// location class, so a second interpretation here could only disagree with the
// first.
type volumeSnapshotCapture struct{ snapshotter VolumeSnapshotter }

var _ ProtectionCapture = (*volumeSnapshotCapture)(nil)

// NewVolumeSnapshotCapture wraps a snapshotter as the cadence worker's capture
// port. Returns an error on a nil snapshotter: a capture port that captures
// nothing would let the worker beat `cadence` — advertising protection that does
// not exist, which is the one outcome this component must never produce.
func NewVolumeSnapshotCapture(snapshotter VolumeSnapshotter) (*volumeSnapshotCapture, error) {
	if snapshotter == nil {
		return nil, errors.New("protection cadence capture: a volume snapshotter is required")
	}
	return &volumeSnapshotCapture{snapshotter: snapshotter}, nil
}

func (c *volumeSnapshotCapture) Capture(ctx context.Context, resourceID, appID uuid.UUID) error {
	_, err := c.snapshotter.SnapshotAppVolumes(ctx, resourceID, appID)
	return err
}

// FleetProtectionCadenceWorker takes each enrolled resource's scheduled recovery
// point on the host it is pinned to, and publishes that it is doing so.
type FleetProtectionCadenceWorker struct {
	ledger   ProtectionCadenceLedger
	capture  ProtectionCapture
	selfHost uuid.UUID
	batch    int
	// retryAfter is the per-resource cooldown after a failed capture; a field so
	// tests drive both sides of it.
	retryAfter time.Duration
	// now is injected so due-times and cooldowns are testable without sleeping.
	now func() time.Time

	stopCh   chan struct{}
	doneCh   chan struct{}
	stopOnce sync.Once
}

// NewFleetProtectionCadenceWorker constructs the worker. Every argument is
// REQUIRED — a nil ledger, a nil capture port or a zero host id is a wiring
// error, not a degraded mode. A worker constructed without any of them could
// still write the heartbeat that tells the accept gate this fleet takes scheduled
// snapshots, and the gate reads that fact precisely because it cannot see the
// wiring.
func NewFleetProtectionCadenceWorker(ledger ProtectionCadenceLedger, capture ProtectionCapture, selfHost uuid.UUID) (*FleetProtectionCadenceWorker, error) {
	if ledger == nil {
		return nil, errors.New("protection cadence worker: the resource ledger is required")
	}
	if capture == nil {
		return nil, errors.New("protection cadence worker: a capture port is required")
	}
	if selfHost == uuid.Nil {
		return nil, errors.New("protection cadence worker: this host's fleet identity is required — a worker that cannot name its own host must not beat a cadence fact")
	}
	return &FleetProtectionCadenceWorker{
		ledger:     ledger,
		capture:    capture,
		selfHost:   selfHost,
		batch:      protectionCadenceBatch,
		retryAfter: protectionCadenceRetryAfter,
		now:        func() time.Time { return time.Now().UTC() },
		stopCh:     make(chan struct{}),
		doneCh:     make(chan struct{}),
	}, nil
}

// Start writes the FIRST heartbeat SYNCHRONOUSLY and then runs the pass loop.
//
// ⚠ Synchronous by contract, and the reason is the boot race: the accept gate
// must be able to see a live fact the moment this process serves. If the first
// beat were written by the spawned goroutine, a provision arriving in that window
// would be refused for a worker that is running perfectly — and, worse, a
// constructed-but-never-Started worker (the exact wiring miss that left
// FleetResourceSharedUC dead for a whole slice) would be indistinguishable from a
// slow one.
func (w *FleetProtectionCadenceWorker) Start(ctx context.Context) {
	w.beat(ctx)
	go w.run(ctx)
}

// Stop signals the loop to exit and waits for it (shutdown group, §21). Waiting
// matters: a pass killed mid-capture would leave a frozen guest to be thawed by
// the snapshotter's own deferred unfreeze, which only runs if the pass gets to
// finish.
func (w *FleetProtectionCadenceWorker) Stop() {
	w.stopOnce.Do(func() { close(w.stopCh) })
	<-w.doneCh
}

func (w *FleetProtectionCadenceWorker) run(ctx context.Context) {
	defer close(w.doneCh)
	defer func() {
		if r := recover(); r != nil {
			logger.FromContext(ctx).Error("protection cadence worker panicked — no scheduled recovery points will be taken on this host until the process is restarted",
				"host_id", w.selfHost, "panic", r)
		}
	}()
	t := time.NewTicker(ProtectionCadenceTickEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.stopCh:
			return
		case <-t.C:
			w.pass(ctx)
		}
	}
}

// pass runs one cadence pass and logs its verdict.
func (w *FleetProtectionCadenceWorker) pass(ctx context.Context) {
	if err := w.RunOnce(ctx); err != nil {
		logger.FromContext(ctx).Error("protection cadence pass had failures — the resources it could not capture have no NEW recovery point from this pass, and their rows say so",
			"host_id", w.selfHost, "err", err)
	}
}

// RunOnce beats and then captures one batch. Exported so the behaviour is
// directly testable without a timer.
//
// ⚠ THE HEARTBEAT COMES FIRST, INCLUDING ON EMPTY PASSES. The PASS is the fact —
// "this worker is alive and looking" — not "work was found". A beat written only
// after successful captures would go stale on an idle fleet and refuse every new
// durable provision for a worker that is functioning perfectly.
//
// A failed beat does NOT abort the pass: the pass protects data, the heartbeat
// only advertises that it does, and a stale beat fails SAFE (accepts refuse).
// Doing it in the other order would trade real protection for advertisement.
func (w *FleetProtectionCadenceWorker) RunOnce(ctx context.Context) error {
	var errs []error
	w.beat(ctx)

	now := w.now()
	due, err := w.ledger.ListResourcesDueSnapshot(ctx, w.selfHost, now, w.retryAfter, w.batch)
	if err != nil {
		return fmt.Errorf("list resources due a scheduled snapshot: %w", err)
	}
	for i := range due {
		if cerr := ctx.Err(); cerr != nil {
			// A cancelled context ends the pass rather than recording every remaining
			// resource as a failure whose cause is our own shutdown.
			errs = append(errs, cerr)
			break
		}
		res := &due[i]
		if res.AppID == nil {
			// The due query already excludes these; belt-and-braces, because
			// dereferencing here would take the whole worker down with a panic.
			continue
		}
		cerr := w.capture.Capture(ctx, res.ID, *res.AppID)
		recordExecution("protection_cadence_snapshot", outcomeFor(cerr))
		if cerr != nil {
			// One resource's failure never aborts the pass: the others are due too, and
			// a single unsnapshottable database must not stop the whole host's
			// protection. The streak columns and the snapshot-failing condition already
			// carry this resource's own alarm.
			logger.FromContext(ctx).Error("protection cadence: scheduled snapshot FAILED — this resource has no new recovery point from this pass",
				"resource_id", res.ID, "app_id", res.AppID, "host_id", w.selfHost, "err", cerr)
			errs = append(errs, fmt.Errorf("capture resource %s: %w", res.ID, cerr))
			continue
		}
	}
	return errors.Join(errs...)
}

// beat records that this host's cadence worker completed the start of a pass. A
// failure is logged and swallowed — see RunOnce for why it must not abort.
func (w *FleetProtectionCadenceWorker) beat(ctx context.Context) {
	if err := w.ledger.UpsertProtectionHeartbeat(ctx, domain.ProtectionComponentCadence, w.selfHost.String(), w.now(), ""); err != nil {
		logger.FromContext(ctx).Error("protection cadence: the pass heartbeat could not be written — durable provisions on this fleet will refuse until it can be",
			"host_id", w.selfHost, "err", err)
	}
}
