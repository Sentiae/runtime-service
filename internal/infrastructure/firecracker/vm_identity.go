package firecracker

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/usecase"
)

// processInspector reads the host facts a VMM identity proof is built from. It
// is an interface purely so the proof is testable without a real microVM: every
// production implementation reads /proc and the jailer's own pidfile.
type processInspector interface {
	// comm returns /proc/<pid>/comm, trimmed.
	comm(pid int) (string, error)
	// cmdline returns /proc/<pid>/cmdline split on its NUL separators, i.e. the
	// process's argv as TOKENS. Tokens, not a string: a substring test over the
	// joined line matches "--id 12" inside "--id 123" and would authorize
	// signalling the wrong VM.
	cmdline(pid int) ([]string, error)
	// pgid returns getpgid(pid).
	pgid(pid int) (int, error)
	// jailPID returns the pid recorded in a jailer pidfile.
	jailPID(path string) (int, error)
}

// procInspector is the production inspector: /proc plus syscall.Getpgid.
type procInspector struct{}

var _ processInspector = procInspector{}

func (procInspector) comm(pid int) (string, error) {
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "comm"))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func (procInspector) cmdline(pid int) ([]string, error) {
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return nil, err
	}
	parts := bytes.Split(bytes.TrimRight(b, "\x00"), []byte{0})
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, string(p))
	}
	return out, nil
}

func (procInspector) pgid(pid int) (int, error) { return syscall.Getpgid(pid) }

func (procInspector) jailPID(path string) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	pid, cerr := strconv.Atoi(strings.TrimSpace(string(b)))
	if cerr != nil {
		return 0, fmt.Errorf("parse jail pidfile %s: %w", path, cerr)
	}
	return pid, nil
}

// proveVMIdentity proves that the process at in.PID is the microVM in describes,
// before any signal is sent to it.
//
// WHY THIS EXISTS: a recorded pid is a NUMBER, and pids are reused. After a host
// reboot, a service restart or a stale row, the pid a replica row carries can
// name an unrelated process — including a completely different tenant's VMM, or
// a host daemon. The old teardown signalled it anyway and then deleted the row,
// so a mis-signal was both destructive and untraceable.
//
// The proof is five INDEPENDENT facts that a coincidence cannot satisfy
// together: the durable row's own coordinates, the process name, its exact argv
// tokens, its process group, and the jailer's pidfile inside the jail those
// coordinates derive. Any mismatch — or any read this function cannot complete —
// is ErrVMTerminationUnproven, never a signal.
//
// ⚠ TWO THINGS ARE DELIBERATELY NOT USED AS IDENTITY, and re-adding them would
// break the proof rather than strengthen it:
//   - /proc/<pid>/root. A jailed Firecracker has pivot-rooted, so it reports "/"
//     — and the seven live orphans report "/ (deleted)" because the old teardown
//     removed their jails before proving exit. It cannot distinguish anything.
//   - /proc/<pid>/exe equal to the installed binary. An atomic binary upgrade
//     legitimately leaves every running VMM's exe link marked deleted, so
//     requiring it would make the whole fleet unterminatable after an upgrade.
func proveVMIdentity(insp processInspector, chrootBase string, in usecase.ImageDecommissionInput) error {
	unproven := func(format string, args ...any) error {
		return fmt.Errorf("%w: pid %d does not prove it is %s %s: "+format,
			append([]any{domain.ErrVMTerminationUnproven, in.PID, in.OwnerKind, in.OwnerID}, args...)...)
	}

	// 1. The DURABLE row must itself be complete. Every check below is derived
	//    from these fields, so an incomplete row cannot prove anything at all.
	if !in.OwnerKind.IsValid() {
		return unproven("its owner kind %q is not a recognized lease owner", in.OwnerKind)
	}
	if in.OwnerID == uuid.Nil {
		return unproven("it records no owner id")
	}
	if in.NetIndex <= 0 {
		return unproven("it records no net index, so no jail slot can be derived")
	}
	localSlot := in.NetIndex % domain.NetSlotStride
	if localSlot < 1 || localSlot > domain.NetMaxSlot {
		return unproven("net index %d derives host-local slot %d, outside the allocatable range [1,%d]",
			in.NetIndex, localSlot, domain.NetMaxSlot)
	}
	// 2. The socket the row names must be this owner's socket. The basename is
	//    the owner uuid by construction (startVM), so a row pointing elsewhere is
	//    describing another VM.
	wantSock := in.OwnerID.String() + ".sock"
	if filepath.Base(in.SocketPath) != wantSock {
		return unproven("its recorded socket %q is not this owner's %q", in.SocketPath, wantSock)
	}

	// 3. The process must BE a firecracker. comm is the post-execve name, which is
	//    what the jailer leaves behind (it execve's firecracker in place).
	comm, err := insp.comm(in.PID)
	if err != nil {
		return unproven("its /proc comm could not be read: %v", err)
	}
	if comm != jailExecName {
		return unproven("its /proc comm is %q, not %q", comm, jailExecName)
	}

	// 4. Its argv must name THIS jail slot and THIS owner's api socket. Tokenized
	//    exact matches — a substring test over the joined cmdline would accept
	//    "--id 1" for slot 12, i.e. would authorize killing a different VM.
	argv, err := insp.cmdline(in.PID)
	if err != nil {
		return unproven("its /proc cmdline could not be read: %v", err)
	}
	if !argvHasPair(argv, "--id", strconv.Itoa(localSlot)) {
		return unproven("its argv does not carry `--id %d`", localSlot)
	}
	if !argvHasPair(argv, "--api-sock", "/run/"+wantSock) {
		return unproven("its argv does not carry `--api-sock /run/%s`", wantSock)
	}

	// 5. It must be its own process group leader, which is the contract startVM
	//    booted it under (SysProcAttr.Setpgid). A recycled pid belonging to some
	//    other process is overwhelmingly unlikely to also lead its own group with
	//    this exact argv.
	pgid, err := insp.pgid(in.PID)
	if err != nil {
		return unproven("its process group could not be read: %v", err)
	}
	if pgid != in.PID {
		return unproven("its process group is %d, not itself (the boot path sets Setpgid)", pgid)
	}

	// 6. The jailer's OWN pidfile, inside the jail the row's slot derives, must
	//    record exactly this pid. This is the fact that ties the number to the
	//    filesystem identity of the VM rather than to the process table alone.
	pidfile := jailPIDFilePath(chrootBase, localSlot)
	recorded, err := insp.jailPID(pidfile)
	if err != nil {
		return unproven("its jail pidfile %s could not be read: %v", pidfile, err)
	}
	if recorded != in.PID {
		return unproven("its jail pidfile %s records pid %d", pidfile, recorded)
	}
	return nil
}

// jailPIDFilePath is where the jailer records the VMM's pid for a slot:
// <chroot-base>/firecracker/<slot>/root/firecracker.pid.
func jailPIDFilePath(chrootBase string, localSlot int) string {
	return filepath.Join(filepath.Clean(chrootBase), jailExecName, strconv.Itoa(localSlot), "root", jailExecName+".pid")
}

// argvHasPair reports whether argv contains flag immediately followed by value,
// as two separate tokens. Exact, positional and whole-token by construction.
func argvHasPair(argv []string, flag, value string) bool {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == flag && argv[i+1] == value {
			return true
		}
	}
	return false
}
