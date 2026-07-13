// Package volume implements the VolumeBackend port: it materializes the durable
// ext4 backing file a persistent volume attaches to as a 2nd virtio-blk device
// (runtime-fleet CP4 rt#9). Firecracker host only — off-host the fail-loud
// backend rejects every call so a volume is never silently faked.
package volume

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/sentiae/runtime-service/internal/usecase"
)

// minBackingMB is the floor size for a backing file so mkfs.ext4 has room to lay
// down a valid filesystem when a descriptor requests a tiny (or zero) volume.
const minBackingMB = 32

// BackingStore materializes ext4 backing files under a host directory.
type BackingStore struct{}

var _ usecase.VolumeBackend = (*BackingStore)(nil)

// NewBackingStore constructs a BackingStore.
func NewBackingStore() *BackingStore { return &BackingStore{} }

// Ensure returns the backing file for a volume, creating it if absent. It is
// idempotent: an existing <Dir>/<volumeID>.ext4 is returned unchanged so a
// re-provision or reboot re-attaches the same data.
func (b *BackingStore) Ensure(_ context.Context, in usecase.VolumeEnsureInput) (usecase.VolumeEnsureOutput, error) {
	if in.Dir == "" {
		return usecase.VolumeEnsureOutput{}, fmt.Errorf("volume dir is required")
	}
	if err := os.MkdirAll(in.Dir, 0o750); err != nil {
		return usecase.VolumeEnsureOutput{}, fmt.Errorf("create volume dir: %w", err)
	}
	path := filepath.Join(in.Dir, in.VolumeID.String()+".ext4")

	if _, err := os.Stat(path); err == nil {
		// Backing file already materialized — idempotent, never re-format (that
		// would destroy the persisted data).
		return usecase.VolumeEnsureOutput{BackingPath: path}, nil
	} else if !os.IsNotExist(err) {
		return usecase.VolumeEnsureOutput{}, fmt.Errorf("stat backing file: %w", err)
	}

	sizeMB := in.SizeMB
	if sizeMB < minBackingMB {
		sizeMB = minBackingMB
	}

	// Sparse backing file: truncate to the requested size, then format ext4.
	f, err := os.Create(path)
	if err != nil {
		return usecase.VolumeEnsureOutput{}, fmt.Errorf("create backing file: %w", err)
	}
	if terr := f.Truncate(sizeMB * 1024 * 1024); terr != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return usecase.VolumeEnsureOutput{}, fmt.Errorf("truncate backing file: %w", terr)
	}
	if cerr := f.Close(); cerr != nil {
		_ = os.Remove(path)
		return usecase.VolumeEnsureOutput{}, fmt.Errorf("close backing file: %w", cerr)
	}

	// TODO(rt#9-luks): wrap backing file with LUKS + Vault-Transit DEK once Vault is productionized
	if o, e := exec.Command("mkfs.ext4", "-q", "-F", path).CombinedOutput(); e != nil {
		_ = os.Remove(path)
		return usecase.VolumeEnsureOutput{}, fmt.Errorf("mkfs.ext4 backing file: %s: %w", strings.TrimSpace(string(o)), e)
	}
	return usecase.VolumeEnsureOutput{BackingPath: path}, nil
}

// Delete removes a backing file. A missing file is not an error (idempotent).
func (b *BackingStore) Delete(_ context.Context, backingPath string) error {
	if backingPath == "" {
		return nil
	}
	if err := os.Remove(backingPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove backing file: %w", err)
	}
	return nil
}
