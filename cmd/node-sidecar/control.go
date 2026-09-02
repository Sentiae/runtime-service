package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"
	"time"
)

// bindingMaxBytes caps what the control socket will read. A binding is a small
// document; anything larger is a mistake or an attempt to exhaust the sidecar.
const bindingMaxBytes = 1 << 20

// controlReadTimeout bounds one delivery attempt on the control socket, so a
// connection that opens and never speaks cannot hold the sidecar unready.
const controlReadTimeout = 10 * time.Second

// ackOK is what `node-sidecar bind` waits for. It is written only AFTER the
// binding has been applied, so a zero exit status from the exec means the
// broker and/or proxy are actually up — not merely that bytes were delivered.
const ackOK = "ok\n"

// binding is the JSON document the runtime hands this process on the attached
// stdin stream of `docker exec -i … bind`. It mirrors the runtime's
// usecase.SidecarBinding on the wire; TestBinding_MatchesRuntimeContract pins
// the two together so a field cannot be renamed on one side only.
//
// It is held in MEMORY ONLY: never written to a file, never echoed to stdout or
// stderr, never logged (T4.13, §9.6).
type binding struct {
	Invocation string                  `json:"invocation"`
	Run        string                  `json:"run"`
	Node       string                  `json:"node"`
	Secrets    map[string]secretAnswer `json:"secrets"`
	Egress     *egressBinding          `json:"egress,omitempty"`
}

// secretAnswer is one resolved secret: the handle the node presents, and the
// value only this process holds.
type secretAnswer struct {
	Handle string `json:"Handle"`
	Found  bool   `json:"Found"`
	Value  string `json:"Value"`
}

// egressBinding is the proxy half: what the node may reach, the bearer its
// requests must carry, and the invocation subnet the proxy binds inside.
type egressBinding struct {
	Patterns []string `json:"patterns"`
	Token    string   `json:"token"`
	Subnet   string   `json:"subnet"`
}

// redactedKeys are the log field names this process REFUSES to emit. It is a
// backstop, not the boundary: the boundary is that the binding is never passed
// to a log call at all. A backstop exists because the cost of one careless
// `"token", tok` is a credential in a container log that §9.6 greps.
var redactedKeys = map[string]bool{"secrets": true, "token": true, "value": true, "handle": true}

// redactedValue replaces a refused field. It is emitted rather than dropped so
// a reader can see that something was refused instead of wondering.
const redactedValue = "[redacted]"

// filterHandler drops the value of every refused key, at any nesting depth.
type filterHandler struct{ inner slog.Handler }

func (h filterHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

func (h filterHandler) Handle(ctx context.Context, r slog.Record) error {
	out := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	r.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(filterAttr(a))
		return true
	})
	return h.inner.Handle(ctx, out)
}

func (h filterHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	filtered := make([]slog.Attr, 0, len(attrs))
	for _, a := range attrs {
		filtered = append(filtered, filterAttr(a))
	}
	return filterHandler{inner: h.inner.WithAttrs(filtered)}
}

func (h filterHandler) WithGroup(name string) slog.Handler {
	return filterHandler{inner: h.inner.WithGroup(name)}
}

func filterAttr(a slog.Attr) slog.Attr {
	if redactedKeys[a.Key] {
		return slog.String(a.Key, redactedValue)
	}
	if a.Value.Kind() == slog.KindGroup {
		grouped := a.Value.Group()
		filtered := make([]any, 0, len(grouped))
		for _, g := range grouped {
			filtered = append(filtered, filterAttr(g))
		}
		return slog.Group(a.Key, filtered...)
	}
	return a
}

// newSidecarLogger builds the sidecar's logger with the refusing filter in it.
func newSidecarLogger(w io.Writer) *slog.Logger {
	return slog.New(filterHandler{inner: slog.NewJSONHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo})})
}

// listenControl opens the private control socket. 0600 on the sidecar's own
// tmpfs: nothing else in this container's namespace has any business handing it
// a binding, and the runtime reaches it through `docker exec` as the same user.
func listenControl(path string) (net.Listener, error) {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("control socket %s: %w", path, err)
	}
	lis, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("control socket %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = lis.Close()
		return nil, fmt.Errorf("control socket %s: chmod 0600: %w", path, err)
	}
	return lis, nil
}

// readBinding reads ONE binding document off an accepted control connection.
func readBinding(conn net.Conn) (binding, error) {
	_ = conn.SetReadDeadline(time.Now().Add(controlReadTimeout))
	raw, err := io.ReadAll(io.LimitReader(conn, bindingMaxBytes))
	if err != nil {
		return binding{}, fmt.Errorf("read binding: %w", err)
	}
	var b binding
	if err := json.Unmarshal(raw, &b); err != nil {
		// The document itself is never echoed: a decode failure names the
		// failure, never the bytes.
		return binding{}, errors.New("binding is not a JSON document")
	}
	if b.Invocation == "" {
		return binding{}, errors.New("binding names no invocation")
	}
	return b, nil
}

// sidecar is the running process: health first, then one binding, then the
// broker and/or the proxy the binding asks for.
type sidecar struct {
	opt options
	log *slog.Logger

	// ready closes when the binding has been applied and every listener the
	// binding asked for is serving. /healthz answers ok only after that.
	ready chan struct{}

	mu        sync.Mutex
	healthAt  string
	brokerAt  string
	proxyAt   string
	localAddr func() ([]net.Addr, error)
}

func newSidecar(opt options, log *slog.Logger) *sidecar {
	return &sidecar{opt: opt, log: log, ready: make(chan struct{}), localAddr: net.InterfaceAddrs}
}

// healthAddr / brokerAddr / proxyAddr report what is actually bound. They are
// how a test asserts that NO proxy exists for a binding without egress (T4.14)
// rather than asserting on a code path.
func (s *sidecar) healthAddr() string { s.mu.Lock(); defer s.mu.Unlock(); return s.healthAt }
func (s *sidecar) brokerAddr() string { s.mu.Lock(); defer s.mu.Unlock(); return s.brokerAt }
func (s *sidecar) proxyAddr() string  { s.mu.Lock(); defer s.mu.Unlock(); return s.proxyAt }

// run is the whole sidecar lifecycle. It returns when ctx is cancelled.
func (s *sidecar) run(ctx context.Context) error {
	control, err := listenControl(s.opt.ControlSocket)
	if err != nil {
		return err
	}
	defer func() { _ = control.Close() }()

	healthLis, err := net.Listen("tcp", s.opt.HealthListen)
	if err != nil {
		return fmt.Errorf("health listener %s: %w", s.opt.HealthListen, err)
	}
	s.mu.Lock()
	s.healthAt = healthLis.Addr().String()
	s.mu.Unlock()
	health := &http.Server{Handler: http.HandlerFunc(s.serveHealth), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		defer func() { _ = recover() }()
		_ = health.Serve(healthLis)
	}()
	defer func() { _ = health.Close() }()

	b, ack, err := s.await(ctx, control)
	if err != nil {
		return err
	}

	closers, applyErr := s.apply(b)
	ack(applyErr)
	if applyErr != nil {
		return applyErr
	}
	defer closers()

	// The binding is applied and every listener it asked for is up: only NOW is
	// this sidecar ready, because the runtime launches the node on this signal
	// and a node that starts first would dial a socket nobody serves.
	close(s.ready)
	s.log.Info("sidecar_bound", "invocation", b.Invocation, "node", b.Node,
		"secret_count", len(b.Secrets), "egress", b.Egress != nil)

	<-ctx.Done()
	return nil
}

// await blocks until a well-formed binding arrives, refusing (and surviving)
// anything else. It returns the document and the ack to call once it has been
// applied.
func (s *sidecar) await(ctx context.Context, lis net.Listener) (binding, func(error), error) {
	type accepted struct {
		conn net.Conn
		b    binding
	}
	results := make(chan accepted, 1)
	go func() {
		defer func() { _ = recover() }()
		for {
			conn, err := lis.Accept()
			if err != nil {
				return
			}
			b, rerr := readBinding(conn)
			if rerr != nil {
				_, _ = io.WriteString(conn, "err "+rerr.Error()+"\n")
				_ = conn.Close()
				s.log.Warn("control_binding_refused", "err", rerr.Error())
				continue
			}
			results <- accepted{conn: conn, b: b}
			return
		}
	}()

	select {
	case <-ctx.Done():
		return binding{}, nil, ctx.Err()
	case got := <-results:
		ack := func(applyErr error) {
			defer func() { _ = got.conn.Close() }()
			_ = got.conn.SetWriteDeadline(time.Now().Add(controlReadTimeout))
			if applyErr != nil {
				_, _ = io.WriteString(got.conn, "err "+applyErr.Error()+"\n")
				return
			}
			_, _ = io.WriteString(got.conn, ackOK)
		}
		return got.b, ack, nil
	}
}

// apply starts exactly what the binding asks for: a broker iff it carries
// secrets, a proxy iff it carries egress. Neither is started speculatively —
// an unbound proxy on the invocation bridge would be an open relay for the one
// container that can reach it.
func (s *sidecar) apply(b binding) (func(), error) {
	var closers []func()
	closeAll := func() {
		for i := len(closers) - 1; i >= 0; i-- {
			closers[i]()
		}
	}

	if len(b.Secrets) > 0 {
		lis, srv, err := startBroker(s.opt.RunDir, b)
		if err != nil {
			closeAll()
			return nil, err
		}
		s.mu.Lock()
		s.brokerAt = lis.Addr().String()
		s.mu.Unlock()
		closers = append(closers, func() { _ = srv.Close() })
	}

	if b.Egress != nil {
		lis, err := s.startProxy(b)
		if err != nil {
			closeAll()
			return nil, err
		}
		s.mu.Lock()
		s.proxyAt = lis.Addr().String()
		s.mu.Unlock()
		closers = append(closers, func() { _ = lis.Close() })
	}

	return closeAll, nil
}

// serveHealth is the readiness endpoint the runtime polls before it launches
// the node. It answers ok ONLY once the binding is applied — never before, so
// "ready" can never mean "the container started".
func (s *sidecar) serveHealth(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/healthz" {
		http.NotFound(w, r)
		return
	}
	select {
	case <-s.ready:
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	default:
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, "binding pending")
	}
}
