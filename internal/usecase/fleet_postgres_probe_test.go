package usecase

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// ─────────────────────────────────────────────────────────────────────
// A fake Postgres: an in-process TCP listener that reads the client's
// StartupMessage, answers with a canned frame, and then records anything the
// client writes afterwards (there must be nothing — the probe stops at the auth
// challenge and never authenticates).
// ─────────────────────────────────────────────────────────────────────

type fakePostgres struct {
	ln net.Listener
	wg sync.WaitGroup

	mu      sync.Mutex
	startup []byte // the full StartupMessage as received (length prefix included)
	after   []byte // anything the client wrote after the StartupMessage
}

// startFakePostgres serves exactly one connection. reply writes the canned
// backend response; a nil reply closes the connection without answering.
func startFakePostgres(t *testing.T, reply func(net.Conn)) *fakePostgres {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	f := &fakePostgres{ln: ln}
	f.wg.Add(1)
	go func() {
		defer f.wg.Done()
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

		var length [4]byte
		if _, err := io.ReadFull(conn, length[:]); err != nil {
			return
		}
		n := int(binary.BigEndian.Uint32(length[:]))
		if n < 4 || n > 1<<16 {
			return
		}
		body := make([]byte, n-4)
		if _, err := io.ReadFull(conn, body); err != nil {
			return
		}
		f.mu.Lock()
		f.startup = append(append([]byte(nil), length[:]...), body...)
		f.mu.Unlock()

		if reply != nil {
			reply(conn)
		} else {
			return // accepted, then closed with no reply
		}

		// Anything read here is a write the probe made after the startup packet.
		// The probe closes immediately, so this returns EOF at once in practice;
		// the short deadline only bounds a probe that misbehaves.
		_ = conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		extra, _ := io.ReadAll(conn)
		f.mu.Lock()
		f.after = extra
		f.mu.Unlock()
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		f.wg.Wait()
	})
	return f
}

func (f *fakePostgres) addr(t *testing.T) (string, int) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(f.ln.Addr().String())
	if err != nil {
		t.Fatalf("split addr: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse port %q: %v", portStr, err)
	}
	return host, port
}

func (f *fakePostgres) received() (startup, after []byte) {
	f.wg.Wait()
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.startup, f.after
}

// ─────────────────────────────────────────────────────────────────────
// Canned backend frames.
// ─────────────────────────────────────────────────────────────────────

func pgFrame(typ byte, body []byte) []byte {
	out := make([]byte, 5, 5+len(body))
	out[0] = typ
	binary.BigEndian.PutUint32(out[1:5], uint32(4+len(body)))
	return append(out, body...)
}

// authenticationOk: 'R' + int32(8) + int32(0).
func authenticationOk() []byte {
	body := make([]byte, 4)
	binary.BigEndian.PutUint32(body, 0)
	return pgFrame('R', body)
}

// authenticationSASL: 'R' + len + int32(10) + "SCRAM-SHA-256\0" + \0.
func authenticationSASL() []byte {
	body := make([]byte, 4)
	binary.BigEndian.PutUint32(body, 10)
	body = append(body, "SCRAM-SHA-256"...)
	body = append(body, 0, 0)
	return pgFrame('R', body)
}

// errorResponse builds an 'E' frame from (field code, value) pairs.
func errorResponse(fields ...[2]string) []byte {
	body := []byte{}
	for _, f := range fields {
		body = append(body, f[0][0])
		body = append(body, f[1]...)
		body = append(body, 0)
	}
	body = append(body, 0)
	return pgFrame('E', body)
}

// hbaRejection is the exact shape the live defect produced.
func hbaRejection() []byte {
	return errorResponse(
		[2]string{"S", "FATAL"},
		[2]string{"V", "FATAL"},
		[2]string{"C", "28000"},
		[2]string{"M", `no pg_hba.conf entry for host "10.0.0.1", user "postgres", database "postgres", no encryption`},
	)
}

// ─────────────────────────────────────────────────────────────────────
// Tests
// ─────────────────────────────────────────────────────────────────────

func TestProbePostgresReady(t *testing.T) {
	tests := []struct {
		name    string
		reply   func(net.Conn)
		wantErr bool
		// wantIn are substrings the failure must surface (the operator reads this
		// out of the resource's last_error, so it has to carry the real reason).
		wantIn []string
	}{
		{
			name:  "authentication challenge means pg_hba admitted the connection",
			reply: func(c net.Conn) { _, _ = c.Write(authenticationSASL()) },
		},
		{
			name:  "authentication ok also means admitted",
			reply: func(c net.Conn) { _, _ = c.Write(authenticationOk()) },
		},
		{
			name:    "pg_hba rejection fails with the server's own words",
			reply:   func(c net.Conn) { _, _ = c.Write(hbaRejection()) },
			wantErr: true,
			wantIn:  []string{"no pg_hba.conf entry", "28000", "FATAL"},
		},
		{
			name:    "an error response with no fields still fails",
			reply:   func(c net.Conn) { _, _ = c.Write(errorResponse()) },
			wantErr: true,
			wantIn:  []string{"refused the connection before authentication"},
		},
		{
			name:    "a garbage first byte fails rather than passing",
			reply:   func(c net.Conn) { _, _ = c.Write([]byte{0xff, 0x00, 0x00, 0x00, 0x08, 1, 2, 3, 4}) },
			wantErr: true,
			wantIn:  []string{"0xff", "neither an authentication request nor an error"},
		},
		{
			name:    "accepted then closed with no reply fails",
			reply:   nil,
			wantErr: true,
			wantIn:  []string{"read startup response"},
		},
		{
			name: "a truncated authentication frame fails (fail-closed on a short read)",
			reply: func(c net.Conn) {
				_, _ = c.Write([]byte{'R', 0x00}) // type byte plus half a length prefix
			},
			wantErr: true,
			wantIn:  []string{"read startup response"},
		},
		{
			name: "an error response whose body never arrives fails",
			reply: func(c net.Conn) {
				_, _ = c.Write([]byte{'E', 0x00, 0x00, 0x00, 0x20}) // header promising 28 body bytes
			},
			wantErr: true,
			wantIn:  []string{"ErrorResponse could not be read"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := startFakePostgres(t, tt.reply)
			host, port := f.addr(t)

			err := probePostgresReady(context.Background(), host, port)
			if tt.wantErr && err == nil {
				t.Fatal("probe passed; a probe that cannot confirm the engine admits clients MUST fail")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("probe failed on an admitted connection: %v", err)
			}
			for _, want := range tt.wantIn {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("failure %q does not surface %q", err, want)
				}
			}

			startup, after := f.received()
			assertStartupMessage(t, startup)
			if len(after) != 0 {
				t.Fatalf("the probe wrote %d byte(s) after the startup packet (%q); it must never try to authenticate", len(after), after)
			}
		})
	}
}

// assertStartupMessage checks the probe speaks a well-formed protocol 3.0
// StartupMessage: self-counting length prefix, protocol 196608, null-terminated
// user + database parameters, and a final zero byte.
func assertStartupMessage(t *testing.T, msg []byte) {
	t.Helper()
	if len(msg) < 9 {
		t.Fatalf("startup message is %d bytes, too short to be one", len(msg))
	}
	if n := binary.BigEndian.Uint32(msg[0:4]); int(n) != len(msg) {
		t.Fatalf("startup length prefix = %d, message is %d bytes (the prefix counts itself)", n, len(msg))
	}
	if v := binary.BigEndian.Uint32(msg[4:8]); v != pgProtocolVersion3 {
		t.Fatalf("protocol version = %d, want %d (3.0)", v, pgProtocolVersion3)
	}
	if msg[len(msg)-1] != 0 {
		t.Fatal("the parameter list must be terminated by a zero byte")
	}
	// The parameter region ends with the last value's own NUL, then the list
	// terminator that was checked above; trim both before splitting.
	params := strings.Split(strings.TrimSuffix(string(msg[8:len(msg)-1]), "\x00"), "\x00")
	if len(params)%2 != 0 {
		t.Fatalf("parameters are not key/value pairs: %q", params)
	}
	got := map[string]string{}
	for i := 0; i < len(params); i += 2 {
		got[params[i]] = params[i+1]
	}
	if got["user"] == "" {
		t.Fatalf("startup message carries no user parameter: %q", params)
	}
	if got["database"] == "" {
		t.Fatalf("startup message carries no database parameter: %q", params)
	}
}

// Nothing listening at all is a failure, not a pass.
func TestProbePostgresReady_NothingListening(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	_, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse port: %v", err)
	}
	if err := ln.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}

	if err := probePostgresReady(context.Background(), "127.0.0.1", port); err == nil {
		t.Fatal("probe passed against a closed port")
	} else if !strings.Contains(err.Error(), "dial postgres") {
		t.Fatalf("failure %q does not say the dial failed", err)
	}
}

// An already-cancelled context fails immediately rather than passing.
func TestProbePostgresReady_CancelledContext(t *testing.T) {
	f := startFakePostgres(t, func(c net.Conn) { _, _ = c.Write(authenticationOk()) })
	host, port := f.addr(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := probePostgresReady(ctx, host, port); err == nil {
		t.Fatal("probe passed on a cancelled context")
	}
}

// The exact bytes on the wire, pinned: a change here changes what every Postgres
// on the fleet is asked, so it must be deliberate.
func TestPgStartupMessage_ExactBytes(t *testing.T) {
	got := pgStartupMessage("postgres", "postgres")
	want := []byte{
		0x00, 0x00, 0x00, 0x29, // length 41, counting itself
		0x00, 0x03, 0x00, 0x00, // protocol 3.0 (196608)
	}
	want = append(want, "user\x00postgres\x00database\x00postgres\x00"...)
	want = append(want, 0) // end of parameters
	if string(got) != string(want) {
		t.Fatalf("startup message =\n %x\nwant\n %x", got, want)
	}
}
