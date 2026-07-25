//go:build integration

// External test package (postgres_test) to match the sibling fleet integration
// test in this directory.
package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/repository/postgres"
	"gorm.io/gorm"
)

// Migration 0021 + the repository translation, proven against a real Postgres:
// the customer-facing endpoint identity is unique, absence is NULL (and NULLs do
// not collide), the shape CHECK holds, and generation is fenced at >= 1.

func newResource(endpointID *string, region string) *domain.FleetResource {
	now := time.Now().UTC()
	return &domain.FleetResource{
		ID:         uuid.New(),
		OwnerOrg:   uuid.New(),
		ClaimKey:   "claim-" + uuid.NewString(),
		Env:        "prod",
		Revision:   1,
		Generation: domain.FleetResourceInitialGeneration,
		Class:      "postgres",
		Tier:       "dedicated",
		Phase:      domain.FleetResourcePhaseProvisioning,
		EndpointID: endpointID,
		Region:     region,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
}

func migratedDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, m := startLeasePG(t)
	migrateAll(t, m)
	return db
}

// TestMigration0021IsReversible — up → down → up (§24). The down is lossy by
// nature (it drops minted permanent names), which is exactly why it must be
// proven to leave a schema the up can be re-applied to.
func TestMigration0021IsReversible(t *testing.T) {
	db, m := startLeasePG(t)
	migrateAll(t, m)

	hasColumn := func(name string) bool {
		var n int64
		if err := db.Raw(`SELECT count(*) FROM information_schema.columns
			WHERE table_name = 'fleet_resources' AND column_name = ?`, name).Scan(&n).Error; err != nil {
			t.Fatalf("introspect %s: %v", name, err)
		}
		return n > 0
	}
	for _, col := range []string{"endpoint_id", "region", "generation"} {
		if !hasColumn(col) {
			t.Fatalf("after up: fleet_resources.%s is missing", col)
		}
	}

	if err := m.Steps(-1); err != nil {
		t.Fatalf("migrate down one: %v", err)
	}
	for _, col := range []string{"endpoint_id", "region", "generation"} {
		if hasColumn(col) {
			t.Fatalf("after down: fleet_resources.%s survived", col)
		}
	}

	migrateAll(t, m)
	for _, col := range []string{"endpoint_id", "region", "generation"} {
		if !hasColumn(col) {
			t.Fatalf("after re-up: fleet_resources.%s is missing", col)
		}
	}
	// The unique index must come back with it — a down that drops the index and an
	// up that does not re-create it is how a uniqueness fence silently disappears.
	var idx int64
	if err := db.Raw(`SELECT count(*) FROM pg_indexes
		WHERE tablename = 'fleet_resources' AND indexname = 'fleet_resources_endpoint_id_key'`).Scan(&idx).Error; err != nil {
		t.Fatalf("introspect index: %v", err)
	}
	if idx != 1 {
		t.Fatalf("fleet_resources_endpoint_id_key present = %d, want 1", idx)
	}
}

func TestEndpointIDIsUniqueAndNullTolerant(t *testing.T) {
	db := migratedDB(t)
	repo := postgres.NewFleetResourceRepository(db)
	ctx := context.Background()

	taken := "quiet-forest-4821"
	if err := repo.SaveResource(ctx, newResource(&taken, "eu-central")); err != nil {
		t.Fatalf("save first: %v", err)
	}

	// The SAME name in a DIFFERENT org must still be refused: the endpoint is
	// globally unique, because it is a global DNS name.
	dup := taken
	err := repo.SaveResource(ctx, newResource(&dup, "eu-central"))
	if !errors.Is(err, domain.ErrEndpointTaken) {
		t.Fatalf("duplicate endpoint: got %v, want domain.ErrEndpointTaken", err)
	}

	// A different name is fine.
	other := "silver-meadow-0007"
	if err := repo.SaveResource(ctx, newResource(&other, "eu-central")); err != nil {
		t.Fatalf("save second: %v", err)
	}

	// Endpoint-less rows (pre-0021 rows, shared-tier claims) do NOT collide: two
	// NULLs are not equal in a Postgres unique index. This is what makes a
	// backfill-free migration safe.
	for i := 0; i < 3; i++ {
		if err := repo.SaveResource(ctx, newResource(nil, "")); err != nil {
			t.Fatalf("save endpoint-less #%d: %v", i, err)
		}
	}

	var nulls int64
	if err := db.Raw(`SELECT count(*) FROM fleet_resources WHERE endpoint_id IS NULL`).Scan(&nulls).Error; err != nil {
		t.Fatalf("count nulls: %v", err)
	}
	if nulls != 3 {
		t.Fatalf("endpoint-less rows = %d, want 3", nulls)
	}
}

func TestEndpointIdentityRoundTrips(t *testing.T) {
	db := migratedDB(t)
	repo := postgres.NewFleetResourceRepository(db)
	ctx := context.Background()

	ep, err := domain.EndpointNaming{Zone: "db.sentiae.com", Region: "eu-central"}.Mint()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	id := ep.ID()
	res := newResource(&id, ep.Region())
	if err := repo.SaveResource(ctx, res); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, err := repo.GetResourceByHandle(ctx, res.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.EndpointID == nil || *got.EndpointID != id {
		t.Fatalf("endpoint_id round trip: got %v, want %q", got.EndpointID, id)
	}
	if got.Region != ep.Region() {
		t.Fatalf("region round trip: got %q, want %q", got.Region, ep.Region())
	}
	if got.Generation != domain.FleetResourceInitialGeneration {
		t.Fatalf("generation = %d, want %d", got.Generation, domain.FleetResourceInitialGeneration)
	}
	rebuilt, err := domain.NewResourceEndpoint(*got.EndpointID, got.Region, "db.sentiae.com")
	if err != nil {
		t.Fatalf("stored identity does not rebuild into a host: %v", err)
	}
	if rebuilt.Host() != ep.Host() {
		t.Fatalf("host = %q, want %q", rebuilt.Host(), ep.Host())
	}
}

// TestStoreFencesRefuseMalformedIdentity — the storage-level fences are the last
// line: a permanent name that is not the minted shape, and a generation below 1,
// must be refused by Postgres itself, not only by the Go path that happens to be
// in front of it today.
func TestStoreFencesRefuseMalformedIdentity(t *testing.T) {
	db := migratedDB(t)

	base := `INSERT INTO fleet_resources (id, owner_org, claim_key, env, class, tier, phase, endpoint_id, generation)
	         VALUES (?, ?, ?, 'prod', 'postgres', 'dedicated', 'provisioning', ?, ?)`
	tests := []struct {
		name       string
		endpointID any
		generation int
	}{
		{"no number", "quiet-forest", 1},
		{"three digits", "quiet-forest-482", 1},
		{"uppercase", "Quiet-forest-4821", 1},
		{"single word", "forest-4821", 1},
		{"generation zero", "quiet-forest-4822", 0},
		{"generation negative", "quiet-forest-4823", -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := db.Exec(base, uuid.New(), uuid.New(), "claim-"+uuid.NewString(), tt.endpointID, tt.generation).Error
			if err == nil {
				t.Fatalf("postgres accepted endpoint_id=%v generation=%d", tt.endpointID, tt.generation)
			}
		})
	}
}
