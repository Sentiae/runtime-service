package usecase

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// ─────────────────────────────────────────────────────────────────────
// A path-keyed in-memory ArtifactStore. Deliberately NOT FilesystemStore: these
// keys are paths (volumes/<vol>/<snap>.ext4), not digests, so a content-addressed
// store would refuse every Put — the same mismatch that once made the local
// snapshot cache silently hold nothing.
//
// It records what it was asked to do, so a test can prove the mirror never touched
// the second store beyond the object it named.
// ─────────────────────────────────────────────────────────────────────

type memStore struct {
	mu      sync.Mutex
	objects map[string][]byte
	gets    []string
	puts    []string

	getErr  error
	putErr  error
	// truncateTo, when > 0, stores only that many bytes of the incoming stream —
	// the shape of an upload the far end did not fully keep. It is what makes the
	// confirm step's checksum the load-bearing check rather than decoration.
	truncateTo int
}

func newMemStore() *memStore { return &memStore{objects: map[string][]byte{}} }

func (m *memStore) Put(key string, r io.Reader) error {
	m.mu.Lock()
	putErr, truncate := m.putErr, m.truncateTo
	m.puts = append(m.puts, key)
	m.mu.Unlock()
	if putErr != nil {
		return putErr
	}
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		return err
	}
	body := buf.Bytes()
	if truncate > 0 && truncate < len(body) {
		body = body[:truncate]
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[key] = body
	return nil
}

func (m *memStore) Get(key string) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gets = append(m.gets, key)
	if m.getErr != nil {
		return nil, m.getErr
	}
	b, ok := m.objects[key]
	if !ok {
		return nil, ErrArtifactNotFound
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (m *memStore) Exists(key string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.objects[key]
	return ok, nil
}

func (m *memStore) VerifyHash(string) error { return nil }

func (m *memStore) seed(key string, body []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[key] = body
}

func (m *memStore) body(key string) []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.objects[key]
}

func (m *memStore) putKeys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.puts...)
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

const mirrorKey = "volumes/vol-1/snap-1.ext4"

// ─────────────────────────────────────────────────────────────────────
// The mirror
// ─────────────────────────────────────────────────────────────────────

// TestArtifactStoreMirror is table-driven over every way the second copy can go
// wrong. The single invariant being proven is that a receipt is issued ONLY for a
// copy that was written AND read back AND hashed to the recorded checksum — because
// the ledger promotes a row to "two failure domains" on nothing but that receipt.
func TestArtifactStoreMirror(t *testing.T) {
	blob := []byte("gzip-compressed-recovery-point-bytes")
	good := sha256Hex(blob)

	tests := []struct {
		name string
		// setup prepares the two stores and returns the checksum to verify against.
		setup   func(primary, secondary *memStore) string
		wantErr error
		// wantErrContains is used where the failure has no sentinel (a store's own
		// error is wrapped, not classified).
		wantErrContains string
		wantCopied      bool
	}{
		{
			name: "confirmed copy",
			setup: func(p, _ *memStore) string {
				p.seed(mirrorKey, blob)
				return good
			},
			wantCopied: true,
		},
		{
			name: "the primary store cannot serve the blob it already holds",
			setup: func(p, _ *memStore) string {
				// Nothing seeded: the copy this mirror was asked to protect is unreadable
				// where it already lives, which is the MORE alarming of the two findings
				// and must not be reported as a mirror problem alone.
				return good
			},
			wantErrContains: "unreadable",
		},
		{
			name: "the second domain refuses the write",
			setup: func(p, s *memStore) string {
				p.seed(mirrorKey, blob)
				s.putErr = errors.New("403 AccessDenied")
				return good
			},
			wantErrContains: "403 AccessDenied",
		},
		{
			name: "the second domain accepted the write but cannot serve it back",
			setup: func(p, s *memStore) string {
				p.seed(mirrorKey, blob)
				// Put succeeds, the confirming Get fails: a write that was accepted is NOT
				// a copy that exists.
				s.getErr = errors.New("500 InternalError")
				return good
			},
			wantErrContains: "500 InternalError",
		},
		{
			name: "the second domain kept only part of the blob",
			setup: func(p, s *memStore) string {
				p.seed(mirrorKey, blob)
				s.truncateTo = 4
				return good
			},
			wantErr: ErrSecondDomainChecksumMismatch,
		},
		{
			name: "the recovery point carries no checksum to verify against",
			setup: func(p, _ *memStore) string {
				p.seed(mirrorKey, blob)
				return ""
			},
			wantErr: ErrSecondDomainNoChecksum,
		},
		{
			name: "the recovery point's checksum describes different bytes",
			setup: func(p, _ *memStore) string {
				p.seed(mirrorKey, blob)
				return sha256Hex([]byte("some other snapshot entirely"))
			},
			wantErr: ErrSecondDomainChecksumMismatch,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			primary, secondary := newMemStore(), newMemStore()
			checksum := tt.setup(primary, secondary)
			m, err := NewArtifactStoreMirror(primary, secondary, "r2:test-bucket")
			if err != nil {
				t.Fatalf("build mirror: %v", err)
			}

			receipt, err := m.Mirror(context.Background(), mirrorKey, checksum)

			switch {
			case tt.wantErr != nil:
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
			case tt.wantErrContains != "":
				if err == nil || !strings.Contains(err.Error(), tt.wantErrContains) {
					t.Fatalf("err = %v, want it to mention %q", err, tt.wantErrContains)
				}
			default:
				if err != nil {
					t.Fatalf("Mirror: %v", err)
				}
			}

			if !tt.wantCopied {
				// ⚠ The whole point: no receipt on any failure. A receipt is what the
				// ledger promotes a row on, so a receipt returned alongside an error would
				// mint a two-domain claim out of a failed copy.
				if receipt != (SecondDomainReceipt{}) {
					t.Fatalf("a FAILED mirror returned a receipt %+v — the ledger would then claim two failure domains for a copy that does not exist", receipt)
				}
				return
			}

			if !bytes.Equal(secondary.body(mirrorKey), blob) {
				t.Errorf("second-domain body = %q, want the primary's bytes verbatim %q", secondary.body(mirrorKey), blob)
			}
			if receipt.Checksum != good {
				t.Errorf("receipt checksum = %q, want %q", receipt.Checksum, good)
			}
			if receipt.Bytes != int64(len(blob)) {
				t.Errorf("receipt verified %d bytes, want %d", receipt.Bytes, len(blob))
			}
			if receipt.Domain != "r2:test-bucket" {
				t.Errorf("receipt domain = %q", receipt.Domain)
			}
			if receipt.At.IsZero() {
				t.Error("receipt carries no time; the ledger stamps second_domain_at from it")
			}
		})
	}
}

// TestArtifactStoreMirrorTouchesOnlyTheNamedObject pins the D-199 capability
// constraint at the level a test can actually reach: the mirror addresses the
// second store by KEY and nothing else. The credential grants object read/write and
// NOT bucket listing (LIST returns 403, verified live), so a mirror that reached for
// the bucket rather than the object would fail in production and pass here — hence
// this asserts the access pattern, and TestArtifactStoreHasNoListOperation asserts
// that a listing call is not even expressible.
func TestArtifactStoreMirrorTouchesOnlyTheNamedObject(t *testing.T) {
	blob := []byte("recovery point")
	primary, secondary := newMemStore(), newMemStore()
	primary.seed(mirrorKey, blob)

	m, err := NewArtifactStoreMirror(primary, secondary, "r2:test-bucket")
	if err != nil {
		t.Fatalf("build mirror: %v", err)
	}
	if _, err := m.Mirror(context.Background(), mirrorKey, sha256Hex(blob)); err != nil {
		t.Fatalf("Mirror: %v", err)
	}

	if got := secondary.putKeys(); len(got) != 1 || got[0] != mirrorKey {
		t.Errorf("second-domain writes = %v, want exactly [%q]", got, mirrorKey)
	}
	for _, k := range secondary.gets {
		if k != mirrorKey {
			t.Errorf("second-domain read key %q; the mirror must only ever address the object it is copying", k)
		}
	}
}

// TestArtifactStoreHasNoListOperation is the structural version of the D-199
// constraint: the second-domain credential cannot enumerate the bucket, so no code
// path may try. Asserting it against the PORT rather than against a call site is
// what makes it durable — the day someone adds a List/ListObjects/Enumerate method
// to ArtifactStore, this fails and the review happens then, before a 403 does the
// explaining in production.
func TestArtifactStoreHasNoListOperation(t *testing.T) {
	typ := reflect.TypeOf((*ArtifactStore)(nil)).Elem()
	for i := 0; i < typ.NumMethod(); i++ {
		name := typ.Method(i).Name
		for _, forbidden := range []string{"List", "Enumerate", "Walk", "Keys"} {
			if strings.Contains(name, forbidden) {
				t.Errorf("ArtifactStore.%s is an enumeration; the D-199 second-domain credential grants object access only (LIST returns 403) and the LEDGER is the source of truth for what exists off-chassis", name)
			}
		}
	}
}

// TestArtifactStoreMirrorRequiresBothStores proves a mirror cannot be built into a
// state where it looks wired and copies nowhere. A host with no second domain must
// hold a NIL mirror, which stamps recovery points primary_only honestly.
func TestArtifactStoreMirrorRequiresBothStores(t *testing.T) {
	tests := []struct {
		name      string
		primary   ArtifactStore
		secondary ArtifactStore
		domain    string
	}{
		{"no primary", nil, newMemStore(), "r2:b"},
		{"no secondary", newMemStore(), nil, "r2:b"},
		{"unnamed domain", newMemStore(), newMemStore(), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewArtifactStoreMirror(tt.primary, tt.secondary, tt.domain); err == nil {
				t.Fatal("NewArtifactStoreMirror accepted an unusable wiring; it must refuse so the caller holds a nil mirror instead of a broken one")
			}
		})
	}
}

// TestArtifactStoreMirrorHonoursCancellation proves a caller that gave up stops
// paying for the WAN transfer. ArtifactStore.Put takes no context, so this only
// works because the reader handed to it is context-aware.
func TestArtifactStoreMirrorHonoursCancellation(t *testing.T) {
	primary, secondary := newMemStore(), newMemStore()
	primary.seed(mirrorKey, []byte("recovery point"))
	m, err := NewArtifactStoreMirror(primary, secondary, "r2:test-bucket")
	if err != nil {
		t.Fatalf("build mirror: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := m.Mirror(ctx, mirrorKey, sha256Hex([]byte("recovery point"))); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
