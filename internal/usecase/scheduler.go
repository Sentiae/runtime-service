package usecase

import (
	"context"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/sentiae/runtime-service/internal/domain"
)

// scheduler implements the SchedulerUseCase interface with a simple
// bin-packing algorithm. It maintains an in-memory registry of available
// hosts and selects the best-fit host for each VM placement request.
//
// For the initial implementation this works with a single localhost host.
// When the Rust agent is deployed on multiple hosts, RegisterHost will be
// called for each agent that comes online.
type scheduler struct {
	mu    sync.RWMutex
	hosts map[string]*HostInfo
}

// NewScheduler creates a new scheduler with an optional initial localhost registration.
func NewScheduler(localVCPU, localMemMB int) SchedulerUseCase {
	s := &scheduler{
		hosts: make(map[string]*HostInfo),
	}

	// Register localhost as the default host for single-node operation
	localhost := HostInfo{
		ID:            "localhost",
		Address:       "127.0.0.1",
		TotalVCPU:     localVCPU,
		TotalMemMB:    localMemMB,
		UsedVCPU:      0,
		UsedMemMB:     0,
		Available:     true,
		LastHeartbeat: time.Now().UTC(),
	}
	s.hosts[localhost.ID] = &localhost

	log.Printf("Scheduler initialized with localhost: vcpu=%d, mem=%dMB", localVCPU, localMemMB)
	return s
}

// SelectHost picks the best host for a VM with the given resource requirements.
// It uses best-fit bin-packing: among hosts that can fit the VM, it chooses
// the one with the least remaining resources (tightest fit) to reduce fragmentation.
func (s *scheduler) SelectHost(_ context.Context, vcpu int, memoryMB int) (*HostInfo, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var candidates []*HostInfo
	for _, h := range s.hosts {
		if h.CanFit(vcpu, memoryMB) {
			candidates = append(candidates, h)
		}
	}

	if len(candidates) == 0 {
		return nil, domain.ErrNoHostAvailable
	}

	// Best-fit: sort by remaining resources (ascending) to pack tightly
	sort.Slice(candidates, func(i, j int) bool {
		remainI := candidates[i].AvailableVCPU() + candidates[i].AvailableMemMB()
		remainJ := candidates[j].AvailableVCPU() + candidates[j].AvailableMemMB()
		return remainI < remainJ
	})

	selected := candidates[0]
	log.Printf("Scheduler selected host %s for request vcpu=%d mem=%dMB (avail: vcpu=%d mem=%dMB)",
		selected.ID, vcpu, memoryMB, selected.AvailableVCPU(), selected.AvailableMemMB())

	return selected, nil
}

// RegisterHost adds or updates a host in the scheduler registry.
func (s *scheduler) RegisterHost(_ context.Context, host HostInfo) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	host.LastHeartbeat = time.Now().UTC()
	s.hosts[host.ID] = &host

	log.Printf("Scheduler registered host: %s (vcpu=%d, mem=%dMB)", host.ID, host.TotalVCPU, host.TotalMemMB)
	return nil
}

// DeregisterHost removes a host from the scheduler registry.
func (s *scheduler) DeregisterHost(_ context.Context, hostID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.hosts[hostID]; !exists {
		return domain.ErrHostNotFound
	}

	delete(s.hosts, hostID)
	log.Printf("Scheduler deregistered host: %s", hostID)
	return nil
}

// ListHosts returns all registered hosts with current capacity information.
func (s *scheduler) ListHosts(_ context.Context) ([]HostInfo, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]HostInfo, 0, len(s.hosts))
	for _, h := range s.hosts {
		result = append(result, *h)
	}
	return result, nil
}

// UpdateHostUsage updates the resource usage counters for a host.
// This is called when a VM is placed on a host.
func (s *scheduler) UpdateHostUsage(_ context.Context, hostID string, usedVCPU int, usedMemoryMB int) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	host, exists := s.hosts[hostID]
	if !exists {
		return domain.ErrHostNotFound
	}

	host.UsedVCPU += usedVCPU
	host.UsedMemMB += usedMemoryMB

	// Clamp to totals
	if host.UsedVCPU > host.TotalVCPU {
		host.UsedVCPU = host.TotalVCPU
	}
	if host.UsedMemMB > host.TotalMemMB {
		host.UsedMemMB = host.TotalMemMB
	}

	return nil
}

// ReleaseHostResources releases resources on a host when a VM is terminated.
func (s *scheduler) ReleaseHostResources(_ context.Context, hostID string, vcpu int, memoryMB int) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	host, exists := s.hosts[hostID]
	if !exists {
		return fmt.Errorf("host %s not found for resource release", hostID)
	}

	host.UsedVCPU -= vcpu
	host.UsedMemMB -= memoryMB

	// Clamp to zero
	if host.UsedVCPU < 0 {
		host.UsedVCPU = 0
	}
	if host.UsedMemMB < 0 {
		host.UsedMemMB = 0
	}

	return nil
}
