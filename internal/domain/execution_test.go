package domain

import (
	"testing"

	"github.com/google/uuid"
)

func TestExecution_Validate(t *testing.T) {
	tests := []struct {
		name    string
		exec    Execution
		wantErr error
	}{
		{
			name: "valid execution",
			exec: Execution{
				ID:             uuid.New(),
				OrganizationID: uuid.New(),
				RequestedBy:    uuid.New(),
				Language:       LanguagePython,
				Code:           "print('hello')",
				Status:         ExecutionStatusPending,
				Resources: ResourceLimit{
					VCPU: 1, MemoryMB: 128, TimeoutSec: 30,
					Network: NetworkModeIsolated,
				},
			},
			wantErr: nil,
		},
		{
			name: "nil ID",
			exec: Execution{
				ID:             uuid.Nil,
				OrganizationID: uuid.New(),
				RequestedBy:    uuid.New(),
				Language:       LanguagePython,
				Code:           "print('hello')",
				Status:         ExecutionStatusPending,
				Resources:      ResourceLimit{VCPU: 1, MemoryMB: 128, TimeoutSec: 30},
			},
			wantErr: ErrInvalidID,
		},
		{
			name: "empty code",
			exec: Execution{
				ID:             uuid.New(),
				OrganizationID: uuid.New(),
				RequestedBy:    uuid.New(),
				Language:       LanguagePython,
				Code:           "",
				Status:         ExecutionStatusPending,
				Resources:      ResourceLimit{VCPU: 1, MemoryMB: 128, TimeoutSec: 30},
			},
			wantErr: ErrEmptyCode,
		},
		{
			name: "invalid language",
			exec: Execution{
				ID:             uuid.New(),
				OrganizationID: uuid.New(),
				RequestedBy:    uuid.New(),
				Language:       Language("cobol"),
				Code:           "DISPLAY 'hello'",
				Status:         ExecutionStatusPending,
				Resources:      ResourceLimit{VCPU: 1, MemoryMB: 128, TimeoutSec: 30},
			},
			wantErr: ErrInvalidLanguage,
		},
		{
			name: "invalid vcpu",
			exec: Execution{
				ID:             uuid.New(),
				OrganizationID: uuid.New(),
				RequestedBy:    uuid.New(),
				Language:       LanguageGo,
				Code:           "package main",
				Status:         ExecutionStatusPending,
				Resources:      ResourceLimit{VCPU: 100, MemoryMB: 128, TimeoutSec: 30},
			},
			wantErr: ErrInvalidVCPU,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.exec.Validate()
			if tt.wantErr == nil && err != nil {
				t.Errorf("Validate() unexpected error: %v", err)
			}
			if tt.wantErr != nil && err != tt.wantErr {
				t.Errorf("Validate() = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestExecution_MarkRunning(t *testing.T) {
	exec := &Execution{Status: ExecutionStatusPending}
	vmID := uuid.New()
	exec.MarkRunning(vmID)

	if exec.Status != ExecutionStatusRunning {
		t.Errorf("Expected status running, got %s", exec.Status)
	}
	if exec.VMID == nil || *exec.VMID != vmID {
		t.Error("Expected VMID to be set")
	}
	if exec.StartedAt == nil {
		t.Error("Expected StartedAt to be set")
	}
}

func TestExecution_MarkCompleted(t *testing.T) {
	exec := &Execution{Status: ExecutionStatusRunning}
	now := exec.StartedAt
	_ = now

	exec.MarkCompleted(0, "output", "")

	if exec.Status != ExecutionStatusCompleted {
		t.Errorf("Expected status completed, got %s", exec.Status)
	}
	if exec.ExitCode == nil || *exec.ExitCode != 0 {
		t.Error("Expected exit code 0")
	}
	if exec.Stdout != "output" {
		t.Errorf("Expected stdout 'output', got %q", exec.Stdout)
	}
	if exec.CompletedAt == nil {
		t.Error("Expected CompletedAt to be set")
	}
}

func TestExecution_MarkFailed(t *testing.T) {
	exec := &Execution{Status: ExecutionStatusRunning}
	exec.MarkFailed("something broke", 1, "", "error output")

	if exec.Status != ExecutionStatusFailed {
		t.Errorf("Expected status failed, got %s", exec.Status)
	}
	if exec.Error != "something broke" {
		t.Errorf("Expected error message, got %q", exec.Error)
	}
	if exec.ExitCode == nil || *exec.ExitCode != 1 {
		t.Error("Expected exit code 1")
	}
}

func TestExecution_MarkTimeout(t *testing.T) {
	exec := &Execution{Status: ExecutionStatusRunning}
	exec.MarkTimeout("partial", "")

	if exec.Status != ExecutionStatusTimeout {
		t.Errorf("Expected status timeout, got %s", exec.Status)
	}
	if exec.Stdout != "partial" {
		t.Errorf("Expected partial stdout, got %q", exec.Stdout)
	}
}

func TestResourceLimit_Validate(t *testing.T) {
	tests := []struct {
		name    string
		rl      ResourceLimit
		wantErr error
	}{
		{
			name:    "valid",
			rl:      ResourceLimit{VCPU: 2, MemoryMB: 256, TimeoutSec: 60},
			wantErr: nil,
		},
		{
			name:    "vcpu too low",
			rl:      ResourceLimit{VCPU: 0, MemoryMB: 128, TimeoutSec: 30},
			wantErr: ErrInvalidVCPU,
		},
		{
			name:    "vcpu too high",
			rl:      ResourceLimit{VCPU: 9, MemoryMB: 128, TimeoutSec: 30},
			wantErr: ErrInvalidVCPU,
		},
		{
			name:    "memory too low",
			rl:      ResourceLimit{VCPU: 1, MemoryMB: 16, TimeoutSec: 30},
			wantErr: ErrInvalidMemory,
		},
		{
			name:    "timeout too high",
			rl:      ResourceLimit{VCPU: 1, MemoryMB: 128, TimeoutSec: 9999},
			wantErr: ErrInvalidTimeout,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.rl.Validate()
			if tt.wantErr == nil && err != nil {
				t.Errorf("Validate() unexpected error: %v", err)
			}
			if tt.wantErr != nil && err != tt.wantErr {
				t.Errorf("Validate() = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestResourceLimit_ApplyDefaults(t *testing.T) {
	rl := ResourceLimit{}
	rl.ApplyDefaults()

	if rl.VCPU != 1 {
		t.Errorf("Expected default VCPU 1, got %d", rl.VCPU)
	}
	if rl.MemoryMB != 128 {
		t.Errorf("Expected default MemoryMB 128, got %d", rl.MemoryMB)
	}
	if rl.TimeoutSec != 30 {
		t.Errorf("Expected default TimeoutSec 30, got %d", rl.TimeoutSec)
	}
	if rl.Network != NetworkModeIsolated {
		t.Errorf("Expected default Network isolated, got %s", rl.Network)
	}
	if rl.DiskMB != 256 {
		t.Errorf("Expected default DiskMB 256, got %d", rl.DiskMB)
	}
}

func TestExecutionStatus_IsTerminal(t *testing.T) {
	terminalStatuses := []ExecutionStatus{
		ExecutionStatusCompleted, ExecutionStatusFailed,
		ExecutionStatusTimeout, ExecutionStatusCancelled,
	}
	for _, s := range terminalStatuses {
		if !s.IsTerminal() {
			t.Errorf("Expected %s to be terminal", s)
		}
	}

	nonTerminalStatuses := []ExecutionStatus{
		ExecutionStatusPending, ExecutionStatusQueued, ExecutionStatusRunning,
	}
	for _, s := range nonTerminalStatuses {
		if s.IsTerminal() {
			t.Errorf("Expected %s to be non-terminal", s)
		}
	}
}

func TestLanguage_IsCompiled(t *testing.T) {
	compiled := []Language{LanguageGo, LanguageRust, LanguageC, LanguageCPP}
	for _, l := range compiled {
		if !l.IsCompiled() {
			t.Errorf("Expected %s to be compiled", l)
		}
	}

	interpreted := []Language{LanguagePython, LanguageJavaScript, LanguageTypeScript, LanguageBash}
	for _, l := range interpreted {
		if l.IsCompiled() {
			t.Errorf("Expected %s to be interpreted", l)
		}
	}
}

func TestMicroVM_IsAvailable(t *testing.T) {
	vm := &MicroVM{Status: VMStatusReady, ExecutionID: nil}
	if !vm.IsAvailable() {
		t.Error("Ready VM with no execution should be available")
	}

	execID := uuid.New()
	vm.ExecutionID = &execID
	if vm.IsAvailable() {
		t.Error("VM with assigned execution should not be available")
	}

	vm.ExecutionID = nil
	vm.Status = VMStatusRunning
	if vm.IsAvailable() {
		t.Error("Running VM should not be available")
	}
}

func TestMicroVM_AssignAndRelease(t *testing.T) {
	vm := &MicroVM{Status: VMStatusReady}
	execID := uuid.New()

	vm.AssignExecution(execID)
	if vm.Status != VMStatusRunning {
		t.Errorf("Expected running after assign, got %s", vm.Status)
	}
	if vm.ExecutionID == nil || *vm.ExecutionID != execID {
		t.Error("Expected execution ID to be set")
	}

	vm.Release()
	if vm.Status != VMStatusReady {
		t.Errorf("Expected ready after release, got %s", vm.Status)
	}
	if vm.ExecutionID != nil {
		t.Error("Expected execution ID to be nil after release")
	}
}

func TestMicroVM_Terminate(t *testing.T) {
	vm := &MicroVM{Status: VMStatusRunning}
	vm.Terminate()

	if vm.Status != VMStatusTerminated {
		t.Errorf("Expected terminated, got %s", vm.Status)
	}
	if vm.TerminatedAt == nil {
		t.Error("Expected TerminatedAt to be set")
	}
}

func TestSnapshot_Validate(t *testing.T) {
	valid := &Snapshot{
		ID:             uuid.New(),
		VMID:           uuid.New(),
		Language:       LanguagePython,
		MemoryFilePath: "/path/to/mem",
		StateFilePath:  "/path/to/state",
	}
	if err := valid.Validate(); err != nil {
		t.Errorf("Valid snapshot should pass: %v", err)
	}

	invalid := &Snapshot{
		ID:       uuid.Nil,
		VMID:     uuid.New(),
		Language: LanguagePython,
	}
	if err := invalid.Validate(); err != ErrInvalidID {
		t.Errorf("Expected ErrInvalidID, got %v", err)
	}
}
