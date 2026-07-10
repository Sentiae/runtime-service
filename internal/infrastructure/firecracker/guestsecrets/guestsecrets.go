// Package guestsecrets holds the constants shared by the host-side secret
// pusher (internal/infrastructure/firecracker) and the in-guest receiver
// (cmd/image-init) so both ends of the host->guest vsock secret channel agree
// on the port and layout. It has no build constraints and no heavy imports so
// the static linux-only image-init binary can depend on it (invariant I32).
package guestsecrets

const (
	// SecretPort is the AF_VSOCK port the guest listens on and the host targets
	// via Firecracker's "CONNECT <port>" host->guest handshake. Both ends must
	// agree; changing it requires rebuilding the image-init binary too.
	SecretPort = 10015

	// MountDir is the in-guest tmpfs (RAM-only, mode 0700) into which the guest
	// writes each received secret as a 0600 file. Never backed by the rootfs.
	MountDir = "/sentiae/secrets"
)
