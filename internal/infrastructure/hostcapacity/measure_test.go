package hostcapacity

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMemTotalMB(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    int64
		wantErr bool
	}{
		{
			// A real /proc/meminfo prefix: MemTotal is not the first field-shaped
			// line an eager parser would grab, and the unit is kB.
			name: "real meminfo shape",
			content: "MemTotal:       16333684 kB\n" +
				"MemFree:         1234567 kB\n" +
				"MemAvailable:    9876543 kB\n",
			want: 15950,
		},
		{
			name:    "memtotal is not the first line",
			content: "Committed_AS:  100 kB\nMemTotal:       1048576 kB\n",
			want:    1024,
		},
		{
			name:    "missing memtotal",
			content: "MemFree: 1024 kB\n",
			wantErr: true,
		},
		{
			name:    "unparseable value",
			content: "MemTotal:       lots kB\n",
			wantErr: true,
		},
		{
			name:    "value-less line",
			content: "MemTotal:\n",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "meminfo")
			if err := os.WriteFile(path, []byte(tt.content), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := memTotalMB(path)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("want an error, got %d", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got != tt.want {
				t.Errorf("memTotalMB = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestMemTotalMB_MissingFile(t *testing.T) {
	if _, err := memTotalMB(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("an unreadable meminfo must be an error, never a default")
	}
}

// statfsNearest must answer for a directory that does not exist yet: the volume
// dir is created by the first volume, and refusing to measure a fresh host would
// keep it permanently out of the fleet.
func TestStatfsNearest_WalksUpToAnExistingAncestor(t *testing.T) {
	base := t.TempDir()
	missing := filepath.Join(base, "volumes", "not", "created", "yet")

	st, dir, err := statfsNearest(missing)
	if err != nil {
		t.Fatalf("statfs: %v", err)
	}
	if dir != base {
		t.Errorf("measured dir = %q, want the nearest existing ancestor %q", dir, base)
	}
	if st.Blocks == 0 {
		t.Error("statfs reported zero blocks for a real filesystem")
	}
}

// Measure is the boot path: on the host it runs on it must produce a complete
// measurement (or a real error — never a plausible default).
func TestMeasure(t *testing.T) {
	m, err := Measure(t.TempDir())
	if err != nil {
		// /proc/meminfo does not exist off Linux; the failure is the contract there.
		if _, statErr := os.Stat(procMemInfo); statErr != nil {
			t.Skipf("no %s on this platform: %v", procMemInfo, err)
		}
		t.Fatalf("measure: %v", err)
	}
	if m.VCPU <= 0 || m.MemTotalMB <= 0 || m.DiskTotalMB <= 0 || m.DiskAvailableMB <= 0 {
		t.Fatalf("incomplete measurement: %+v", m)
	}
	if m.DiskAvailableMB > m.DiskTotalMB {
		t.Errorf("free disk %dMB exceeds total %dMB", m.DiskAvailableMB, m.DiskTotalMB)
	}
}
