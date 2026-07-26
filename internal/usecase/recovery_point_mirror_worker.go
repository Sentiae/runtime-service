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
// The second failure domain, mirrored FROM THE CONTROL PLANE (D-200).
//
// ⚠ WHY THIS IS NOT ON THE FLEET HOST. The first version of this slice mirrored
// synchronously on the snapshot path, which meant the fleet host had to hold an
// off-chassis object-store credential. D-200 rejected that: the fleet host is
// TENANT-ADJACENT, and D-125 deliberately leaves its `svc/runtime` identity with
// ZERO standing Vault capability. Granting it a whole-bucket, all-tenant R2 token
// would have converted a non-exposure into a standing one — a new CLASS of
// exposure, not a second location for an existing one.
//
// So the copy moves to the control plane, which is already the all-tenant TCB: it
// holds the ledger and the primary object store itself. The fleet host writes only
// to the local primary and now holds NO off-chassis object-store credential at all.
// That is strictly stronger than any host-side scoping scheme, and it preserves
// D-125 to the letter instead of reversing it.
//
// ⚠ THE COST, STATED RATHER THAN HIDDEN. The mirror is no longer synchronous with
// the capture, so a fresh recovery point is single-domain for up to one pass
// interval — and a resource's FINAL recovery point (the decommission one) is
// mirrored by this worker afterwards rather than before the RPC returns. The ledger
// says so the whole time: the row is stamped `primary_only` at capture and only a
// CONFIRMED copy promotes it. What is visible is the truth, which is the property
// that matters more than the window.
//
// ⚠ SINGLE WRITER, SO NO CLAIMING PROTOCOL. There is exactly ONE control-plane
// instance (the mesh `runtime-service`; every other instance runs
// `executor_type=firecracker` and is gated out — see di.newRecoveryPointMirrorWorker),
// so two workers cannot race for the same multi-GB blob and no lease/claim table is
// built. If that ever stops being true the worst outcome is duplicated WORK, never a
// wrong ledger: the copy is idempotent ON THE OBJECT KEY (the same key carries the
// same bytes, and the second domain refuses deletes for 30 days regardless), and
// MarkRecoveryPointInSecondDomain is a no-op on a row that already holds the claim.
// ─────────────────────────────────────────────────────────────────────

// RecoveryPointMirrorEvery is how often the worker looks for unmirrored recovery
// points. One minute, matched to the durability collector: the census that reports
// how many copies sit in one failure domain refreshes on that cadence, so a slower
// mirror would leave the gauge describing a backlog the worker had already cleared.
// Passes never overlap (the loop is sequential), so on a busy fleet the interval is
// only the idle poll rate.
const RecoveryPointMirrorEvery = time.Minute

// RecoveryPointMirrorBatch bounds one pass. Each item is a full read of a
// multi-GB blob out of the primary store and a WAN write, so the batch exists to
// keep a pass bounded in time, not to keep it cheap — the backlog is drained
// oldest-first across passes.
const RecoveryPointMirrorBatch = 20

// recoveryPointMirrorRetryAfter is how long a FAILED recovery point is skipped
// before it is tried again.
//
// It exists because the batch is ordered oldest-first: a permanently unmirrorable
// row at the head (a blob the primary store can no longer serve — the ledger audit
// finds live instances of exactly that) would otherwise consume a batch slot on
// every single pass and starve every row behind it. The cooldown is IN MEMORY and
// deliberately not a column: 0023's schema is what the ledger CLAIMS, and a retry
// hint is not a claim about where data is. A restart simply retries sooner.
const recoveryPointMirrorRetryAfter = 15 * time.Minute

// RecoveryPointMirrorLedger is the narrow slice of the resource ledger the worker
// needs. It reads the backlog and records each outcome; it can reach nothing else.
type RecoveryPointMirrorLedger interface {
	// ListRecoveryPointsToMirror returns recovery points stamped primary_only,
	// oldest first.
	ListRecoveryPointsToMirror(ctx context.Context, limit int) ([]domain.FleetResourceRecoveryPoint, error)
	// MarkRecoveryPointInSecondDomain promotes a row to the two-domain class.
	MarkRecoveryPointInSecondDomain(ctx context.Context, id uuid.UUID, store string, at time.Time) error
	// RecordRecoveryPointMirrorFailure records the cause WITHOUT touching locations.
	RecordRecoveryPointMirrorFailure(ctx context.Context, id uuid.UUID, at time.Time, cause string) error
}

// RecoveryPointMirrorReport is one pass's outcome.
type RecoveryPointMirrorReport struct {
	// Considered is how many rows the backlog query returned.
	Considered int
	// Mirrored is how many were copied, CONFIRMED by checksum, and recorded.
	Mirrored int
	// Failed is how many could not be copied (or could not be recorded). Every one
	// of these leaves its row saying primary_only with the cause on it.
	Failed int
	// Deferred is how many were skipped because a recent attempt failed.
	Deferred int
}

// RecoveryPointMirrorWorker drains the primary_only backlog into the second
// failure domain.
//
// ⚠ IT NEVER PRETENDS. A copy that fails leaves the row primary_only with the cause
// recorded, logged at Error, and counted — never silently "succeeded". That is the
// single line this whole component exists to forbid: a one-domain copy reading as a
// two-domain one converts an alarming state into a reassuring one, which is worse
// than having no mirror at all.
type RecoveryPointMirrorWorker struct {
	ledger RecoveryPointMirrorLedger
	mirror SecondDomainMirror
	batch  int
	// now is injected so the cooldown and the metric timestamps are testable
	// without sleeping (§30.6).
	now func() time.Time

	// retryAfter is the per-row cooldown; a field so tests drive both sides of it.
	retryAfter time.Duration
	// cooldown maps a recovery point to the time it becomes eligible again. Bounded
	// by the backlog it has actually seen, and pruned as entries expire.
	cooldown map[uuid.UUID]time.Time

	stopCh   chan struct{}
	doneCh   chan struct{}
	stopOnce sync.Once
}

// NewRecoveryPointMirrorWorker constructs the worker. Both arguments are required:
// a nil ledger or a nil mirror is a wiring error, not a degraded mode — a control
// plane with no second domain configured must hold NO worker, so every recovery
// point stays honestly primary_only rather than being processed by something that
// copies nowhere.
func NewRecoveryPointMirrorWorker(ledger RecoveryPointMirrorLedger, mirror SecondDomainMirror) (*RecoveryPointMirrorWorker, error) {
	if ledger == nil {
		return nil, errors.New("recovery-point mirror worker: the resource ledger is required")
	}
	if mirror == nil {
		return nil, errors.New("recovery-point mirror worker: the second-domain mirror is required")
	}
	return &RecoveryPointMirrorWorker{
		ledger:     ledger,
		mirror:     mirror,
		batch:      RecoveryPointMirrorBatch,
		now:        func() time.Time { return time.Now().UTC() },
		retryAfter: recoveryPointMirrorRetryAfter,
		cooldown:   map[uuid.UUID]time.Time{},
		stopCh:     make(chan struct{}),
		doneCh:     make(chan struct{}),
	}, nil
}

// Domain names the second failure domain this worker copies into.
func (w *RecoveryPointMirrorWorker) Domain() string { return w.mirror.Domain() }

// Start runs the drain loop. The first pass fires immediately so a control plane
// that restarted with a backlog begins clearing it at once rather than after a
// full interval.
func (w *RecoveryPointMirrorWorker) Start(ctx context.Context) {
	go w.run(ctx)
}

// Stop signals the loop to exit and waits for it (shutdown group, §21). Waiting
// matters here specifically: a pass killed mid-copy would leave a partial object
// under a key the ledger has not claimed, and the next pass must be the thing that
// overwrites it, not a torn shutdown.
func (w *RecoveryPointMirrorWorker) Stop() {
	w.stopOnce.Do(func() { close(w.stopCh) })
	<-w.doneCh
}

func (w *RecoveryPointMirrorWorker) run(ctx context.Context) {
	defer close(w.doneCh)
	defer func() {
		if r := recover(); r != nil {
			logger.FromContext(ctx).Error("recovery-point mirror worker panicked — recovery points will stay in ONE failure domain until this process is restarted", "panic", r)
		}
	}()
	w.pass(ctx)
	t := time.NewTicker(RecoveryPointMirrorEvery)
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

// pass runs one drain and publishes it. A failed pass is logged and counted; it
// does NOT advance the last-success timestamp, which is the only thing that makes
// a stalled mirror distinguishable from an idle one.
//
// ⚠ NEITHER DOES A PASS THAT ONLY DEFERRED. A row is on cooldown BECAUSE it just
// failed, so for the fifteen minutes that follow, every pass would otherwise skip it
// and stamp a clean success — `now() - last_success` never grows and a permanently
// unmirrorable head-of-queue row reads as a healthy idle mirror. The timestamp means
// "this pass left nothing on one chassis", which a deferred row contradicts as much
// as a failed one does. The cooldown still does its job (the row does not consume a
// batch slot); it just no longer buys silence.
func (w *RecoveryPointMirrorWorker) pass(ctx context.Context) {
	rep, err := w.RunOnce(ctx)
	if err != nil {
		logger.FromContext(ctx).Error("recovery-point mirror pass FAILED — the recovery points it could not copy exist on ONE chassis only, and their rows say so",
			"second_domain", w.mirror.Domain(),
			"considered", rep.Considered, "mirrored", rep.Mirrored, "failed", rep.Failed, "deferred", rep.Deferred,
			"err", err)
		return
	}
	if rep.Deferred > 0 {
		logger.FromContext(ctx).Warn("recovery-point mirror pass skipped recovery points that are still cooling down from an earlier failure — they exist on ONE chassis and this pass did not change that, so it is NOT recorded as a success",
			"second_domain", w.mirror.Domain(),
			"considered", rep.Considered, "mirrored", rep.Mirrored, "deferred", rep.Deferred,
			"retry_after", w.retryAfter)
		return
	}
	PublishRecoveryPointMirrorPass(w.now())
}

// RunOnce drains one batch and returns what it did.
//
// The returned error is the PASS's verdict, never the copy's: a recovery point that
// could not be mirrored is recorded on its own row and folded into rep.Failed, and
// the joined error is what withholds the pass's success timestamp. Exported so the
// behaviour is directly testable without a timer.
func (w *RecoveryPointMirrorWorker) RunOnce(ctx context.Context) (RecoveryPointMirrorReport, error) {
	var rep RecoveryPointMirrorReport
	points, err := w.ledger.ListRecoveryPointsToMirror(ctx, w.batch)
	if err != nil {
		return rep, fmt.Errorf("list recovery points to mirror: %w", err)
	}
	rep.Considered = len(points)

	now := w.now()
	w.pruneCooldown(now)

	var errs []error
	for i := range points {
		if err := ctx.Err(); err != nil {
			// A cancelled context ends the pass rather than converting every remaining
			// row into a recorded "failure" whose cause is our own shutdown.
			errs = append(errs, err)
			break
		}
		rp := &points[i]
		if until, ok := w.cooldown[rp.ID]; ok && w.now().Before(until) {
			rep.Deferred++
			continue
		}
		if err := w.mirrorOne(ctx, rp); err != nil {
			rep.Failed++
			errs = append(errs, err)
			continue
		}
		rep.Mirrored++
	}
	return rep, errors.Join(errs...)
}

// mirrorOne copies and records ONE recovery point.
//
// ⚠ Every failure path below writes the cause to the row and returns the error.
// There is no branch that discards it — `_ = err` on this path would let a
// single-domain copy read as a two-domain one, which is the one outcome the whole
// component exists to prevent.
func (w *RecoveryPointMirrorWorker) mirrorOne(ctx context.Context, rp *domain.FleetResourceRecoveryPoint) error {
	log := logger.FromContext(ctx)
	receipt, err := w.mirror.Mirror(ctx, rp.ObjectKey, rp.Checksum)
	if err != nil {
		w.deferRetry(rp.ID)
		log.Error("recovery point could NOT be copied to a second failure domain — it exists ONLY on the fleet chassis, so losing that machine loses it",
			"resource_id", rp.ResourceID, "recovery_point_id", rp.ID, "object_key", rp.ObjectKey,
			"second_domain", w.mirror.Domain(), "err", err)
		recordRecoveryPointMirror(mirrorOutcomeFailed)
		if rerr := w.ledger.RecordRecoveryPointMirrorFailure(ctx, rp.ID, w.now(), err.Error()); rerr != nil {
			// The row keeps saying primary_only either way (RecordRecoveryPointMirrorFailure
			// never touches `locations`), so durability is not overstated — but the CAUSE
			// is now only in this log line, which is worth saying out loud.
			log.Error("the second-domain failure above could not be recorded on the recovery point, so only this log line carries the cause",
				"recovery_point_id", rp.ID, "err", rerr)
			return fmt.Errorf("mirror recovery point %s: %w (and recording the cause failed: %v)", rp.ID, err, rerr)
		}
		return fmt.Errorf("mirror recovery point %s: %w", rp.ID, err)
	}

	// Confirmed and checksum-verified — only NOW may the row claim two domains.
	if rerr := w.ledger.MarkRecoveryPointInSecondDomain(ctx, rp.ID, receipt.Domain, receipt.At); rerr != nil {
		// The copy IS in the second domain; the LEDGER does not know it. Understating
		// durability is the safe direction of this failure, and the next pass re-copies
		// (idempotent on the key) and re-records. It is counted as a failure because the
		// two-domain claim was not established.
		w.deferRetry(rp.ID)
		recordRecoveryPointMirror(mirrorOutcomeFailed)
		log.Error("the recovery point WAS copied to the second failure domain but the ledger could not be updated — it keeps reporting as single-domain",
			"resource_id", rp.ResourceID, "recovery_point_id", rp.ID, "second_domain", receipt.Domain, "err", rerr)
		return fmt.Errorf("record recovery point %s in second domain: %w", rp.ID, rerr)
	}
	delete(w.cooldown, rp.ID)
	recordRecoveryPointMirror(mirrorOutcomeMirrored)
	log.Info("recovery point confirmed in a SECOND failure domain",
		"resource_id", rp.ResourceID, "recovery_point_id", rp.ID, "object_key", rp.ObjectKey,
		"second_domain", receipt.Domain, "verified_bytes", receipt.Bytes)
	return nil
}

// deferRetry puts a failed recovery point on cooldown so it cannot monopolise the
// oldest-first batch.
func (w *RecoveryPointMirrorWorker) deferRetry(id uuid.UUID) {
	w.cooldown[id] = w.now().Add(w.retryAfter)
}

// pruneCooldown drops expired entries so the map cannot grow without bound over a
// long-lived process.
func (w *RecoveryPointMirrorWorker) pruneCooldown(now time.Time) {
	for id, until := range w.cooldown {
		if !now.Before(until) {
			delete(w.cooldown, id)
		}
	}
}
