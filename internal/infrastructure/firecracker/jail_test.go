//go:build unit

package firecracker

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// defaultChrootBaseForTest mirrors the firecracker.chroot_base default in
// pkg/config — the socket-length budget is a property of that exact value.
const defaultChrootBaseForTest = "/var/lib/firecracker/j"

func TestVMJailPaths(t *testing.T) {
	base := t.TempDir()
	id := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	j := newVMJail(base, "7", 100007)

	wantJail := filepath.Join(base, "firecracker", "7")
	if got := j.jailDir(); got != wantJail {
		t.Fatalf("jailDir = %q, want %q", got, wantJail)
	}
	wantRoot := filepath.Join(wantJail, "root")
	if got := j.rootDir(); got != wantRoot {
		t.Fatalf("rootDir = %q, want %q", got, wantRoot)
	}
	if got, want := j.hostPath("run/"+id.String()+".sock"), filepath.Join(wantRoot, "run", id.String()+".sock"); got != want {
		t.Fatalf("hostPath = %q, want %q", got, want)
	}
	if got, want := j.chrootPath("run/"+id.String()+".sock"), "/run/"+id.String()+".sock"; got != want {
		t.Fatalf("chrootPath = %q, want %q", got, want)
	}
	if got, want := j.chrootPath("kernel/vmlinux"), "/kernel/vmlinux"; got != want {
		t.Fatalf("chrootPath = %q, want %q", got, want)
	}
	if j.gid != j.uid {
		t.Fatalf("gid %d != uid %d", j.gid, j.uid)
	}
}

func TestVMUID(t *testing.T) {
	tests := []struct {
		name  string
		base  int
		index int
		want  int
	}{
		{"first index", 100000, 1, 100001},
		{"mid index", 100000, 4000, 104000},
		{"custom base", 200000, 7, 200007},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := vmUID(tt.base, tt.index); got != tt.want {
				t.Fatalf("vmUID(%d,%d) = %d, want %d", tt.base, tt.index, got, tt.want)
			}
		})
	}
}

// TestCheckSocketPathFits pins the AF_UNIX budget: the default chroot base must
// leave room for the longest jail id plus the .vsock suffix, and an over-long
// base must refuse the boot instead of producing a socket that stats fine and
// EINVALs on every connect.
func TestCheckSocketPathFits(t *testing.T) {
	vmID := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	socketRel := "run/" + vmID.String() + ".sock"

	// imgMaxIndex is the longest jail id the allocator can hand out.
	for _, id := range []string{"1", "4000"} {
		sock := newVMJail(defaultChrootBaseForTest, id, 100001).hostPath(socketRel)
		if err := checkSocketPathFits(sock); err != nil {
			t.Fatalf("default chroot base rejected for jail id %s (%d bytes): %v", id, len(sock), err)
		}
	}

	long := "/var/lib/firecracker/" + strings.Repeat("x", 64)
	sock := newVMJail(long, "4000", 100001).hostPath(socketRel)
	err := checkSocketPathFits(sock)
	if err == nil {
		t.Fatalf("over-long chroot base accepted: %s (%d bytes)", sock, len(sock))
	}
	if !strings.Contains(err.Error(), sock) {
		t.Fatalf("error does not name the path: %v", err)
	}
}

func TestJailDirFromSocketPath(t *testing.T) {
	vmID := "11111111-2222-3333-4444-555555555555"
	base := defaultChrootBaseForTest

	tests := []struct {
		name       string
		chrootBase string
		socketPath string
		want       string
	}{
		{
			name:       "numeric jail id socket",
			chrootBase: base,
			socketPath: base + "/firecracker/7/root/run/" + vmID + ".sock",
			want:       base + "/firecracker/7",
		},
		{
			name:       "highest numeric jail id",
			chrootBase: base,
			socketPath: base + "/firecracker/4000/root/run/" + vmID + ".sock",
			want:       base + "/firecracker/4000",
		},
		{
			name:       "jailed vsock",
			chrootBase: base,
			socketPath: base + "/firecracker/7/root/run/" + vmID + ".sock.vsock",
			want:       base + "/firecracker/7",
		},
		{
			name:       "legacy unjailed socket",
			chrootBase: base,
			socketPath: "/tmp/firecracker/" + vmID + ".sock",
			want:       "",
		},
		{
			name:       "base only a substring of the path",
			chrootBase: base,
			socketPath: base + "-old/firecracker/7/root/run/" + vmID + ".sock",
			want:       "",
		},
		{
			name:       "empty chroot base",
			chrootBase: "",
			socketPath: base + "/firecracker/7/root/run/" + vmID + ".sock",
			want:       "",
		},
		{
			name:       "empty socket path",
			chrootBase: base,
			socketPath: "",
			want:       "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := jailDirFromSocketPath(tt.chrootBase, tt.socketPath); got != tt.want {
				t.Fatalf("jailDirFromSocketPath(%q,%q) = %q, want %q", tt.chrootBase, tt.socketPath, got, tt.want)
			}
		})
	}
}
