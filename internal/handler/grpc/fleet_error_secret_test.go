package grpc

import (
	"errors"
	"fmt"
	"testing"

	"github.com/sentiae/platform-kit/secret"
	"github.com/sentiae/runtime-service/internal/app"
	"github.com/sentiae/runtime-service/internal/domain"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestFleetErrorMapsSecretSentinels is the T-HARDEN Wave 0 guard
// (#secret-resolve-no-sentinel, D-162b).
//
// The defect it pins: fleetError hand-mapped ~17 runtime-local errors with
// precision and NONE of platform-kit/secret's, so every secret failure —
// including a genuine CROSS-TENANT DENIAL — fell to the default and left this
// service as codes.Internal "internal server error", wire-identical to a
// nil-pointer panic. Behavior was always fail-closed (the VM never boots); what
// was broken was the ability to TELL the two apart.
//
// The errors are wrapped exactly as usecase.resolveBootSecrets wraps them
// (fmt.Errorf("resolve secret %q: %w", ref, err)), so this exercises the real
// %w chain from platform-kit through the usecase to the handler boundary.
func TestFleetErrorMapsSecretSentinels(t *testing.T) {
	app.RegisterErrors()

	const ref = "tenants/c883c1d0-249a-4262-bf9c-f4c30f0850b6/prod/app#db_password"

	tests := []struct {
		name string
		err  error
		want codes.Code
	}{
		{
			// THE POINT OF THIS FILE. A tenant reaching for another tenant's
			// secret is an authorization denial and must say so.
			name: "cross-tenant denial is PermissionDenied, never Internal",
			err:  secret.ErrCrossTenantSecret,
			want: codes.PermissionDenied,
		},
		{
			// Covers both a malformed ref and a non-tenant-namespaced one —
			// authorizeRef returns this sentinel for either.
			name: "unscoped ref is InvalidArgument",
			err:  secret.ErrUnscopedSecretRef,
			want: codes.InvalidArgument,
		},
		{
			name: "missing secret is NotFound",
			err:  secret.ErrSecretNotFound,
			want: codes.NotFound,
		},
		{
			name: "no vault client is FailedPrecondition",
			err:  secret.ErrVaultUnavailable,
			want: codes.FailedPrecondition,
		},
		{
			name: "no handed token is FailedPrecondition",
			err:  secret.ErrNoHandedToken,
			want: codes.FailedPrecondition,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wrapped := fmt.Errorf("resolve secret %q: %w", ref, tt.err)

			got := fleetError(wrapped)
			if status.Code(got) != tt.want {
				t.Fatalf("fleetError code = %v, want %v (err = %v)", status.Code(got), tt.want, got)
			}
			if status.Code(got) == codes.Internal {
				t.Fatalf("%v surfaced as Internal — indistinguishable from a crash", tt.err)
			}
			// The sentinel must survive the wrap; if this breaks, the mapping
			// above is passing for the wrong reason.
			if !errors.Is(wrapped, tt.err) {
				t.Fatalf("sentinel lost through the usecase wrap: %v", wrapped)
			}
		})
	}
}

// TestFleetErrorPreservesExistingMappings pins the blast-radius promise: routing
// the secret sentinels through the platform registry must not have disturbed any
// of the runtime-local errors fleetError already hand-mapped, nor the curated
// non-leaky default. An unregistered error must still yield a STATIC message —
// pkerrors.ToGRPC's own default would echo raw err.Error() to the caller.
func TestFleetErrorPreservesExistingMappings(t *testing.T) {
	app.RegisterErrors()

	tests := []struct {
		name string
		err  error
		want codes.Code
	}{
		{"workload not found", domain.ErrWorkloadNotFound, codes.NotFound},
		{"unsupported class", domain.ErrUnsupportedClass, codes.InvalidArgument},
		{"secret resolver unavailable", domain.ErrSecretResolverUnavailable, codes.FailedPrecondition},
		{"secret owner org missing", domain.ErrSecretOwnerOrgMissing, codes.InvalidArgument},
		{"fleet host not found", domain.ErrFleetHostNotFound, codes.NotFound},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := fleetError(tt.err); status.Code(got) != tt.want {
				t.Fatalf("fleetError code = %v, want %v", status.Code(got), tt.want)
			}
		})
	}

	t.Run("unregistered error stays Internal with a non-leaky static message", func(t *testing.T) {
		leaky := errors.New("pq: password authentication failed for user \"runtime\"")
		got := fleetError(leaky)
		if status.Code(got) != codes.Internal {
			t.Fatalf("code = %v, want Internal", status.Code(got))
		}
		if msg := status.Convert(got).Message(); msg != "internal server error" {
			t.Fatalf("message = %q, want the curated %q — internal error text must never reach a caller", msg, "internal server error")
		}
	})
}
