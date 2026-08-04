package buildstamp

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// revisionRx is the shape of a git object name as the Go toolchain records it.
var revisionRx = regexp.MustCompile(`^[0-9a-f]{40}$`)

// buildSettingRx matches one `build <key>=<value>` line of `go version -m`
// output. The value is taken to the end of the line because settings such as
// -gcflags legitimately contain `=`.
var buildSettingRx = regexp.MustCompile(`(?m)^\s*build\s+([^\s=]+)=(.*)$`)

// TestUnstampedBuildCarriesVCSStamp is the mechanical control over the OTHER
// half of the build identity: the fallback.
//
// TestStampIsReadableFromLinkedBinary proves that ldflags, WHEN PASSED, reach
// buildinfo. But neither fleet-host build path passes any ldflags
// (infrastructure/scripts/deploy-fleet-host.sh, infrastructure/bake/scripts/
// publish-fleet-runtime.sh), so on those hosts buildinfo.Get() reaches its
// runtime/debug.ReadBuildInfo() fallback and reads the vcs.* settings the Go
// toolchain stamps at `go build` time. That fallback is the ONLY thing that
// lets an image-born fleet host report which revision it is running, and an
// image-born host can never be patched afterwards — so the property has to be
// held by a test that fails BEFORE the image is baked.
//
// Mechanism: build the real main package exactly as the fleet paths do — no
// -ldflags at all — and read the settings back out of the produced binary with
// `go version -m`. It cannot be an in-process check: the toolchain records
// vcs.* only into a `go build` of a main package and never into a test binary,
// so an in-process assertion would pass vacuously.
//
// It never skips. A guard that skips when .git is missing, when git is absent,
// or when the stamp is empty fails open, which is precisely the defect class it
// exists to prevent.
func TestUnstampedBuildCarriesVCSStamp(t *testing.T) {
	root := repoRoot(t)
	bin := filepath.Join(t.TempDir(), "runtime-service")

	// -buildvcs=true is explicit rather than left at auto: auto silently omits
	// the stamp in cases the toolchain considers ambiguous, and an omitted
	// stamp is the failure being guarded against, not an acceptable outcome.
	build := exec.Command("go", "build", "-trimpath", "-buildvcs=true", "-o", bin, "./cmd/server")
	build.Dir = root
	// GOFLAGS is cleared so an ambient -ldflags in the developer's or CI's
	// environment cannot stamp the binary and make the fallback look healthy
	// when it is not. GOPROXY=off proves the guard needs no network.
	build.Env = append(envWithout(os.Environ(), "GOFLAGS", "GOPROXY"), "GOFLAGS=", "GOPROXY=off")

	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building ./cmd/server without ldflags failed: %v\noutput:\n%s", err, out)
	}

	settings := buildSettings(t, bin)

	head := gitHead(t, root)

	revisions := settings["vcs.revision"]
	if len(revisions) != 1 {
		t.Fatalf("want exactly one vcs.revision build setting, got %d (%q).\n"+
			"Neither fleet-host build path passes -ldflags, so buildinfo falls back to this setting;\n"+
			"without it the reborn host reports an empty revision forever and cannot be patched.",
			len(revisions), revisions)
	}
	revision := revisions[0]
	if !revisionRx.MatchString(revision) {
		t.Fatalf("vcs.revision = %q, want a 40-hex git revision", revision)
	}
	if revision != head {
		t.Fatalf("vcs.revision = %q, want %q (git rev-parse HEAD): "+
			"the binary would report a revision that is not the one it was built from", revision, head)
	}

	modified := settings["vcs.modified"]
	if len(modified) != 1 {
		t.Fatalf("want exactly one vcs.modified build setting, got %d (%q)", len(modified), modified)
	}
	if _, err := strconv.ParseBool(modified[0]); err != nil {
		t.Fatalf("vcs.modified = %q, want a boolean: %v", modified[0], err)
	}
}

// buildSettings reads the build settings back out of a compiled binary.
func buildSettings(t *testing.T, bin string) map[string][]string {
	t.Helper()

	out, err := exec.Command("go", "version", "-m", bin).CombinedOutput()
	if err != nil {
		t.Fatalf("go version -m %s: %v\noutput:\n%s", bin, err, out)
	}

	settings := map[string][]string{}
	for _, m := range buildSettingRx.FindAllStringSubmatch(string(out), -1) {
		settings[m[1]] = append(settings[m[1]], strings.TrimSpace(m[2]))
	}
	return settings
}

// gitHead is the revision the build must claim. Failing here rather than
// skipping is deliberate: no checkout state is an acceptable reason to ship an
// image that cannot say what it is.
func gitHead(t *testing.T, root string) string {
	t.Helper()

	out, err := exec.Command("git", "-C", root, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD in %s: %v: "+
			"the build provenance guard cannot be satisfied without a checkout to compare against", root, err)
	}
	return strings.TrimSpace(string(out))
}

// envWithout returns env with the named variables removed, so the caller can
// set them to an exact value rather than appending a shadowing duplicate.
func envWithout(env []string, names ...string) []string {
	drop := map[string]bool{}
	for _, n := range names {
		drop[n] = true
	}
	kept := make([]string, 0, len(env))
	for _, kv := range env {
		if k, _, ok := strings.Cut(kv, "="); ok && drop[k] {
			continue
		}
		kept = append(kept, kv)
	}
	return kept
}
