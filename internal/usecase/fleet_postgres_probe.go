package usecase

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

// ─────────────────────────────────────────────────────────────────────
// Credential-free Postgres readiness probe (#p19-restore-false-green-health).
//
// dialTCP proves only that SOMETHING is listening: it opens a TCP connection and
// closes it. A restored Postgres whose pg_hba.conf came back torn listens and
// then refuses every client with `FATAL: no pg_hba.conf entry for host ...` —
// which dialTCP reads as healthy. That false green shipped twice over customer
// data, so the restore path needs a probe that can tell "admitted" from
// "rejected".
//
// It can, with NO credentials, because Postgres evaluates pg_hba.conf BEFORE
// authentication: the server matches the connection's host/user/database against
// the HBA file first and only then decides what to ask for. So the FIRST message
// after a StartupMessage already carries the verdict — an AuthenticationRequest
// ('R', the auth challenge or AuthenticationOk) means the connection was
// admitted and reached authentication; an ErrorResponse ('E') means it was
// rejected before that. The probe stops exactly there. It never sends a password
// and never completes authentication — it holds no credentials by design and
// needs none, which is precisely what makes it safe to run on every restore.
// ─────────────────────────────────────────────────────────────────────

const (
	// postgresProbeTimeout bounds the WHOLE probe — dial, startup write and first
	// response — in the same spirit as dialTCP's 2s.
	postgresProbeTimeout = 2 * time.Second
	// pgProtocolVersion3 is the frontend/backend protocol number (3.0) sent in the
	// StartupMessage: major 3 in the high 16 bits, minor 0 in the low 16.
	pgProtocolVersion3 uint32 = 196608
	// pgMsgAuthenticationRequest ('R') is the first backend message on an ADMITTED
	// connection, whether it carries a challenge or AuthenticationOk.
	pgMsgAuthenticationRequest byte = 'R'
	// pgMsgErrorResponse ('E') is the first backend message on a REJECTED one.
	pgMsgErrorResponse byte = 'E'
	// pgErrorResponseMax caps how much of an ErrorResponse body is read. The probe
	// is talking to something that has not been authenticated as Postgres yet, so
	// the length prefix is untrusted input and must never size an unbounded read.
	pgErrorResponseMax = 8 << 10
	// pgProbeUser / pgProbeDatabase are the identities the StartupMessage carries.
	// They are matched against pg_hba.conf and never authenticated, so any values
	// work; `postgres` is used because it is the identity every default HBA rule
	// covers, which keeps a rejection meaning "the HBA file is broken" rather than
	// "the probe named something unusual".
	pgProbeUser     = "postgres"
	pgProbeDatabase = "postgres"
)

// probePostgresReady reports whether the Postgres reachable at host:port ADMITS
// a client connection — not merely that something accepts TCP there. A nil
// return means the server answered the startup handshake with an authentication
// request, i.e. it is up, has read its pg_hba.conf, and lets this client through
// to authentication.
//
// It is FAIL-CLOSED in every direction: a dial failure, a write failure, a
// truncated or unreadable response, an unexpected message type, a timeout or a
// cancelled context all return an error. A readiness probe that passes when it
// cannot tell is the exact defect this replaces.
func probePostgresReady(ctx context.Context, host string, port int) error {
	ctx, cancel := context.WithTimeout(ctx, postgresProbeTimeout)
	defer cancel()

	addr := net.JoinHostPort(host, strconv.Itoa(port))
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("dial postgres at %s: %w", addr, err)
	}
	defer conn.Close()
	// One deadline for write+read: the context timeout above only covers the dial.
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return fmt.Errorf("set probe deadline on %s: %w", addr, err)
		}
	}

	if _, err := conn.Write(pgStartupMessage(pgProbeUser, pgProbeDatabase)); err != nil {
		return fmt.Errorf("write startup message to %s: %w", addr, err)
	}
	// Nothing is ever written after the startup packet — see the file comment.

	var header [5]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		return fmt.Errorf("read startup response from %s: %w", addr, err)
	}
	// The length prefix counts itself but not the leading type byte.
	bodyLen := int64(binary.BigEndian.Uint32(header[1:])) - 4

	switch header[0] {
	case pgMsgAuthenticationRequest:
		return nil
	case pgMsgErrorResponse:
		return pgErrorResponse(conn, addr, bodyLen)
	default:
		return fmt.Errorf("postgres at %s answered the startup message with message type 0x%02x, which is neither an authentication request nor an error — the endpoint cannot be confirmed usable", addr, header[0])
	}
}

// pgStartupMessage builds a protocol 3.0 StartupMessage. It is the one frontend
// message with NO leading type byte: int32 total length (counting itself), int32
// protocol version, then null-terminated key/value pairs, closed by a final zero
// byte.
func pgStartupMessage(user, database string) []byte {
	params := make([]byte, 0, 64)
	appendParam := func(key, value string) {
		params = append(params, key...)
		params = append(params, 0)
		params = append(params, value...)
		params = append(params, 0)
	}
	appendParam("user", user)
	appendParam("database", database)
	params = append(params, 0) // end of the parameter list

	msg := make([]byte, 8, 8+len(params))
	binary.BigEndian.PutUint32(msg[0:4], uint32(8+len(params)))
	binary.BigEndian.PutUint32(msg[4:8], pgProtocolVersion3)
	return append(msg, params...)
}

// pgErrorResponse reads an ErrorResponse body and renders it as the probe's
// failure. The body is a sequence of (1-byte field code, null-terminated value)
// pairs closed by a zero byte; the fields that matter to an operator are the
// severity ('S'), the SQLSTATE ('C') and the message ('M') — a torn pg_hba.conf
// surfaces here as 28000 / "no pg_hba.conf entry for host ...", which is the
// whole reason this text is propagated rather than flattened to "not ready".
func pgErrorResponse(r io.Reader, addr string, bodyLen int64) error {
	if bodyLen < 0 || bodyLen > pgErrorResponseMax {
		return fmt.Errorf("postgres at %s rejected the connection with an ErrorResponse of implausible length %d", addr, bodyLen)
	}
	body := make([]byte, bodyLen)
	if _, err := io.ReadFull(r, body); err != nil {
		return fmt.Errorf("postgres at %s rejected the connection but its ErrorResponse could not be read: %w", addr, err)
	}

	var severity, sqlstate, message string
	for len(body) > 0 && body[0] != 0 {
		code := body[0]
		body = body[1:]
		end := bytes.IndexByte(body, 0)
		if end < 0 {
			break // truncated field: keep whatever was already parsed
		}
		value := string(body[:end])
		body = body[end+1:]
		switch code {
		case 'S':
			severity = value
		case 'C':
			sqlstate = value
		case 'M':
			message = value
		}
	}
	return fmt.Errorf("postgres at %s refused the connection before authentication: %s", addr, pgErrorText(severity, sqlstate, message))
}

// pgErrorText renders the parsed ErrorResponse fields, degrading gracefully when
// the server sent fewer than expected.
func pgErrorText(severity, sqlstate, message string) string {
	parts := make([]string, 0, 3)
	if severity != "" {
		parts = append(parts, severity)
	}
	if sqlstate != "" {
		parts = append(parts, "SQLSTATE "+sqlstate)
	}
	if message != "" {
		parts = append(parts, message)
	}
	if len(parts) == 0 {
		return "the ErrorResponse carried no severity, SQLSTATE or message"
	}
	return strings.Join(parts, ": ")
}
