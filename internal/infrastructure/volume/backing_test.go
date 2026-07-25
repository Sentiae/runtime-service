package volume

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/usecase"
)

// write creates a file with a byte of content so its existence is unambiguous.
func write(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	if err == nil {
		return true
	}
	if !os.IsNotExist(err) {
		t.Fatalf("stat %s: %v", path, err)
	}
	return false
}

// TestEnsureModeDecidesWhatAnAbsentFileMeans is the data-loss regression. An
// absent backing file is not one condition but two, and the backend cannot tell
// them apart from the filesystem: on a FIRST provision it means "make it", on an
// ATTACH it means the customer's data is gone. Inferring create-if-absent made
// the second silently become the first — a fleet host that lost its disk while
// the control-plane DB survived would mint a fresh empty filesystem for every
// surviving volume row and report the deploy healthy.
//
// The adopt-with-no-file row is the whole bug: it must REFUSE and leave the
// directory untouched. Revert the mode guard in Ensure and that row fails by
// finding a file that was created behind the ledger's back.
func TestEnsureModeDecidesWhatAnAbsentFileMeans(t *testing.T) {
	// A byte pattern that no mkfs.ext4 output could coincidentally reproduce, so
	// "adopted unchanged" is proven by the CONTENT, not merely by the file's
	// existence — a re-format would leave a superblock here instead.
	sentinel := []byte("SENTIAE-VOLUME-DATA-DO-NOT-REFORMAT")

	tests := []struct {
		name string
		mode usecase.VolumeEnsureMode
		// filePresent seeds the backing file with the sentinel pattern first.
		filePresent bool
		wantErr     error
		// wantFile is whether a backing file must exist when Ensure returns.
		wantFile bool
		// wantPreserved asserts the sentinel content survived (adoption), and
		// implies no mkfs was run over it.
		wantPreserved bool
		// needsMkfs marks the row that actually formats a filesystem.
		needsMkfs bool
	}{
		{
			name:      "first provision, no file, create intent → materialized",
			mode:      usecase.VolumeEnsureCreate,
			wantFile:  true,
			needsMkfs: true,
		},
		{
			name:          "attach intent, file present → adopted unchanged, never re-formatted",
			mode:          usecase.VolumeEnsureAdopt,
			filePresent:   true,
			wantFile:      true,
			wantPreserved: true,
		},
		{
			name:      "attach intent, file MISSING → refuses and creates nothing",
			mode:      usecase.VolumeEnsureAdopt,
			wantErr:   domain.ErrVolumeBackingFileMissing,
			wantFile:  false,
			needsMkfs: false,
		},
		{
			name:          "create intent, file present → still adopted, never re-formatted",
			mode:          usecase.VolumeEnsureCreate,
			filePresent:   true,
			wantFile:      true,
			wantPreserved: true,
		},
		{
			name:     "unset mode → refused, and nothing is created",
			mode:     "",
			wantErr:  errUnsetModeMarker,
			wantFile: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.needsMkfs {
				if _, err := exec.LookPath("mkfs.ext4"); err != nil {
					t.Skip("mkfs.ext4 not on PATH (non-Linux dev host) — this row formats a real filesystem")
				}
			}
			dir := t.TempDir()
			id := uuid.New()
			path := filepath.Join(dir, id.String()+".ext4")
			if tt.filePresent {
				// Seeded at the FULL ledger size (sparse), because adoption now also
				// asserts the file is at least as large as its row records — a 35-byte
				// "volume" is legitimately refused. The sentinel still sits at offset 0,
				// so "was it re-formatted" is answered exactly as before.
				if err := os.WriteFile(path, sentinel, 0o600); err != nil {
					t.Fatalf("seed backing file: %v", err)
				}
				if err := os.Truncate(path, backingBytes(32)); err != nil {
					t.Fatalf("size seeded backing file: %v", err)
				}
			}

			out, err := NewBackingStore().Ensure(context.Background(), usecase.VolumeEnsureInput{
				VolumeID: id, SizeMB: 32, Dir: dir, Mode: tt.mode,
			})

			switch {
			// t.Errorf, not Fatal: the refusal rows must ALSO reach the
			// "created nothing" assertions below — that a fresh filesystem was
			// minted is the louder half of the failure.
			case tt.wantErr == errUnsetModeMarker:
				if err == nil {
					t.Errorf("an unset mode must be refused, got out=%+v", out)
				}
			case tt.wantErr != nil:
				if !errors.Is(err, tt.wantErr) {
					t.Errorf("got err %v, want %v", err, tt.wantErr)
				}
			default:
				if err != nil {
					t.Fatalf("Ensure: %v", err)
				}
				if out.BackingPath != path {
					t.Fatalf("BackingPath = %q, want %q", out.BackingPath, path)
				}
			}

			if got := exists(t, path); got != tt.wantFile {
				t.Fatalf("backing file present = %v, want %v (%s)", got, tt.wantFile, path)
			}
			// The refusal must not litter the directory with anything else either.
			if !tt.wantFile {
				entries, rerr := os.ReadDir(dir)
				if rerr != nil {
					t.Fatalf("read dir: %v", rerr)
				}
				if len(entries) != 0 {
					t.Fatalf("a refusal must create nothing, found %d entries", len(entries))
				}
			}
			if tt.wantPreserved {
				got := make([]byte, len(sentinel))
				f, rerr := os.Open(path)
				if rerr != nil {
					t.Fatalf("open backing file: %v", rerr)
				}
				_, rerr = io.ReadFull(f, got)
				_ = f.Close()
				if rerr != nil {
					t.Fatalf("read backing file: %v", rerr)
				}
				if !bytes.Equal(got, sentinel) {
					t.Fatalf("backing file content was rewritten (mkfs ran over live data): got %q, want the %d-byte sentinel", got, len(sentinel))
				}
			}
		})
	}
}

// requireMkfs skips a row that needs a real filesystem, so the table still runs
// on a non-Linux dev host. blkid is required alongside mkfs.ext4: a probe that
// cannot run is deliberately TOLERATED by the code (unverifiable ≠ wrong), so
// without the tool every identity row would "pass" by taking the legacy branch.
func requireMkfs(t *testing.T) {
	t.Helper()
	for _, bin := range []string{"mkfs.ext4", "blkid"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH (non-Linux dev host) — this row needs a real ext4 filesystem", bin)
		}
	}
}

// formatVolume lays down a real stamped volume at path, as Ensure's create path
// does. It goes through mkfs directly (not through Ensure) so a row can stamp an
// identity that is NOT the volume being adopted — which is the whole point.
func formatVolume(t *testing.T, path string, fsUUID string, label string, sizeMB int64) {
	t.Helper()
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	if err := os.Truncate(path, sizeMB*1024*1024); err != nil {
		t.Fatalf("truncate %s: %v", path, err)
	}
	args := []string{"-q", "-F", "-U", fsUUID}
	if label != "" {
		args = append(args, "-L", label)
	}
	args = append(args, path)
	if o, err := exec.Command("mkfs.ext4", args...).CombinedOutput(); err != nil {
		t.Fatalf("mkfs.ext4 %v: %s: %v", args, o, err)
	}
}

// TestAdoptVerifiesIdentityNotMereExistence is the #adopt-verifies-existence
// regression. Adoption was `os.Stat(path) == nil → return path`: it proved a FILE
// was there and then handed it to a VM as the customer's database. A stale file
// left at a reused uuid-derived path, a `.prerestore`/`.failed-*` sibling an
// operator renamed into place, and a row whose size was raised all adopted
// silently.
//
// The mismatch row is the dangerous one, and it asserts the refusal touches
// NOTHING: the foreign file is evidence, and destroying or re-formatting it would
// be strictly worse than the bug.
//
// The legacy row is the deliberate tolerance and is load-bearing in the other
// direction: every volume created before the stamp carries mkfs's own random
// UUID, so refusing on "uuid ≠ volume id" alone would hard-fail every existing
// volume in the fleet. The LABEL is what separates "stamped as something else"
// from "never stamped", which is why it exists.
func TestAdoptVerifiesIdentityNotMereExistence(t *testing.T) {
	const ledgerMB = 64
	otherVolume := uuid.MustParse("a1b2c3d4-e5f6-4a5b-8c9d-0e1f2a3b4c5d")

	tests := []struct {
		name string
		// stampUUID: "" → format with the adopting volume's own id (the happy path).
		stampUUID string
		// label: "" → format with no label, i.e. a pre-stamp legacy volume.
		label string
		// sizeMB the file is actually laid down at.
		fileMB  int64
		wantErr error
	}{
		{
			name:   "identity matches → adopted",
			label:  stampLabel,
			fileMB: ledgerMB,
		},
		{
			name:      "stamped as a DIFFERENT volume → refused",
			stampUUID: otherVolume.String(),
			label:     stampLabel,
			fileMB:    ledgerMB,
			wantErr:   domain.ErrVolumeIdentityMismatch,
		},
		{
			name:      "legacy volume, no stamp → adopted unverified",
			stampUUID: otherVolume.String(), // mkfs's own random uuid, in effect
			fileMB:    ledgerMB,
		},
		{
			name:    "smaller than the ledger records → refused",
			label:   stampLabel,
			fileMB:  ledgerMB / 2,
			wantErr: domain.ErrVolumeBackingFileUndersized,
		},
		{
			name:      "undersized AND foreign → still refused (size is checked first, needs no tool)",
			stampUUID: otherVolume.String(),
			label:     stampLabel,
			fileMB:    ledgerMB / 2,
			wantErr:   domain.ErrVolumeBackingFileUndersized,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requireMkfs(t)
			dir := t.TempDir()
			id := uuid.New()
			path := filepath.Join(dir, id.String()+".ext4")

			stamp := tt.stampUUID
			if stamp == "" {
				stamp = id.String()
			}
			formatVolume(t, path, stamp, tt.label, tt.fileMB)
			before, serr := os.Stat(path)
			if serr != nil {
				t.Fatalf("stat seeded volume: %v", serr)
			}
			digest := fileDigest(t, path)

			out, err := NewBackingStore().Ensure(context.Background(), usecase.VolumeEnsureInput{
				VolumeID: id, SizeMB: ledgerMB, Dir: dir, Mode: usecase.VolumeEnsureAdopt,
			})

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("got err %v, want %v", err, tt.wantErr)
				}
				if out.BackingPath != "" {
					t.Errorf("a refusal must return no path, got %q", out.BackingPath)
				}
			} else {
				if err != nil {
					t.Fatalf("Ensure: %v", err)
				}
				if out.BackingPath != path {
					t.Fatalf("BackingPath = %q, want %q", out.BackingPath, path)
				}
			}

			// EVERY row — refusal and adoption alike — must leave the file exactly as
			// it found it. Nothing here may ever format, grow, or delete.
			after, serr := os.Stat(path)
			if serr != nil {
				t.Fatalf("the backing file must survive untouched: %v", serr)
			}
			if after.Size() != before.Size() {
				t.Errorf("backing file size changed: %d → %d", before.Size(), after.Size())
			}
			if got := fileDigest(t, path); got != digest {
				t.Errorf("backing file CONTENT changed — something formatted or wrote over it")
			}
		})
	}
}

// TestCreateStampsTheVolumeIdIntoTheFilesystem proves the other half: without the
// stamp at create there is nothing for adopt to verify against, so this asserts
// the created filesystem carries BOTH facts — the volume id as its UUID and the
// marker label that makes that UUID authoritative.
func TestCreateStampsTheVolumeIdIntoTheFilesystem(t *testing.T) {
	requireMkfs(t)
	dir := t.TempDir()
	id := uuid.New()

	out, err := NewBackingStore().Ensure(context.Background(), usecase.VolumeEnsureInput{
		VolumeID: id, SizeMB: 64, Dir: dir, Mode: usecase.VolumeEnsureCreate,
	})
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	fsUUID, label, perr := probeExtIdentity(context.Background(), out.BackingPath)
	if perr != nil {
		t.Fatalf("probe the volume just created: %v", perr)
	}
	if !strings.EqualFold(fsUUID, id.String()) {
		t.Errorf("filesystem uuid = %s, want the volume id %s — adoption can never verify identity without this", fsUUID, id)
	}
	if label != stampLabel {
		t.Errorf("filesystem label = %q, want %q — without the marker every mismatch reads as a legacy volume and the check can refuse nothing", label, stampLabel)
	}

	// And the volume it just created must adopt cleanly: create → adopt is the
	// reboot/re-provision path, and a check that refuses its own output is worse
	// than no check.
	if _, aerr := NewBackingStore().Ensure(context.Background(), usecase.VolumeEnsureInput{
		VolumeID: id, SizeMB: 64, Dir: dir, Mode: usecase.VolumeEnsureAdopt,
	}); aerr != nil {
		t.Fatalf("a freshly created volume must adopt: %v", aerr)
	}
}

// fileDigest hashes a file so "untouched" is asserted on content, not on size.
func fileDigest(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatalf("hash %s: %v", path, err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// errUnsetModeMarker is a table-only marker: an unset mode is refused with a
// plain wiring error (like the empty-dir check beside it), not a domain
// sentinel, so the row asserts "refused + created nothing" rather than identity.
var errUnsetModeMarker = errors.New("unset mode")

// TestDeleteReclaimsFailedRestoreSiblings is the
// #decommission-leaves-failed-restore-sibling regression: deleting a volume must
// take the forensic copies a rolled-back restore parked beside it (one observed
// live at 2.4GB, orphaned forever), and must NOT take anything else.
//
// The neighbour cases are the dangerous half. A loose pattern (e.g. Glob on
// "<backing>*") would match a DIFFERENT volume's files, which is strictly worse
// than the leak being fixed — so each of them is asserted to survive.
func TestDeleteReclaimsFailedRestoreSiblings(t *testing.T) {
	dir := t.TempDir()
	backing := filepath.Join(dir, "a.ext4")

	// Siblings of the volume being deleted — these must go. The .prerestore goes
	// only because the live backing file is present here (see
	// TestDeleteKeepsPrerestoreWhileTheVolumeIsMidRestore for the inverse).
	mine := []string{
		backing + ".failed-" + "3f0f5d1e-1f2a-4c6b-9a8d-0b1c2d3e4f50",
		backing + ".failed-" + "8c1b6a2d-3e4f-4a5b-8c9d-0e1f2a3b4c5d", // two rolled-back restores
		backing + ".prerestore",
	}
	// Everything else in the directory must survive.
	others := map[string]string{
		"a different volume's backing file":       filepath.Join(dir, "b.ext4"),
		"a different volume's forensic copy":      filepath.Join(dir, "b.ext4.failed-3f0f5d1e"),
		"a different volume's pre-restore copy":   filepath.Join(dir, "b.ext4.prerestore"),
		"a volume whose name EXTENDS ours":        filepath.Join(dir, "a.ext4x.failed-3f0f5d1e"),
		"a volume whose base extends ours":        filepath.Join(dir, "ab.ext4.failed-3f0f5d1e"),
		"the prefix with no recovery-point id":    filepath.Join(dir, "a.ext4.failed-"),
		"a pre-restore name that is not exact":    filepath.Join(dir, "a.ext4.prerestore.old"),
		"an unrelated file sharing the base name": filepath.Join(dir, "a.ext4-copy"),
		// A restore's staging file is keyed by RECOVERY POINT, not by volume, and
		// every volume on the host shares this directory — so it can belong to
		// another volume's in-flight restore and is never ours to remove.
		"another volume's in-flight restore staging": filepath.Join(dir, ".restore-3f0f5d1e.tmp"),
	}

	write(t, backing)
	for _, p := range mine {
		write(t, p)
	}
	for _, p := range others {
		write(t, p)
	}
	// A DIRECTORY matching the sibling pattern is not ours to remove recursively.
	dirSibling := backing + ".failed-adirectory"
	if err := os.Mkdir(dirSibling, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if err := NewBackingStore().Delete(context.Background(), backing); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if exists(t, backing) {
		t.Fatalf("backing file %s should be gone", backing)
	}
	for _, p := range mine {
		if exists(t, p) {
			t.Fatalf("failed-restore sibling %s should have been reclaimed", p)
		}
	}
	for what, p := range others {
		if !exists(t, p) {
			t.Fatalf("%s (%s) must never be touched", what, p)
		}
	}
	if !exists(t, dirSibling) {
		t.Fatalf("a directory matching the sibling name must be left alone")
	}
}

// TestDeleteKeepsPrerestoreWhileTheVolumeIsMidRestore is the inversion that
// makes the guard worth having. Mid-restore the live volume has been RENAMED to
// "<backing>.prerestore", so the backing path does not exist and that sibling is
// the customer's ONLY remaining copy. A delete landing in that window must take
// nothing: removing it would destroy the last copy of the data while the file it
// was asked to delete was already gone.
//
// The .failed-* copy is reclaimed even here — it can only exist after a rollback
// already reinstated an original, so it is never the only copy.
func TestDeleteKeepsPrerestoreWhileTheVolumeIsMidRestore(t *testing.T) {
	dir := t.TempDir()
	backing := filepath.Join(dir, "a.ext4") // deliberately NOT created: mid-restore
	pre := backing + ".prerestore"
	failed := backing + ".failed-3f0f5d1e"
	write(t, pre)
	write(t, failed)

	if err := NewBackingStore().Delete(context.Background(), backing); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if !exists(t, pre) {
		t.Fatal("the pre-restore copy is the ONLY remaining data while the live path is absent — it must never be removed")
	}
	if exists(t, failed) {
		t.Fatalf("the forensic copy is never the only copy and should still be reclaimed")
	}
}

// TestDeletePrerestoreRemovedOnlyWithTheLiveVolume states the asymmetry as one
// table so an inversion of the guard cannot pass.
func TestDeletePrerestoreRemovedOnlyWithTheLiveVolume(t *testing.T) {
	tests := []struct {
		name        string
		liveExists  bool
		wantPreGone bool
	}{
		{"live volume present → its pre-restore copy is moot", true, true},
		{"live volume absent → a restore owns it, keep", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			backing := filepath.Join(dir, "a.ext4")
			pre := backing + ".prerestore"
			if tt.liveExists {
				write(t, backing)
			}
			write(t, pre)

			if err := NewBackingStore().Delete(context.Background(), backing); err != nil {
				t.Fatalf("Delete: %v", err)
			}
			if got := !exists(t, pre); got != tt.wantPreGone {
				t.Fatalf("pre-restore removed = %v, want %v", got, tt.wantPreGone)
			}
		})
	}
}

func TestDeleteIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	backing := filepath.Join(dir, "gone.ext4")
	b := NewBackingStore()

	if err := b.Delete(context.Background(), backing); err != nil {
		t.Fatalf("deleting a missing backing file must be a no-op, got %v", err)
	}
	// Second pass over a directory that never had the file, plus the empty path.
	if err := b.Delete(context.Background(), backing); err != nil {
		t.Fatalf("second Delete: %v", err)
	}
	if err := b.Delete(context.Background(), ""); err != nil {
		t.Fatalf("empty backing path must be a no-op, got %v", err)
	}
}

// TestDeleteMissingBackingStillReclaimsSiblings covers the retry shape: the
// backing file is already gone (a previous delete removed it) but a forensic
// sibling survived, and the next delete must still reclaim it.
func TestDeleteMissingBackingStillReclaimsSiblings(t *testing.T) {
	dir := t.TempDir()
	backing := filepath.Join(dir, "c.ext4")
	sibling := backing + ".failed-3f0f5d1e"
	write(t, sibling)

	if err := NewBackingStore().Delete(context.Background(), backing); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if exists(t, sibling) {
		t.Fatalf("sibling %s should have been reclaimed", sibling)
	}
}

// TestDeleteMissingDirIsNoop: the whole volume directory can be gone (host
// re-imaged, dir removed by hand) — that is not an error.
func TestDeleteMissingDirIsNoop(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-dir", "d.ext4")
	if err := NewBackingStore().Delete(context.Background(), missing); err != nil {
		t.Fatalf("Delete on a missing directory: %v", err)
	}
}
