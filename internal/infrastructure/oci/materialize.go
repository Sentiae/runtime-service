package oci

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// ImageRef is a registry-neutral OCI image reference.
type ImageRef struct {
	Registry   string
	Repository string
	Digest     string
	ChangeID   string
}

// MaterializeRequest is the input to Materialize.
type MaterializeRequest struct {
	Image   ImageRef
	WorkDir string            // per-workload working directory (staging + rootfs live here)
	EnvVars map[string]string // descriptor env vars, appended after image Env (later wins)
	Mode    string            // "test" | "resident"
	TestCmd string            // test class: overrides the image entrypoint when non-empty
	Port    int               // resident class: guest port
}

// MaterializeResult is the output of Materialize.
type MaterializeResult struct {
	RootfsPath string
	Config     ImageConfig
}

// runtimeSpec is written to /sentiae/runtime.json inside the rootfs and read by
// the image-init at boot.
type runtimeSpec struct {
	Entrypoint  []string `json:"entrypoint"`
	Env         []string `json:"env"`
	WorkDir     string   `json:"workdir"`
	Mode        string   `json:"mode"`
	TestCommand string   `json:"test_command"`
	Port        int      `json:"port"`
}

// Materializer pulls an OCI image and lays it down as a Firecracker ext4 rootfs.
type Materializer struct {
	client *Client
	// initPath is the host path to the prebuilt image-init binary copied into
	// the rootfs as /sentiae/init.
	initPath string
}

// NewMaterializer constructs a Materializer.
func NewMaterializer(client *Client, initPath string) *Materializer {
	return &Materializer{client: client, initPath: initPath}
}

// Stage pulls the image and builds the staging directory tree (all layers
// applied with whiteout semantics, plus /sentiae/runtime.json and /sentiae/init).
// It is the unit-testable core of Materialize — mkfs is not invoked here.
func (m *Materializer) Stage(ctx context.Context, req MaterializeRequest, stagingDir string) (ImageConfig, error) {
	man, err := m.client.FetchManifest(ctx, req.Image.Repository, req.Image.Digest)
	if err != nil {
		return ImageConfig{}, err
	}

	if err := os.MkdirAll(stagingDir, 0o755); err != nil {
		return ImageConfig{}, fmt.Errorf("create staging dir: %w", err)
	}

	var unpacked int64 // per-call decompression-bomb budget (concurrent-safe)
	for _, layer := range man.layers {
		if err := m.applyLayer(ctx, req.Image.Repository, layer.Digest, stagingDir, &unpacked); err != nil {
			return ImageConfig{}, fmt.Errorf("apply layer %s: %w", layer.Digest, err)
		}
	}

	if err := writeRuntimeJSON(stagingDir, req, man.config); err != nil {
		return ImageConfig{}, err
	}
	if err := m.copyInit(stagingDir); err != nil {
		return ImageConfig{}, err
	}
	return man.config, nil
}

// applyLayer pulls a layer blob and unpacks it into stagingDir with OCI
// whiteout semantics. gzip is detected from the blob's magic bytes so it works
// regardless of the declared mediaType.
func (m *Materializer) applyLayer(ctx context.Context, repo, digest, stagingDir string, budget *int64) error {
	rc, err := m.client.FetchBlob(ctx, repo, digest)
	if err != nil {
		return err
	}
	defer rc.Close()

	br := bufio.NewReader(rc)
	var src io.Reader = br
	if magic, _ := br.Peek(2); len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		gz, err := gzip.NewReader(br)
		if err != nil {
			return fmt.Errorf("gzip reader: %w", err)
		}
		defer gz.Close()
		src = gz
	}

	return unpackTar(src, stagingDir, budget)
}

// maxUnpackedBytes caps the total DECOMPRESSED bytes written across all of an
// image's layers — a decompression bomb must not fill the fleet host's disk
// before the post-extraction size check runs.
const maxUnpackedBytes = 8 << 30 // 8 GiB

// unpackTar applies one tar stream into dir with whiteout semantics and path
// traversal guarding. Exported logic lives here so the materializer test can
// drive it directly. budget is the cross-layer decompressed-bytes counter
// (nil = uncapped, tests only).
//
// Threat model: a hostile IMAGE. Lexical target checks stop "../" escapes; the
// symlink class (an earlier entry plants a symlink to a host path, a later
// entry writes THROUGH it) is stopped by safeParent — every filesystem
// operation first proves the entry's parent chain still physically resolves
// inside dir. Symlink CONTENTS stay unrestricted (absolute in-guest links like
// /dev/stdout are legitimate and never followed on the host).
func unpackTar(r io.Reader, dir string, budget *int64) error {
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return fmt.Errorf("resolve staging dir: %w", err)
	}
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read tar entry: %w", err)
		}

		name := filepath.Clean(hdr.Name)
		if name == "" || name == "." {
			continue
		}
		target := filepath.Join(dir, name)
		// Path traversal guard: the resolved target must stay under dir. Join
		// cleans "../" segments, so an escaping entry lands outside dir and is
		// rejected here (an absolute path is anchored under dir, which is safe).
		if !withinDir(dir, target) {
			return fmt.Errorf("tar entry escapes staging dir: %s", hdr.Name)
		}

		base := filepath.Base(target)
		parent := filepath.Dir(target)

		// Symlink-parent guard: the parent chain must still physically resolve
		// inside dir BEFORE anything is created/removed through it.
		if err := safeParent(realDir, parent); err != nil {
			return fmt.Errorf("tar entry %s: %w", hdr.Name, err)
		}

		// Whiteout handling.
		if base == ".wh..wh..opq" {
			// Opaque: clear the parent directory's prior contents.
			if err := clearDirContents(parent); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(base, ".wh.") {
			// Delete the referenced path from the staging tree.
			victim := filepath.Join(parent, strings.TrimPrefix(base, ".wh."))
			if err := os.RemoveAll(victim); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("apply whiteout %s: %w", hdr.Name, err)
			}
			continue
		}

		if err := os.MkdirAll(parent, 0o755); err != nil {
			return fmt.Errorf("mkdir parent for %s: %w", name, err)
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(hdr.Mode)&0o777); err != nil {
				return fmt.Errorf("mkdir %s: %w", name, err)
			}
		case tar.TypeReg, tar.TypeRegA:
			// Overwrite any prior file at this path (later layer wins).
			// O_NOFOLLOW: if the path itself is a planted symlink the open
			// fails instead of writing through it (RemoveAll deletes a link,
			// not its referent, so this is belt-and-braces).
			_ = os.RemoveAll(target)
			f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY|syscall.O_NOFOLLOW, os.FileMode(hdr.Mode)&0o777)
			if err != nil {
				return fmt.Errorf("create %s: %w", name, err)
			}
			n, err := io.Copy(f, tr)
			if err != nil {
				_ = f.Close()
				return fmt.Errorf("write %s: %w", name, err)
			}
			if budget != nil {
				*budget += n
				if *budget > maxUnpackedBytes {
					_ = f.Close()
					return fmt.Errorf("image exceeds the %d-byte unpacked ceiling (decompression bomb guard)", int64(maxUnpackedBytes))
				}
			}
			if err := f.Close(); err != nil {
				return fmt.Errorf("close %s: %w", name, err)
			}
		case tar.TypeSymlink:
			_ = os.RemoveAll(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return fmt.Errorf("symlink %s: %w", name, err)
			}
		case tar.TypeLink:
			linkTarget := filepath.Join(dir, filepath.Clean(hdr.Linkname))
			// Resolve symlinks in the source path too: a planted symlink in the
			// link's parent chain would otherwise let os.Link hardlink a HOST
			// file's content into the image (read disclosure). Re-check the
			// physically-resolved source stays inside realDir.
			if resolved, rerr := resolveExistingAncestor(linkTarget); rerr != nil || !withinDir(realDir, resolved) {
				return fmt.Errorf("hardlink target escapes staging dir: %s", hdr.Linkname)
			}
			_ = os.RemoveAll(target)
			if err := os.Link(linkTarget, target); err != nil {
				// A dangling hardlink is not fatal — skip it rather than abort
				// the whole materialization.
				continue
			}
		case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
			// Device / fifo nodes require mknod (root) and are irrelevant to a
			// microVM rootfs — skip them.
			continue
		default:
			// Unknown types (e.g. GNU sparse/xattr headers) — skip.
			continue
		}
	}
}

// clearDirContents removes every entry inside dir but keeps dir itself.
func clearDirContents(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read dir for opaque whiteout: %w", err)
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return fmt.Errorf("clear opaque dir entry %s: %w", e.Name(), err)
		}
	}
	return nil
}

// withinDir reports whether target is dir or lives under dir.
// safeParent proves the entry's parent directory chain still physically
// resolves inside realDir — i.e. no earlier tar entry planted a symlink that a
// later entry would write THROUGH onto the host (the classic tar-extraction
// escape). realDir must already be symlink-resolved.
func safeParent(realDir, parent string) error {
	anc, err := resolveExistingAncestor(parent)
	if err != nil {
		return fmt.Errorf("resolve parent %s: %w", parent, err)
	}
	if !withinDir(realDir, anc) {
		return fmt.Errorf("parent path resolves outside the staging dir (symlink traversal): %s -> %s", parent, anc)
	}
	return nil
}

// resolveExistingAncestor walks p upward to its deepest existing path and
// returns that path with every symlink resolved.
func resolveExistingAncestor(p string) (string, error) {
	for cur := p; ; {
		resolved, err := filepath.EvalSymlinks(cur)
		if err == nil {
			return resolved, nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		next := filepath.Dir(cur)
		if next == cur {
			return cur, nil
		}
		cur = next
	}
}

func withinDir(dir, target string) bool {
	rel, err := filepath.Rel(dir, target)
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != "..")
}

// writeRuntimeJSON writes /sentiae/runtime.json into the staging tree.
func writeRuntimeJSON(stagingDir string, req MaterializeRequest, cfg ImageConfig) error {
	spec := runtimeSpec{
		Entrypoint:  buildEntrypoint(cfg),
		Env:         buildEnv(cfg.Env, req.EnvVars),
		WorkDir:     cfg.WorkingDir,
		Mode:        req.Mode,
		TestCommand: req.TestCmd,
		Port:        req.Port,
	}
	dir := filepath.Join(stagingDir, "sentiae")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create /sentiae dir: %w", err)
	}
	body, err := json.Marshal(spec)
	if err != nil {
		return fmt.Errorf("marshal runtime spec: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "runtime.json"), body, 0o644); err != nil {
		return fmt.Errorf("write runtime.json: %w", err)
	}
	return nil
}

// copyInit copies the prebuilt image-init binary into the staging tree as
// /sentiae/init (mode 0755).
func (m *Materializer) copyInit(stagingDir string) error {
	if m.initPath == "" {
		return fmt.Errorf("image-init binary path not configured")
	}
	in, err := os.Open(m.initPath)
	if err != nil {
		return fmt.Errorf("open image-init binary: %w", err)
	}
	defer in.Close()
	dir := filepath.Join(stagingDir, "sentiae")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create /sentiae dir: %w", err)
	}
	dst := filepath.Join(dir, "init")
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return fmt.Errorf("create /sentiae/init: %w", err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return fmt.Errorf("copy image-init: %w", err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("close /sentiae/init: %w", err)
	}
	return nil
}

// buildEntrypoint combines image Entrypoint + Cmd per OCI semantics.
func buildEntrypoint(cfg ImageConfig) []string {
	argv := make([]string, 0, len(cfg.Entrypoint)+len(cfg.Cmd))
	argv = append(argv, cfg.Entrypoint...)
	argv = append(argv, cfg.Cmd...)
	return argv
}

// buildEnv returns image Env with descriptor env vars appended after (later
// wins because the init applies the slice in order). Descriptor keys are sorted
// for deterministic output.
func buildEnv(imageEnv []string, extra map[string]string) []string {
	env := make([]string, 0, len(imageEnv)+len(extra))
	env = append(env, imageEnv...)
	keys := make([]string, 0, len(extra))
	for k := range extra {
		keys = append(keys, k)
	}
	sortStrings(keys)
	for _, k := range keys {
		env = append(env, k+"="+extra[k])
	}
	return env
}

// sortStrings is a tiny insertion sort to avoid importing sort for one call.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

// Materialize pulls the image, stages it, and produces an ext4 rootfs at
// <workDir>/rootfs.ext4. The staging dir is removed on success.
func (m *Materializer) Materialize(ctx context.Context, req MaterializeRequest) (MaterializeResult, error) {
	if req.WorkDir == "" {
		return MaterializeResult{}, fmt.Errorf("work dir is required")
	}
	if err := os.MkdirAll(req.WorkDir, 0o755); err != nil {
		return MaterializeResult{}, fmt.Errorf("create work dir: %w", err)
	}
	stagingDir := filepath.Join(req.WorkDir, "staging")
	_ = os.RemoveAll(stagingDir)

	cfg, err := m.Stage(ctx, req, stagingDir)
	if err != nil {
		return MaterializeResult{}, err
	}

	rootfsPath := filepath.Join(req.WorkDir, "rootfs.ext4")
	if err := buildExt4(stagingDir, rootfsPath); err != nil {
		return MaterializeResult{}, err
	}

	// Clean the staging dir now the image is baked into the rootfs.
	_ = os.RemoveAll(stagingDir)

	return MaterializeResult{RootfsPath: rootfsPath, Config: cfg}, nil
}

// buildExt4 creates an ext4 image at rootfsPath from stagingDir. It prefers
// `mkfs.ext4 -d` (populate from a directory, no mount / no root) and falls back
// to a sparse-file + loop-mount + copy when -d is unsupported.
func buildExt4(stagingDir, rootfsPath string) error {
	sizeMB := ext4SizeMB(stagingDir)

	_ = os.Remove(rootfsPath)
	sizeArg := fmt.Sprintf("%dM", sizeMB)
	out, err := exec.Command("mkfs.ext4", "-q", "-d", stagingDir, rootfsPath, sizeArg).CombinedOutput()
	if err == nil {
		return nil
	}

	// Fallback: sparse file → mkfs → loop mount → copy tree → unmount.
	_ = os.Remove(rootfsPath)
	f, cerr := os.Create(rootfsPath)
	if cerr != nil {
		return fmt.Errorf("create rootfs file (mkfs -d failed: %s: %v): %w", strings.TrimSpace(string(out)), err, cerr)
	}
	if terr := f.Truncate(int64(sizeMB) * 1024 * 1024); terr != nil {
		_ = f.Close()
		return fmt.Errorf("truncate rootfs file: %w", terr)
	}
	_ = f.Close()

	if o, e := exec.Command("mkfs.ext4", "-q", "-F", rootfsPath).CombinedOutput(); e != nil {
		return fmt.Errorf("mkfs.ext4 fallback: %s: %w", strings.TrimSpace(string(o)), e)
	}
	mnt, e := os.MkdirTemp("", "oci-ext4-")
	if e != nil {
		return fmt.Errorf("create mount dir: %w", e)
	}
	defer os.RemoveAll(mnt)
	if o, e := exec.Command("mount", "-o", "loop", rootfsPath, mnt).CombinedOutput(); e != nil {
		return fmt.Errorf("mount rootfs: %s: %w", strings.TrimSpace(string(o)), e)
	}
	defer func() { _ = exec.Command("umount", mnt).Run() }()
	if o, e := exec.Command("cp", "-a", stagingDir+"/.", mnt+"/").CombinedOutput(); e != nil {
		return fmt.Errorf("copy tree into rootfs: %s: %w", strings.TrimSpace(string(o)), e)
	}
	return nil
}

// ext4SizeMB computes the rootfs size: staging bytes + 50% headroom, min 256MB.
func ext4SizeMB(stagingDir string) int {
	const minMB = 256
	var total int64
	_ = filepath.Walk(stagingDir, func(_ string, info os.FileInfo, err error) error {
		if err != nil || info == nil {
			return nil
		}
		if info.Mode().IsRegular() {
			total += info.Size()
		} else {
			total += 4096 // approximate per-inode overhead for dirs/symlinks
		}
		return nil
	})
	sizeMB := int(total/(1024*1024))*3/2 + 32
	if sizeMB < minMB {
		sizeMB = minMB
	}
	return sizeMB
}
