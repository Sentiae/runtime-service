// Package volume implements the VolumeBackend port: it materializes the durable
// ext4 backing file a persistent volume attaches to as a 2nd virtio-blk device
// (runtime-fleet CP4 rt#9). Firecracker host only — off-host the fail-loud
// backend rejects every call so a volume is never silently faked.
package volume

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/sentiae/platform-kit/logger"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/usecase"
)

// minBackingMB is the floor size for a backing file so mkfs.ext4 has room to lay
// down a valid filesystem when a descriptor requests a tiny (or zero) volume.
const minBackingMB = 32

// stampLabel is written into the ext4 volume LABEL at create, alongside the
// filesystem UUID that carries the volume id. It is what makes identity
// verification possible AT ALL, and it is not decoration:
//
// every ext4 filesystem has a UUID, so "the UUID does not match the volume id"
// cannot by itself distinguish a volume this code stamped and an OLD volume
// formatted before the stamp existed (which carries mkfs's random UUID). Without
// a marker the only safe reading of every mismatch is "probably legacy, adopt",
// i.e. the check could never refuse anything. The label says "this file was
// created by the stamping code, so its UUID is authoritative" — and only then is
// a mismatch a real mismatch.
//
// ext4 labels are capped at 16 bytes; this is 11.
//
// Under the future CoW backend (ZFS, D-184 Phase 2) the same two facts become a
// dataset user property — presence of the property replaces the label, its value
// replaces the UUID — so the shape carries forward unchanged.
const stampLabel = "sentiae-vol"

// The siblings a restore parks beside a live backing file (see
// usecase/fleet_volume_restore.go). Both are FULL-VOLUME-SIZE and nothing ever
// removed them, so each rolled-back or interrupted restore stranded one forever
// (2.4GB observed live, #decommission-leaves-failed-restore-sibling). They are
// reclaimed here, and only here: when the volume they belong to is being deleted
// anyway, at which point they are unreadable garbage.
const (
	// failedRestoreInfix names the forensic copy a ROLLED-BACK restore keeps,
	// "<backing>.failed-<recovery-point-id>" (swapBack). Keeping it while the
	// volume lives is deliberate — it is the only evidence of what the recovery
	// point actually contained.
	failedRestoreInfix = ".failed-"
	// prerestoreSuffix names the pre-restore original a restore parks before it
	// installs the staged image, "<backing>.prerestore" (swapIn).
	prerestoreSuffix = ".prerestore"
)

// BackingStore materializes ext4 backing files under a host directory.
type BackingStore struct{}

var _ usecase.VolumeBackend = (*BackingStore)(nil)

// NewBackingStore constructs a BackingStore.
func NewBackingStore() *BackingStore { return &BackingStore{} }

// Ensure returns the backing file for a volume. An existing <Dir>/<volumeID>.ext4
// is ALWAYS returned unchanged (never re-formatted) so a re-provision or reboot
// re-attaches the same data; what an ABSENT file means is decided by in.Mode and
// never inferred here.
//
// ⚠ The mode is the data-loss control, and it is required. This backend is handed
// a path and cannot see the ledger, so it cannot tell a first provision (nothing
// to lose) from an attach whose data has vanished (everything to lose) — and the
// two demand opposite answers. It used to answer "create" to both, which meant a
// fleet host that lost its disk while the control-plane DB survived would mint a
// fresh empty filesystem for every surviving volume row and report success. The
// use case owns the ledger and therefore owns the decision (VolumeEnsureMode);
// deciding it here from the filesystem would put a ledger question in the one
// layer that cannot answer it.
func (b *BackingStore) Ensure(ctx context.Context, in usecase.VolumeEnsureInput) (usecase.VolumeEnsureOutput, error) {
	if in.Dir == "" {
		return usecase.VolumeEnsureOutput{}, fmt.Errorf("volume dir is required")
	}
	// No default: an unset mode is a wiring bug, and the only two ways to guess
	// are "create" (which is the data-loss path this control exists to close) and
	// "adopt" (which would silently break first provisions). Refuse instead.
	if in.Mode != usecase.VolumeEnsureCreate && in.Mode != usecase.VolumeEnsureAdopt {
		return usecase.VolumeEnsureOutput{}, fmt.Errorf("volume ensure mode is required (create|adopt), got %q", in.Mode)
	}
	if err := os.MkdirAll(in.Dir, 0o750); err != nil {
		return usecase.VolumeEnsureOutput{}, fmt.Errorf("create volume dir: %w", err)
	}
	path := filepath.Join(in.Dir, in.VolumeID.String()+".ext4")

	if st, err := os.Stat(path); err == nil {
		// Backing file already materialized — idempotent under BOTH modes, and
		// never re-formatted (that would destroy the persisted data).
		//
		// Under ADOPT the ledger is asserting "this volume's data is here", and a
		// stat proves only that A FILE is here. Those are different claims, so the
		// adopt path checks the file actually IS this volume before handing it to a
		// VM. Create is deliberately not checked: there is no ledger row yet, so
		// there is no identity to check the file against.
		if in.Mode == usecase.VolumeEnsureAdopt {
			if verr := b.verifyAdopted(ctx, in, path, st.Size()); verr != nil {
				return usecase.VolumeEnsureOutput{}, verr
			}
		}
		// Created stays FALSE: this call validated a file that already existed, it
		// did not bring it into being. The caller's compensation seam keys its only
		// delete on that distinction.
		return usecase.VolumeEnsureOutput{BackingPath: path, Created: false}, nil
	} else if !os.IsNotExist(err) {
		return usecase.VolumeEnsureOutput{}, fmt.Errorf("stat backing file: %w", err)
	}

	if in.Mode == usecase.VolumeEnsureAdopt {
		// The ledger says this volume exists; the host says its data does not. The
		// only honest move is to fail — loudly, because a silent refusal leaves an
		// operator staring at a failed deploy with no clue that a DISK is gone.
		logger.FromContext(ctx).Error(
			"fleet volume: backing file missing on attach — the ledger records this volume but its data is NOT on this host; refusing to create an empty replacement",
			"volume_id", in.VolumeID, "backing_path", path, "volume_dir", in.Dir)
		return usecase.VolumeEnsureOutput{}, fmt.Errorf("%w: %s", domain.ErrVolumeBackingFileMissing, path)
	}

	// Sparse backing file: truncate to the requested size, then format ext4.
	f, err := os.Create(path)
	if err != nil {
		return usecase.VolumeEnsureOutput{}, fmt.Errorf("create backing file: %w", err)
	}
	if terr := f.Truncate(backingBytes(in.SizeMB)); terr != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return usecase.VolumeEnsureOutput{}, fmt.Errorf("truncate backing file: %w", terr)
	}
	if cerr := f.Close(); cerr != nil {
		_ = os.Remove(path)
		return usecase.VolumeEnsureOutput{}, fmt.Errorf("close backing file: %w", cerr)
	}

	// TODO(rt#9-luks): wrap backing file with LUKS + Vault-Transit DEK once Vault is productionized
	//
	// -U stamps the volume id INTO the filesystem, so the file carries its own
	// identity and a later adopt can prove the data at this path is this volume's
	// rather than inferring it from the path (which is only a naming convention an
	// operator can break with one mv). -L marks it as stamped at all — see
	// stampLabel for why presence and value are two different facts.
	if o, e := exec.CommandContext(ctx, "mkfs.ext4", "-q", "-F",
		"-U", in.VolumeID.String(), "-L", stampLabel, path).CombinedOutput(); e != nil {
		_ = os.Remove(path)
		return usecase.VolumeEnsureOutput{}, fmt.Errorf("mkfs.ext4 backing file: %s: %w", strings.TrimSpace(string(o)), e)
	}
	// Created=true is reported ONLY here: past the absent-file check, past the
	// truncate and past a successful mkfs. Every earlier return either found the
	// file already present or failed, and in both cases the file is not this
	// invocation's to reclaim.
	return usecase.VolumeEnsureOutput{BackingPath: path, Created: true}, nil
}

// backingBytes is the size a backing file is materialized at, and therefore the
// size an adopt expects to find. It is ONE function on purpose: create clamps
// tiny/zero requests up to minBackingMB, so a verifier that compared against the
// raw ledger size would flag every small volume as undersized.
func backingBytes(sizeMB int64) int64 {
	if sizeMB < minBackingMB {
		sizeMB = minBackingMB
	}
	return sizeMB * 1024 * 1024
}

// verifyAdopted proves that the file already at a volume's backing path is that
// volume, before it is handed to a VM as the customer's data.
//
// Two independent claims are checked, and each has its own refusal:
//
//   - SIZE — the file must be at least what the ledger row says. A row whose
//     size was raised otherwise keeps attaching the smaller old filesystem
//     forever, and the guest hits ENOSPC far from the cause. Growing the file is
//     a deliberate operation (resize2fs), never a side effect of an attach, so
//     this refuses rather than repairs.
//   - IDENTITY — a STAMPED file's filesystem UUID must equal the volume id.
//
// Legacy files (created before the stamp) and files carrying no readable
// filesystem signature at all are ADOPTED, with a Warn recording that identity
// could not be verified. Refusing them would hard-fail every volume that predates
// this check — turning a hardening measure into a fleet-wide outage over data
// that is almost certainly fine. The Warn is the migration signal: once it stops
// appearing, every live volume is stamped.
//
// A failure to run the probe at all (blkid absent, unreadable) is treated the
// same way, for the same reason: an unverifiable file is not a proven-wrong file.
// The size check runs first and does NOT have that tolerance — it needs no tool.
func (b *BackingStore) verifyAdopted(ctx context.Context, in usecase.VolumeEnsureInput, path string, actualBytes int64) error {
	if want := backingBytes(in.SizeMB); actualBytes < want {
		logger.FromContext(ctx).Error(
			"fleet volume: backing file is SMALLER than its ledger row records — refusing to attach it; the row's size was raised without growing the filesystem, or this is not the file the row means",
			"volume_id", in.VolumeID, "backing_path", path, "actual_bytes", actualBytes, "expected_bytes", want)
		return fmt.Errorf("%w: %s is %d bytes, ledger records %d", domain.ErrVolumeBackingFileUndersized, path, actualBytes, want)
	}

	id, label, err := probeExtIdentity(ctx, path)
	switch {
	case err != nil:
		logger.FromContext(ctx).Warn(
			"fleet volume: could not read the backing file's filesystem identity, adopting UNVERIFIED — this file is being attached on the strength of its path alone",
			"volume_id", in.VolumeID, "backing_path", path, "err", err)
		return nil
	case label != stampLabel:
		logger.FromContext(ctx).Warn(
			"fleet volume: backing file carries no Sentiae identity stamp (created before stamping), adopting UNVERIFIED — its filesystem uuid is not authoritative, so it is being attached on the strength of its path alone",
			"volume_id", in.VolumeID, "backing_path", path, "fs_uuid", id, "fs_label", label)
		return nil
	case !strings.EqualFold(id, in.VolumeID.String()):
		// Stamped, and stamped as SOMETHING ELSE. This is the one case the whole
		// check exists for, and it is refused without touching the file: whatever
		// this is, it is evidence, and it is not this volume.
		logger.FromContext(ctx).Error(
			"fleet volume: the backing file at this volume's path belongs to a DIFFERENT volume — refusing to attach it; the path was reused or a sibling copy was renamed into place",
			"volume_id", in.VolumeID, "backing_path", path, "fs_uuid", id)
		return fmt.Errorf("%w: %s carries volume id %s, expected %s", domain.ErrVolumeIdentityMismatch, path, id, in.VolumeID)
	}
	return nil
}

// probeExtIdentity reads the filesystem UUID and LABEL off a backing file.
//
// blkid is run in LOW-LEVEL PROBE mode (-p). That is not a detail: without it
// blkid answers from its on-disk cache (/run/blkid/blkid.tab), keyed by device
// name — so a path whose file was replaced could be verified against the identity
// of the file that used to be there, which is precisely the failure this check
// exists to catch. -p reads the superblock every time.
//
// An empty/unformatted file makes blkid exit non-zero with no output; that is
// reported as an error and the caller treats it as unverifiable, not as wrong.
func probeExtIdentity(ctx context.Context, path string) (fsUUID string, label string, err error) {
	out, err := exec.CommandContext(ctx, "blkid", "-p", "-s", "UUID", "-s", "LABEL", "-o", "export", path).Output()
	if err != nil {
		return "", "", fmt.Errorf("blkid probe %s: %w", path, err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch k {
		case "UUID":
			fsUUID = v
		case "LABEL":
			label = v
		}
	}
	if fsUUID == "" {
		return "", "", fmt.Errorf("blkid probe %s reported no filesystem uuid", path)
	}
	return fsUUID, label, nil
}

// Delete removes a backing file AND the siblings this layout owns. A missing
// file is not an error (idempotent).
//
// The backend is the only component that knows a volume is a FILE LAYOUT rather
// than a single path, so it is the only place that can reclaim the whole layout.
// Deleting just the path the volume row carries left every parked sibling behind
// at full volume size with nothing left to point at it.
func (b *BackingStore) Delete(_ context.Context, backingPath string) error {
	if backingPath == "" {
		return nil
	}
	// Whether the volume itself is here decides which siblings are ours to take
	// (see removeRestoreSiblings), so it is sampled BEFORE the removal below.
	_, statErr := os.Stat(backingPath)
	if statErr != nil && !os.IsNotExist(statErr) {
		return fmt.Errorf("stat backing file: %w", statErr)
	}
	livePresent := statErr == nil

	if err := os.Remove(backingPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove backing file: %w", err)
	}
	return b.removeRestoreSiblings(backingPath, livePresent)
}

// removeRestoreSiblings reclaims the restore siblings of a backing file that has
// just been deleted: the "<backing>.failed-<recovery-point-id>" forensic copies
// and — only when the live volume was still present — "<backing>.prerestore".
//
// ⚠ THE livePresent GUARD IS A DATA-LOSS GUARD, not caution for its own sake.
// Mid-restore the live volume has been RENAMED to .prerestore, so the backing
// path does not exist and that sibling is the customer's ONLY remaining copy —
// the rollback's sole anchor. A delete landing in that window and removing it
// unconditionally would destroy the last copy of the data while the thing it was
// asked to delete was already gone. The rule is therefore exactly:
//
//	if the real volume is here and I am destroying it, every restore-in-flight
//	sibling beside it is moot; if the real volume is absent, a restore owns this
//	directory and I touch nothing of its.
//
// .failed-* needs no such guard and deliberately does not have one: it is a copy
// that can only exist AFTER a rollback already reinstated the original, so it is
// never anyone's only copy — and reclaiming it unconditionally is what makes a
// retry after a partially failed delete converge.
//
// ⚠ Matching is a LITERAL PREFIX test over the directory listing, deliberately
// not filepath.Glob:
//   - Glob would interpret any '*', '?', '[' or '\' occurring in the backing
//     path as pattern syntax, so a directory name containing one would silently
//     match files that are not ours (or nothing at all). There is no
//     filepath.QuoteMeta to defend with.
//   - The prefix is the EXACT base name plus ".failed-", and a candidate must
//     carry at least one further character (the recovery-point id). A
//     neighbouring volume can never satisfy that: every backing file is named
//     "<uuid>.ext4", which neither equals the prefix nor extends it, because a
//     uuid is fixed-length and contains no ".failed-". So the only names that
//     can match are siblings of THIS backing path.
//
// A directory is skipped even when its name matches: this layout only ever
// writes files, so a directory by that name is not ours to remove recursively.
//
// The first failure is returned (after every candidate has been attempted) so a
// caller sees that the volume's files are not fully gone; the volume row is then
// kept and the next delete retries, which converges because each removal is
// independent and idempotent.
func (b *BackingStore) removeRestoreSiblings(backingPath string, livePresent bool) error {
	dir := filepath.Dir(backingPath)
	base := filepath.Base(backingPath)
	failedPrefix := base + failedRestoreInfix
	prerestore := base + prerestoreSuffix

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("list volume dir %s: %w", dir, err)
	}
	var firstErr error
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !siblingOf(name, failedPrefix, prerestore, livePresent) {
			continue
		}
		if rerr := os.Remove(filepath.Join(dir, name)); rerr != nil && !os.IsNotExist(rerr) && firstErr == nil {
			firstErr = fmt.Errorf("remove restore sibling %s: %w", name, rerr)
		}
	}
	return firstErr
}

// siblingOf reports whether name is a restore sibling this delete may reclaim.
// The .failed- arm requires at least one further character (the recovery-point
// id), and the .prerestore arm is an EXACT name — neither can be satisfied by a
// neighbouring volume, whose files are all named "<uuid>.ext4".
func siblingOf(name, failedPrefix, prerestore string, livePresent bool) bool {
	if len(name) > len(failedPrefix) && strings.HasPrefix(name, failedPrefix) {
		return true
	}
	return livePresent && name == prerestore
}
