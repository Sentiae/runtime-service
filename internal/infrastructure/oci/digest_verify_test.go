package oci

import (
	"errors"
	"testing"
)

// TestVerifyDigest covers the content-trust helper directly: a by-digest fetch
// whose bytes hash to the requested address passes; tampered/corrupt bytes fail
// closed with ErrDigestMismatch; a non-digest reference (a tag) has nothing to
// verify and is skipped.
func TestVerifyDigest(t *testing.T) {
	content := []byte("the-real-layer-bytes")
	good := digestOf(content)

	tests := []struct {
		name      string
		requested string
		content   []byte
		wantErr   error
	}{
		{"matching digest passes", good, content, nil},
		{"tampered content fails closed", good, []byte("mitm-swapped-bytes"), ErrDigestMismatch},
		{"corrupt truncated content fails closed", good, content[:5], ErrDigestMismatch},
		{"tag reference is not verified", "latest", content, nil},
		{"empty reference is not verified", "", content, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := verifyDigest(tt.requested, tt.content)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("verifyDigest(%q) = %v, want errors.Is(_, %v)", tt.requested, err, tt.wantErr)
			}
		})
	}
}
