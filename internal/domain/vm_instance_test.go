package domain

import (
	"testing"

	"github.com/google/uuid"
)

func TestVMInstance_Validate(t *testing.T) {
	tests := []struct {
		name    string
		inst    VMInstance
		wantErr error
	}{
		{
			name: "valid instance",
			inst: VMInstance{
				ID:           uuid.New(),
				State:        VMInstanceStatePending,
				DesiredState: VMInstanceStateRunning,
				Language:     LanguagePython,
				VCPU:         1,
				MemoryMB:     128,
			},
			wantErr: nil,
		},
		{
			name: "nil ID",
			inst: VMInstance{
				ID:           uuid.Nil,
				State:        VMInstanceStatePending,
				DesiredState: VMInstanceStateRunning,
				Language:     LanguagePython,
				VCPU:         1,
				MemoryMB:     128,
			},
			wantErr: ErrInvalidID,
		},
		{
			name: "invalid state",
			inst: VMInstance{
				ID:           uuid.New(),
				State:        VMInstanceState("bogus"),
				DesiredState: VMInstanceStateRunning,
				Language:     LanguagePython,
				VCPU:         1,
				MemoryMB:     128,
			},
			wantErr: ErrInvalidData,
		},
		{
			name: "invalid desired state",
			inst: VMInstance{
				ID:           uuid.New(),
				State:        VMInstanceStatePending,
				DesiredState: VMInstanceState("bogus"),
				Language:     LanguagePython,
				VCPU:         1,
				MemoryMB:     128,
			},
			wantErr: ErrInvalidData,
		},
		{
			name: "invalid language",
			inst: VMInstance{
				ID:           uuid.New(),
				State:        VMInstanceStatePending,
				DesiredState: VMInstanceStateRunning,
				Language:     Language("cobol"),
				VCPU:         1,
				MemoryMB:     128,
			},
			wantErr: ErrInvalidLanguage,
		},
		{
			name: "invalid vcpu",
			inst: VMInstance{
				ID:           uuid.New(),
				State:        VMInstanceStatePending,
				DesiredState: VMInstanceStateRunning,
				Language:     LanguageGo,
				VCPU:         0,
				MemoryMB:     128,
			},
			wantErr: ErrInvalidVCPU,
		},
		{
			name: "invalid memory",
			inst: VMInstance{
				ID:           uuid.New(),
				State:        VMInstanceStatePending,
				DesiredState: VMInstanceStateRunning,
				Language:     LanguageGo,
				VCPU:         1,
				MemoryMB:     16,
			},
			wantErr: ErrInvalidMemory,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.inst.Validate()
			if tt.wantErr == nil && err != nil {
				t.Errorf("Validate() unexpected error: %v", err)
			}
			if tt.wantErr != nil && err != tt.wantErr {
				t.Errorf("Validate() = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestVMInstance_NeedsReconciliation(t *testing.T) {
	tests := []struct {
		name   string
		state  VMInstanceState
		desired VMInstanceState
		needs  bool
	}{
		{"pending -> running", VMInstanceStatePending, VMInstanceStateRunning, true},
		{"running -> running", VMInstanceStateRunning, VMInstanceStateRunning, false},
		{"running -> paused", VMInstanceStateRunning, VMInstanceStatePaused, true},
		{"running -> terminated", VMInstanceStateRunning, VMInstanceStateTerminated, true},
		{"terminated -> terminated", VMInstanceStateTerminated, VMInstanceStateTerminated, false},
		{"error -> terminated", VMInstanceStateError, VMInstanceStateTerminated, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inst := &VMInstance{
				State:        tt.state,
				DesiredState: tt.desired,
			}
			if got := inst.NeedsReconciliation(); got != tt.needs {
				t.Errorf("NeedsReconciliation() = %v, want %v", got, tt.needs)
			}
		})
	}
}

func TestVMInstance_SetState(t *testing.T) {
	inst := &VMInstance{State: VMInstanceStatePending}
	inst.SetState(VMInstanceStateRunning)

	if inst.State != VMInstanceStateRunning {
		t.Errorf("Expected running, got %s", inst.State)
	}
	if inst.UpdatedAt.IsZero() {
		t.Error("Expected UpdatedAt to be set")
	}
}

func TestVMInstance_SetError(t *testing.T) {
	inst := &VMInstance{State: VMInstanceStateBooting}
	inst.SetError("boot failed")

	if inst.State != VMInstanceStateError {
		t.Errorf("Expected error state, got %s", inst.State)
	}
	if inst.ErrorMessage != "boot failed" {
		t.Errorf("Expected error message, got %q", inst.ErrorMessage)
	}
}

func TestVMInstance_MarkTerminated(t *testing.T) {
	inst := &VMInstance{State: VMInstanceStateRunning}
	inst.MarkTerminated()

	if inst.State != VMInstanceStateTerminated {
		t.Errorf("Expected terminated, got %s", inst.State)
	}
	if inst.TerminatedAt == nil {
		t.Error("Expected TerminatedAt to be set")
	}
}

func TestVMInstanceState_IsValid(t *testing.T) {
	validStates := []VMInstanceState{
		VMInstanceStatePending, VMInstanceStateBooting, VMInstanceStateRunning,
		VMInstanceStatePaused, VMInstanceStateTerminated, VMInstanceStateError,
	}
	for _, s := range validStates {
		if !s.IsValid() {
			t.Errorf("Expected %s to be valid", s)
		}
	}

	if VMInstanceState("invalid").IsValid() {
		t.Error("Expected 'invalid' to be invalid")
	}
}

func TestVMInstanceState_IsTerminal(t *testing.T) {
	if !VMInstanceStateTerminated.IsTerminal() {
		t.Error("terminated should be terminal")
	}
	if !VMInstanceStateError.IsTerminal() {
		t.Error("error should be terminal")
	}
	if VMInstanceStateRunning.IsTerminal() {
		t.Error("running should not be terminal")
	}
	if VMInstanceStatePending.IsTerminal() {
		t.Error("pending should not be terminal")
	}
}
