package oci

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeRegistry serves an OCI image: index → linux/amd64 manifest → config +
// two gzipped layers. The second layer whiteouts a file the first created.
type fakeRegistry struct {
	repo      string
	manifests map[string][]byte // digest → raw manifest / index
	blobs     map[string][]byte // digest → raw blob
	types     map[string]string // digest → content-type for manifests
}

func (f *fakeRegistry) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		// Basic auth must be present.
		if u, p, ok := r.BasicAuth(); !ok || u != "registry-client" || p != "sekret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		path := r.URL.Path
		switch {
		case containsSeg(path, "manifests"):
			dig := lastSeg(path)
			raw, ok := f.manifests[dig]
			if !ok {
				http.Error(w, "manifest not found", http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", f.types[dig])
			_, _ = w.Write(raw)
		case containsSeg(path, "blobs"):
			dig := lastSeg(path)
			raw, ok := f.blobs[dig]
			if !ok {
				http.Error(w, "blob not found", http.StatusNotFound)
				return
			}
			_, _ = w.Write(raw)
		default:
			http.Error(w, "bad path", http.StatusBadRequest)
		}
	})
	return mux
}

func containsSeg(path, seg string) bool {
	return filepath.Base(filepath.Dir(path)) == seg
}
func lastSeg(path string) string { return filepath.Base(path) }

// gzTar builds a gzipped tar from a set of entries.
type tarEntry struct {
	name     string
	body     string
	typeflag byte
	linkname string
	mode     int64
}

func gzTar(entries []tarEntry) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		hdr := &tar.Header{
			Name:     e.name,
			Typeflag: e.typeflag,
			Mode:     mode,
			Size:     int64(len(e.body)),
			Linkname: e.linkname,
		}
		if e.typeflag == 0 {
			hdr.Typeflag = tar.TypeReg
		}
		if hdr.Typeflag != tar.TypeReg {
			hdr.Size = 0
		}
		_ = tw.WriteHeader(hdr)
		if hdr.Typeflag == tar.TypeReg {
			_, _ = tw.Write([]byte(e.body))
		}
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

func TestMaterializerStage(t *testing.T) {
	// Two layers: layer1 creates app/keep.txt + app/gone.txt; layer2 whiteouts
	// app/gone.txt and overwrites app/keep.txt.
	layer1 := gzTar([]tarEntry{
		{name: "app/", typeflag: tar.TypeDir, mode: 0o755},
		{name: "app/keep.txt", body: "v1"},
		{name: "app/gone.txt", body: "bye"},
		{name: "bin/run", body: "#!/bin/sh\n", mode: 0o755},
	})
	layer2 := gzTar([]tarEntry{
		{name: "app/.wh.gone.txt", typeflag: tar.TypeReg},
		{name: "app/keep.txt", body: "v2"},
		{name: "app/link", typeflag: tar.TypeSymlink, linkname: "keep.txt"},
	})

	configBody, _ := json.Marshal(map[string]any{
		"config": map[string]any{
			"Entrypoint": []string{"/bin/run"},
			"Cmd":        []string{"--serve"},
			"Env":        []string{"FOO=bar", "PATH=/bin"},
			"WorkingDir": "/app",
		},
	})

	configDigest := "sha256:cfg"
	l1Digest := "sha256:layer1"
	l2Digest := "sha256:layer2"
	manDigest := "sha256:manifest"
	idxDigest := "sha256:index"

	manifest, _ := json.Marshal(imageManifest{
		MediaType: mediaTypeOCIManifest,
		Config:    descriptor{MediaType: "application/vnd.oci.image.config.v1+json", Digest: configDigest},
		Layers: []descriptor{
			{MediaType: "application/vnd.oci.image.layer.v1.tar+gzip", Digest: l1Digest},
			{MediaType: "application/vnd.oci.image.layer.v1.tar+gzip", Digest: l2Digest},
		},
	})
	index, _ := json.Marshal(imageIndex{
		MediaType: mediaTypeOCIIndex,
		Manifests: []descriptor{
			{MediaType: mediaTypeOCIManifest, Digest: manDigest, Platform: &platformSpec{OS: "linux", Architecture: "amd64"}},
			{MediaType: mediaTypeOCIManifest, Digest: "sha256:arm", Platform: &platformSpec{OS: "linux", Architecture: "arm64"}},
		},
	})

	reg := &fakeRegistry{
		repo: "org/component",
		manifests: map[string][]byte{
			idxDigest: index,
			manDigest: manifest,
		},
		types: map[string]string{
			idxDigest: mediaTypeOCIIndex,
			manDigest: mediaTypeOCIManifest,
		},
		blobs: map[string][]byte{
			configDigest: configBody,
			l1Digest:     layer1,
			l2Digest:     layer2,
		},
	}
	srv := httptest.NewServer(reg.handler(t))
	defer srv.Close()

	host := srv.URL[len("http://"):]
	client := NewClient(Config{Host: host, Username: "registry-client", Password: "sekret"})

	// A fake image-init binary to copy in.
	initFile := filepath.Join(t.TempDir(), "image-init")
	if err := os.WriteFile(initFile, []byte("INITBIN"), 0o755); err != nil {
		t.Fatal(err)
	}
	m := NewMaterializer(client, initFile)

	staging := filepath.Join(t.TempDir(), "staging")
	cfg, err := m.Stage(context.Background(), MaterializeRequest{
		Image:   ImageRef{Registry: host, Repository: "org/component", Digest: idxDigest},
		EnvVars: map[string]string{"EXTRA": "1"},
		Mode:    "test",
		TestCmd: "echo hi",
		Port:    0,
	}, staging)
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}

	// keep.txt overwritten to v2.
	if got := readFile(t, filepath.Join(staging, "app", "keep.txt")); got != "v2" {
		t.Errorf("keep.txt = %q, want v2 (later layer wins)", got)
	}
	// gone.txt whiteouted.
	if _, err := os.Lstat(filepath.Join(staging, "app", "gone.txt")); !os.IsNotExist(err) {
		t.Errorf("app/gone.txt should have been whiteouted, err=%v", err)
	}
	// symlink present.
	if fi, err := os.Lstat(filepath.Join(staging, "app", "link")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("app/link should be a symlink, err=%v", err)
	}
	// runtime.json written with merged entrypoint + env.
	var spec runtimeSpec
	raw := readFile(t, filepath.Join(staging, "sentiae", "runtime.json"))
	if err := json.Unmarshal([]byte(raw), &spec); err != nil {
		t.Fatalf("decode runtime.json: %v", err)
	}
	if len(spec.Entrypoint) != 2 || spec.Entrypoint[0] != "/bin/run" || spec.Entrypoint[1] != "--serve" {
		t.Errorf("entrypoint = %v, want [/bin/run --serve]", spec.Entrypoint)
	}
	if len(spec.Env) != 3 || spec.Env[2] != "EXTRA=1" {
		t.Errorf("env = %v, want image env + EXTRA appended last", spec.Env)
	}
	if spec.WorkDir != "/app" || spec.Mode != "test" || spec.TestCommand != "echo hi" {
		t.Errorf("spec meta wrong: %+v", spec)
	}
	// image-init copied to /sentiae/init.
	if got := readFile(t, filepath.Join(staging, "sentiae", "init")); got != "INITBIN" {
		t.Errorf("/sentiae/init = %q, want INITBIN", got)
	}
	if len(cfg.Entrypoint) != 1 {
		t.Errorf("returned config entrypoint = %v", cfg.Entrypoint)
	}
}

func TestUnpackTarPathTraversal(t *testing.T) {
	evil := gzTar([]tarEntry{{name: "../escape.txt", body: "x"}})
	gz, _ := gzip.NewReader(bytes.NewReader(evil))
	dir := t.TempDir()
	err := unpackTar(gz, dir, nil)
	if err == nil {
		t.Fatal("expected path-traversal rejection, got nil")
	}
}

func TestUnpackTarOpaqueWhiteout(t *testing.T) {
	dir := t.TempDir()
	// Pre-populate a dir that the opaque marker must clear.
	if err := os.MkdirAll(filepath.Join(dir, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "data", "old.txt"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	layer := gzTar([]tarEntry{
		{name: "data/.wh..wh..opq", typeflag: tar.TypeReg},
		{name: "data/new.txt", body: "new"},
	})
	gz, _ := gzip.NewReader(bytes.NewReader(layer))
	if err := unpackTar(gz, dir, nil); err != nil {
		t.Fatalf("unpackTar: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "data", "old.txt")); !os.IsNotExist(err) {
		t.Errorf("opaque whiteout should have removed old.txt, err=%v", err)
	}
	if got := readFile(t, filepath.Join(dir, "data", "new.txt")); got != "new" {
		t.Errorf("new.txt = %q, want new", got)
	}
}

func TestBuildEnvOrder(t *testing.T) {
	env := buildEnv([]string{"A=1", "B=2"}, map[string]string{"Z": "9", "A": "override"})
	// image env first, descriptor keys sorted appended after.
	want := []string{"A=1", "B=2", "A=override", "Z=9"}
	if len(env) != len(want) {
		t.Fatalf("env = %v, want %v", env, want)
	}
	for i := range want {
		if env[i] != want[i] {
			t.Errorf("env[%d] = %q, want %q", i, env[i], want[i])
		}
	}
}

func TestBuildEntrypointOCISemantics(t *testing.T) {
	// No entrypoint → cmd is the argv.
	got := buildEntrypoint(ImageConfig{Cmd: []string{"python", "app.py"}})
	if len(got) != 2 || got[0] != "python" {
		t.Errorf("cmd-only = %v", got)
	}
	// Entrypoint + cmd → concatenated.
	got = buildEntrypoint(ImageConfig{Entrypoint: []string{"/entry"}, Cmd: []string{"--x"}})
	if len(got) != 2 || got[0] != "/entry" || got[1] != "--x" {
		t.Errorf("entry+cmd = %v", got)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// TestUnpackTarSymlinkParentTraversal is the classic tar-extraction escape: an
// earlier entry plants a symlink to a directory OUTSIDE the staging dir; a
// later entry writes a file THROUGH it. safeParent must reject the write and
// the outside directory must stay untouched.
func TestUnpackTarSymlinkParentTraversal(t *testing.T) {
	outside := t.TempDir() // the "host" directory the attack targets
	dir := t.TempDir()

	evil := gzTar([]tarEntry{
		{name: "x", typeflag: tar.TypeSymlink, linkname: outside},
		{name: "x/passwd", body: "owned"},
	})
	gz, _ := gzip.NewReader(bytes.NewReader(evil))
	err := unpackTar(gz, dir, nil)
	if err == nil {
		t.Fatal("expected symlink-parent traversal rejection, got nil")
	}
	if _, statErr := os.Stat(filepath.Join(outside, "passwd")); !os.IsNotExist(statErr) {
		t.Fatalf("attack escaped: %s/passwd exists (unpack err was %v)", outside, err)
	}
}

// TestUnpackTarRelativeSymlinkParentTraversal: same escape via a relative
// linkname climbing out of the staging dir.
func TestUnpackTarRelativeSymlinkParentTraversal(t *testing.T) {
	dir := t.TempDir()
	evil := gzTar([]tarEntry{
		{name: "up", typeflag: tar.TypeSymlink, linkname: "../../"},
		{name: "up/owned.txt", body: "x"},
	})
	gz, _ := gzip.NewReader(bytes.NewReader(evil))
	if err := unpackTar(gz, dir, nil); err == nil {
		t.Fatal("expected relative symlink-parent traversal rejection, got nil")
	}
}

// TestUnpackTarLegitAbsoluteSymlink: absolute in-guest symlinks (nginx's
// access.log -> /dev/stdout) are legitimate image content — they must extract
// (they are never followed on the host unless written through, which the
// parent guard blocks).
func TestUnpackTarLegitAbsoluteSymlink(t *testing.T) {
	dir := t.TempDir()
	ok := gzTar([]tarEntry{
		{name: "var/log/", typeflag: tar.TypeDir, mode: 0o755},
		{name: "var/log/access.log", typeflag: tar.TypeSymlink, linkname: "/dev/stdout"},
	})
	gz, _ := gzip.NewReader(bytes.NewReader(ok))
	if err := unpackTar(gz, dir, nil); err != nil {
		t.Fatalf("legit absolute symlink rejected: %v", err)
	}
	fi, err := os.Lstat(filepath.Join(dir, "var", "log", "access.log"))
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("symlink not created: %v", err)
	}
}

// TestUnpackTarDecompressionBudget: total unpacked bytes past the budget abort
// the extraction.
func TestUnpackTarDecompressionBudget(t *testing.T) {
	dir := t.TempDir()
	big := gzTar([]tarEntry{{name: "blob.bin", body: strings.Repeat("A", 4096)}})
	gz, _ := gzip.NewReader(bytes.NewReader(big))
	budget := int64(maxUnpackedBytes - 1024) // 1KB left of the ceiling
	if err := unpackTar(gz, dir, &budget); err == nil {
		t.Fatal("expected decompression-budget rejection, got nil")
	}
}
