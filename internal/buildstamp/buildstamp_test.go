package buildstamp

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/sentiae/platform-kit/buildinfo"
)

// probeEnv marks the child process. The guard re-enters `go test` on this same
// package with the Dockerfile's linker flags applied; the marker keeps the
// child from recursing and keeps the parent from running the probe body
// unstamped.
const probeEnv = "SENTIAE_BUILDSTAMP_PROBE"

const (
	// A revision no build could ever produce, so a match cannot be a
	// coincidence — only the linker writing the sentinel puts it there.
	sentinelRevision = "0123456789abcdef0123456789abcdef01234567"
	// "true" rather than "false" so the assertion also fails when the Modified
	// symbol is wrong: false is buildinfo's value for an absent stamp.
	sentinelModified = "true"
)

// TestStampIsReadableFromLinkedBinary is the mechanical control over the Go
// linker's silence: `-X` on a symbol path that does not exist is IGNORED
// without any diagnostic, so a renamed variable or a moved package bakes a
// permanently empty build identity into an image nobody can patch. That
// near-miss already happened once (a publisher stamping `PrimaryRevision` while
// the package exports `Revision`).
//
// It reads the -X arguments out of the Dockerfile itself — not a copy of them —
// relinks this package with them under a sentinel value, and asserts the
// sentinel is what buildinfo.Get() returns inside that binary. If the symbol
// path in the Dockerfile is wrong, the linker drops the flag, buildinfo falls
// back to the checkout's own vcs.revision, and this test fails.
//
// Mechanism: `go test -ldflags` on this package, in a subprocess. Chosen over
// `go tool nm` (which proves a symbol exists, not that the flag reached it) and
// over building a real binary and running it (needs a main that does not start
// a server). It needs no network (GOPROXY=off, the module cache is already warm
// from compiling this very test) and no docker.
func TestStampIsReadableFromLinkedBinary(t *testing.T) {
	if os.Getenv(probeEnv) != "" {
		t.Skip("probe child: the assertion runs in TestStampProbe")
	}

	root := repoRoot(t)
	ldflags := dockerfileStampLdflags(t, root)

	cmd := exec.Command("go", "test", "-count=1", "-v",
		"-run", "^TestStampProbe$",
		"-ldflags", ldflags,
		"./internal/buildstamp")
	cmd.Dir = root
	// GOPROXY=off proves the guard needs no network: every dependency it links
	// is already in the module cache, because this test binary linked them.
	cmd.Env = append(os.Environ(), probeEnv+"=1", "GOPROXY=off")

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("relinking with the Dockerfile's ldflags failed: %v\nldflags=%s\noutput:\n%s", err, ldflags, out)
	}

	want := fmt.Sprintf("PROBE primary_revision=%s modified=true", sentinelRevision)
	if !strings.Contains(string(out), want) {
		t.Fatalf("the Dockerfile's -X flags did not reach buildinfo.\n"+
			"The linker ignores -X on a symbol path that does not exist, so the shipped image would\n"+
			"report a build identity the deploy authority never chose.\n"+
			"want line: %s\nldflags:   %s\noutput:\n%s", want, ldflags, out)
	}
}

// TestStampProbe reports what the linked binary actually carries. It only runs
// inside the child process, where the ldflags under test have been applied.
func TestStampProbe(t *testing.T) {
	if os.Getenv(probeEnv) == "" {
		t.Skip("probe body: driven by TestStampIsReadableFromLinkedBinary")
	}
	info := buildinfo.Get()
	fmt.Printf("PROBE primary_revision=%s modified=%t source_manifest_digest=%s\n",
		info.PrimaryRevision, info.Modified, info.SourceManifestDigest)
}

// stampFlagRx matches a linker -X argument whose value comes from one of the
// Dockerfile's provenance build args, capturing the symbol path and the arg.
var stampFlagRx = regexp.MustCompile(`-X\s+([^\s=]+)=\$\{(VCS_REVISION|VCS_MODIFIED)\}`)

// dockerfileStampLdflags builds the -ldflags string from the Dockerfile's own
// -X arguments, substituting the sentinel values. Reading the real file is the
// point: a copy of the symbol paths here would drift with the defect.
func dockerfileStampLdflags(t *testing.T, root string) string {
	t.Helper()

	b, err := os.ReadFile(filepath.Join(root, "Dockerfile"))
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}

	values := map[string]string{
		"VCS_REVISION": sentinelRevision,
		"VCS_MODIFIED": sentinelModified,
	}
	seen := map[string]bool{}
	var flags []string
	for _, m := range stampFlagRx.FindAllStringSubmatch(string(b), -1) {
		symbol, arg := m[1], m[2]
		seen[arg] = true
		flags = append(flags, fmt.Sprintf("-X %s=%s", symbol, values[arg]))
	}

	for arg := range values {
		if !seen[arg] {
			t.Fatalf("the Dockerfile no longer links %s into a build-identity symbol: "+
				"the deployed image would report a build identity nobody stamped", arg)
		}
	}
	return strings.Join(flags, " ")
}

// repoRoot walks up from the package directory to the module root.
func repoRoot(t *testing.T) string {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod above %s", dir)
		}
		dir = parent
	}
}
