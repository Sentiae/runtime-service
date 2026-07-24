// Command guest-agent is the in-guest execution agent for WARM Firecracker
// microVMs. Unlike the single-shot rootfs-injection model (boot → run /init →
// power off), a warm VM boots this agent ONCE and keeps it resident: the host
// POSTs code to it and gets results back, with no per-execution boot.
//
// It is the foundation of the fast-start path (CS-2 / P5): a warmed template VM
// running this agent is snapshotted; restoring/cloning the snapshot yields a VM
// that is already listening and ready to run code in ~hundreds of ms instead of
// a multi-second cold boot.
//
// Transport for increment 1 is HTTP over the VM's existing TAP interface (the
// host reaches it on the per-VM /30). The isolation channel (AF_VSOCK, no guest
// network) is a follow-up hardening — the request/response contract here is
// transport-agnostic so the switch is local to main().
//
// Build: CGO_ENABLED=0 GOOS=linux GOARCH=amd64 — a static binary baked into each
// language rootfs and started as the guest's init.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// capBuf is a bytes buffer capped at maxOutput so a runaway program (infinite
// print loop) can't exhaust the agent's memory. Writes past the cap are dropped
// and a truncation marker is appended once.
type capBuf struct {
	buf       []byte
	truncated bool
}

const maxOutput = 1 << 20 // 1 MiB per stream

func (c *capBuf) Write(p []byte) (int, error) {
	if room := maxOutput - len(c.buf); room > 0 {
		if len(p) > room {
			c.buf = append(c.buf, p[:room]...)
			if !c.truncated {
				c.buf = append(c.buf, "\n[output truncated]"...)
				c.truncated = true
			}
		} else {
			c.buf = append(c.buf, p...)
		}
	}
	return len(p), nil // always report full write so the program isn't blocked
}

func (c *capBuf) String() string { return string(c.buf) }

// runRequest is what the host POSTs to /run. language selects the interpreter;
// code is the program; stdin is fed to it on stdin. timeout_ms bounds the run.
type runRequest struct {
	Language  string `json:"language"`
	Code      string `json:"code"`
	Stdin     string `json:"stdin"`
	TimeoutMS int    `json:"timeout_ms"`
}

// runResponse is the result handed back. It mirrors the well-known files the
// single-shot path writes (stdout/stderr/exit_code) so the host-side result
// mapping is identical across cold and warm execution.
type runResponse struct {
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	ExitCode   int    `json:"exit_code"`
	DurationMS int64  `json:"duration_ms"`
	Error      string `json:"error,omitempty"`
}

// langCommand maps a language to (file extension, argv) for running a program
// file. Kept tiny and explicit — the rootfs ships exactly these interpreters.
func langCommand(language, file string) (string, []string, bool) {
	switch language {
	case "python", "python3", "":
		return ".py", []string{"python3", file}, true
	case "javascript", "node", "js":
		return ".js", []string{"node", file}, true
	}
	return "", nil, false
}

func main() {
	// The warm rootfs is READ-ONLY (one shared ext4 inode across the template and
	// every concurrent clone), so a writable tmpfs on /tmp is what every run's
	// working directory lives on. Must happen before the first /run.
	mountRunTmpfs()

	// Running as the guest's init means no inherited PATH — set one so the
	// interpreter lookup (exec.LookPath at command time) and the child env both
	// resolve `python3`/`node`.
	if os.Getenv("PATH") == "" {
		_ = os.Setenv("PATH", "/usr/local/bin:/usr/local/sbin:/usr/bin:/usr/sbin:/bin:/sbin")
	}

	addr := os.Getenv("AGENT_ADDR")
	if addr == "" {
		addr = "0.0.0.0:8000"
	}

	mux := http.NewServeMux()
	// /healthz lets the host poll readiness after boot/restore before it sends
	// the first /run (the warm-pool + restore paths gate on this).
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/run", handleRun)

	srv := &http.Server{Addr: addr, Handler: mux}
	// The agent IS the long-lived process; if ListenAndServe returns the VM has
	// nothing else to do, so exit non-zero to make the failure visible in logs.
	if err := srv.ListenAndServe(); err != nil {
		fmt.Fprintf(os.Stderr, "guest-agent: serve: %v\n", err)
		os.Exit(1)
	}
}

func handleRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req runRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, runResponse{ExitCode: -1, Error: "decode request: " + err.Error()})
		return
	}

	ext, argv, ok := langCommand(req.Language, "")
	if !ok {
		writeJSON(w, runResponse{ExitCode: -1, Error: "unsupported language: " + req.Language})
		return
	}

	// Each run gets an isolated working dir under /tmp so concurrent or
	// sequential runs never collide on the code file.
	dir, err := os.MkdirTemp("", "run-")
	if err != nil {
		writeJSON(w, runResponse{ExitCode: -1, Error: "mktemp: " + err.Error()})
		return
	}
	defer os.RemoveAll(dir)

	codeFile := filepath.Join(dir, "code"+ext)
	if err := os.WriteFile(codeFile, []byte(req.Code), 0o600); err != nil {
		writeJSON(w, runResponse{ExitCode: -1, Error: "write code: " + err.Error()})
		return
	}
	// Re-resolve argv with the real file path.
	_, argv, _ = langCommand(req.Language, codeFile)

	timeout := time.Duration(req.TimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	start := time.Now()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	// The agent runs as the guest's init, which has no inherited PATH/HOME, so
	// interpreter lookup (`python3`, `node`) would fail. Seed a sane env for the
	// child explicitly rather than depend on the init script.
	//
	// HOME/TMPDIR/XDG_CACHE_HOME all point at the per-run dir (on the tmpfs) and
	// NOT at /root: the rootfs is read-only, so an interpreter writing its cache
	// to $HOME (npm/pip caches, ~/.cache) would hit EROFS. Per-run also means the
	// caches die with the run — no leakage between two tenants' clones.
	cmd.Env = append(os.Environ(),
		"PATH=/usr/local/bin:/usr/local/sbin:/usr/bin:/usr/sbin:/bin:/sbin",
		"HOME="+dir,
		"TMPDIR="+dir,
		"XDG_CACHE_HOME="+dir,
	)
	if req.Stdin != "" {
		cmd.Stdin = strings.NewReader(req.Stdin)
	}
	var stdout, stderr capBuf
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()
	resp := runResponse{
		Stdout:     stdout.String(),
		Stderr:     stderr.String(),
		DurationMS: time.Since(start).Milliseconds(),
	}
	if ctx.Err() == context.DeadlineExceeded {
		resp.ExitCode = -1
		resp.Error = "timeout after " + strconv.Itoa(int(timeout/time.Millisecond)) + "ms"
	} else if runErr != nil {
		if ee, isExit := runErr.(*exec.ExitError); isExit {
			resp.ExitCode = ee.ExitCode()
		} else {
			resp.ExitCode = -1
			resp.Error = runErr.Error()
		}
	}
	writeJSON(w, resp)
}

func writeJSON(w http.ResponseWriter, v runResponse) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
