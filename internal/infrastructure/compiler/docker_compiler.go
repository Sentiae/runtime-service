// Package compiler implements the ProjectCompiler port using an ephemeral
// build container. Each Compile call writes the file set to a host temp
// dir and runs a single `docker run --rm` against a toolchain image,
// capturing structured diagnostics. The built artifact is never executed
// and the container is removed on exit — this path is deliberately
// independent of the Firecracker execution / VM pool.
package compiler

import (
	"archive/tar"
	"bytes"
	"context"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/usecase"
)

// Compile-time assertion that DockerCompiler satisfies the port.
var _ usecase.ProjectCompiler = (*DockerCompiler)(nil)

const (
	// defaultTimeoutSec bounds a build when the caller passes zero.
	defaultTimeoutSec = 120
	// maxTimeoutSec caps the per-build budget regardless of request.
	maxTimeoutSec = 600
	// rawOutputCap truncates RawOutput to keep responses bounded (~8KB).
	rawOutputCap = 8 * 1024
)

// DockerCompiler builds projects in ephemeral toolchain containers.
type DockerCompiler struct {
	// dockerPath is the resolved path to the docker CLI, or "" when docker
	// is unavailable (compile then fails fast with a toolchain error).
	dockerPath string
}

// NewDockerCompiler resolves the docker CLI once at construction. When
// docker is not on PATH, Compile returns domain.ErrCompileToolchainUnavailable.
func NewDockerCompiler() *DockerCompiler {
	path, err := exec.LookPath("docker")
	if err != nil {
		path = ""
	}
	return &DockerCompiler{dockerPath: path}
}

// toolchain describes how to build a given language: which image to run,
// the shell build command, the diagnostic parser, and the named cache
// volumes to mount so repeat builds don't re-download dependencies.
type toolchain struct {
	image    string
	buildCmd string
	parse    func(output string) []domain.CompileDiagnostic
	// caches maps a NAMED docker volume to its mount point inside the build
	// container (e.g. the Go module + build caches). Named volumes (not bind
	// mounts) are managed by the host daemon, so they work under docker-out-of-
	// docker and persist across ephemeral `--rm` builds — turning a ~18s
	// cold module download into a near-instant cache hit. Concurrent builds
	// share them safely (the go toolchain locks the module/build caches).
	caches map[string]string
}

// toolchainFor returns the build recipe for a language, or false when the
// language is unsupported.
//
// Network: the Go build needs the default container network to fetch
// modules (e.g. gorm, uuid) via `go build ./...` with GOFLAGS=-mod=mod, so
// we deliberately do NOT pass --network=none. The TypeScript path installs
// the compiler from npm and likewise needs network. Isolation comes from
// --rm + resource caps + the context timeout, not from network removal.
func toolchainFor(language string) (toolchain, bool) {
	switch language {
	case "go", "golang":
		return toolchain{
			image:    "golang:1.25-alpine",
			buildCmd: "GOFLAGS=-mod=mod CGO_ENABLED=0 go build ./...",
			parse:    parseGoDiagnostics,
			caches: map[string]string{
				"sentiae-compile-gomodcache": "/go/pkg/mod",       // downloaded modules
				"sentiae-compile-gocache":    "/root/.cache/go-build", // build cache
			},
		}, true
	case "typescript", "ts":
		return toolchain{
			image:    "node:22-alpine",
			buildCmd: "npm i -g typescript >/dev/null 2>&1 && tsc --noEmit -p tsconfig.json",
			parse:    parseTSDiagnostics,
			caches: map[string]string{
				"sentiae-compile-npmcache": "/root/.npm", // npm global install cache
			},
		}, true
	}
	return toolchain{}, false
}

// Compile writes the file set to a host temp dir and builds it inside an
// ephemeral container. It returns a CompileResult on a clean compile
// (success or failure) and a non-nil error only on infrastructure faults.
func (c *DockerCompiler) Compile(ctx context.Context, language string, files []domain.SourceFile, timeoutSec int) (*domain.CompileResult, error) {
	tc, ok := toolchainFor(language)
	if !ok {
		return nil, domain.ErrUnsupportedCompileLanguage
	}
	if c.dockerPath == "" {
		// Build backend is not installed — surface as an infra error so the
		// caller degrades rather than reporting a compile failure.
		return nil, domain.ErrCompileToolchainUnavailable
	}

	timeoutSec = clampTimeout(timeoutSec)
	start := time.Now()

	// Stream the project INTO the build container as a tar on stdin rather than a
	// bind mount. Under docker-out-of-docker (this process runs in a container and
	// talks to the host daemon), a `-v hostpath:/work` is resolved by the HOST
	// daemon against the HOST filesystem — a temp dir created in THIS container is
	// invisible there, so /work would be empty. Tar-over-stdin needs no shared path.
	tarball, err := tarProject(files)
	if err != nil {
		return nil, domain.ErrCompileToolchainUnavailable
	}

	runCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
	defer cancel()

	// docker run --rm -i: ephemeral, reads the tar on stdin, no --privileged.
	// Resource caps bound the build; the context timeout is the hard stop. Default
	// network is kept so Go module / npm resolution works (see toolchainFor).
	// `sh -c` (NOT `-lc`): a login shell re-reads /etc/profile and resets PATH to a
	// default that drops the image's toolchain dir (golang's /usr/local/go/bin →
	// "go: not found"); `-c` preserves the image ENV PATH. The wrapper extracts the
	// streamed tar into /work, then runs the build there.
	args := []string{
		"run", "--rm", "-i",
		"--memory=512m",
		"--cpus=1",
	}
	// Persist dependency caches across builds via named volumes (sorted for a
	// deterministic arg order).
	cacheVols := make([]string, 0, len(tc.caches))
	for vol := range tc.caches {
		cacheVols = append(cacheVols, vol)
	}
	sort.Strings(cacheVols)
	for _, vol := range cacheVols {
		args = append(args, "-v", vol+":"+tc.caches[vol])
	}
	args = append(args,
		tc.image,
		"sh", "-c", "mkdir -p /work && cd /work && tar -xf - && "+tc.buildCmd,
	)

	cmd := exec.CommandContext(runCtx, c.dockerPath, args...)
	cmd.Stdin = bytes.NewReader(tarball)
	var combined bytes.Buffer
	cmd.Stdout = &combined
	cmd.Stderr = &combined

	runErr := cmd.Run()
	elapsed := time.Since(start).Milliseconds()
	output := combined.String()

	// A timeout / kill is an infra-shaped failure surfaced through the
	// result so the caller still sees partial diagnostics.
	if runCtx.Err() == context.DeadlineExceeded {
		return &domain.CompileResult{
			OK:            false,
			Diagnostics:  tc.parse(output),
			RawOutput:     truncate(output, rawOutputCap),
			CompileTimeMS: elapsed,
		}, nil
	}

	ok = runErr == nil // exit 0 → success
	return &domain.CompileResult{
		OK:            ok,
		Diagnostics:  tc.parse(output),
		RawOutput:     truncate(output, rawOutputCap),
		CompileTimeMS: elapsed,
	}, nil
}

// tarProject builds an in-memory tar of the file set, streamed to the build
// container on stdin. Paths are cleaned and made relative (a leading "/" or ".."
// can't escape the extraction dir) so the tree extracts faithfully under /work.
func tarProject(files []domain.SourceFile) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range files {
		// Force a clean relative path: strip any leading slash / ".." traversal.
		name := strings.TrimPrefix(filepath.ToSlash(filepath.Clean("/"+f.Path)), "/")
		if name == "" {
			continue
		}
		hdr := &tar.Header{
			Name: name,
			Mode: 0o644,
			Size: int64(len(f.Content)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, err
		}
		if _, err := tw.Write([]byte(f.Content)); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func clampTimeout(timeoutSec int) int {
	if timeoutSec <= 0 {
		return defaultTimeoutSec
	}
	if timeoutSec > maxTimeoutSec {
		return maxTimeoutSec
	}
	return timeoutSec
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}

// goDiagRx matches `path/file.go:LINE:COL: message`.
var goDiagRx = regexp.MustCompile(`^(.+\.go):(\d+):(\d+): (.*)$`)

// tsDiagRx matches `path/file.ts(LINE,COL): error TSxxxx: message`.
var tsDiagRx = regexp.MustCompile(`^(.+\.ts)\((\d+),(\d+)\): error TS\d+: (.*)$`)

// parseGoDiagnostics extracts structured diagnostics from `go build`
// output. Lines that don't match the compiler format are ignored.
func parseGoDiagnostics(output string) []domain.CompileDiagnostic {
	return parseWith(output, goDiagRx)
}

// parseTSDiagnostics extracts structured diagnostics from `tsc` output.
func parseTSDiagnostics(output string) []domain.CompileDiagnostic {
	return parseWith(output, tsDiagRx)
}

// parseWith runs a `file:line:col:msg`-shaped regex over each output line.
func parseWith(output string, rx *regexp.Regexp) []domain.CompileDiagnostic {
	var diags []domain.CompileDiagnostic
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimRight(line, "\r")
		m := rx.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		lineNo, _ := strconv.Atoi(m[2])
		colNo, _ := strconv.Atoi(m[3])
		diags = append(diags, domain.CompileDiagnostic{
			File:    m[1],
			Line:    lineNo,
			Column:  colNo,
			Message: m[4],
		})
	}
	return diags
}
