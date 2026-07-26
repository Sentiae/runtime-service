package usecase

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"
)

// ─────────────────────────────────────────────────────────────────────
// The second failure domain (D-192 / D-195 / D-199).
//
// A recovery point that exists only in the MinIO container running on the fleet
// host's own chassis is one machine's arithmetic wearing a durability promise. The
// mirror copies an ALREADY-STORED blob into a store in a DIFFERENT failure domain
// (Cloudflare R2, 30-day object lock over all prefixes) and CONFIRMS it, so the
// ledger can record two domains as a fact instead of a configuration.
//
// ⚠ NO CODE PATH HERE MAY ENUMERATE THE SECOND STORE. The D-199 credential grants
// object read/write on `sentiae-recovery-points` and NOT bucket listing — a LIST
// returns 403, verified live. Every access below is by KEY (Get / Put / Exists,
// which is a HEAD). The LEDGER is the source of truth for what exists off-chassis;
// the bucket is not queryable for it, and a future change that adds a listing call
// will fail closed at runtime rather than at review.
//
// ⚠ IT IS ALSO NOT A DELETE PATH, and cannot become one: the object lock refuses
// deletion for 30 days regardless of credential (proven live —
// ObjectLockedByBucketPolicy). Anything written there is permanent for that window.
// ─────────────────────────────────────────────────────────────────────

// ErrSecondDomainChecksumMismatch is returned when the bytes read back out of the
// second domain do not hash to the checksum the recovery point recorded. It is the
// one outcome that must never be treated as a successful mirror: a corrupt second
// copy recorded as a good one is worse than no second copy, because it converts an
// alarming state into a reassuring one.
var ErrSecondDomainChecksumMismatch = errors.New("second-domain copy: checksum mismatch")

// ErrSecondDomainNoChecksum is returned when a recovery point carries no checksum
// to verify the copy against. Pre-D-184 rows are in this state. The blob is still
// copyable, but the copy could not be PROVEN, and this mirror refuses to stamp a
// two-domain claim it did not verify.
var ErrSecondDomainNoChecksum = errors.New("second-domain copy: recovery point has no checksum to verify against")

// SecondDomainReceipt is the evidence a confirmed mirror produces. It exists so
// the ledger writes facts the mirror actually established, not the ones the caller
// hoped for.
type SecondDomainReceipt struct {
	// Domain identifies the store that now holds the copy, as recorded on the row.
	Domain string
	// Bytes is how many bytes were read back out of the second domain and hashed —
	// i.e. what the verification actually covered.
	Bytes int64
	// Checksum is the hex sha256 of those bytes. Equal to the recovery point's
	// checksum by construction (a mismatch is an error, never a receipt).
	Checksum string
	// At is when the verification completed.
	At time.Time
}

// SecondDomainMirror copies a stored recovery-point blob into a second failure
// domain and confirms it. It is a port: the snapshotter depends on it, and it is
// nil on a host with no second domain configured — in which case recovery points
// are recorded primary_only, which is the truth rather than a degraded mode.
type SecondDomainMirror interface {
	// Domain names the second failure domain (recorded on the row).
	Domain() string
	// Mirror copies objectKey out of the primary store into the second domain and
	// verifies the stored copy hashes to expectChecksum. It returns a receipt ONLY
	// when the copy is confirmed; every other outcome is an error and must leave the
	// ledger claiming one domain.
	Mirror(ctx context.Context, objectKey, expectChecksum string) (SecondDomainReceipt, error)
}

// ArtifactStoreMirror implements SecondDomainMirror over two ArtifactStores.
//
// It reads the blob back OUT of the primary store rather than re-reading the
// source volume, which is deliberate and buys three things: the guest is never
// frozen for the second (WAN) leg; the bytes copied are provably the bytes the
// primary holds; and the read-back exercises the primary copy's retrievability,
// which nothing else does until a restore.
//
// The blob is copied VERBATIM — it is already gzip-compressed by the upload path
// and the recovery point's checksum is over exactly those stored bytes (see
// snapshotUpload.Checksum), so re-compressing or re-framing here would make the
// recorded checksum unverifiable against the second copy.
type ArtifactStoreMirror struct {
	primary   ArtifactStore
	secondary ArtifactStore
	domain    string
	now       func() time.Time
}

var _ SecondDomainMirror = (*ArtifactStoreMirror)(nil)

// NewArtifactStoreMirror wires a mirror from primary into secondary. All three
// arguments are required: a nil store or an unnamed domain is a wiring error, not
// a degraded mode — a caller that has no second domain must hold a nil
// SecondDomainMirror so the ledger records primary_only honestly, rather than a
// mirror that silently copies nowhere.
func NewArtifactStoreMirror(primary, secondary ArtifactStore, domain string) (*ArtifactStoreMirror, error) {
	if primary == nil || secondary == nil {
		return nil, errors.New("second-domain mirror: both the primary and the second-domain store are required")
	}
	if domain == "" {
		return nil, errors.New("second-domain mirror: the second domain must be named (it is recorded on every row it protects)")
	}
	return &ArtifactStoreMirror{
		primary:   primary,
		secondary: secondary,
		domain:    domain,
		now:       func() time.Time { return time.Now().UTC() },
	}, nil
}

// Domain names the second failure domain.
func (m *ArtifactStoreMirror) Domain() string { return m.domain }

// Mirror copies and then CONFIRMS. The two steps are separate on purpose: a
// successful Put means the store accepted the stream, whereas the two-domain claim
// is about what can be READ BACK, and those differ under a truncated upload, a
// silently-dropped multipart part, or a bucket that accepted bytes it did not
// durably keep. Only the read-back's hash earns the receipt.
func (m *ArtifactStoreMirror) Mirror(ctx context.Context, objectKey, expectChecksum string) (SecondDomainReceipt, error) {
	var zero SecondDomainReceipt
	if objectKey == "" {
		return zero, errors.New("second-domain mirror: object key is required")
	}
	if expectChecksum == "" {
		// Refused rather than copied-and-hoped: without a checksum this mirror cannot
		// distinguish a good copy from a truncated one, and stamping the two-domain
		// class on an unverifiable copy is the exact fail-open the class exists to
		// close.
		return zero, fmt.Errorf("%w: %s", ErrSecondDomainNoChecksum, objectKey)
	}
	if err := ctx.Err(); err != nil {
		return zero, fmt.Errorf("second-domain mirror %s: %w", objectKey, err)
	}

	if err := m.copy(ctx, objectKey); err != nil {
		return zero, err
	}
	return m.confirm(ctx, objectKey, expectChecksum)
}

// copy streams the blob from the primary store into the second domain.
//
// ctxReader is what makes a cancelled mirror an actual exit path: ArtifactStore.Put
// takes no context (the interface is deliberately context-free — see
// artifact_store.go), so without it a caller that gave up would still pay for the
// whole WAN transfer.
func (m *ArtifactStoreMirror) copy(ctx context.Context, objectKey string) error {
	src, err := m.primary.Get(objectKey)
	if err != nil {
		// A blob the PRIMARY store cannot serve is the more alarming finding of the
		// two, so it is named as such: the mirror did not fail, the copy it was asked
		// to protect is unreadable where it already lives.
		return fmt.Errorf("second-domain mirror: read %s back out of the primary store (the copy that already exists is unreadable): %w", objectKey, err)
	}
	defer src.Close()

	if err := m.secondary.Put(objectKey, &ctxReader{ctx: ctx, r: src}); err != nil {
		return fmt.Errorf("second-domain mirror: put %s into %s: %w", objectKey, m.domain, err)
	}
	return nil
}

// confirm reads the second copy back BY KEY and hashes it. Never a listing (see
// the package note): the key is known, so the weaker, permitted access is also the
// sufficient one.
func (m *ArtifactStoreMirror) confirm(ctx context.Context, objectKey, expectChecksum string) (SecondDomainReceipt, error) {
	var zero SecondDomainReceipt
	rc, err := m.secondary.Get(objectKey)
	if err != nil {
		return zero, fmt.Errorf("second-domain mirror: read %s back out of %s to confirm it: %w", objectKey, m.domain, err)
	}
	defer rc.Close()

	h := sha256.New()
	counted := &countingReader{r: &ctxReader{ctx: ctx, r: rc}}
	if _, err := io.Copy(h, counted); err != nil {
		return zero, fmt.Errorf("second-domain mirror: hash %s in %s: %w", objectKey, m.domain, err)
	}
	actual := hex.EncodeToString(h.Sum(nil))
	if actual != expectChecksum {
		return zero, fmt.Errorf("%w: %s in %s: declared=%s actual=%s",
			ErrSecondDomainChecksumMismatch, objectKey, m.domain, expectChecksum, actual)
	}
	return SecondDomainReceipt{
		Domain:   m.domain,
		Bytes:    counted.n,
		Checksum: actual,
		At:       m.now(),
	}, nil
}
