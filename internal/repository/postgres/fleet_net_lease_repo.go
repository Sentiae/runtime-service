package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/repository"
	"gorm.io/gorm"
)

// netLeaseRepository is the GORM-backed NetLeaseRepository — the durable microVM
// addressing plane (migrations/0020).
//
// ⚠ There is deliberately NO caching, no in-memory used-set and no advisory lock
// here. The unique indexes on fleet_net_leases ARE the concurrency control: every
// allocation is an INSERT that either wins or is refused. A cache in front of
// them would be a second source of truth for which addresses are held, and the
// in-memory allocator this replaces is precisely what that costs.
type netLeaseRepository struct {
	db *gorm.DB
}

var _ repository.NetLeaseRepository = (*netLeaseRepository)(nil)

// NewNetLeaseRepository creates a new PostgreSQL microVM net-lease repository.
func NewNetLeaseRepository(db *gorm.DB) *netLeaseRepository {
	return &netLeaseRepository{db: db}
}

// ordinalAssignAttempts bounds the EnsureHostOrdinal retry loop. Each retry is a
// LOST RACE against another host claiming the ordinal this call picked, so the
// bound only has to exceed the number of hosts that can race — and it must exist,
// because an unbounded loop under a real conflict would spin forever.
const ordinalAssignAttempts = 32

// Acquire inserts the lease, translating any unique-fence rejection into
// domain.ErrNetLeaseConflict.
//
// The translation is total on purpose: whichever of the five fences fired, the
// answer is the same — this allocation is NOT held by this caller, so it must not
// be used. The conflicting coordinates are carried in the message because a
// conflict on a live host is an operator-visible event, not routine.
func (r *netLeaseRepository) Acquire(ctx context.Context, lease *domain.NetLease) error {
	err := r.db.WithContext(ctx).Create(lease).Error
	if isUniqueViolation(err) {
		return fmt.Errorf("%w: net_index=%d host=%s slot=%d tap=%s uid=%d owner=%s/%s: %v",
			domain.ErrNetLeaseConflict, lease.NetIndex, lease.HostID, lease.LocalSlot,
			lease.TapName, lease.VMUID, lease.OwnerKind, lease.OwnerID, err)
	}
	if err != nil {
		return fmt.Errorf("insert net lease: %w", err)
	}
	return nil
}

func (r *netLeaseRepository) UsedSlots(ctx context.Context, hostID uuid.UUID) ([]int, error) {
	var slots []int
	err := r.db.WithContext(ctx).
		Model(&domain.NetLease{}).
		Where("host_id = ?", hostID).
		Order("local_slot ASC").
		Pluck("local_slot", &slots).Error
	if err != nil {
		return nil, fmt.Errorf("list used net-lease slots: %w", err)
	}
	return slots, nil
}

func (r *netLeaseRepository) FindByOwner(ctx context.Context, kind domain.NetLeaseOwnerKind, ownerID uuid.UUID) (*domain.NetLease, error) {
	var lease domain.NetLease
	err := r.db.WithContext(ctx).
		Where("owner_kind = ? AND owner_id = ?", kind, ownerID).
		First(&lease).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, domain.ErrNetLeaseNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("find net lease by owner: %w", err)
	}
	return &lease, nil
}

func (r *netLeaseRepository) ListByHost(ctx context.Context, hostID uuid.UUID) ([]domain.NetLease, error) {
	var leases []domain.NetLease
	err := r.db.WithContext(ctx).
		Where("host_id = ?", hostID).
		Order("local_slot ASC").
		Find(&leases).Error
	if err != nil {
		return nil, fmt.Errorf("list net leases by host: %w", err)
	}
	return leases, nil
}

// Release deletes an owner's lease. A DELETE that matches nothing is a success:
// teardown is never blockable (see ImageBooter.Decommission), and a lease that is
// already gone is the state the caller asked for.
func (r *netLeaseRepository) Release(ctx context.Context, kind domain.NetLeaseOwnerKind, ownerID uuid.UUID) error {
	err := r.db.WithContext(ctx).
		Where("owner_kind = ? AND owner_id = ?", kind, ownerID).
		Delete(&domain.NetLease{}).Error
	if err != nil {
		return fmt.Errorf("release net lease: %w", err)
	}
	return nil
}

// EnsureHostOrdinal assigns this host the lowest free ordinal, or returns the one
// it already holds.
//
// ⚠ The read-then-write is made safe by the UNIQUE index, not by the read. Two
// hosts registering at once both see the same free ordinal; the loser's UPDATE is
// rejected by fleet_hosts_net_ordinal_key and it retries with the next one. The
// UPDATE is additionally guarded by `net_ordinal IS NULL`, so a concurrent
// assignment to THIS host can never be overwritten — an ordinal is assigned once
// and never moves, because moving it would re-point a block whose leases (and
// whose live VMs) are already addressed out of it.
func (r *netLeaseRepository) EnsureHostOrdinal(ctx context.Context, hostID uuid.UUID) (int, error) {
	for attempt := 0; attempt < ordinalAssignAttempts; attempt++ {
		var host domain.Host
		if err := r.db.WithContext(ctx).Where("id = ?", hostID).First(&host).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return 0, domain.ErrFleetHostNotFound
			}
			return 0, fmt.Errorf("load host for ordinal assignment: %w", err)
		}
		if host.NetOrdinal != nil {
			ord := *host.NetOrdinal
			if ord < 0 || ord > domain.NetMaxOrdinal {
				// A stored ordinal outside the fence would derive addresses outside the
				// fleet subnet. Refuse rather than repair: the row was written by
				// something that does not share this plane's rules.
				return 0, fmt.Errorf("%w: host %s has stored ordinal %d",
					domain.ErrNetCoordinateOutOfRange, hostID, ord)
			}
			return ord, nil
		}

		var taken []int
		if err := r.db.WithContext(ctx).
			Model(&domain.Host{}).
			Where("net_ordinal IS NOT NULL").
			Pluck("net_ordinal", &taken).Error; err != nil {
			return 0, fmt.Errorf("list assigned host ordinals: %w", err)
		}
		used := make(map[int]bool, len(taken))
		for _, o := range taken {
			used[o] = true
		}
		ord := -1
		for candidate := 0; candidate <= domain.NetMaxOrdinal; candidate++ {
			if !used[candidate] {
				ord = candidate
				break
			}
		}
		if ord < 0 {
			return 0, fmt.Errorf("%w: all %d ordinals are assigned",
				domain.ErrNetOrdinalExhausted, domain.NetMaxOrdinal+1)
		}

		res := r.db.WithContext(ctx).
			Model(&domain.Host{}).
			Where("id = ? AND net_ordinal IS NULL", hostID).
			Update("net_ordinal", ord)
		if isUniqueViolation(res.Error) {
			// Another host won this ordinal between the read and the write. Retry.
			continue
		}
		if res.Error != nil {
			return 0, fmt.Errorf("assign host ordinal: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			// This host was assigned an ordinal concurrently; re-read it rather than
			// assuming the value this iteration picked.
			continue
		}
		return ord, nil
	}
	return 0, fmt.Errorf("%w: could not claim a free ordinal in %d attempts",
		domain.ErrNetOrdinalExhausted, ordinalAssignAttempts)
}

// isUniqueViolation reports whether err is a Postgres unique-constraint
// violation. Matched on SQLSTATE via the pgconn error (never on message text,
// §30.5/§16.5); gorm.ErrDuplicatedKey is also accepted so this keeps working if
// the driver is ever configured with TranslateError.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return true
	}
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation
}
