package usecase

import (
	"context"
	"testing"

	"github.com/sentiae/runtime-service/internal/domain"
)

func TestScheduler_SelectHost_SingleHost(t *testing.T) {
	s := NewScheduler(4, 4096)

	host, err := s.SelectHost(context.Background(), 1, 128)
	if err != nil {
		t.Fatalf("SelectHost() error: %v", err)
	}
	if host.ID != "localhost" {
		t.Errorf("Expected localhost, got %s", host.ID)
	}
}

func TestScheduler_SelectHost_InsufficientResources(t *testing.T) {
	s := NewScheduler(2, 1024)

	_, err := s.SelectHost(context.Background(), 4, 128)
	if err != domain.ErrNoHostAvailable {
		t.Errorf("Expected ErrNoHostAvailable, got %v", err)
	}

	_, err = s.SelectHost(context.Background(), 1, 2048)
	if err != domain.ErrNoHostAvailable {
		t.Errorf("Expected ErrNoHostAvailable for memory, got %v", err)
	}
}

func TestScheduler_RegisterAndDeregister(t *testing.T) {
	s := NewScheduler(4, 4096)

	// Register a second host
	err := s.RegisterHost(context.Background(), HostInfo{
		ID:         "host-2",
		Address:    "192.168.1.10",
		TotalVCPU:  8,
		TotalMemMB: 16384,
		Available:  true,
	})
	if err != nil {
		t.Fatalf("RegisterHost() error: %v", err)
	}

	hosts, _ := s.ListHosts(context.Background())
	if len(hosts) != 2 {
		t.Errorf("Expected 2 hosts, got %d", len(hosts))
	}

	// Deregister the second host
	err = s.DeregisterHost(context.Background(), "host-2")
	if err != nil {
		t.Fatalf("DeregisterHost() error: %v", err)
	}

	hosts, _ = s.ListHosts(context.Background())
	if len(hosts) != 1 {
		t.Errorf("Expected 1 host after deregister, got %d", len(hosts))
	}
}

func TestScheduler_DeregisterNonExistent(t *testing.T) {
	s := NewScheduler(4, 4096)

	err := s.DeregisterHost(context.Background(), "non-existent")
	if err != domain.ErrHostNotFound {
		t.Errorf("Expected ErrHostNotFound, got %v", err)
	}
}

func TestScheduler_UpdateHostUsage(t *testing.T) {
	s := NewScheduler(4, 4096)

	err := s.UpdateHostUsage(context.Background(), "localhost", 2, 1024)
	if err != nil {
		t.Fatalf("UpdateHostUsage() error: %v", err)
	}

	hosts, _ := s.ListHosts(context.Background())
	if len(hosts) != 1 {
		t.Fatal("Expected 1 host")
	}
	h := hosts[0]
	if h.UsedVCPU != 2 {
		t.Errorf("Expected UsedVCPU 2, got %d", h.UsedVCPU)
	}
	if h.UsedMemMB != 1024 {
		t.Errorf("Expected UsedMemMB 1024, got %d", h.UsedMemMB)
	}

	// Now trying to place a VM that would exceed remaining capacity should fail
	_, err = s.SelectHost(context.Background(), 3, 128)
	if err != domain.ErrNoHostAvailable {
		t.Errorf("Expected ErrNoHostAvailable after resource usage, got %v", err)
	}
}

func TestScheduler_ReleaseHostResources(t *testing.T) {
	s := NewScheduler(4, 4096)

	_ = s.UpdateHostUsage(context.Background(), "localhost", 4, 4096)

	// Host should be full
	_, err := s.SelectHost(context.Background(), 1, 128)
	if err != domain.ErrNoHostAvailable {
		t.Errorf("Expected ErrNoHostAvailable when full, got %v", err)
	}

	// Release some resources
	err = s.ReleaseHostResources(context.Background(), "localhost", 2, 2048)
	if err != nil {
		t.Fatalf("ReleaseHostResources() error: %v", err)
	}

	// Now should be able to place a VM
	host, err := s.SelectHost(context.Background(), 1, 128)
	if err != nil {
		t.Fatalf("SelectHost() should succeed after release: %v", err)
	}
	if host.AvailableVCPU() != 2 {
		t.Errorf("Expected 2 available vCPU, got %d", host.AvailableVCPU())
	}
}

func TestScheduler_BestFitBinPacking(t *testing.T) {
	s := NewScheduler(2, 2048) // small localhost

	// Register a larger host
	_ = s.RegisterHost(context.Background(), HostInfo{
		ID:         "big-host",
		Address:    "10.0.0.1",
		TotalVCPU:  16,
		TotalMemMB: 32768,
		Available:  true,
	})

	// Best-fit should select localhost (tighter fit) for a small VM
	host, err := s.SelectHost(context.Background(), 1, 128)
	if err != nil {
		t.Fatalf("SelectHost() error: %v", err)
	}
	if host.ID != "localhost" {
		t.Errorf("Expected best-fit to select localhost (tighter), got %s", host.ID)
	}
}

func TestHostInfo_CanFit(t *testing.T) {
	h := HostInfo{
		TotalVCPU:  4,
		TotalMemMB: 4096,
		UsedVCPU:   2,
		UsedMemMB:  2048,
		Available:  true,
	}

	if !h.CanFit(2, 2048) {
		t.Error("Should fit exactly")
	}
	if h.CanFit(3, 2048) {
		t.Error("Should not fit: insufficient vCPU")
	}
	if h.CanFit(1, 3000) {
		t.Error("Should not fit: insufficient memory")
	}

	h.Available = false
	if h.CanFit(1, 128) {
		t.Error("Should not fit: host unavailable")
	}
}
