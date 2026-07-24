// Package gateway holds runtime-service's outbound port interfaces — the
// contracts a use case depends on and an adapter under internal/infrastructure
// implements (CLAUDE.md §7).
//
// This service predates the constitution and keeps most of its ports inline in
// internal/usecase/interfaces.go (see the service CLAUDE.md). This package is
// the canonical home for genuinely NEW seams, per that file's standing
// instruction not to deepen the deviation.
package gateway

import (
	"context"

	"github.com/sentiae/runtime-service/internal/domain"
)

// GuestControl is the host's post-boot control channel into a RESIDENT guest
// (D-185a). Every method addresses the VM by its host-view Firecracker API
// socket path — the same handle every other consumer already carries
// (FleetReplica.SocketPath); the vsock UDS is derived from it as
// socketPath + ".vsock".
//
// It exists because Firecracker's Pause freezes vCPUs without flushing the
// GUEST kernel's dirty page cache: quiescing a data volume host-side alone
// produces torn state. The host has to ask the guest.
type GuestControl interface {
	// SyncFS asks the guest to flush its data filesystem (syncfs(2)).
	SyncFS(ctx context.Context, socketPath string) error
	// Freeze asks the guest to flush and then freeze its data filesystem. The
	// guest arms a dead-man auto-thaw, so a host that dies before Thaw cannot
	// wedge the guest permanently — but the caller still owns thawing.
	Freeze(ctx context.Context, socketPath string) error
	// Thaw releases a Freeze. Thawing a filesystem that is not frozen is a
	// benign success, so a crash-recovery path may call it unconditionally.
	Thaw(ctx context.Context, socketPath string) error
	// Shutdown asks the guest to stop the workload gracefully (SIGINT — the
	// engine image's STOPSIGNAL, i.e. Postgres fast shutdown) and returns once
	// the workload child has exited.
	Shutdown(ctx context.Context, socketPath string) error
}

// FailLoudGuestControl is wired when the executor is not firecracker. Every
// call fails with domain.ErrGuestControlUnavailable so a quiesce is never
// silently faked on a host that has no microVMs to talk to — a faked quiesce
// would let a caller believe a volume was consistent when it was not.
type FailLoudGuestControl struct{}

var _ GuestControl = FailLoudGuestControl{}

func (FailLoudGuestControl) SyncFS(context.Context, string) error {
	return domain.ErrGuestControlUnavailable
}

func (FailLoudGuestControl) Freeze(context.Context, string) error {
	return domain.ErrGuestControlUnavailable
}

func (FailLoudGuestControl) Thaw(context.Context, string) error {
	return domain.ErrGuestControlUnavailable
}

func (FailLoudGuestControl) Shutdown(context.Context, string) error {
	return domain.ErrGuestControlUnavailable
}
