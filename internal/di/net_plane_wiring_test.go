//go:build unit

package di

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/usecase"
)

// recordingBooter stands in for the real Firecracker booter.
type recordingBooter struct{ teardowns int }

var _ usecase.ImageBooter = (*recordingBooter)(nil)

func (b *recordingBooter) BootTest(context.Context, usecase.ImageBootInput) (usecase.ImageTestResult, error) {
	return usecase.ImageTestResult{}, nil
}
func (b *recordingBooter) BootResident(context.Context, usecase.ImageBootInput) (usecase.ImageResidentResult, error) {
	return usecase.ImageResidentResult{}, nil
}
func (b *recordingBooter) Decommission(context.Context, usecase.ImageDecommissionInput) error {
	b.teardowns++
	return nil
}

// TestPublishImageBooterFailsClosedOnAnUnreconciledPlane pins the wiring rule that
// makes the whole plane safe: if the boot-time reconcile could not prove which
// addresses this host holds, the fleet's booter seam REFUSES every boot.
//
// The negative control matters as much as the positive one. The defect this
// replaces logged its startup failure and carried on with an empty picture of what
// was allocated — so "an error was returned" is not the property under test;
// "the published booter refuses boots" is.
func TestPublishImageBooterFailsClosedOnAnUnreconciledPlane(t *testing.T) {
	real := &recordingBooter{}
	planeErr := errors.New("list leases held on host: connection refused")

	published := publishImageBooter(real, planeErr)

	if _, err := published.BootResident(context.Background(), usecase.ImageBootInput{}); !errors.Is(err, domain.ErrNetPlaneUnreconciled) {
		t.Fatalf("BootResident on an unreconciled plane = %v, want ErrNetPlaneUnreconciled", err)
	}
	if _, err := published.BootTest(context.Background(), usecase.ImageBootInput{}); !errors.Is(err, domain.ErrNetPlaneUnreconciled) {
		t.Fatalf("BootTest on an unreconciled plane = %v, want ErrNetPlaneUnreconciled", err)
	}
	// The refusal must carry the underlying cause; an operator cannot act on
	// "unreconciled" alone.
	_, err := published.BootResident(context.Background(), usecase.ImageBootInput{})
	if !errors.Is(err, domain.ErrNetPlaneUnreconciled) || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("refusal does not name the cause: %v", err)
	}
	// Teardown still works: a customer's running VM must remain releasable.
	if terr := published.Decommission(context.Background(), usecase.ImageDecommissionInput{}); terr != nil {
		t.Fatalf("Decommission through the fail-closed seam: %v", terr)
	}
	if real.teardowns != 1 {
		t.Fatalf("teardown delegations = %d, want 1", real.teardowns)
	}
}

// A reconciled plane publishes the real booter unchanged — no wrapper, no
// behaviour change on the healthy path.
func TestPublishImageBooterPassesTheRealBooterThroughWhenReconciled(t *testing.T) {
	real := &recordingBooter{}
	if got := publishImageBooter(real, nil); got != usecase.ImageBooter(real) {
		t.Fatalf("published booter = %T, want the real booter itself", got)
	}
}
