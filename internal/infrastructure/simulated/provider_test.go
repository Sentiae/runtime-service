package simulated

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/usecase"
)

func TestBoot(t *testing.T) {
	p := NewProvider()
	vmID := uuid.New()

	result, err := p.Boot(context.Background(), usecase.VMBootConfig{
		VMID:     vmID,
		Language: domain.LanguagePython,
		VCPU:     1,
		MemoryMB: 128,
	})
	if err != nil {
		t.Fatalf("Boot returned error: %v", err)
	}
	if result.SocketPath == "" {
		t.Error("expected non-empty SocketPath")
	}
	if result.BootTimeMS <= 0 {
		t.Error("expected positive BootTimeMS")
	}
}

func TestTerminate(t *testing.T) {
	p := NewProvider()
	if err := p.Terminate(context.Background(), "simulated-vm-test", 0); err != nil {
		t.Fatalf("Terminate returned error: %v", err)
	}
}

func TestRun_Python(t *testing.T) {
	p := NewProvider()
	vm := &domain.MicroVM{
		ID:       uuid.New(),
		Language: domain.LanguagePython,
	}
	exec := &domain.Execution{
		ID:       uuid.New(),
		Language: domain.LanguagePython,
		Code:     "print('hello world')",
	}

	result, err := p.Run(context.Background(), vm, exec)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("expected exit code 0, got %d", result.ExitCode)
	}
	if result.Stdout == "" {
		t.Error("expected non-empty stdout")
	}
	if result.Stdout != "hello world\n" {
		t.Errorf("expected 'hello world\\n', got %q", result.Stdout)
	}
}

func TestRun_JavaScript(t *testing.T) {
	p := NewProvider()
	vm := &domain.MicroVM{ID: uuid.New(), Language: domain.LanguageJavaScript}
	exec := &domain.Execution{
		ID:       uuid.New(),
		Language: domain.LanguageJavaScript,
		Code:     "console.log('hi')",
	}

	result, err := p.Run(context.Background(), vm, exec)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("expected exit code 0, got %d", result.ExitCode)
	}
	if result.Stdout != "hi\n" {
		t.Errorf("expected 'hi\\n', got %q", result.Stdout)
	}
}

func TestRun_Bash(t *testing.T) {
	p := NewProvider()
	vm := &domain.MicroVM{ID: uuid.New(), Language: domain.LanguageBash}
	exec := &domain.Execution{
		ID:       uuid.New(),
		Language: domain.LanguageBash,
		Code:     "echo test123",
	}

	result, err := p.Run(context.Background(), vm, exec)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Stdout != "test123\n" {
		t.Errorf("expected 'test123\\n', got %q", result.Stdout)
	}
}

func TestRun_ContextCancelled(t *testing.T) {
	p := NewProvider()
	vm := &domain.MicroVM{ID: uuid.New(), Language: domain.LanguagePython}
	exec := &domain.Execution{
		ID:       uuid.New(),
		Language: domain.LanguagePython,
		Code:     "print('should timeout')",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Millisecond)
	defer cancel()
	time.Sleep(5 * time.Millisecond) // ensure context expires before Run's sleep

	result, err := p.Run(ctx, vm, exec)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.ExitCode != 124 {
		t.Errorf("expected exit code 124 for timeout, got %d", result.ExitCode)
	}
}

func TestRun_NoPrintFallback(t *testing.T) {
	p := NewProvider()
	vm := &domain.MicroVM{ID: uuid.New(), Language: domain.LanguageGo}
	exec := &domain.Execution{
		ID:       uuid.New(),
		Language: domain.LanguageGo,
		Code:     "package main\nfunc main() {}\n",
	}

	result, err := p.Run(context.Background(), vm, exec)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("expected exit code 0, got %d", result.ExitCode)
	}
	if result.Stdout == "" {
		t.Error("expected non-empty fallback stdout")
	}
}

func TestCollectMetrics(t *testing.T) {
	p := NewProvider()
	m, err := p.CollectMetrics(context.Background(), "")
	if err != nil {
		t.Fatalf("CollectMetrics returned error: %v", err)
	}
	if m == nil {
		t.Error("expected non-nil metrics")
	}
}
