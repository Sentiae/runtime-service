//go:build unit

package firecracker

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
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
			if slot < ephSlotFloor {
				t.Fatalf("slot %d is below the ephemeral floor %d (image-boot, template or clone range)", slot, ephSlotFloor)
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
		// Span leaves exactly two allocatable slots (floor, floor+1).
		p := ephTestProvider(ephSlotFloor + 2)
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
		// The reserved template slot, a warm-clone slot and any image-boot index
		// must not be returned to the range — freeing one must not make it
		// allocatable.
		p.freeEphSlot(ephUIDOffset)
		p.freeEphSlot(ephUIDOffset + 1)
		p.freeEphSlot(ephUIDOffset + maxCloneIndex)
		p.freeEphSlot(7)
		slot, err := p.allocEphSlot()
		if err != nil {
			t.Fatalf("allocEphSlot: %v", err)
		}
		if slot < ephSlotFloor {
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
			socketPath: base + "/firecracker/" + strconv.Itoa(ephSlotFloor) + "/root/run/" + vmID + ".sock",
			wantSlot:   ephSlotFloor,
			wantOK:     true,
		},
		{
			name:       "image-boot jail socket is not allocator-owned",
			socketPath: base + "/firecracker/7/root/run/" + vmID + ".sock",
			wantOK:     false,
		},
		{
			name:       "reserved warm-template slot is not allocator-owned",
			socketPath: base + "/firecracker/" + strconv.Itoa(ephUIDOffset) + "/root/run/" + vmID + ".sock",
			wantOK:     false,
		},
		{
			// Clone jail ids are non-numeric, and their uid slots sit in the
			// carved-out window — neither may be mistaken for an ephemeral slot.
			name:       "warm clone jail id is not allocator-owned",
			socketPath: base + "/firecracker/" + cloneJailID(maxCloneIndex) + "/root/run/" + vmID + ".sock",
			wantOK:     false,
		},
		{
			name:       "warm clone uid slot as a numeric id is below the floor",
			socketPath: base + "/firecracker/" + strconv.Itoa(ephUIDOffset+maxCloneIndex) + "/root/run/" + vmID + ".sock",
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
	id := strconv.Itoa(ephSlotFloor)
	sock := base + "/firecracker/" + id + "/root/run/11111111-2222-3333-4444-555555555555.sock"

	j := vmJailFromSocketPath(base, 100000, sock)
	if j == nil {
		t.Fatal("jailed socket yielded no jail")
	}
	if j.id != id {
		t.Fatalf("jail id = %q, want %s", j.id, id)
	}
	if want := 100000 + ephSlotFloor; j.uid != want || j.gid != want {
		t.Fatalf("jail uid/gid = %d/%d, want %d/%d", j.uid, j.gid, want, want)
	}
	if got := j.hostPath("snap/x.state"); got != base+"/firecracker/"+id+"/root/snap/x.state" {
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

	// The longest jail ids either allocator can hand out: domain.NetMaxSlot for the
	// image-boot path (its jail id is the lease's host-local slot), VMUIDSpan-1 for
	// the ephemeral (cold-exec / single-shot) paths, which is what prepareJailedVM
	// builds its socket under.
	for _, id := range []string{"1", strconv.Itoa(domain.NetMaxSlot), strconv.Itoa(ephSlotFloor), "8191"} {
		sock := newVMJail(defaultChrootBaseForTest, id, 100001).hostPath(socketRel)
		if err := checkSocketPathFits(sock); err != nil {
			t.Fatalf("default chroot base rejected for jail id %s (%d bytes): %v", id, len(sock), err)
		}
	}

	long := "/var/lib/firecracker/" + strings.Repeat("x", 64)
	sock := newVMJail(long, "1023", 100001).hostPath(socketRel)
	err := checkSocketPathFits(sock)
	if err == nil {
		t.Fatalf("over-long chroot base accepted: %s (%d bytes)", sock, len(sock))
	}
	if !strings.Contains(err.Error(), sock) {
		t.Fatalf("error does not name the path: %v", err)
	}
}

// TestWarmSocketPathsFit pins the AF_UNIX budget for the warm path's own jail
// ids, which are non-numeric ("warm", "cloneN") and therefore longer than the
// numeric ones the other paths use. It builds the exact host socket paths
// BootWarm and CloneFromSnapshot build.
func TestWarmSocketPathsFit(t *testing.T) {
	uid := 100000 + ephUIDOffset

	template := newVMJail(defaultChrootBaseForTest, warmJailID, uid).
		hostPath("run/" + uuid.MustParse("11111111-2222-3333-4444-555555555555").String() + ".sock")
	if err := checkSocketPathFits(template); err != nil {
		t.Fatalf("warm template socket rejected (%d bytes): %v", len(template), err)
	}

	// maxCloneIndex is the longest clone jail id + namespace the pool can produce.
	d := cloneNaming(maxCloneIndex)
	clone := newVMJail(defaultChrootBaseForTest, cloneJailID(maxCloneIndex), uid).
		hostPath("run/" + d.namespace + ".sock")
	if err := checkSocketPathFits(clone); err != nil {
		t.Fatalf("warm clone socket rejected (%d bytes): %v", len(clone), err)
	}
}

// uidRange is one closed slot range [lo,hi] in the single shared uid space.
type uidRange struct {
	name string
	lo   int
	hi   int
}

// uidSpaceRanges is the full carve-up of the slot space, in the order it is laid
// out. Every producer of a jail uid must draw from exactly one of these.
func uidSpaceRanges(span int) []uidRange {
	return []uidRange{
		{"image-boot", 1, domain.NetMaxSlot},
		{"warm-template", ephUIDOffset, ephUIDOffset},
		{"warm-clone", ephUIDOffset + 1, ephUIDOffset + maxCloneIndex},
		{"ephemeral", ephSlotFloor, span - 1},
	}
}

// TestUIDRangesAreDisjoint is the proof that makes per-clone uids safe.
//
// Three INDEPENDENT allocators write into one uid space: the image-boot network
// index (image_boot.go), the warm pool's clone index (warm_pool.go), and the
// ephemeral slot map (jail.go). None of them can see the others, so an overlap
// would not fail loudly — it would hand two live VMs the same (uid,gid) and be
// discovered only as cross-tenant access. Pairwise disjointness is therefore a
// property that has to be asserted, not assumed.
func TestUIDRangesAreDisjoint(t *testing.T) {
	const span = 8192
	ranges := uidSpaceRanges(span)

	t.Run("every range is non-empty", func(t *testing.T) {
		for _, r := range ranges {
			if r.lo > r.hi {
				t.Fatalf("range %s is empty: [%d,%d]", r.name, r.lo, r.hi)
			}
		}
	})

	t.Run("no two ranges overlap", func(t *testing.T) {
		for i := 0; i < len(ranges); i++ {
			for j := i + 1; j < len(ranges); j++ {
				a, b := ranges[i], ranges[j]
				if a.lo <= b.hi && b.lo <= a.hi {
					t.Fatalf("ranges %s [%d,%d] and %s [%d,%d] overlap — two allocators can hand out the same uid",
						a.name, a.lo, a.hi, b.name, b.lo, b.hi)
				}
			}
		}
	})

	t.Run("ephemeral allocator never returns a reserved slot", func(t *testing.T) {
		p := ephTestProvider(span)
		reserved := ranges[:len(ranges)-1] // everything except the ephemeral range
		for i := 0; i < 64; i++ {
			slot, err := p.allocEphSlot()
			if err != nil {
				t.Fatalf("allocEphSlot #%d: %v", i, err)
			}
			for _, r := range reserved {
				if slot >= r.lo && slot <= r.hi {
					t.Fatalf("ephemeral slot %d falls inside the %s range [%d,%d]", slot, r.name, r.lo, r.hi)
				}
			}
		}
	})

	t.Run("ephemeral allocator starts at the floor", func(t *testing.T) {
		p := ephTestProvider(span)
		slot, err := p.allocEphSlot()
		if err != nil {
			t.Fatalf("allocEphSlot: %v", err)
		}
		if slot != ephSlotFloor {
			t.Fatalf("first ephemeral slot = %d, want the floor %d", slot, ephSlotFloor)
		}
	})

	t.Run("floor sits exactly above the clone window", func(t *testing.T) {
		if want := ephUIDOffset + maxCloneIndex + 1; ephSlotFloor != want {
			t.Fatalf("ephSlotFloor = %d, want %d (offset %d + maxCloneIndex %d + 1)",
				ephSlotFloor, want, ephUIDOffset, maxCloneIndex)
		}
	})
}

// TestCloneIndexBoundMatchesCarveOut pins the carve-out to the allocator it
// protects: the clone window is sized by maxCloneIndex, so if the pool's index
// allocator or CloneFromSnapshot ever accepted an index above it, clones would
// land in the ephemeral range and silently share a live cold VM's uid.
func TestCloneIndexBoundMatchesCarveOut(t *testing.T) {
	m := NewWarmManager(ephTestProvider(8192))

	// The 10.200.N.x /24 scheme is what bounds the index at 254; the uid carve-out
	// is sized from the same constant.
	if maxCloneIndex != 254 {
		t.Fatalf("maxCloneIndex = %d, want 254 (the last valid 10.200.N.x octet)", maxCloneIndex)
	}

	for _, n := range []int{0, -1, maxCloneIndex + 1} {
		if _, err := m.CloneFromSnapshot(context.Background(), &TemplateSnapshot{}, n); err == nil {
			t.Fatalf("CloneFromSnapshot accepted out-of-range clone index %d", n)
		}
	}

	// The pool's allocator must not hand out an index the carve-out does not cover.
	pool := &WarmPool{active: make(map[int]bool)}
	for n := 1; n <= maxCloneIndex; n++ {
		idx, err := pool.allocIndex()
		if err != nil {
			t.Fatalf("allocIndex #%d: %v", n, err)
		}
		if idx < 1 || idx > maxCloneIndex {
			t.Fatalf("allocIndex returned %d, outside [1,%d]", idx, maxCloneIndex)
		}
	}
	if _, err := pool.allocIndex(); err == nil {
		t.Fatal("exhausted clone-index allocator handed out an index instead of refusing")
	}
}

// TestCloneUIDsArePrivate pins the per-clone identity: every clone has its own
// uid, distinct from the template's and from every other clone's, and none of
// them is reachable by the ephemeral allocator.
func TestCloneUIDsArePrivate(t *testing.T) {
	p := ephTestProvider(8192)
	m := NewWarmManager(p)

	if got, want := m.templateUID(), 100000+ephUIDOffset; got != want {
		t.Fatalf("templateUID = %d, want %d", got, want)
	}

	seen := map[int]int{}
	for n := 1; n <= maxCloneIndex; n++ {
		uid := m.cloneUID(n)
		if uid == m.templateUID() {
			t.Fatalf("clone %d shares the template uid %d", n, uid)
		}
		if prev, dup := seen[uid]; dup {
			t.Fatalf("clones %d and %d share uid %d", prev, n, uid)
		}
		seen[uid] = n
	}

	for i := 0; i < 32; i++ {
		slot, err := p.allocEphSlot()
		if err != nil {
			t.Fatalf("allocEphSlot: %v", err)
		}
		uid := vmUID(p.cfg.VMUIDBase, slot)
		if uid == m.templateUID() {
			t.Fatalf("ephemeral slot %d collides with the template uid", slot)
		}
		if n, dup := seen[uid]; dup {
			t.Fatalf("ephemeral slot %d collides with clone %d's uid %d", slot, n, uid)
		}
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
