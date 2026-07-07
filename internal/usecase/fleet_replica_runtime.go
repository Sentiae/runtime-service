package usecase

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"github.com/sentiae/platform-kit/logger"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/repository"
)

// FleetReplicaRuntime turns a resident replica into a durable, bootable,
// health-checkable, decommissionable microVM on top of the CP3 ImageBooter
// (runtime-fleet CP4 §9#6). It owns only the per-replica boot/teardown/health
// mechanics — placement, scaling, and reconciliation live elsewhere.
type FleetReplicaRuntime struct {
	materializer ImageMaterializer
	booter       ImageBooter
	replicas     repository.ReplicaRepository
	apps         repository.FleetAppRepository
	workDir      string
	advertise    string
}

// NewFleetReplicaRuntime constructs the use case. workDir is the per-replica
// materialize staging root (mirrors FleetProvision); advertiseHost is the host
// address published in a resident replica's endpoint URL.
func NewFleetReplicaRuntime(
	materializer ImageMaterializer,
	booter ImageBooter,
	replicas repository.ReplicaRepository,
	apps repository.FleetAppRepository,
	workDir, advertiseHost string,
) *FleetReplicaRuntime {
	return &FleetReplicaRuntime{
		materializer: materializer,
		booter:       booter,
		replicas:     replicas,
		apps:         apps,
		workDir:      workDir,
		advertise:    advertiseHost,
	}
}

// BootReplica materializes the replica's app image and boots it as a resident
// microVM, recording the full Firecracker handle on the replica. It is
// idempotent: an already-resident replica is a no-op.
func (uc *FleetReplicaRuntime) BootReplica(ctx context.Context, replicaID uuid.UUID) error {
	replica, err := uc.replicas.FindByID(ctx, replicaID)
	if err != nil {
		return err
	}
	if replica.State == domain.ReplicaStateResident {
		return nil
	}

	app, err := uc.apps.FindByID(ctx, replica.AppID)
	if err != nil {
		return fmt.Errorf("load app: %w", err)
	}

	replica.State = domain.ReplicaStateBooting
	replica.Message = ""
	replica.UpdatedAt = time.Now().UTC()
	if err := uc.replicas.Update(ctx, replica); err != nil {
		return fmt.Errorf("persist booting replica: %w", err)
	}

	matIn := ImageMaterializeInput{
		Repository: app.ImageRepository,
		Digest:     app.ImageDigest,
		WorkDir:    filepath.Join(uc.workDir, replica.ID.String()),
		Mode:       string(domain.ImageWorkloadClassResident),
		Port:       app.Port,
	}
	mat, err := uc.materializer.Materialize(ctx, matIn)
	if err != nil {
		return uc.markDead(ctx, replica, fmt.Errorf("materialize: %w", err))
	}

	res, err := uc.booter.BootResident(ctx, ImageBootInput{
		WorkloadID: replica.ID,
		RootfsPath: mat.RootfsPath,
		VCPU:       app.ResourcesVCPU,
		MemoryMB:   int(app.ResourcesMemMB),
		Port:       app.Port,
	})
	if err != nil {
		return uc.markDead(ctx, replica, fmt.Errorf("boot resident: %w", err))
	}

	pid := res.PID
	replica.PID = &pid
	replica.GuestIP = res.GuestIP
	replica.HostPort = res.HostPort
	replica.NetIndex = res.NetIndex
	replica.TapName = res.TapName
	replica.SocketPath = res.SocketPath
	replica.RootfsPath = mat.RootfsPath
	replica.Port = app.Port
	replica.Endpoint = fmt.Sprintf("http://%s:%d", uc.advertise, res.HostPort)
	replica.State = domain.ReplicaStateResident
	replica.Message = ""
	replica.UpdatedAt = time.Now().UTC()
	if err := uc.replicas.Update(ctx, replica); err != nil {
		// The VM is up; tear it down so we don't leak an untracked resident.
		_ = uc.booter.Decommission(ctx, replicaDecommissionInput(replica))
		return fmt.Errorf("persist resident replica: %w", err)
	}
	return nil
}

// DecommissionReplica tears down a replica's microVM (best-effort) and deletes
// the replica row. Idempotent: a missing replica is a no-op.
func (uc *FleetReplicaRuntime) DecommissionReplica(ctx context.Context, replicaID uuid.UUID) error {
	replica, err := uc.replicas.FindByID(ctx, replicaID)
	if err != nil {
		if errors.Is(err, domain.ErrReplicaNotFound) {
			return nil
		}
		return err
	}
	if replica.PID != nil || replica.SocketPath != "" || replica.TapName != "" {
		if err := uc.booter.Decommission(ctx, replicaDecommissionInput(replica)); err != nil {
			// Best-effort: a half-gone VM must still clear its row.
			logger.FromContext(ctx).Warn("fleet replica decommission teardown", "replica_id", replica.ID, "err", err)
		}
	}
	if err := uc.replicas.Delete(ctx, replica.ID); err != nil {
		return fmt.Errorf("delete replica: %w", err)
	}
	return nil
}

// RefreshHealth probes a resident replica (process alive && guest port
// accepting) and, when it has died, marks it dead. It never restarts — that is
// the reconciler's job (§9#7). Non-resident replicas are reported not-healthy
// without a probe.
func (uc *FleetReplicaRuntime) RefreshHealth(ctx context.Context, replica *domain.Replica) (bool, error) {
	if replica.State != domain.ReplicaStateResident {
		return false, nil
	}
	alive := replica.PID != nil && processAlive(*replica.PID)
	if alive && replica.GuestIP != "" && replica.Port > 0 {
		alive = dialTCP(replica.GuestIP, replica.Port)
	}
	if !alive {
		replica.State = domain.ReplicaStateDead
		replica.Message = "vm process exited"
		replica.UpdatedAt = time.Now().UTC()
		if err := uc.replicas.Update(ctx, replica); err != nil {
			return false, fmt.Errorf("persist dead replica: %w", err)
		}
		return false, nil
	}
	return true, nil
}

// markDead records a boot/materialize failure on the replica and returns the
// original cause wrapped.
func (uc *FleetReplicaRuntime) markDead(ctx context.Context, replica *domain.Replica, cause error) error {
	replica.State = domain.ReplicaStateDead
	replica.Message = cause.Error()
	replica.UpdatedAt = time.Now().UTC()
	if err := uc.replicas.Update(ctx, replica); err != nil {
		logger.FromContext(ctx).Error("fleet replica: persist dead-state failed", "replica_id", replica.ID, "err", err)
	}
	return cause
}

// replicaDecommissionInput builds the teardown handle from a replica.
func replicaDecommissionInput(replica *domain.Replica) ImageDecommissionInput {
	pid := 0
	if replica.PID != nil {
		pid = *replica.PID
	}
	return ImageDecommissionInput{
		PID:        pid,
		SocketPath: replica.SocketPath,
		TapName:    replica.TapName,
		NetIndex:   replica.NetIndex,
		GuestIP:    replica.GuestIP,
		HostPort:   replica.HostPort,
		Port:       replica.Port,
		RootfsPath: replica.RootfsPath,
	}
}
