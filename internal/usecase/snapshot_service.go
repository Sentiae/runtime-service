package usecase

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/repository"
)

type snapshotService struct {
	snapshotRepo repository.SnapshotRepository
	vmRepo       repository.MicroVMRepository
	vmProvider   VMProvider

	// store, when non-nil, makes snapshots durable: mem/state files are
	// uploaded to it after creation and re-fetched from it on restore
	// when the local copy is absent (e.g. restoring on a different host).
	// Nil store preserves the original local-only create/restore flow.
	store ArtifactStore
}

// NewSnapshotService creates a new snapshot use case service
func NewSnapshotService(
	snapshotRepo repository.SnapshotRepository,
	vmRepo repository.MicroVMRepository,
	vmProvider VMProvider,
) SnapshotUseCase {
	return &snapshotService{
		snapshotRepo: snapshotRepo,
		vmRepo:       vmRepo,
		vmProvider:   vmProvider,
	}
}

// SnapshotStoreInjectable is implemented by snapshot services that accept
// a durable object-store backing. DI type-asserts against this interface
// to wire the store without depending on the unexported concrete type
// (mirrors the eventPublishable pattern used for the execution service).
type SnapshotStoreInjectable interface {
	SetArtifactStore(store ArtifactStore)
}

// SetArtifactStore wires a durable object-store backing for snapshots.
// Passing nil is a no-op that keeps the local-only flow.
func (s *snapshotService) SetArtifactStore(store ArtifactStore) {
	s.store = store
}

// snapshotMemKey / snapshotStateKey derive stable object-store keys from
// the snapshot id. Keys are namespaced under snapshots/<id>/ so the
// durable store stays browsable and a snapshot's two blobs sort together.
func snapshotMemKey(id uuid.UUID) string   { return "snapshots/" + id.String() + "/mem" }
func snapshotStateKey(id uuid.UUID) string { return "snapshots/" + id.String() + "/state" }

// uploadSnapshotFile streams a local file into the artifact store under
// the given key. Used to make a freshly-created snapshot durable.
func uploadSnapshotFile(store ArtifactStore, key, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	if err := store.Put(key, f); err != nil {
		return fmt.Errorf("put %s: %w", key, err)
	}
	return nil
}

// uploadSnapshotFileHashed streams a local file into the artifact store and
// returns the lowercase hex sha256 of exactly the bytes the store consumed.
// Hashing rides the SAME single pass as the upload (io.TeeReader) — a second
// read of a multi-GB backing file would double the snapshot's IO cost.
//
// The byte-count cross-check is the honesty gate: if the store did not consume
// the whole file, the digest would describe a prefix, and a restore would
// "verify" against a checksum of bytes nobody stored. Refuse instead.
func uploadSnapshotFileHashed(store ArtifactStore, key, path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("stat %s: %w", path, err)
	}
	h := sha256.New()
	counted := &countingReader{r: io.TeeReader(f, h)}
	if err := store.Put(key, counted); err != nil {
		return "", fmt.Errorf("put %s: %w", key, err)
	}
	if counted.n != st.Size() {
		return "", fmt.Errorf("artifact store consumed %d of %d bytes for %s", counted.n, st.Size(), key)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// countingReader counts the bytes a consumer actually read.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// ensureLocalFile guarantees a snapshot file is present at localPath,
// pulling it from the artifact store under objectKey when the local copy
// is missing. Returns the path to use (always localPath on success). When
// no store or key is available it just reports whether the local file
// exists so the caller can fail with a clear "not found" rather than
// handing a missing path to Firecracker.
func ensureLocalFile(store ArtifactStore, objectKey, localPath string) error {
	if _, err := os.Stat(localPath); err == nil {
		return nil // already present — fast local restore path
	}
	if store == nil || objectKey == "" {
		return fmt.Errorf("snapshot file missing locally and no object store key: %s", localPath)
	}
	rc, err := store.Get(objectKey)
	if err != nil {
		return fmt.Errorf("fetch %s from object store: %w", objectKey, err)
	}
	defer rc.Close()
	if err := os.MkdirAll(dirOf(localPath), 0o750); err != nil {
		return fmt.Errorf("mkdir for %s: %w", localPath, err)
	}
	tmp, err := os.CreateTemp(dirOf(localPath), ".snap-*.tmp")
	if err != nil {
		return fmt.Errorf("tempfile for %s: %w", localPath, err)
	}
	tmpPath := tmp.Name()
	if _, err := io.Copy(tmp, rc); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("write %s: %w", localPath, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, localPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename into %s: %w", localPath, err)
	}
	return nil
}

func dirOf(p string) string { return filepath.Dir(p) }

// resumeQuietly resumes a VM after a snapshot-path failure, logging (but
// not propagating) any resume error — matching the existing failure
// handling in CreateSnapshot.
func (s *snapshotService) resumeQuietly(ctx context.Context, socketPath string, vmID uuid.UUID) {
	if resumeErr := s.vmProvider.Resume(ctx, socketPath); resumeErr != nil {
		log.Printf("Warning: failed to resume VM %s after snapshot failure: %v", vmID, resumeErr)
	}
}

func (s *snapshotService) CreateSnapshot(ctx context.Context, vmID uuid.UUID, description string) (*domain.Snapshot, error) {
	vm, err := s.vmRepo.FindByID(ctx, vmID)
	if err != nil {
		return nil, err
	}

	if vm.Status != domain.VMStatusRunning && vm.Status != domain.VMStatusReady {
		return nil, domain.ErrVMNotReady
	}

	// Pause the VM for a consistent snapshot
	if err := s.vmProvider.Pause(ctx, vm.SocketPath); err != nil {
		return nil, fmt.Errorf("failed to pause VM for snapshot: %w", err)
	}

	start := time.Now()
	snapshotID := uuid.New()

	// Call the real Firecracker snapshot API via the provider
	result, err := s.vmProvider.CreateSnapshot(ctx, vm.SocketPath, snapshotID)
	if err != nil {
		// Resume the VM even if snapshot fails
		if resumeErr := s.vmProvider.Resume(ctx, vm.SocketPath); resumeErr != nil {
			log.Printf("Warning: failed to resume VM %s after snapshot failure: %v", vmID, resumeErr)
		}
		return nil, fmt.Errorf("failed to create snapshot via Firecracker API: %w", err)
	}

	createTimeMS := time.Since(start).Milliseconds()

	// Make the snapshot durable: upload mem + state to the object store so
	// the snapshot is restorable on any host (object store = source of
	// truth). Nil store keeps the original local-only flow. An upload
	// failure aborts the snapshot — a snapshot that only exists on this
	// host's disk is not the durable artifact callers asked for; we resume
	// the VM and clean up the local files before returning the error.
	var memObjectKey, stateObjectKey string
	if s.store != nil {
		memObjectKey = snapshotMemKey(snapshotID)
		stateObjectKey = snapshotStateKey(snapshotID)
		if err := uploadSnapshotFile(s.store, memObjectKey, result.MemoryFilePath); err != nil {
			s.resumeQuietly(ctx, vm.SocketPath, vmID)
			if delErr := s.vmProvider.DeleteSnapshotFiles(result.MemoryFilePath, result.StateFilePath); delErr != nil {
				log.Printf("Warning: failed to clean up snapshot files for %s after upload failure: %v", snapshotID, delErr)
			}
			return nil, fmt.Errorf("failed to upload snapshot memory to object store: %w", err)
		}
		if err := uploadSnapshotFile(s.store, stateObjectKey, result.StateFilePath); err != nil {
			s.resumeQuietly(ctx, vm.SocketPath, vmID)
			if delErr := s.vmProvider.DeleteSnapshotFiles(result.MemoryFilePath, result.StateFilePath); delErr != nil {
				log.Printf("Warning: failed to clean up snapshot files for %s after upload failure: %v", snapshotID, delErr)
			}
			return nil, fmt.Errorf("failed to upload snapshot state to object store: %w", err)
		}
		log.Printf("Snapshot uploaded to object store: %s (mem=%s, state=%s)", snapshotID, memObjectKey, stateObjectKey)
	}

	snapshot := &domain.Snapshot{
		ID:              snapshotID,
		VMID:            vmID,
		ExecutionID:     vm.ExecutionID,
		Language:        vm.Language,
		MemoryFilePath:  result.MemoryFilePath,
		StateFilePath:   result.StateFilePath,
		MemoryObjectKey: memObjectKey,
		StateObjectKey:  stateObjectKey,
		SizeBytes:       result.SizeBytes,
		VCPU:            vm.VCPU,
		MemoryMB:        vm.MemoryMB,
		Description:     description,
		RestoreTimeMS:   &createTimeMS,
		CreatedAt:       time.Now().UTC(),
	}

	if err := s.snapshotRepo.Create(ctx, snapshot); err != nil {
		// Resume VM even if snapshot record save fails
		if resumeErr := s.vmProvider.Resume(ctx, vm.SocketPath); resumeErr != nil {
			log.Printf("Warning: failed to resume VM %s after snapshot record failure: %v", vmID, resumeErr)
		}
		// Clean up snapshot files since we could not persist the record
		if delErr := s.vmProvider.DeleteSnapshotFiles(result.MemoryFilePath, result.StateFilePath); delErr != nil {
			log.Printf("Warning: failed to clean up snapshot files for %s: %v", snapshotID, delErr)
		}
		return nil, fmt.Errorf("failed to save snapshot record: %w", err)
	}

	// Resume the VM after successful snapshot
	if err := s.vmProvider.Resume(ctx, vm.SocketPath); err != nil {
		log.Printf("Warning: failed to resume VM %s after snapshot: %v", vmID, err)
	}

	log.Printf("Snapshot created: %s (vm=%s, lang=%s, size=%d bytes, time=%dms)",
		snapshotID, vmID, vm.Language, result.SizeBytes, createTimeMS)
	return snapshot, nil
}

func (s *snapshotService) RestoreSnapshot(ctx context.Context, snapshotID uuid.UUID) (*domain.MicroVM, error) {
	snapshot, err := s.snapshotRepo.FindByID(ctx, snapshotID)
	if err != nil {
		return nil, err
	}

	// Create a new VM record for the restored instance
	vmID := uuid.New()
	vm := &domain.MicroVM{
		ID:          vmID,
		Status:      domain.VMStatusCreating,
		VCPU:        snapshot.VCPU,
		MemoryMB:    snapshot.MemoryMB,
		KernelPath:  "", // Restored from snapshot state
		RootfsPath:  "", // Restored from snapshot state
		NetworkMode: domain.NetworkModeIsolated,
		Language:    snapshot.Language,
		CreatedAt:   time.Now().UTC(),
	}

	if err := s.vmRepo.Create(ctx, vm); err != nil {
		return nil, fmt.Errorf("failed to create VM record for restore: %w", err)
	}

	start := time.Now()

	// Boot a fresh Firecracker process (no VM config needed -- snapshot will provide it)
	bootResult, err := s.vmProvider.Boot(ctx, VMBootConfig{
		VMID:        vmID,
		Language:    snapshot.Language,
		VCPU:        snapshot.VCPU,
		MemoryMB:    snapshot.MemoryMB,
		NetworkMode: domain.NetworkModeIsolated,
	})
	if err != nil {
		vm.Status = domain.VMStatusError
		_ = s.vmRepo.Update(ctx, vm)
		return nil, fmt.Errorf("failed to boot Firecracker for restore: %w", err)
	}

	// Load the snapshot into the fresh Firecracker instance
	socketPath := bootResult.SocketPath
	if socketPath == "" {
		// Derive the socket path the same way the provider does
		socketPath = fmt.Sprintf("/tmp/firecracker/%s.sock", vmID.String())
	}

	// Ensure the mem/state files exist locally before handing them to
	// Firecracker. On a fresh host the local paths recorded at create time
	// won't exist — pull the blobs from the durable object store using the
	// recorded keys. When the files are already present (same-host restore)
	// this is a cheap stat and the local fast path is preserved.
	if err := ensureLocalFile(s.store, snapshot.MemoryObjectKey, snapshot.MemoryFilePath); err != nil {
		if bootResult.PID > 0 {
			_ = s.vmProvider.Terminate(ctx, socketPath, bootResult.PID)
		}
		vm.Status = domain.VMStatusError
		_ = s.vmRepo.Update(ctx, vm)
		return nil, fmt.Errorf("failed to stage snapshot memory file: %w", err)
	}
	if err := ensureLocalFile(s.store, snapshot.StateObjectKey, snapshot.StateFilePath); err != nil {
		if bootResult.PID > 0 {
			_ = s.vmProvider.Terminate(ctx, socketPath, bootResult.PID)
		}
		vm.Status = domain.VMStatusError
		_ = s.vmRepo.Update(ctx, vm)
		return nil, fmt.Errorf("failed to stage snapshot state file: %w", err)
	}

	if err := s.vmProvider.RestoreSnapshot(ctx, socketPath, snapshot.MemoryFilePath, snapshot.StateFilePath); err != nil {
		// Terminate the fresh Firecracker process on failure
		if bootResult.PID > 0 {
			_ = s.vmProvider.Terminate(ctx, socketPath, bootResult.PID)
		}
		vm.Status = domain.VMStatusError
		_ = s.vmRepo.Update(ctx, vm)
		return nil, fmt.Errorf("failed to restore snapshot: %w", err)
	}

	restoreTimeMS := time.Since(start).Milliseconds()

	// Update VM with boot/restore results
	vm.Status = domain.VMStatusReady
	vm.PID = &bootResult.PID
	vm.IPAddress = bootResult.IPAddress
	vm.BootTimeMS = &restoreTimeMS
	vm.SocketPath = socketPath

	if err := s.vmRepo.Update(ctx, vm); err != nil {
		return nil, fmt.Errorf("failed to update VM after restore: %w", err)
	}

	log.Printf("Snapshot restored: %s -> VM %s (restore=%dms)", snapshotID, vmID, restoreTimeMS)
	return vm, nil
}

func (s *snapshotService) GetSnapshot(ctx context.Context, id uuid.UUID) (*domain.Snapshot, error) {
	return s.snapshotRepo.FindByID(ctx, id)
}

func (s *snapshotService) ListSnapshotsByExecution(ctx context.Context, executionID uuid.UUID) ([]domain.Snapshot, error) {
	return s.snapshotRepo.FindByExecution(ctx, executionID)
}

func (s *snapshotService) GetBaseSnapshot(ctx context.Context, language domain.Language) (*domain.Snapshot, error) {
	return s.snapshotRepo.FindBaseByLanguage(ctx, language)
}

func (s *snapshotService) DeleteSnapshot(ctx context.Context, id uuid.UUID) error {
	// Retrieve snapshot to get file paths before deleting
	snapshot, err := s.snapshotRepo.FindByID(ctx, id)
	if err != nil {
		return err
	}

	// Delete snapshot files from disk
	if err := s.vmProvider.DeleteSnapshotFiles(snapshot.MemoryFilePath, snapshot.StateFilePath); err != nil {
		log.Printf("Warning: failed to delete snapshot files for %s: %v", id, err)
	}

	return s.snapshotRepo.Delete(ctx, id)
}
