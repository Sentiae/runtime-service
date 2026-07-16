package oci

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestWriteRuntimeJSONJobCommandIsArgvExact asserts a job's entrypoint override
// reaches the guest as an ARGV LIST, preserved element-for-element. This is the
// contract that makes shell interpolation impossible: the guest execs
// argv[0]+argv[1:] directly, so an element containing shell metacharacters is an
// argument, never a command. If this ever collapsed to a joined string, the
// guest would need a shell to re-parse it and the injection surface would return.
func TestWriteRuntimeJSONJobCommandIsArgvExact(t *testing.T) {
	staging := t.TempDir()
	// Deliberately hostile argv: metacharacters, spaces, and quotes that a shell
	// would act on but a direct exec must carry through verbatim as ONE argument.
	argv := []string{
		"/app/migrate",
		"up",
		"--dsn=postgres://u:p@db/app?x=1&y=2",
		"; rm -rf /",
		"$(whoami)",
		"a b\tc",
	}
	req := MaterializeRequest{Mode: "job", JobCmd: argv}
	if err := writeRuntimeJSON(staging, req, ImageConfig{Entrypoint: []string{"/bin/orig"}}); err != nil {
		t.Fatalf("writeRuntimeJSON: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(staging, "sentiae", "runtime.json"))
	if err != nil {
		t.Fatalf("read runtime.json: %v", err)
	}
	var spec runtimeSpec
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("decode runtime.json: %v", err)
	}

	if spec.Mode != "job" {
		t.Errorf("mode = %q, want job", spec.Mode)
	}
	if len(spec.JobCommand) != len(argv) {
		t.Fatalf("job_command = %v (len %d), want %d elements preserved exactly", spec.JobCommand, len(spec.JobCommand), len(argv))
	}
	for i := range argv {
		if spec.JobCommand[i] != argv[i] {
			t.Errorf("job_command[%d] = %q, want %q (argv must survive verbatim)", i, spec.JobCommand[i], argv[i])
		}
	}
	// A job never rides the shell-interpolated test_command path.
	if spec.TestCommand != "" {
		t.Errorf("test_command = %q, want empty on a job (job_command is argv-exact, test_command is /bin/sh -c)", spec.TestCommand)
	}
}

// TestWriteRuntimeJSONNoJobCommandOmitsIt asserts an empty job_command is absent
// from runtime.json, so the guest falls back to the image's own entrypoint.
func TestWriteRuntimeJSONNoJobCommandOmitsIt(t *testing.T) {
	staging := t.TempDir()
	req := MaterializeRequest{Mode: "job"}
	if err := writeRuntimeJSON(staging, req, ImageConfig{Entrypoint: []string{"/bin/orig"}, Cmd: []string{"--go"}}); err != nil {
		t.Fatalf("writeRuntimeJSON: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(staging, "sentiae", "runtime.json"))
	if err != nil {
		t.Fatalf("read runtime.json: %v", err)
	}
	var spec runtimeSpec
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("decode runtime.json: %v", err)
	}
	if len(spec.JobCommand) != 0 {
		t.Errorf("job_command = %v, want empty (the image entrypoint runs)", spec.JobCommand)
	}
	if len(spec.Entrypoint) != 2 || spec.Entrypoint[0] != "/bin/orig" || spec.Entrypoint[1] != "--go" {
		t.Errorf("entrypoint = %v, want the image's own [/bin/orig --go]", spec.Entrypoint)
	}
}
