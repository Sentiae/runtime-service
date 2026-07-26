package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/repository"
	"gorm.io/gorm"
)

// fleetResourceRepository is the GORM-backed FleetResourceRepository.
type fleetResourceRepository struct {
	db *gorm.DB
}

var _ repository.FleetResourceRepository = (*fleetResourceRepository)(nil)

// NewFleetResourceRepository creates a new PostgreSQL fleet-resource repository.
func NewFleetResourceRepository(db *gorm.DB) *fleetResourceRepository {
	return &fleetResourceRepository{db: db}
}

// endpointIDUniqueIndex is the arbiter of customer-facing endpoint uniqueness
// (migration 0021). Named explicitly so the translation below cannot swallow an
// unrelated 23505 — the claim-triple collision in particular means something
// completely different to the caller (another racer won the claim, adopt it),
// while this one means "re-mint and try again".
const endpointIDUniqueIndex = "fleet_resources_endpoint_id_key"

func (r *fleetResourceRepository) SaveResource(ctx context.Context, resource *domain.FleetResource) error {
	err := r.db.WithContext(ctx).Save(resource).Error
	if isEndpointIDConflict(err) {
		return domain.ErrEndpointTaken
	}
	return err
}

// isEndpointIDConflict reports whether err is the unique violation on the
// endpoint_id index specifically. Matched on SQLSTATE + constraint name via the
// pgconn error (never on message text, §16.5) — the same driver-level inspection
// routeRepository uses, because this driver is not configured with gorm's
// TranslateError.
func isEndpointIDConflict(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) &&
		pgErr.Code == pgUniqueViolation &&
		pgErr.ConstraintName == endpointIDUniqueIndex
}

func (r *fleetResourceRepository) GetResourceByHandle(ctx context.Context, id uuid.UUID) (*domain.FleetResource, error) {
	var resource domain.FleetResource
	err := r.db.WithContext(ctx).Where("id = ?", id).First(&resource).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, domain.ErrResourceNotFound
		}
		return nil, err
	}
	return &resource, nil
}

func (r *fleetResourceRepository) FindResource(ctx context.Context, ownerOrg uuid.UUID, claimKey, env string) (*domain.FleetResource, error) {
	var resource domain.FleetResource
	err := r.db.WithContext(ctx).
		Where("owner_org = ? AND claim_key = ? AND env = ?", ownerOrg, claimKey, env).
		First(&resource).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, domain.ErrResourceNotFound
		}
		return nil, err
	}
	return &resource, nil
}

// FindLiveResourceByApp resolves the live claim backed by appID (the
// fleet_resources_app_id_idx lookup). "Live" is decommissioned_at IS NULL AND
// phase <> 'decommissioned': BOTH, because the timestamp is stamped when a
// teardown starts and the phase only when it finishes, so either one alone
// would still call an in-flight teardown live and deadlock the resource's own
// path. Newest first purely for determinism — the (owner_org, claim_key, env)
// unique index means at most one live claim per app in practice.
func (r *fleetResourceRepository) FindLiveResourceByApp(ctx context.Context, appID uuid.UUID) (*domain.FleetResource, error) {
	var resource domain.FleetResource
	err := r.db.WithContext(ctx).
		Where("app_id = ? AND decommissioned_at IS NULL AND phase <> ?", appID, domain.FleetResourcePhaseDecommissioned).
		Order("created_at DESC").
		First(&resource).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, domain.ErrResourceNotFound
		}
		return nil, err
	}
	return &resource, nil
}

func (r *fleetResourceRepository) UpdateResourcePhase(ctx context.Context, id uuid.UUID, phase domain.FleetResourcePhase) error {
	res := r.db.WithContext(ctx).
		Model(&domain.FleetResource{}).
		Where("id = ?", id).
		Update("phase", phase)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return domain.ErrResourceNotFound
	}
	return nil
}

// CompareAndSwapPhase advances the phase only from one of `from`, reporting
// whether a row changed. Single statement — the WHERE runs under the row lock
// the UPDATE takes, so two concurrent callers cannot both observe 1 row.
func (r *fleetResourceRepository) CompareAndSwapPhase(ctx context.Context, id uuid.UUID, from []domain.FleetResourcePhase, to domain.FleetResourcePhase) (bool, error) {
	if len(from) == 0 {
		return false, nil
	}
	res := r.db.WithContext(ctx).
		Model(&domain.FleetResource{}).
		Where("id = ? AND phase IN ?", id, from).
		Updates(map[string]any{"phase": to, "updated_at": time.Now().UTC()})
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected == 1, nil
}

func (r *fleetResourceRepository) SetResourceLastError(ctx context.Context, id uuid.UUID, msg string) error {
	res := r.db.WithContext(ctx).
		Model(&domain.FleetResource{}).
		Where("id = ?", id).
		Updates(map[string]any{"last_error": msg, "updated_at": time.Now().UTC()})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return domain.ErrResourceNotFound
	}
	return nil
}

// RecordSnapshotFailure increments the consecutive-failure count IN the UPDATE
// (gorm.Expr, not a read-modify-write) so two snapshots failing concurrently both
// count — a read-modify-write would silently collapse them into one and make a
// sustained outage look milder than it is.
func (r *fleetResourceRepository) RecordSnapshotFailure(ctx context.Context, id uuid.UUID, at time.Time, cause string) error {
	res := r.db.WithContext(ctx).
		Model(&domain.FleetResource{}).
		Where("id = ?", id).
		Updates(map[string]any{
			"consecutive_snapshot_failures": gorm.Expr("consecutive_snapshot_failures + 1"),
			"last_snapshot_failure_at":      at,
			"last_snapshot_error":           cause,
			"updated_at":                    at,
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return domain.ErrResourceNotFound
	}
	return nil
}

// RecordSnapshotSuccess clears the failure streak. last_snapshot_error is cleared
// with it: the stored text describes a streak that is over, and leaving it would
// make a protected resource read as a failing one.
func (r *fleetResourceRepository) RecordSnapshotSuccess(ctx context.Context, id uuid.UUID, at time.Time) error {
	res := r.db.WithContext(ctx).
		Model(&domain.FleetResource{}).
		Where("id = ?", id).
		Updates(map[string]any{
			"consecutive_snapshot_failures": 0,
			"last_snapshot_error":           "",
			"last_snapshot_success_at":      at,
			"updated_at":                    at,
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return domain.ErrResourceNotFound
	}
	return nil
}

func (r *fleetResourceRepository) ListResourcesByPhase(ctx context.Context, phase domain.FleetResourcePhase) ([]domain.FleetResource, error) {
	var resources []domain.FleetResource
	err := r.db.WithContext(ctx).
		Where("phase = ? AND decommissioned_at IS NULL", phase).
		Order("updated_at ASC").
		Find(&resources).Error
	return resources, err
}

// ListExpiredShared returns shared-variant resources (app_id IS NULL) whose TTL
// has elapsed and that are not yet tombstoned. Dedicated resources carry an
// app_id and are reclaimed through their FleetApp, so they are excluded here.
func (r *fleetResourceRepository) ListExpiredShared(ctx context.Context, now time.Time) ([]domain.FleetResource, error) {
	var resources []domain.FleetResource
	err := r.db.WithContext(ctx).
		Where("app_id IS NULL AND expires_at IS NOT NULL AND expires_at <= ? AND decommissioned_at IS NULL", now).
		Order("expires_at ASC").
		Find(&resources).Error
	return resources, err
}

func (r *fleetResourceRepository) SaveRecoveryPoint(ctx context.Context, rp *domain.FleetResourceRecoveryPoint) error {
	return r.db.WithContext(ctx).Create(rp).Error
}

func (r *fleetResourceRepository) ListRecoveryPoints(ctx context.Context, resourceID uuid.UUID) ([]domain.FleetResourceRecoveryPoint, error) {
	var points []domain.FleetResourceRecoveryPoint
	err := r.db.WithContext(ctx).
		Where("resource_id = ?", resourceID).
		Order("created_at DESC").
		Find(&points).Error
	return points, err
}

// GetRecoveryPointByRef resolves a ref WITHIN one resource. The resource_id
// predicate is the security boundary, not an optimization: object keys are
// guessable-shaped (volumes/<vol>/<uuid>.ext4), so a global lookup would let a
// leaked key from another org's resource be restored into this one.
func (r *fleetResourceRepository) GetRecoveryPointByRef(ctx context.Context, resourceID uuid.UUID, objectKey string) (*domain.FleetResourceRecoveryPoint, error) {
	var rp domain.FleetResourceRecoveryPoint
	err := r.db.WithContext(ctx).
		Where("resource_id = ? AND object_key = ?", resourceID, objectKey).
		Order("created_at DESC").
		First(&rp).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, domain.ErrRecoveryPointNotFound
		}
		return nil, err
	}
	return &rp, nil
}

// ListResourceDurability aggregates every live claim against its recovery-point
// catalog in ONE query.
//
// A LEFT JOIN, never an inner one: a resource with no recovery point must appear
// in the result with a NULL latest and a count of 0. An inner join would DROP
// exactly those rows, and a dropped row becomes a missing metric series, which
// reads as "nothing to report" — the false-green this projection exists to
// prevent. GROUP BY r.id is sufficient in Postgres (every selected r column is
// functionally dependent on the primary key).
func (r *fleetResourceRepository) ListResourceDurability(ctx context.Context) ([]repository.ResourceDurability, error) {
	var out []repository.ResourceDurability
	err := r.db.WithContext(ctx).Raw(`
		SELECT r.id                            AS resource_id,
		       r.owner_org                     AS owner_org,
		       r.phase                         AS phase,
		       r.class                         AS class,
		       r.tier                          AS tier,
		       r.consecutive_snapshot_failures AS consecutive_snapshot_failures,
		       r.last_snapshot_success_at      AS last_snapshot_success_at,
		       MAX(rp.created_at)              AS latest_recovery_point_at,
		       COUNT(rp.id)                    AS recovery_point_count
		FROM fleet_resources r
		LEFT JOIN fleet_resource_recovery_points rp ON rp.resource_id = r.id
		WHERE r.decommissioned_at IS NULL
		GROUP BY r.id`).Scan(&out).Error
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListRecoveryPointsToMirror returns the control-plane mirror's backlog (D-200).
//
// OLDEST FIRST, because the alert this drains is an AGE
// (sentiae_fleet_recovery_point_oldest_single_domain_age_seconds): copying the
// newest first would leave the worst number untouched while the worker looked busy.
// The predicate is `locations = 'primary_only'` exactly — see the interface note on
// why `unknown` is not swept in.
func (r *fleetResourceRepository) ListRecoveryPointsToMirror(ctx context.Context, limit int) ([]domain.FleetResourceRecoveryPoint, error) {
	if limit <= 0 {
		return nil, nil
	}
	var out []domain.FleetResourceRecoveryPoint
	err := r.db.WithContext(ctx).
		Where("locations = ?", domain.RecoveryPointLocationsPrimaryOnly).
		Order("created_at ASC").
		Limit(limit).
		Find(&out).Error
	if err != nil {
		return nil, err
	}
	return out, nil
}

// MarkRecoveryPointInSecondDomain promotes a row to the two-domain class.
//
// ⚠ The predicate is `locations <> 'primary_and_second_domain'` and NOT a bare id
// match, so the write is idempotent: a re-confirmation of a row that already holds
// the claim affects no rows and is reported as success, rather than restamping
// second_domain_at to a later time and thereby claiming the copy is newer than it
// is. `unknown` is promotable — a legacy row whose blob has now been verified in the
// second domain genuinely IS in two domains.
func (r *fleetResourceRepository) MarkRecoveryPointInSecondDomain(ctx context.Context, id uuid.UUID, store string, at time.Time) error {
	if store == "" {
		// Migration 0023's CHECK forbids the claim without a named store; refuse here
		// with a legible error rather than surfacing a constraint violation.
		return fmt.Errorf("mark recovery point %s in second domain: the store must be named", id)
	}
	res := r.db.WithContext(ctx).
		Model(&domain.FleetResourceRecoveryPoint{}).
		Where("id = ? AND locations <> ?", id, domain.RecoveryPointLocationsSecondDomain).
		Updates(map[string]any{
			"locations":           domain.RecoveryPointLocationsSecondDomain,
			"second_domain_store": store,
			"second_domain_at":    at,
			"second_domain_error": "",
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		// Either the row is gone or it already holds the claim. Distinguish them: a
		// missing row is a caller bug, an already-claimed row is a no-op success.
		var count int64
		if err := r.db.WithContext(ctx).
			Model(&domain.FleetResourceRecoveryPoint{}).
			Where("id = ?", id).
			Count(&count).Error; err != nil {
			return err
		}
		if count == 0 {
			return domain.ErrRecoveryPointNotFound
		}
	}
	return nil
}

// RecordRecoveryPointMirrorFailure records WHY a second-domain copy failed without
// touching `locations` — the row keeps saying what is true (one domain) and gains
// the cause an operator needs. It touches nothing on fleet_resources: a failed
// mirror is not a failed snapshot.
func (r *fleetResourceRepository) RecordRecoveryPointMirrorFailure(ctx context.Context, id uuid.UUID, at time.Time, cause string) error {
	res := r.db.WithContext(ctx).
		Model(&domain.FleetResourceRecoveryPoint{}).
		Where("id = ?", id).
		Updates(map[string]any{
			"second_domain_error": cause,
			// The attempt time, so "when did this last fail" is answerable. It is NOT
			// second_domain_at's success meaning — that column stays NULL, because the
			// CHECK constraint and every reader treat a non-NULL second_domain_at with a
			// two-domain class as evidence of a confirmed copy.
			"second_domain_at": nil,
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return domain.ErrRecoveryPointNotFound
	}
	return nil
}

// ListRecoveryPointLocations aggregates the whole catalog by location class in ONE
// query.
//
// No `decommissioned_at IS NULL` filter and no join to fleet_resources, on purpose:
// a tombstoned resource's recovery points survive it and are still restorable
// customer data, so excluding them would under-report the blobs that one machine's
// loss would destroy (see RecoveryPointLocationFacts).
func (r *fleetResourceRepository) ListRecoveryPointLocations(ctx context.Context) ([]repository.RecoveryPointLocationFacts, error) {
	var out []repository.RecoveryPointLocationFacts
	err := r.db.WithContext(ctx).Raw(`
		SELECT locations        AS locations,
		       COUNT(*)         AS count,
		       MIN(created_at)  AS oldest_created_at
		FROM fleet_resource_recovery_points
		GROUP BY locations`).Scan(&out).Error
	if err != nil {
		return nil, err
	}
	return out, nil
}

// MarkRecoveryPointRestoredInPlace sets the `verified` COLUMN, which records
// RestoredInPlaceOK — an in-place restore that came back serving, not a
// verification drill. The column keeps its 0012 name on purpose (see
// domain.FleetResourceRecoveryPoint.RestoredInPlaceOK).
func (r *fleetResourceRepository) MarkRecoveryPointRestoredInPlace(ctx context.Context, id uuid.UUID) error {
	res := r.db.WithContext(ctx).
		Model(&domain.FleetResourceRecoveryPoint{}).
		Where("id = ?", id).
		Update("verified", true)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return domain.ErrRecoveryPointNotFound
	}
	return nil
}
