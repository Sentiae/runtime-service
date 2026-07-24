//go:build unit

package firecracker

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/pkg/config"
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

// ephTestProvider builds a Provider carrying only the uid-range config the
// ephemeral allocator reads.
func ephTestProvider(span int) *Provider {
	return NewProvider(config.FirecrackerConfig{
		ChrootBase: defaultChrootBaseForTest,
		VMUIDBase:  100000,
		VMUIDSpan:  span,
	})
}

// TestAllocEphSlot pins the ephemeral uid allocator: slots are unique and
// disjoint from the image-boot range, a freed slot is reusable, and exhaustion
// refuses rather than wrapping onto a live VM's uid.
func TestAllocEphSlot(t *testing.T) {
	t.Run("allocates distinct slots above the image-boot range", func(t *testing.T) {
		p := ephTestProvider(8192)
		seen := make(map[int]bool)
		for i := 0; i < 8; i++ {
			slot, err := p.allocEphSlot()
			if err != nil {
				t.Fatalf("allocEphSlot: %v", err)
			}
			if slot <= ephUIDOffset {
				t.Fatalf("slot %d is inside the image-boot range (offset %d)", slot, ephUIDOffset)
			}
			if slot >= p.cfg.VMUIDSpan {
				t.Fatalf("slot %d outside the uid span %d", slot, p.cfg.VMUIDSpan)
			}
			if seen[slot] {
				t.Fatalf("slot %d handed out twice", slot)
			}
			seen[slot] = true
		}
	})

	t.Run("freed slot is reused", func(t *testing.T) {
		p := ephTestProvider(8192)
		first, err := p.allocEphSlot()
		if err != nil {
			t.Fatalf("allocEphSlot: %v", err)
		}
		second, err := p.allocEphSlot()
		if err != nil {
			t.Fatalf("allocEphSlot: %v", err)
		}
		p.freeEphSlot(first)
		again, err := p.allocEphSlot()
		if err != nil {
			t.Fatalf("allocEphSlot after free: %v", err)
		}
		if again != first {
			t.Fatalf("expected freed slot %d to be reused, got %d", first, again)
		}
		if again == second {
			t.Fatalf("reused slot collides with the still-held slot %d", second)
		}
	})

	t.Run("exhaustion refuses the boot", func(t *testing.T) {
		// Span leaves exactly two allocatable slots (offset+1, offset+2).
		p := ephTestProvider(ephUIDOffset + 3)
		for i := 0; i < 2; i++ {
			if _, err := p.allocEphSlot(); err != nil {
				t.Fatalf("allocEphSlot #%d: %v", i, err)
			}
		}
		if _, err := p.allocEphSlot(); err == nil {
			t.Fatal("exhausted allocator handed out a slot instead of refusing")
		}
	})

	t.Run("free ignores non-allocator slots", func(t *testing.T) {
		p := ephTestProvider(8192)
		// The reserved fixed slot and any image-boot index must not be returned
		// to the range — freeing one must not make it allocatable.
		p.freeEphSlot(ephUIDOffset)
		p.freeEphSlot(7)
		slot, err := p.allocEphSlot()
		if err != nil {
			t.Fatalf("allocEphSlot: %v", err)
		}
		if slot <= ephUIDOffset {
			t.Fatalf("allocator handed out reserved slot %d", slot)
		}
	})
}

// TestEphSlotFromSocketPath pins the teardown-side recovery of the slot: without
// it Terminate (which only receives the socket path) leaks a slot per VM.
func TestEphSlotFromSocketPath(t *testing.T) {
	vmID := "11111111-2222-3333-4444-555555555555"
	base := defaultChrootBaseForTest

	tests := []struct {
		name       string
		socketPath string
		wantSlot   int
		wantOK     bool
	}{
		{
			name:       "ephemeral jail socket",
			socketPath: base + "/firecracker/4097/root/run/" + vmID + ".sock",
			wantSlot:   4097,
			wantOK:     true,
		},
		{
			name:       "image-boot jail socket is not allocator-owned",
			socketPath: base + "/firecracker/7/root/run/" + vmID + ".sock",
			wantOK:     false,
		},
		{
			name:       "reserved warm-template slot is not allocator-owned",
			socketPath: base + "/firecracker/4096/root/run/" + vmID + ".sock",
			wantOK:     false,
		},
		{
			name:       "unjailed socket",
			socketPath: "/tmp/firecracker/" + vmID + ".sock",
			wantOK:     false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			slot, ok := ephSlotFromSocketPath(base, tt.socketPath)
			if ok != tt.wantOK || (tt.wantOK && slot != tt.wantSlot) {
				t.Fatalf("ephSlotFromSocketPath(%q) = (%d,%v), want (%d,%v)", tt.socketPath, slot, ok, tt.wantSlot, tt.wantOK)
			}
		})
	}
}

// TestVMJailFromSocketPath pins the post-boot reconstruction the snapshot paths
// depend on: the jail id and uid must come back out of the socket path exactly
// as prepareJailedVM put them in.
func TestVMJailFromSocketPath(t *testing.T) {
	base := defaultChrootBaseForTest
	sock := base + "/firecracker/4097/root/run/11111111-2222-3333-4444-555555555555.sock"

	j := vmJailFromSocketPath(base, 100000, sock)
	if j == nil {
		t.Fatal("jailed socket yielded no jail")
	}
	if j.id != "4097" {
		t.Fatalf("jail id = %q, want 4097", j.id)
	}
	if j.uid != 104097 || j.gid != 104097 {
		t.Fatalf("jail uid/gid = %d/%d, want 104097/104097", j.uid, j.gid)
	}
	if got := j.hostPath("snap/x.state"); got != base+"/firecracker/4097/root/snap/x.state" {
		t.Fatalf("hostPath = %q", got)
	}
	if j := vmJailFromSocketPath(base, 100000, "/tmp/firecracker/x.sock"); j != nil {
		t.Fatalf("unjailed socket yielded a jail: %+v", j)
	}
}

// TestCheckSocketPathFits pins the AF_UNIX budget: the default chroot base must
// leave room for the longest jail id plus the .vsock suffix, and an over-long
// base must refuse the boot instead of producing a socket that stats fine and
// EINVALs on every connect.
func TestCheckSocketPathFits(t *testing.T) {
	vmID := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	socketRel := "run/" + vmID.String() + ".sock"

	// The longest jail ids either allocator can hand out: imgMaxIndex for the
	// image-boot path, VMUIDSpan-1 for the ephemeral (cold-exec / single-shot)
	// paths, which is what prepareJailedVM builds its socket under.
	for _, id := range []string{"1", "4000", "4097", "8191"} {
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
