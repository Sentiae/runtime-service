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
type recordingBooter struct {
	boots     int
	teardowns int
}

var _ usecase.ImageBooter = (*recordingBooter)(nil)

func (b *recordingBooter) BootTest(context.Context, usecase.ImageBootInput) (usecase.ImageTestResult, error) {
	b.boots++
	return usecase.ImageTestResult{}, nil
}
func (b *recordingBooter) BootResident(context.Context, usecase.ImageBootInput) (usecase.ImageResidentResult, error) {
	b.boots++
	return usecase.ImageResidentResult{}, nil
}
func (b *recordingBooter) Decommission(context.Context, usecase.ImageDecommissionInput) error {
	b.teardowns++
	return nil
}

// TestPublishImageBooterFailsClosedOnAnUnreconciledPlane pins the wiring rule that
// makes the whole plane safe: while the plane cannot prove which addresses this
// host holds, the fleet's booter seam REFUSES every boot.
//
// The negative control matters as much as the positive one. The defect this
// replaces logged its startup failure and carried on with an empty picture of what
// was allocated — so "an error was returned" is not the property under test;
// "the published booter refuses boots" is.
func TestPublishImageBooterFailsClosedOnAnUnreconciledPlane(t *testing.T) {
	real := &recordingBooter{}
	planeErr := errors.New("list leases held on host: connection refused")
	verify := func(context.Context) error { return planeErr }

	published := publishImageBooter(real, verify, planeErr)

	if _, err := published.BootResident(context.Background(), usecase.ImageBootInput{}); err == nil {
		t.Fatal("BootResident on an unreconciled plane succeeded")
	}
	if _, err := published.BootTest(context.Background(), usecase.ImageBootInput{}); err == nil {
		t.Fatal("BootTest on an unreconciled plane succeeded")
	}
	if real.boots != 0 {
		t.Fatalf("boots reached the real booter = %d, want 0", real.boots)
	}
	// The refusal must carry the underlying cause; an operator cannot act on
	// "unreconciled" alone.
	_, err := published.BootResident(context.Background(), usecase.ImageBootInput{})
	if !strings.Contains(err.Error(), "connection refused") {
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

// ⚠ THE ANTI-LATCH RULE, at the wiring seam. A boot-time reconcile failure must not
// outlive its cause: the published seam re-asks the plane on every boot, so the
// host starts serving again by itself. Live, the frozen verdict kept a second fleet
// host refusing every boot for 10+ minutes after the offending row was deleted from
// both tables, and only a service restart cleared it.
func TestPublishImageBooterStopsRefusingOnceThePlaneIsProvable(t *testing.T) {
	real := &recordingBooter{}
	planeErr := errors.New("occupying row claims an index with NO lease")
	broken := true
	verify := func(context.Context) error {
		if broken {
			return planeErr
		}
		return nil
	}

	published := publishImageBooter(real, verify, planeErr)
	if _, err := published.BootResident(context.Background(), usecase.ImageBootInput{}); err == nil {
		t.Fatal("BootResident while the plane is unprovable succeeded")
	}

	broken = false // the operator removed the offending row

	if _, err := published.BootResident(context.Background(), usecase.ImageBootInput{}); err != nil {
		t.Fatalf("BootResident after the cause was resolved = %v, want the boot to be served", err)
	}
	if real.boots != 1 {
		t.Fatalf("boots reached the real booter = %d, want 1", real.boots)
	}
}

// A plane that could not be CONSTRUCTED (no host identity, no assigned ordinal) has
// no verifier, and that refusal DOES stay put: the allocator was built with the
// same missing input, so nothing this process can observe would change the answer.
// It must still name the cause and still serve teardown.
func TestPublishImageBooterLatchesWhenThePlaneCannotBeConstructed(t *testing.T) {
	real := &recordingBooter{}
	planeErr := errors.New("host has no assigned net ordinal")

	published := publishImageBooter(real, nil, planeErr)

	_, err := published.BootResident(context.Background(), usecase.ImageBootInput{})
	if !errors.Is(err, domain.ErrNetPlaneUnreconciled) || !strings.Contains(err.Error(), "no assigned net ordinal") {
		t.Fatalf("BootResident with no constructible plane = %v, want ErrNetPlaneUnreconciled naming the cause", err)
	}
	if real.boots != 0 {
		t.Fatalf("boots reached the real booter = %d, want 0", real.boots)
	}
	if terr := published.Decommission(context.Background(), usecase.ImageDecommissionInput{}); terr != nil || real.teardowns != 1 {
		t.Fatalf("teardown through the latched seam: err=%v teardowns=%d", terr, real.teardowns)
	}
}

// A reconciled plane serves boots through the real booter — the guard adds a
// precondition, never a behaviour change on the healthy path.
func TestPublishImageBooterServesBootsWhenReconciled(t *testing.T) {
	real := &recordingBooter{}
	published := publishImageBooter(real, func(context.Context) error { return nil }, nil)

	if _, err := published.BootResident(context.Background(), usecase.ImageBootInput{}); err != nil {
		t.Fatalf("BootResident on a reconciled plane = %v", err)
	}
	if real.boots != 1 {
		t.Fatalf("boots reached the real booter = %d, want 1", real.boots)
	}
}
