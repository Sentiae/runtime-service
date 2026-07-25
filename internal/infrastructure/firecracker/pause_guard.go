package firecracker

import (
	"fmt"

	"github.com/sentiae/runtime-service/internal/domain"
)

// VMClass declares what a VM is for, and specifically whether anything is
// allowed to PAUSE it.
//
// ⚠ WHY this exists, and why it looks removable but is not:
// Firecracker v1.16.0's vsock does NOT survive Pause/Resume
// (#fc-vsock-dies-on-pause-resume — proven live on the fleet host). After ONE
// pause, the guest control channel of that VM is dead for the rest of its
// lifetime. Everything the resident/data path depends on rides that channel:
// quiesced snapshots, clean shutdown, park. A paused-once database VM therefore
// still looks alive while every durability guarantee it exists to provide is
// already gone — silently.
//
// Today no resident VM is registered with a pausing component. That is an
// accident of WIRING, not a contract, which is exactly the shape
// (#fail-open-is-the-library-contract) that keeps biting this program. So the
// declaration is mandatory and the check is fail-closed: an UNDECLARED VM is
// refused too, so a future caller cannot slip a data VM into a pause path by
// simply not setting a flag.
type VMClass string

const (
	// VMClassPausable is a VM whose guest control channel is disposable: the
	// single-shot exec VMs and the warm template VM. Losing their vsock costs at
	// most the VM itself, which is thrown away anyway.
	VMClassPausable VMClass = "pausable"
	// VMClassResident is a long-lived, data-bearing VM (a customer database).
	// Nothing may pause it — see the type comment.
	VMClassResident VMClass = "resident"
)

// checkPausable refuses every VM that must not meet a Pause: resident VMs
// because pausing them kills their control channel forever, and undeclared VMs
// because a missing declaration is indistinguishable from a resident one that
// nobody classified. component names the pausing component in the error so the
// refusal is actionable rather than a mystery at a call site far away.
//
// Modelled on checkSocketPathFits in jail.go: an impossible-to-miss refusal at
// the seam beats a comment at the call site.
func checkPausable(component string, class VMClass) error {
	switch class {
	case VMClassPausable:
		return nil
	case VMClassResident:
		return fmt.Errorf("%s pauses the VMs it holds and firecracker vsock does not survive Pause/Resume: %w", component, domain.ErrPauseUnsafeForResidentVM)
	default:
		return fmt.Errorf("%s pauses the VMs it holds, so class must be %q or %q, got %q: %w",
			component, VMClassPausable, VMClassResident, class, domain.ErrVMClassUndeclared)
	}
}
