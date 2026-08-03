//go:build integration

// External test package (postgres_test) to match the sibling fleet integration
// tests in this directory; it reuses their startLeasePG / migrateAll / seedHost
// helpers.
package postgres_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/repository"
	"github.com/sentiae/runtime-service/internal/repository/postgres"
)

// BindHostAffinity — the per-volume host compare-and-set the host-authority
// fence rests on (#fleet-reconciler-acts-on-foreign-host-replicas), proven
// against a real Postgres.
//
// It is tested HERE and not only with a fake because the property that matters
// is atomicity: the compare, the write and the reported outcome must be ONE
// command under a row lock. A read-then-Save loop passes every single-threaded
// test and still lets two hosts each observe host_affinity NULL and each believe
// it won — which, for a volume, means two machines each believing a customer's
// bytes are on their filesystem.

// seedVolume inserts an app-attached volume with the given (nullable) affinity
// and returns its id. The mount path is per-volume unique because fleet_volumes
// is uniquely indexed on (app_id, mount_path) — the ledger is keyed by the
// mount, so one app cannot hold two volumes at "/data".
func seedVolume(t *testing.T, db *gorm.DB, appID uuid.UUID, host *uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	now := time.Now().UTC()
	err := db.Exec(`INSERT INTO fleet_volumes
		(id, app_id, size_mb, host_affinity, mount_path, backing_path, status, device_name, created_at, updated_at)
		VALUES (?, ?, 1024, ?, ?, ?, 'available', '/dev/vdb', ?, ?)`,
		id, appID, host, "/data-"+id.String()[:8], "/srv/volumes/"+id.String()+".ext4", now, now).Error
	if err != nil {
		t.Fatalf("seed volume: %v", err)
	}
	return id
}

// seedVolumeApp inserts the fleet_apps row a volume's FK requires.
func seedVolumeApp(t *testing.T, db *gorm.DB) uuid.UUID {
	t.Helper()
	id := uuid.New()
	now := time.Now().UTC()
	err := db.Exec(`INSERT INTO fleet_apps
		(id, component_id, env, owner_org, image_repository, image_digest, desired_replicas,
		 port, resources_vcpu, resources_mem_mb, restart_policy, secret_refs, created_at, updated_at)
		VALUES (?, ?, 'prod', ?, 'org/app', 'sha256:abc', 1, 8080, 2, 1024, 'always', '[]', ?, ?)`,
		id, "comp-"+uuid.NewString()[:8], uuid.NewString(), now, now).Error
	if err != nil {
		t.Fatalf("seed fleet app: %v", err)
	}
	return id
}

func volumeRow(t *testing.T, db *gorm.DB, id uuid.UUID) (host *uuid.UUID, updatedAt time.Time) {
	t.Helper()
	var row struct {
		HostAffinity *uuid.UUID
		UpdatedAt    time.Time
	}
	if err := db.Raw(`SELECT host_affinity, updated_at FROM fleet_volumes WHERE id = ?`, id).
		Scan(&row).Error; err != nil {
		t.Fatalf("read volume %s: %v", id, err)
	}
	return row.HostAffinity, row.UpdatedAt
}

func TestBindHostAffinity(t *testing.T) {
	db, m := startLeasePG(t)
	migrateAll(t, m)
	repo := postgres.NewVolumeRepository(db)
	ctx := context.Background()

	hostA, hostB := uuid.New(), uuid.New()
	seedHost(t, db, hostA, time.Now().Add(-time.Hour))
	seedHost(t, db, hostB, time.Now())
	appID := seedVolumeApp(t, db)

	t.Run("nil affinity binds to the asked host", func(t *testing.T) {
		volID := seedVolume(t, db, appID, nil)
		res, err := repo.BindHostAffinity(ctx, volID, hostA)
		if err != nil {
			t.Fatalf("BindHostAffinity: %v", err)
		}
		if res.Outcome != repository.VolumeHostBindBound {
			t.Fatalf("outcome = %q, want %q", res.Outcome, repository.VolumeHostBindBound)
		}
		got, _ := volumeRow(t, db, volID)
		if got == nil || *got != hostA {
			t.Fatalf("host_affinity = %v, want %v", got, hostA)
		}
	})

	t.Run("same host is idempotent and writes nothing", func(t *testing.T) {
		volID := seedVolume(t, db, appID, &hostA)
		_, before := volumeRow(t, db, volID)
		// A second of separation, so an accidental now() write cannot be mistaken
		// for the seeded timestamp.
		time.Sleep(1100 * time.Millisecond)

		res, err := repo.BindHostAffinity(ctx, volID, hostA)
		if err != nil {
			t.Fatalf("BindHostAffinity: %v", err)
		}
		if res.Outcome != repository.VolumeHostBindAlreadyBound {
			t.Fatalf("outcome = %q, want %q", res.Outcome, repository.VolumeHostBindAlreadyBound)
		}
		got, after := volumeRow(t, db, volID)
		if got == nil || *got != hostA {
			t.Fatalf("host_affinity = %v, want it unchanged at %v", got, hostA)
		}
		if !after.Equal(before) {
			t.Fatalf("updated_at moved (%s → %s) — the idempotent re-bind must write NOTHING", before, after)
		}
	})

	t.Run("a foreign affinity is a conflict and is never overwritten", func(t *testing.T) {
		volID := seedVolume(t, db, appID, &hostB)
		_, before := volumeRow(t, db, volID)
		time.Sleep(1100 * time.Millisecond)

		res, err := repo.BindHostAffinity(ctx, volID, hostA)
		if err != nil {
			t.Fatalf("BindHostAffinity: %v", err)
		}
		if res.Outcome != repository.VolumeHostBindConflict {
			t.Fatalf("outcome = %q, want %q", res.Outcome, repository.VolumeHostBindConflict)
		}
		if res.ActualHost != hostB {
			t.Fatalf("ActualHost = %v, want the row's real owner %v", res.ActualHost, hostB)
		}
		got, after := volumeRow(t, db, volID)
		if got == nil || *got != hostB {
			t.Fatalf("host_affinity = %v, want it UNCHANGED at %v — the bytes are on that machine", got, hostB)
		}
		if !after.Equal(before) {
			t.Fatalf("updated_at moved (%s → %s) — a conflict writes nothing at all", before, after)
		}
	})

	t.Run("a missing row is ErrVolumeNotFound", func(t *testing.T) {
		_, err := repo.BindHostAffinity(ctx, uuid.New(), hostA)
		if !errors.Is(err, domain.ErrVolumeNotFound) {
			t.Fatalf("BindHostAffinity on a missing row = %v, want ErrVolumeNotFound", err)
		}
	})

	// THE test. Two hosts adopt the same legacy nil-affinity row at the same
	// instant. The row lock must serialize them into exactly one `bound` and one
	// `conflict`, and the stored host must be the one the winner reported — a
	// read-then-Save loop produces two winners here, and the loser then deletes
	// (or re-points) bytes that are not its own.
	t.Run("two concurrent different-host binders produce exactly one winner", func(t *testing.T) {
		volID := seedVolume(t, db, appID, nil)

		type result struct {
			res  repository.VolumeHostBindResult
			err  error
			for_ uuid.UUID
		}
		var wg sync.WaitGroup
		results := make([]result, 2)
		start := make(chan struct{})
		for i, host := range []uuid.UUID{hostA, hostB} {
			wg.Add(1)
			go func(i int, host uuid.UUID) {
				defer wg.Done()
				<-start
				r, err := repo.BindHostAffinity(ctx, volID, host)
				results[i] = result{res: r, err: err, for_: host}
			}(i, host)
		}
		close(start)
		wg.Wait()

		bound, conflict := 0, 0
		var winner uuid.UUID
		for _, r := range results {
			if r.err != nil {
				t.Fatalf("BindHostAffinity(%s): %v", r.for_, r.err)
			}
			switch r.res.Outcome {
			case repository.VolumeHostBindBound:
				bound++
				winner = r.for_
			case repository.VolumeHostBindConflict:
				conflict++
			default:
				t.Fatalf("unexpected outcome %q", r.res.Outcome)
			}
		}
		if bound != 1 || conflict != 1 {
			t.Fatalf("bound=%d conflict=%d, want exactly 1 and 1 — two winners means two hosts each believe they hold the bytes",
				bound, conflict)
		}
		got, _ := volumeRow(t, db, volID)
		if got == nil || *got != winner {
			t.Fatalf("host_affinity = %v, want the reported winner %v", got, winner)
		}
	})
}

// TestBindHostAffinityAndBindVolumesToResourceAreIndependent: the two commands
// write DIFFERENT columns (host_affinity vs resource_id), and neither may clear
// or overwrite the other's. They run against the same rows on every dedicated
// provision, so a shared Save() would silently undo one of them.
func TestBindHostAffinityAndBindVolumesToResourceAreIndependent(t *testing.T) {
	db, m := startLeasePG(t)
	migrateAll(t, m)
	repo := postgres.NewVolumeRepository(db)
	ctx := context.Background()

	host := uuid.New()
	seedHost(t, db, host, time.Now())
	appID := seedVolumeApp(t, db)
	volID := seedVolume(t, db, appID, nil)
	resourceID := uuid.New()
	now := time.Now().UTC()
	if err := db.Exec(`INSERT INTO fleet_resources
		(id, owner_org, claim_key, env, revision, class, tier, phase, generation, durability, created_at, updated_at)
		VALUES (?, ?, ?, 'prod', 1, 'postgres', 'dedicated', 'ready', 1, 'durable', ?, ?)`,
		resourceID, uuid.New(), "claim-"+uuid.NewString(), now, now).Error; err != nil {
		t.Fatalf("seed resource: %v", err)
	}

	if _, err := repo.BindHostAffinity(ctx, volID, host); err != nil {
		t.Fatalf("BindHostAffinity: %v", err)
	}
	if _, err := repo.BindVolumesToResource(ctx, appID, resourceID); err != nil {
		t.Fatalf("BindVolumesToResource: %v", err)
	}

	var row struct {
		HostAffinity *uuid.UUID
		ResourceID   *uuid.UUID
	}
	if err := db.Raw(`SELECT host_affinity, resource_id FROM fleet_volumes WHERE id = ?`, volID).
		Scan(&row).Error; err != nil {
		t.Fatalf("read volume: %v", err)
	}
	if row.HostAffinity == nil || *row.HostAffinity != host {
		t.Fatalf("host_affinity = %v after the claim bind, want %v", row.HostAffinity, host)
	}
	if row.ResourceID == nil || *row.ResourceID != resourceID {
		t.Fatalf("resource_id = %v, want %v", row.ResourceID, resourceID)
	}

	// And in the other order, on a second volume.
	vol2 := seedVolume(t, db, appID, nil)
	if _, err := repo.BindVolumesToResource(ctx, appID, resourceID); err != nil {
		t.Fatalf("BindVolumesToResource (second): %v", err)
	}
	if _, err := repo.BindHostAffinity(ctx, vol2, host); err != nil {
		t.Fatalf("BindHostAffinity (second): %v", err)
	}
	if err := db.Raw(`SELECT host_affinity, resource_id FROM fleet_volumes WHERE id = ?`, vol2).
		Scan(&row).Error; err != nil {
		t.Fatalf("read volume 2: %v", err)
	}
	if row.HostAffinity == nil || *row.HostAffinity != host || row.ResourceID == nil || *row.ResourceID != resourceID {
		t.Fatalf("volume 2 = {host:%v resource:%v}, want both stamped", row.HostAffinity, row.ResourceID)
	}
}
