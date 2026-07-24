package main

import (
	"fmt"
	"os"
	"syscall"
)

// runTmpDir is where every per-run working directory is created (os.MkdirTemp
// with an empty dir resolves here). It is the ONLY writable path in a warm
// guest.
const runTmpDir = "/tmp"

// mountRunTmpfs mounts a tmpfs on /tmp. It mirrors image-init's mountPseudoFS
// (same syscall, same EBUSY/EEXIST tolerance, same non-fatal console log).
//
// Why this is mandatory, not a nicety: the warm rootfs is one ext4 file shared
// by the template VM and every concurrent clone, so it is served READ-ONLY at
// the virtio-blk device (see firecracker.warmRootfsDriveBody) and mounted `ro`.
// Without a writable tmpfs the agent cannot create its per-run working
// directory and EVERY execution fails EROFS. RAM-backed also means one run's
// files never survive into another clone's view.
//
// The agent is PID 1 in a warm guest (/sbin/warm-init execs it), so it holds
// CAP_SYS_ADMIN in the guest and may mount; /proc and /sys are already mounted
// by warm-init before the exec.
func mountRunTmpfs() {
	_ = os.MkdirAll(runTmpDir, 0o755)
	if err := syscall.Mount("tmpfs", runTmpDir, "tmpfs", 0, ""); err != nil {
		// Already mounted (by warm-init or a base image) is success, not failure.
		if err == syscall.EBUSY || err == syscall.EEXIST {
			return
		}
		// Non-fatal: log to the console and carry on — runs will then fail with a
		// legible EROFS rather than the agent refusing to serve at all.
		fmt.Fprintf(os.Stderr, "guest-agent: mount %s: %v\n", runTmpDir, err)
	}
}
