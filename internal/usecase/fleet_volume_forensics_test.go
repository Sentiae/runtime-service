package usecase

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
)

// TestRolledBackRestoreKeepsForensicSibling pins the OTHER half of the
// #decommission-leaves-failed-restore-sibling fix: reclaiming the forensic copy
// at volume-delete time must not weaken why it exists. A rolled-back restore
// still parks the failed image as "<backing>.failed-<recovery-point-id>" and
// leaves it there while the volume lives — it is the only evidence of what the
// recovery point contained. Its removal happens exactly once, in the backing
// store's Delete (see internal/infrastructure/volume,
// TestDeleteReclaimsFailedRestoreSiblings), when the volume is going away anyway.
//
// It lives in its own file rather than in fleet_volume_restore_test.go because
// it asserts the sibling's LIFECYCLE across the two components, not the restore
// state machine.
func TestRolledBackRestoreKeepsForensicSibling(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "vol.ext4")
	pre := live + prerestoreSuffix
	failed := live + ".failed-" + uuid.NewString()

	// The state a rollback starts from: the restored image is live and failed to
	// boot, the pre-restore original is parked beside it.
	if err := os.WriteFile(live, []byte("restored-bad"), 0o600); err != nil {
		t.Fatalf("write live: %v", err)
	}
	if err := os.WriteFile(pre, []byte("original-good"), 0o600); err != nil {
		t.Fatalf("write pre: %v", err)
	}

	if err := swapBack(live, pre, failed); err != nil {
		t.Fatalf("swapBack: %v", err)
	}

	got, err := os.ReadFile(live)
	if err != nil || string(got) != "original-good" {
		t.Fatalf("live volume = %q (err %v), want the pre-restore original", got, err)
	}
	forensic, err := os.ReadFile(failed)
	if err != nil {
		t.Fatalf("the forensic copy must survive a rollback: %v", err)
	}
	if string(forensic) != "restored-bad" {
		t.Fatalf("forensic copy = %q, want the bytes that failed to boot", forensic)
	}
	if _, err := os.Stat(pre); !os.IsNotExist(err) {
		t.Fatalf("the pre-restore anchor should be consumed by the rollback, stat err = %v", err)
	}
}
