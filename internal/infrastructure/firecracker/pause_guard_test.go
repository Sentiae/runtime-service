package firecracker

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/sentiae/runtime-service/internal/domain"
)

// A resident/data VM must be REFUSED by anything that can pause it — firecracker
// vsock does not survive Pause/Resume, so one pause kills the guest control
// channel a database VM's snapshots, shutdown and park all ride on. The refusal
// is asserted, not "it happens not to be wired today".
func TestCheckpointSchedulerRegisterRefusesUnpausableVMs(t *testing.T) {
	tests := []struct {
		name    string
		class   VMClass
		wantErr error
	}{
		{"resident data VM is refused", VMClassResident, domain.ErrPauseUnsafeForResidentVM},
		{"undeclared class is refused", "", domain.ErrVMClassUndeclared},
		{"unknown class is refused", VMClass("database"), domain.ErrVMClassUndeclared},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backend := newFakeBackend()
			sched := NewCheckpointScheduler(backend, silentLogger())
			defer sched.Close()

			err := sched.Register(context.Background(), VMRegistration{
				VMID:                      uuid.New(),
				SocketPath:                "/tmp/sock",
				Class:                     tt.class,
				CheckpointIntervalMinutes: 1,
			})
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Register(%q) = %v, want %v", tt.class, err, tt.wantErr)
			}
			if sched.Tracking() != 0 {
				t.Fatalf("refused VM must not be tracked, got %d", sched.Tracking())
			}
		})
	}
}

func TestCheckpointSchedulerRegisterAcceptsPausableVM(t *testing.T) {
	backend := newFakeBackend()
	sched := NewCheckpointScheduler(backend, silentLogger())
	defer sched.Close()

	if err := sched.Register(context.Background(), VMRegistration{
		VMID:                      uuid.New(),
		SocketPath:                "/tmp/sock",
		Class:                     VMClassPausable,
		CheckpointIntervalMinutes: 1,
	}); err != nil {
		t.Fatalf("pausable VM must register: %v", err)
	}
	if sched.Tracking() != 1 {
		t.Fatalf("tracking = %d, want 1", sched.Tracking())
	}
}

// The warm manager has no Register seam, so its guard sits on the pausing call
// itself: a WarmVM that is not the pausable template class never reaches
// PATCH /vm {"state":"Paused"}. The nil Provider proves it: the guard returns
// before anything that would dereference it.
func TestCreateTemplateSnapshotRefusesUnpausableVMs(t *testing.T) {
	tests := []struct {
		name    string
		class   VMClass
		wantErr error
	}{
		{"resident data VM is refused", VMClassResident, domain.ErrPauseUnsafeForResidentVM},
		{"undeclared class is refused", "", domain.ErrVMClassUndeclared},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &WarmManager{}
			warm := &WarmVM{
				ID:         uuid.New(),
				SocketPath: "/tmp/warm.sock",
				Class:      tt.class,
				jail:       newVMJail("/srv/jail", "warm", 30000),
			}
			_, err := m.CreateTemplateSnapshot(context.Background(), warm)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("CreateTemplateSnapshot(class=%q) = %v, want %v", tt.class, err, tt.wantErr)
			}
		})
	}
}
