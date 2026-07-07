package usecase

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/repository"
)

// FleetHostRegistry owns the durable fleet host inventory (runtime-fleet CP4
// §9#4): host registration, heartbeat, and the live-host query the scheduler
// (§9#5) consumes. It holds no placement, scheduling, or reconciliation logic.
type FleetHostRegistry struct {
	repo repository.HostRepository
}

// NewFleetHostRegistry constructs the registry use case.
func NewFleetHostRegistry(repo repository.HostRepository) *FleetHostRegistry {
	return &FleetHostRegistry{repo: repo}
}

// RegisterHost upserts a host by id. A new host is created healthy/active with
// allocatable seeded from capacity; re-registering an existing host refreshes
// its spec (region/labels/capacity/endpoint) and marks it healthy/active
// without clobbering the live allocatable accounting a heartbeat maintains.
func (uc *FleetHostRegistry) RegisterHost(ctx context.Context, host domain.Host) (domain.Host, error) {
	now := time.Now().UTC()
	if host.ID == uuid.Nil {
		host.ID = uuid.New()
	}
	// labels is JSONB NOT NULL DEFAULT '{}' — GORM serializes a nil map to
	// SQL NULL (the DB default never applies on an explicit insert), so
	// normalize before any write.
	if host.Labels == nil {
		host.Labels = map[string]string{}
	}

	existing, err := uc.repo.FindByID(ctx, host.ID)
	if err != nil && !errors.Is(err, domain.ErrFleetHostNotFound) {
		return domain.Host{}, fmt.Errorf("load host: %w", err)
	}

	if existing != nil {
		existing.Region = host.Region
		existing.Labels = host.Labels
		existing.CapacityVCPU = host.CapacityVCPU
		existing.CapacityMemMB = host.CapacityMemMB
		existing.CapacityDiskMB = host.CapacityDiskMB
		existing.Endpoint = host.Endpoint
		existing.Health = domain.HostHealthHealthy
		existing.Status = domain.HostStatusActive
		existing.LastHeartbeat = &now
		existing.UpdatedAt = now
		if err := uc.repo.Update(ctx, existing); err != nil {
			return domain.Host{}, fmt.Errorf("update host: %w", err)
		}
		return *existing, nil
	}

	host.AllocatableVCPU = host.CapacityVCPU
	host.AllocatableMemMB = host.CapacityMemMB
	host.AllocatableDiskMB = host.CapacityDiskMB
	host.Health = domain.HostHealthHealthy
	host.Status = domain.HostStatusActive
	host.LastHeartbeat = &now
	host.CreatedAt = now
	host.UpdatedAt = now
	if err := uc.repo.Create(ctx, &host); err != nil {
		return domain.Host{}, fmt.Errorf("create host: %w", err)
	}
	return host, nil
}

// Heartbeat refreshes a host's liveness + allocatable capacity. An empty health
// keeps the prior value; a non-empty health must be a recognized HostHealth.
func (uc *FleetHostRegistry) Heartbeat(ctx context.Context, hostID uuid.UUID, allocVCPU int, allocMemMB, allocDiskMB int64, health string) error {
	host, err := uc.repo.FindByID(ctx, hostID)
	if err != nil {
		return err
	}
	if health != "" {
		h := domain.HostHealth(health)
		if !h.IsValid() {
			return domain.ErrInvalidHostHealth
		}
		host.Health = h
	}
	now := time.Now().UTC()
	host.AllocatableVCPU = allocVCPU
	host.AllocatableMemMB = allocMemMB
	host.AllocatableDiskMB = allocDiskMB
	host.LastHeartbeat = &now
	host.UpdatedAt = now
	if err := uc.repo.Update(ctx, host); err != nil {
		return fmt.Errorf("update host heartbeat: %w", err)
	}
	return nil
}

// ListHosts returns every host in the inventory.
func (uc *FleetHostRegistry) ListHosts(ctx context.Context) ([]domain.Host, error) {
	return uc.repo.List(ctx)
}

// ListLive returns hosts that are active, healthy, and have heartbeated within
// staleness of now — the placement candidate set the scheduler (§9#5) consumes.
func (uc *FleetHostRegistry) ListLive(ctx context.Context, staleness time.Duration) ([]domain.Host, error) {
	hosts, err := uc.repo.ListByStatus(ctx, domain.HostStatusActive)
	if err != nil {
		return nil, err
	}
	cutoff := time.Now().UTC().Add(-staleness)
	live := make([]domain.Host, 0, len(hosts))
	for i := range hosts {
		h := hosts[i]
		if h.Health != domain.HostHealthHealthy {
			continue
		}
		if h.LastHeartbeat == nil || h.LastHeartbeat.Before(cutoff) {
			continue
		}
		live = append(live, h)
	}
	return live, nil
}
