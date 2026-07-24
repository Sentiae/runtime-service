package firecracker

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

// jailExecName is the directory jailer derives from basename(--exec-file); the
// chroot it builds is <chroot-base>/<jailExecName>/<id>/root.
const jailExecName = "firecracker"

// maxUnixSocketPath is the usable AF_UNIX sun_path budget (108 bytes minus the
// NUL terminator). A longer path still stats fine but every connect() returns
// EINVAL, so the jail layout must be checked against it rather than discovered
// as a mid-boot API failure.
const maxUnixSocketPath = 107

// vmJail models one microVM's jailer chroot: the host view runtime-service (root)
// uses for mkdir/chown/link/dial/remove, and the chroot view the jailed VMM sees
// after it drops to its unprivileged uid — every path handed to the jailer CLI or
// to the Firecracker API must be the chroot view, because the VMM opens it after
// the chroot.
type vmJail struct {
	base string
	id   string
	uid  int
	gid  int
}

// newVMJail derives the jail for one VM. gid mirrors uid: the per-VM identity is
// a private (uid, gid) pair, so no two VMs ever share a group either.
//
// id is the caller's jail identity, NOT the VM uuid: it lands twice in the host
// socket path (jail dir + socket basename), and two uuids blow past the 107-byte
// AF_UNIX sun_path budget — every connect() would return EINVAL. The caller
// passes the per-workload network index, which is unique across live VMs by the
// same allocator the uid comes from.
func newVMJail(base, id string, uid int) *vmJail {
	return &vmJail{
		base: filepath.Clean(base),
		id:   id,
		uid:  uid,
		gid:  uid,
	}
}

// checkSocketPathFits refuses a host API socket path that cannot be dialed. The
// readiness probe only stats the socket, which has no length limit, so without
// this the VM looks healthy and every API call fails with EINVAL instead.
func checkSocketPathFits(socketPath string) error {
	// The vsock secret channel hangs off the same path with a suffix, so the
	// budget has to cover the longer of the two.
	if n := len(socketPath) + len(".vsock"); n > maxUnixSocketPath {
		return fmt.Errorf("host socket path %s is %d bytes with the .vsock suffix, over the AF_UNIX sun_path limit of %d: shorten the firecracker chroot base", socketPath, n, maxUnixSocketPath)
	}
	return nil
}

func (j *vmJail) jailDir() string {
	return filepath.Join(j.base, jailExecName, j.id)
}

func (j *vmJail) rootDir() string {
	return filepath.Join(j.jailDir(), "root")
}

// hostPath maps a chroot-relative path to the host path runtime-service uses.
func (j *vmJail) hostPath(rel string) string {
	return filepath.Join(j.rootDir(), rel)
}

// chrootPath maps a chroot-relative path to what the jailed VMM sees.
func (j *vmJail) chrootPath(rel string) string {
	return path.Join("/", rel)
}

// prepare creates a clean chroot root. The RemoveAll is not tidiness: jailer
// mknods /dev/kvm, /dev/net/tun and /dev/urandom inside the chroot, and stale
// nodes left by a crashed VM are a boot hazard.
func (j *vmJail) prepare() error {
	if err := os.RemoveAll(j.jailDir()); err != nil {
		return fmt.Errorf("clear jail dir %s: %w", j.jailDir(), err)
	}
	if err := os.MkdirAll(j.rootDir(), 0o750); err != nil {
		return fmt.Errorf("create jail root %s: %w", j.rootDir(), err)
	}
	if err := os.Chown(j.rootDir(), j.uid, j.gid); err != nil {
		return fmt.Errorf("chown jail root %s: %w", j.rootDir(), err)
	}
	return nil
}

// mkdir creates a directory inside the chroot owned by the VM's uid.
func (j *vmJail) mkdir(rel string) error {
	dir := j.hostPath(rel)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create jail dir %s: %w", dir, err)
	}
	if err := os.Chown(dir, j.uid, j.gid); err != nil {
		return fmt.Errorf("chown jail dir %s: %w", dir, err)
	}
	return nil
}

// link hard-links hostSrc into the chroot at rel and returns its chroot view.
// A hard link (never a copy) is mandatory: a copy of the persistent data volume
// would silently break persistence — the guest's writes would land on a throwaway
// duplicate. chown is for files this VM exclusively owns; shared read-only files
// (the kernel) must not be chowned, because the link shares the inode and the
// chown would change the original for every other VM too.
func (j *vmJail) link(hostSrc, rel string, chown bool) (string, error) {
	dst := j.hostPath(rel)
	if err := os.Link(hostSrc, dst); err != nil {
		return "", fmt.Errorf("hard-link %s into jail at %s: %w", hostSrc, dst, err)
	}
	if chown {
		if err := os.Chown(dst, j.uid, j.gid); err != nil {
			return "", fmt.Errorf("chown jailed file %s: %w", dst, err)
		}
	}
	return j.chrootPath(rel), nil
}

// remove tears the chroot down (best-effort — it runs on failure paths).
func (j *vmJail) remove() {
	_ = os.RemoveAll(j.jailDir())
}

// vmUID derives a VM's unprivileged uid from its per-workload network index,
// which is already unique by construction and reclaimed across restarts.
func vmUID(base, index int) int {
	return base + index
}

// ephUIDOffset opens the uid (and jail-id) sub-range the EPHEMERAL boot paths
// draw from. The image-boot path derives uid = VMUIDBase + network index from a
// persisted, restart-reclaimed index in [1,imgMaxIndex]; the cold-exec / single-
// shot paths have no such durable index and allocate in memory instead. Starting
// their range above imgMaxIndex makes the two schemes disjoint by construction,
// so an in-memory allocator that forgets everything on restart can never hand a
// VM the uid — or the chroot — a live image-boot VM already holds.
//
// The offset itself is NOT allocatable: it is the fixed slot reserved for the
// single warm template VM (only one runs at a time, like its fixed tap name).
const ephUIDOffset = 4096

// allocEphSlot reserves an ephemeral uid slot. The slot doubles as the jail id,
// so it must be unique across live VMs; it is returned to the range by
// freeEphSlot on teardown. Exhaustion refuses the boot rather than wrapping —
// wrapping would hand a running VM's uid to a second tenant, which is exactly
// the isolation the jail exists to provide.
func (p *Provider) allocEphSlot() (int, error) {
	p.ephMu.Lock()
	defer p.ephMu.Unlock()
	if p.ephSlots == nil {
		p.ephSlots = make(map[int]bool)
	}
	for slot := ephUIDOffset + 1; slot < p.cfg.VMUIDSpan; slot++ {
		if !p.ephSlots[slot] {
			p.ephSlots[slot] = true
			return slot, nil
		}
	}
	return 0, fmt.Errorf("ephemeral vm uid slots exhausted (range [%d,%d))", ephUIDOffset+1, p.cfg.VMUIDSpan)
}

// freeEphSlot returns a slot to the range. Slots at or below the offset are not
// allocator-owned (the reserved warm-template slot, or a non-ephemeral id).
func (p *Provider) freeEphSlot(slot int) {
	if slot <= ephUIDOffset {
		return
	}
	p.ephMu.Lock()
	delete(p.ephSlots, slot)
	p.ephMu.Unlock()
}

// ephSlotFromSocketPath recovers the ephemeral slot from a jailed VM's host
// socket path. Teardown (Terminate) receives only (socketPath, pid), so the slot
// has to be read back out of the path it was written into — otherwise every VM
// leaks a slot and the range drains over the process lifetime.
func ephSlotFromSocketPath(chrootBase, socketPath string) (int, bool) {
	dir := jailDirFromSocketPath(chrootBase, socketPath)
	if dir == "" {
		return 0, false
	}
	slot, err := strconv.Atoi(filepath.Base(dir))
	if err != nil || slot <= ephUIDOffset {
		return 0, false
	}
	return slot, true
}

// vmJailFromSocketPath rebuilds a running VM's jail handle from its host socket
// path, or nil when the socket is not inside chrootBase. The post-boot API calls
// (snapshot create/restore) are handed nothing but the socket, and both the jail
// id and the uid are derivable from it, so the handle is reconstructed instead of
// tracked in a second source of truth that could drift from the filesystem.
func vmJailFromSocketPath(chrootBase string, uidBase int, socketPath string) *vmJail {
	dir := jailDirFromSocketPath(chrootBase, socketPath)
	if dir == "" {
		return nil
	}
	id := filepath.Base(dir)
	index, err := strconv.Atoi(id)
	if err != nil {
		return nil
	}
	return newVMJail(chrootBase, id, vmUID(uidBase, index))
}

// jailDirFromSocketPath maps a persisted host-view API socket path back to the
// jail dir to remove. It returns "" when the socket is not inside chrootBase —
// VMs booted before the image-boot path was jailed keep their old socket paths
// and must not have an unrelated directory removed under them.
func jailDirFromSocketPath(chrootBase, socketPath string) string {
	if chrootBase == "" || socketPath == "" {
		return ""
	}
	prefix := filepath.Join(filepath.Clean(chrootBase), jailExecName) + string(filepath.Separator)
	clean := filepath.Clean(socketPath)
	if !strings.HasPrefix(clean, prefix) {
		return ""
	}
	id, _, _ := strings.Cut(clean[len(prefix):], string(filepath.Separator))
	if id == "" {
		return ""
	}
	return prefix + id
}
