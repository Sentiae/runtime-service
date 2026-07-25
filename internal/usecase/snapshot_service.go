package usecase

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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

// snapshotUpload reports what an upload actually stored. Compression makes
// "bytes stored" and "bytes of the volume" two different numbers, and the two
// have different jobs, so both are returned rather than conflated.
type snapshotUpload struct {
	// Checksum is the lowercase hex sha256 of the bytes AS STORED (i.e. of the
	// compressed stream). "Did the blob arrive intact" is a question about the
	// stored bytes, and hashing those is what lets a restore verify what it
	// downloaded BEFORE it materializes anything.
	Checksum string
	// StoredBytes is the compressed transfer size — observability only.
	StoredBytes int64
	// LogicalBytes is the uncompressed source size: what a restore produces.
	// This is what `size_bytes` on a recovery point means.
	LogicalBytes int64
}

// errStoreStoppedEarly unparks a compressor still blocked writing into the
// upload pipe because the artifact store stopped reading. It is what the
// compressor reports back, so a truncated upload names its own cause instead of
// surfacing as a bare "read/write on closed pipe".
var errStoreStoppedEarly = errors.New("artifact store stopped reading before the whole snapshot was consumed")

// uploadSnapshotFileHashed streams a local file into the artifact store,
// GZIP-COMPRESSED, and returns the sha256 of exactly the bytes the store
// consumed together with both sizes. Hashing rides the SAME single pass as the
// upload (io.TeeReader) — a second read of a multi-GB backing file would double
// the snapshot's IO cost.
//
// ⚠ WHY COMPRESS AT ALL, AND WHY AT BestSpeed. A volume backing file is SPARSE:
// a 20GB-nominal volume holding 4.6GB of data costs 4.6GB on disk and copies
// cheaply (`cp --sparse=always`). But a hole READS BACK AS ZEROS, so an
// uncompressed upload transfers the full nominal size — measured live, a 20GB
// volume could not be snapshotted inside a 300s deadline, and because a durable
// resource refuses decommission without a final snapshot, a large database
// could then never be deleted either. gzip collapses those zero runs to almost
// nothing, so the transfer costs about what the real data costs. The win is
// ELIMINATING HOLES, not squeezing real data — a stronger level would burn CPU
// over the real 4.6GB for a few percent. Do not "optimize" the level.
//
// The byte-count cross-checks are the honesty gate: if the compressor did not
// read the whole file, or the store did not consume the whole compressed
// stream, the digest would describe something other than a complete snapshot,
// and a restore would "verify" against it. Refuse instead.
func uploadSnapshotFileHashed(ctx context.Context, store ArtifactStore, key, path string) (snapshotUpload, error) {
	var zero snapshotUpload
	f, err := os.Open(path)
	if err != nil {
		return zero, fmt.Errorf("open %s: %w", path, err)
	}
	// This function is the SOLE owner of the descriptor it opened. The compressor
	// goroutine inside uploadSnapshotStreamHashed only borrows it and is always
	// joined before that call returns, so exactly one Close runs on every exit —
	// success, error, context cancellation and panic. A descriptor left open here
	// is not just a leak: a temp file it points at may be unlinked on the way out,
	// so its disk is not reclaimed until the process dies (measured live: ~28GB of
	// held-open deleted files after repeated cancels).
	defer f.Close()
	return uploadSnapshotStreamHashed(ctx, store, key, path, f)
}

// uploadSnapshotStreamHashed is uploadSnapshotFileHashed over a descriptor the
// CALLER owns and keeps open. It exists because the fleet volume snapshotter
// streams a live volume's backing file DIRECTLY into the store (no local staging
// copy) and wants the same single descriptor it just fsync'd — see
// FleetVolumeSnapshotter.uploadBackingFile.
//
// It never closes f: the caller does, after this returns and after the
// compressor goroutine has been joined.
func uploadSnapshotStreamHashed(ctx context.Context, store ArtifactStore, key, path string, f *os.File) (snapshotUpload, error) {
	var zero snapshotUpload
	st, err := f.Stat()
	if err != nil {
		return zero, fmt.Errorf("stat %s: %w", path, err)
	}

	pr, pw := io.Pipe()
	// source counts UNCOMPRESSED bytes read off the file; emitted counts the
	// COMPRESSED bytes handed to the store. ctxReader is what makes a cancelled
	// snapshot an actual exit path: the artifact store's Put takes no context, so
	// without it a caller that gave up still pays for the whole transfer while
	// this descriptor stays open.
	source := &countingReader{r: &ctxReader{ctx: ctx, r: f}}
	emitted := &countingWriter{w: pw}

	var compressErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if r := recover(); r != nil {
				compressErr = fmt.Errorf("snapshot compressor panicked: %v", r)
			}
			// Closing the write end unblocks the store's read with the same error,
			// so a failed compressor can never leave Put waiting forever.
			_ = pw.CloseWithError(compressErr)
		}()
		compressErr = gzipStream(emitted, source)
	}()
	// Unpark and JOIN the compressor on EVERY exit — including a panic inside the
	// store's Put. It borrows f, so the owner's Close (registered before this call,
	// therefore running after it) must not fire while it is still reading. Closing
	// the read end is what unparks a compressor blocked writing into the pipe.
	// Both operations are idempotent, so the explicit join below is free.
	defer func() {
		_ = pr.CloseWithError(errStoreStoppedEarly)
		<-done
	}()

	h := sha256.New()
	stored := &countingReader{r: io.TeeReader(pr, h)}
	putErr := store.Put(key, stored)
	_ = pr.CloseWithError(errStoreStoppedEarly)
	<-done

	if putErr != nil {
		return zero, fmt.Errorf("put %s: %w", key, putErr)
	}
	if compressErr != nil {
		return zero, fmt.Errorf("compress %s for %s: %w", path, key, compressErr)
	}
	if source.n != st.Size() {
		return zero, fmt.Errorf("compressed %d of %d bytes of %s", source.n, st.Size(), path)
	}
	if stored.n != emitted.n {
		return zero, fmt.Errorf("artifact store consumed %d of %d compressed bytes for %s", stored.n, emitted.n, key)
	}
	return snapshotUpload{
		Checksum:     hex.EncodeToString(h.Sum(nil)),
		StoredBytes:  stored.n,
		LogicalBytes: st.Size(),
	}, nil
}

// gzipStream compresses r into w and flushes the trailer. Close is what writes
// the trailer, so its error is a write error and must not be swallowed.
func gzipStream(w io.Writer, r io.Reader) error {
	gz, err := gzip.NewWriterLevel(w, gzip.BestSpeed)
	if err != nil {
		return fmt.Errorf("gzip writer: %w", err)
	}
	if _, err := io.Copy(gz, r); err != nil {
		_ = gz.Close()
		return fmt.Errorf("compress: %w", err)
	}
	if err := gz.Close(); err != nil {
		return fmt.Errorf("flush compressed stream: %w", err)
	}
	return nil
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

// countingWriter counts the bytes actually written through it.
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// ctxReader aborts a long read loop as soon as its context ends. Wrapping the
// SOURCE (rather than plumbing a context into the store) is what stops a
// cancelled snapshot from streaming multi-GB nobody is waiting for.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
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
