//go:build unit

package di

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

// A fleet host's identity is minted at host birth and pinned in its env file. It
// is never derived from a routable address — the derivation that used to be the
// fallback here made a re-IP'd host mint a SECOND identity (orphaning every
// volume pinned to the first) and made two hosts sharing an advertise address
// collide onto ONE row. So an absent or unparseable value is a fatal
// misconfiguration that must refuse boot, and the message has to name the env var
// an operator has to go fix.
func TestResolveFleetHostID(t *testing.T) {
	valid := uuid.New()
	tests := []struct {
		name    string
		raw     string
		want    uuid.UUID
		wantErr bool
	}{
		{name: "empty refuses", raw: "", wantErr: true},
		{name: "whitespace refuses", raw: "   ", wantErr: true},
		{name: "garbage refuses", raw: "not-a-uuid", wantErr: true},
		{name: "nil uuid refuses", raw: uuid.Nil.String(), wantErr: true},
		{name: "valid uuid used verbatim", raw: valid.String(), want: valid},
		{name: "uppercase uuid used verbatim", raw: strings.ToUpper(valid.String()), want: valid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveFleetHostID(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("resolveFleetHostID(%q) = %v, want an error", tt.raw, got)
				}
				// The error is the operator's only instruction: it must name the knob.
				if !strings.Contains(err.Error(), "APP_FLEET_HOST_ID") {
					t.Fatalf("error must name APP_FLEET_HOST_ID, got: %v", err)
				}
				if got != uuid.Nil {
					t.Fatalf("a refused id must return the nil uuid, got %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveFleetHostID(%q): %v", tt.raw, err)
			}
			if got != tt.want {
				t.Fatalf("resolveFleetHostID(%q) = %v, want %v (the configured id must be used verbatim, never re-derived)", tt.raw, got, tt.want)
			}
		})
	}
}
