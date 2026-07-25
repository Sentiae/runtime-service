package usecase

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/sentiae/platform-kit/logger"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/port/gateway"
	"github.com/sentiae/runtime-service/internal/repository"
)

// residentPGPort is the fixed in-guest port a dedicated Postgres data-VM listens
// on (mirrors the dedicated provision descriptor). Referenced by the resource
// provisioner's endpoint; kept here so the snapshot + provision paths agree.
const residentPGPort = 5432

const (
	// defaultThawDeadline bounds the post-upload thaw retries. The snapshot bytes
	// are already in the store by then, so this is only about not leaving guest
	// writers blocked.
	defaultThawDeadline = 10 * time.Second
	// defaultThawRetry is the pause between thaw attempts inside that deadline.
	defaultThawRetry = 500 * time.Millisecond
	// defaultThawAttempt bounds ONE thaw attempt. It must sit comfortably under
	// defaultThawDeadline or there are no retries at all: the control client's own
	// timeout is 30s (firecracker.controlCallTimeout, sized for a syncfs over a
	// large dirty page cache), so without a per-attempt deadline the first attempt
	// alone outlives the whole budget and the loop below runs exactly once.
	defaultThawAttempt = 2 * time.Second
	// defaultFreezeHeartbeat is how often the upload RENEWs the guest's dead-man
	// auto-thaw. The guest clamps the window to 60s and lets the host only SHORTEN
	// it (guestcontrol.DefaultDeadMan / deadManFor), and with the vCPUs running
	// that timer advances during the upload — so an upload longer than the window
	// would auto-thaw mid-flight and produce a torn snapshot. A third of the window
	// leaves room for two missed beats before the guest gives up on us.
	defaultFreezeHeartbeat = 20 * time.Second
)

// ─────────────────────────────────────────────────────────────────────
// FleetVolumeSnapshotter use case (D-080).
// ─────────────────────────────────────────────────────────────────────

// FleetVolumeSnapshotter takes GUEST-CONSISTENT snapshots of a resource's
// persistent volumes and records them as durable recovery points (D-080, CP4.5
// §9 #3). For an attached volume it quiesces the guest filesystem (syncfs +
// fsfreeze over the D-185a control channel) and then STREAMS THE BACKING FILE
// ITSELF into the artifact store while the freeze holds, thawing once the upload
// has finished.
//
// Consistency comes from the GUEST freeze and from NOTHING else. A frozen
// filesystem cannot change, which is exactly what makes the backing file stable
// while it is read with the vCPUs still running. A guest that cannot be
// quiesced is REFUSED (ErrSnapshotNotQuiescible) — no recovery point, no
// SnapshotRef — because everything downstream treats a recovery point as
// trustworthy and the checksum gate proves only transport integrity, never
// source consistency.
//
// ⚠ DO NOT REINTRODUCE A LOCAL STAGING COPY ("snapshot the file quickly, upload
// the copy later"). It looks like the safe, freeze-shortening option and it is a
// product-breaking one:
//
//   - It buys NO consistency the freeze does not already give. The staging copy
//     was only ever standing in for "the source cannot change while we upload" —
//     and a FROZEN filesystem cannot change, so the copy is redundant, not safer.
//   - It requires a full second copy of the volume on the host. A host without
//     that headroom cannot snapshot; and because a durable resource refuses
//     decommission without a FINAL snapshot, it cannot free the space by deleting
//     the resource either. That is a deadlock, not a slowdown — measured live at
//     ~22GB free against a 20GB volume class.
//
// The freeze is therefore held for the whole upload (heartbeat below), which is
// the price of never needing local headroom.
//
// ⚠ DO NOT REINTRODUCE A VMM Pause AROUND THE CAPTURE. It looks like an obvious
// extra safety measure and it is not one:
//
//   - It adds no consistency the freeze does not already give (Firecracker's
//     Pause stops vCPUs without flushing the guest kernel's dirty page cache —
//     #p19-snapshot-not-guest-consistent — so it was never the thing making the
//     capture safe).
//   - Firecracker v1.16.0's VSOCK DEVICE DOES NOT SURVIVE Pause/Resume. After
//     the first pause the guest control channel is dead for the rest of the VM's
//     life: the host-side CONNECT handshake gets no reply at all (the guest is
//     provably still alive — Postgres keeps answering TCP, the console keeps
//     logging — and a second pause/resume does not recover it). Firecracker's
//     own resume log shows net and block being kicked back to life
//     ("[Net:eth0] notifying queues", "[Block:data] notifying queues") while
//     vsock only "signals its event queue" and stays dead.
//   - The consequence shipped for one release: every snapshot failed to Thaw,
//     the host escalated by killing and rebooting the customer's Postgres (~45s
//     down, ~32s of it with /data frozen), and any VM paused once was
//     permanently unsnapshottable.
//
// The pause's only real job was suspending the in-guest dead-man auto-thaw for
// the length of the capture. That job now belongs to the freeze heartbeat below.
type FleetVolumeSnapshotter struct {
	guest    gateway.GuestControl
	store    ArtifactStore
	volumes  repository.VolumeRepository
	replicas repository.ReplicaRepository
	recovery repository.FleetResourceRepository

	// Thaw + heartbeat budget. Fields (not constants) so tests drive the retry and
	// heartbeat paths without waiting on the real timers.
	thawDeadline    time.Duration
	thawRetry       time.Duration
	thawAttempt     time.Duration
	freezeHeartbeat time.Duration
}

// NewFleetVolumeSnapshotter constructs the use case. guest is the post-boot
// control channel every attached-volume snapshot quiesces through.
func NewFleetVolumeSnapshotter(
	guest gateway.GuestControl,
	store ArtifactStore,
	volumes repository.VolumeRepository,
	replicas repository.ReplicaRepository,
	recovery repository.FleetResourceRepository,
) *FleetVolumeSnapshotter {
	return &FleetVolumeSnapshotter{
		guest:           guest,
		store:           store,
		volumes:         volumes,
		replicas:        replicas,
		recovery:        recovery,
		thawDeadline:    defaultThawDeadline,
		thawRetry:       defaultThawRetry,
		thawAttempt:     defaultThawAttempt,
		freezeHeartbeat: defaultFreezeHeartbeat,
	}
}

// SnapshotAppVolumes snapshots every persistent volume of the app backing a
// resource and returns the recovery points it created. It aborts on the first
// volume that fails (a partial snapshot of a data resource is not a recovery
// point the caller can trust).
//
// ⚠ An app with NO volumes returns ([], nil) — success, zero recovery points.
// That is honest (there was nothing to snapshot) but it means a successful CALL
// is not evidence a recovery point EXISTS. Every caller whose guarantee depends
// on one existing must check the length itself: the SnapshotResource RPC does,
// and so does the snapshot-first DecommissionDedicated.
func (s *FleetVolumeSnapshotter) SnapshotAppVolumes(ctx context.Context, resourceID, appID uuid.UUID) ([]domain.FleetResourceRecoveryPoint, error) {
	vols, err := s.volumes.ListByApp(ctx, appID)
	if err != nil {
		return nil, fmt.Errorf("list volumes: %w", err)
	}
	points := make([]domain.FleetResourceRecoveryPoint, 0, len(vols))
	for i := range vols {
		rp, err := s.snapshotVolume(ctx, resourceID, &vols[i])
		if err != nil {
			return nil, err
		}
		points = append(points, rp)
	}
	return points, nil
}

// snapshotVolume snapshots one volume. Attached: quiesce the guest (syncfs +
// freeze) → fsync the host fd → stream the backing file straight to the artifact
// store (with the dead-man heartbeat holding the freeze open for the whole
// upload) → thaw → recovery-point row → SnapshotRef. A guest that cannot be
// quiesced, a dead-man re-arm that fails, and an upload that fails all abort with
// NO recovery-point row.
//
// Nothing is staged on local disk on either branch: the peak host disk a snapshot
// costs is the compressor's buffers, not a second copy of the volume.
func (s *FleetVolumeSnapshotter) snapshotVolume(ctx context.Context, resourceID uuid.UUID, vol *domain.Volume) (domain.FleetResourceRecoveryPoint, error) {
	var zero domain.FleetResourceRecoveryPoint
	if vol.BackingPath == "" {
		return zero, fmt.Errorf("volume %s has no backing file to snapshot", vol.ID)
	}
	snapshotID := uuid.New()
	objectKey := fmt.Sprintf("volumes/%s/%s.ext4", vol.ID, snapshotID)

	var up snapshotUpload
	if vol.AttachedReplica != nil {
		replica, err := s.replicas.FindByID(ctx, *vol.AttachedReplica)
		if err != nil {
			return zero, fmt.Errorf("load attached replica: %w", err)
		}
		// Quiesce the GUEST first. Everything after this reads a filesystem the
		// guest kernel has flushed and frozen; without it the upload below can miss
		// or tear writes the guest already acked.
		if err := s.quiesce(ctx, replica); err != nil {
			return zero, err
		}
		// From here the guest filesystem is FROZEN — every exit path must thaw it.
		// The vCPUs keep running throughout (see the type comment on why pausing
		// them is forbidden), so the upload reads a filesystem that cannot change
		// while the guest's other work is undisturbed.
		var upErr error
		up, upErr = s.uploadUnderFreeze(ctx, replica, vol.BackingPath, objectKey)
		s.thawAfterUpload(ctx, replica)
		if upErr != nil {
			// Nothing was stored (or what was stored is unreferenced): abort with no
			// catalog row.
			return zero, fmt.Errorf("upload snapshot: %w", upErr)
		}
	} else {
		// No attached replica: nothing is writing the file, so there is no guest to
		// quiesce and nothing a staging copy could protect against — streaming is
		// byte-for-byte what copying-then-uploading would have produced. This
		// snapshot is only as consistent as the last STOP of the engine that wrote
		// it was clean — which today it never is, because a resident stop is a VMM
		// kill (#resident-stop-is-vmm-kill). Making the detached branch trustworthy
		// depends on that fix, not on this one.
		var upErr error
		up, upErr = s.uploadBackingFile(ctx, vol.BackingPath, objectKey)
		if upErr != nil {
			return zero, fmt.Errorf("upload snapshot: %w", upErr)
		}
	}
	// Both sizes, because they diverge: the recovery point records the LOGICAL
	// size (what a restore materializes) while the transfer only pays for the
	// compressed bytes. The gap is the volume's holes, and it is the number to
	// look at when a snapshot is slower than the data it holds.
	logger.FromContext(ctx).Info("fleet snapshot: uploaded",
		"volume_id", vol.ID, "object_key", objectKey,
		"logical_bytes", up.LogicalBytes, "stored_bytes", up.StoredBytes)

	now := time.Now().UTC()
	volID := vol.ID
	rp := domain.FleetResourceRecoveryPoint{
		ID:         snapshotID,
		ResourceID: resourceID,
		VolumeID:   &volID,
		ObjectKey:  objectKey,
		Kind:       "snapshot",
		SizeBytes:  up.LogicalBytes,
		Checksum:   up.Checksum,
		Verified:   false,
		CreatedAt:  now,
	}
	if err := s.recovery.SaveRecoveryPoint(ctx, &rp); err != nil {
		return zero, fmt.Errorf("save recovery point: %w", err)
	}

	vol.SnapshotRef = objectKey
	vol.UpdatedAt = now
	if err := s.volumes.Update(ctx, vol); err != nil {
		return zero, fmt.Errorf("update volume snapshot ref: %w", err)
	}
	return rp, nil
}

// quiesce flushes and then freezes the guest's data filesystem over the D-185a
// control channel. Any failure refuses the snapshot (ErrSnapshotNotQuiescible):
// reading an unfrozen guest's volume may tear, and a torn copy recorded as a
// recovery point is indistinguishable from a good one forever after.
func (s *FleetVolumeSnapshotter) quiesce(ctx context.Context, replica *domain.Replica) error {
	if s.guest == nil {
		return quiesceRefusal(replica, "reach", domain.ErrGuestControlUnavailable)
	}
	if err := s.guest.SyncFS(ctx, replica.SocketPath); err != nil {
		return quiesceRefusal(replica, "flush", err)
	}
	if err := s.guest.Freeze(ctx, replica.SocketPath); err != nil {
		// The guest arms its dead-man only on a freeze it completed, so a REFUSED
		// freeze left nothing frozen — but a freeze whose response was lost looks
		// identical from here and did leave the guest frozen. One best-effort thaw
		// (benign on an unfrozen filesystem) collapses that window instead of
		// waiting out the dead-man, which stays the backstop.
		if terr := s.guest.Thaw(ctx, replica.SocketPath); terr != nil {
			logger.FromContext(ctx).Warn("fleet snapshot: thaw after failed freeze",
				"replica_id", replica.ID, "err", terr)
		}
		return quiesceRefusal(replica, "freeze", err)
	}
	return nil
}

// quiesceRefusal wraps a control-channel failure in the refusal sentinel. The
// two causes get different text because they send an operator to DIFFERENT
// fixes, and telling them the wrong one costs a wasted reboot or a wasted hunt:
//
//   - ErrGuestControlUnavailable — the host holds no control token for this VM,
//     i.e. it was booted BEFORE the channel existed (D-185a) or with no sealed
//     push. It has never had a channel, so no retry can ever succeed; only a
//     reboot gives it one.
//   - anything else — the channel IS armed for this VM and the call still
//     failed: it was reachable at boot and is not answering (or the guest
//     refused) NOW. A reboot is not the first move; read the guest console and
//     the runtime logs.
func quiesceRefusal(replica *domain.Replica, op string, cause error) error {
	if errors.Is(cause, domain.ErrGuestControlUnavailable) {
		return fmt.Errorf("%w: replica %s has NO guest control channel — this VM was booted BEFORE the channel existed (D-185a), so it can never be quiesced as it stands and no retry will help; reboot the replica (it comes back with a channel) and snapshot again: %w",
			domain.ErrSnapshotNotQuiescible, replica.ID, cause)
	}
	return fmt.Errorf("%w: replica %s HAS a guest control channel but the %s call failed — the channel was armed at boot and is unreachable or refusing now; check the guest console and the runtime-service logs before rebooting anything: %w",
		domain.ErrSnapshotNotQuiescible, replica.ID, op, cause)
}

// uploadUnderFreeze streams the backing file into the artifact store while the
// guest filesystem stays frozen. The freeze covers the WHOLE upload — that is
// what removes the local staging copy, and with it the host-headroom deadlock
// (see the type comment).
//
// It is the freeze — not any host-side or VMM-side trick — that makes reading a
// live volume's backing file safe, so the ONE thing that can invalidate the
// snapshot is the guest thawing itself mid-flight. With the vCPUs running (they
// must be: see the type comment), the dead-man auto-thaw the guest armed at
// Freeze keeps counting, and the guest clamps that window to 60s and lets the
// host only SHORTEN it. So the upload RENEWS it on an interval — the op that
// extends the window while touching the filesystem not at all. Re-sending FREEZE
// cannot do this job: FIFREEZE on an already-frozen filesystem returns EBUSY, and
// the guest re-arms only on a freeze that succeeded, so the re-arm would fail
// precisely when it is needed.
//
// A renew that FAILS invalidates the snapshot — either the dead-man can now fire
// at any moment, or (ErrDeadManNotArmed) the freeze is already gone — so the
// upload is aborted (its source reads are context-bound, see ctxReader) and the
// snapshot fails. Continuing would produce exactly the torn recovery point this
// whole path exists to prevent, and a torn recovery point is indistinguishable
// from a good one forever after.
func (s *FleetVolumeSnapshotter) uploadUnderFreeze(ctx context.Context, replica *domain.Replica, src, objectKey string) (snapshotUpload, error) {
	uploadCtx, abort := context.WithCancel(ctx)
	defer abort()

	heartbeatErr := make(chan error, 1)
	heartbeatDone := make(chan struct{})
	go s.holdFreeze(uploadCtx, abort, replica, heartbeatErr, heartbeatDone)

	up, err := s.uploadBackingFile(uploadCtx, src, objectKey)

	abort()
	<-heartbeatDone
	// A heartbeat failure outranks the upload's own error: when it aborts the
	// upload, the upload's error is just "cancelled", and the real, actionable
	// cause is the re-arm that did not land.
	select {
	case hbErr := <-heartbeatErr:
		return snapshotUpload{}, hbErr
	default:
	}
	if err != nil {
		return snapshotUpload{}, err
	}
	return up, nil
}

// uploadBackingFile fsyncs the host's view of the backing file and streams THAT
// descriptor through the shared gzip + sha256 + store pipeline. No intermediate
// file exists anywhere, so the snapshot needs no local headroom at all.
//
// The fsync is the design's mandated park-capture ordering
// (docs/designs/sentiae-database-platform.md §4.2). It is close to a no-op today
// because the upload reads back through that same page cache, but it becomes
// load-bearing the moment the snapshot is taken BELOW the cache (a ZFS dataset
// snapshot in Phase 2 captures the block layer only, and "memory believes a write
// the snapshot lacks" is the silent-corruption direction). Opening read-only is
// enough: fsync(2) flushes the inode's dirty pages regardless of the descriptor's
// access mode, and read-only is the weaker handle to take on a live VM's disk.
//
// The truncation guards inside uploadSnapshotStreamHashed are what make streaming
// as trustworthy as staging was: the bytes read must equal the source's Stat()
// size and the store must have consumed every compressed byte, so a source that
// shrank or a store that stopped early cannot mint a checksum over a prefix.
func (s *FleetVolumeSnapshotter) uploadBackingFile(ctx context.Context, src, objectKey string) (snapshotUpload, error) {
	f, err := os.Open(src)
	if err != nil {
		return snapshotUpload{}, fmt.Errorf("open backing file %s to snapshot: %w", src, err)
	}
	// Sole owner of the descriptor: uploadSnapshotStreamHashed joins its compressor
	// before returning, so this Close never races a reader.
	defer f.Close()
	if err := f.Sync(); err != nil {
		return snapshotUpload{}, fmt.Errorf("fsync backing file %s: %w", src, err)
	}
	return uploadSnapshotStreamHashed(ctx, s.store, objectKey, src, f)
}

// holdFreeze renews the guest's dead-man until ctx ends, reporting the first
// failed renew on failed and aborting the upload through abort. It closes done on
// exit so the caller can join it deterministically instead of racing the upload.
func (s *FleetVolumeSnapshotter) holdFreeze(ctx context.Context, abort context.CancelFunc, replica *domain.Replica, failed chan<- error, done chan<- struct{}) {
	defer close(done)
	defer func() {
		if r := recover(); r != nil {
			// A panicked heartbeat is a stopped heartbeat, so it invalidates the upload
			// exactly like a failed re-arm does.
			select {
			case failed <- fmt.Errorf("guest dead-man heartbeat panicked: %v", r):
			default:
			}
			abort()
			logger.FromContext(ctx).Error("fleet snapshot: dead-man heartbeat panicked",
				"replica_id", replica.ID, "panic", r)
		}
	}()

	ticker := time.NewTicker(s.freezeHeartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.guest.Renew(ctx, replica.SocketPath); err != nil {
				select {
				case failed <- fmt.Errorf("re-arm guest dead-man mid-upload for replica %s: %w", replica.ID, err):
				default:
				}
				abort()
				return
			}
		}
	}
}

// thawAfterUpload releases the freeze, retrying inside a bounded deadline.
//
// A thaw failure never fails the snapshot — the bytes are already in the store by
// the time this runs — and it deliberately does NOT kill the replica either. The
// guest's own dead-man auto-thaw is the designed backstop and releases the
// filesystem within its window; killing the VM would trade that bounded stall
// for a full database restart, which is strictly worse for the customer. It stays
// loud so the condition is still investigated.
func (s *FleetVolumeSnapshotter) thawAfterUpload(ctx context.Context, replica *domain.Replica) {
	lastErr := s.thawWithRetries(ctx, replica)
	if lastErr == nil {
		return
	}
	logger.FromContext(ctx).Error("fleet snapshot: guest filesystem NOT thawed after retries — leaving the replica RUNNING and relying on the guest's dead-man auto-thaw to release it within its window; the guest control channel is not answering and needs investigation",
		"replica_id", replica.ID, "thaw_deadline", s.thawDeadline.String(), "err", lastErr)
}

// thawWithRetries thaws until it succeeds, the deadline passes, or ctx ends,
// returning the last failure (nil on success).
//
// Each attempt carries its OWN deadline: the control client's timeout is 30s,
// three times the whole budget here, so without one the first attempt would
// consume the entire deadline and the "retries" would never run (they did not,
// before this).
func (s *FleetVolumeSnapshotter) thawWithRetries(ctx context.Context, replica *domain.Replica) error {
	if s.guest == nil {
		// Unreachable: quiesce already refused a snapshot with no control channel.
		return domain.ErrGuestControlUnavailable
	}
	deadline := time.Now().Add(s.thawDeadline)
	for {
		err := s.thawOnce(ctx, replica)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil || !time.Now().Before(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
		case <-time.After(s.thawRetry):
		}
	}
}

// thawOnce bounds one thaw attempt with its own context deadline, which is the
// only way to shorten the call without changing the control client's timeout for
// every other caller (a syncfs over a large dirty page cache legitimately needs
// the long one).
func (s *FleetVolumeSnapshotter) thawOnce(ctx context.Context, replica *domain.Replica) error {
	attemptCtx, cancel := context.WithTimeout(ctx, s.thawAttempt)
	defer cancel()
	return s.guest.Thaw(attemptCtx, replica.SocketPath)
}
