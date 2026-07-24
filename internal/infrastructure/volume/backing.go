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

	"github.com/sentiae/runtime-service/internal/usecase"
)

// minBackingMB is the floor size for a backing file so mkfs.ext4 has room to lay
// down a valid filesystem when a descriptor requests a tiny (or zero) volume.
const minBackingMB = 32

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

// Ensure returns the backing file for a volume, creating it if absent. It is
// idempotent: an existing <Dir>/<volumeID>.ext4 is returned unchanged so a
// re-provision or reboot re-attaches the same data.
func (b *BackingStore) Ensure(_ context.Context, in usecase.VolumeEnsureInput) (usecase.VolumeEnsureOutput, error) {
	if in.Dir == "" {
		return usecase.VolumeEnsureOutput{}, fmt.Errorf("volume dir is required")
	}
	if err := os.MkdirAll(in.Dir, 0o750); err != nil {
		return usecase.VolumeEnsureOutput{}, fmt.Errorf("create volume dir: %w", err)
	}
	path := filepath.Join(in.Dir, in.VolumeID.String()+".ext4")

	if _, err := os.Stat(path); err == nil {
		// Backing file already materialized — idempotent, never re-format (that
		// would destroy the persisted data).
		return usecase.VolumeEnsureOutput{BackingPath: path}, nil
	} else if !os.IsNotExist(err) {
		return usecase.VolumeEnsureOutput{}, fmt.Errorf("stat backing file: %w", err)
	}

	sizeMB := in.SizeMB
	if sizeMB < minBackingMB {
		sizeMB = minBackingMB
	}

	// Sparse backing file: truncate to the requested size, then format ext4.
	f, err := os.Create(path)
	if err != nil {
		return usecase.VolumeEnsureOutput{}, fmt.Errorf("create backing file: %w", err)
	}
	if terr := f.Truncate(sizeMB * 1024 * 1024); terr != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return usecase.VolumeEnsureOutput{}, fmt.Errorf("truncate backing file: %w", terr)
	}
	if cerr := f.Close(); cerr != nil {
		_ = os.Remove(path)
		return usecase.VolumeEnsureOutput{}, fmt.Errorf("close backing file: %w", cerr)
	}

	// TODO(rt#9-luks): wrap backing file with LUKS + Vault-Transit DEK once Vault is productionized
	if o, e := exec.Command("mkfs.ext4", "-q", "-F", path).CombinedOutput(); e != nil {
		_ = os.Remove(path)
		return usecase.VolumeEnsureOutput{}, fmt.Errorf("mkfs.ext4 backing file: %s: %w", strings.TrimSpace(string(o)), e)
	}
	return usecase.VolumeEnsureOutput{BackingPath: path}, nil
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
