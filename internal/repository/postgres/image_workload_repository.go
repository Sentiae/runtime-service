package postgres

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/sentiae/runtime-service/internal/domain"
	"github.com/sentiae/runtime-service/internal/repository"
	"gorm.io/gorm"
)

// pgUniqueViolation is the SQLSTATE Postgres raises when an INSERT collides with
// a unique index — here, two concurrent jobs racing on the same (owner_org,
// idempotency_key). This driver is not configured with gorm's TranslateError, so
// the pgconn error is inspected directly rather than via gorm.ErrDuplicatedKey.
const pgUniqueViolation = "23505"

// imageWorkloadRepository is the GORM-backed ImageWorkloadRepository.
type imageWorkloadRepository struct {
	db *gorm.DB
}

var _ repository.ImageWorkloadRepository = (*imageWorkloadRepository)(nil)

// NewImageWorkloadRepository creates a new PostgreSQL image-workload repository.
func NewImageWorkloadRepository(db *gorm.DB) *imageWorkloadRepository {
	return &imageWorkloadRepository{db: db}
}

func (r *imageWorkloadRepository) Create(ctx context.Context, workload *domain.ImageWorkload) error {
	return r.db.WithContext(ctx).Create(workload).Error
}

func (r *imageWorkloadRepository) Update(ctx context.Context, workload *domain.ImageWorkload) error {
	return r.db.WithContext(ctx).Save(workload).Error
}

func (r *imageWorkloadRepository) FindByID(ctx context.Context, id uuid.UUID) (*domain.ImageWorkload, error) {
	var workload domain.ImageWorkload
	err := r.db.WithContext(ctx).Where("id = ?", id).First(&workload).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, domain.ErrWorkloadNotFound
		}
		return nil, err
	}
	return &workload, nil
}

func (r *imageWorkloadRepository) FindActive(ctx context.Context) ([]domain.ImageWorkload, error) {
	var workloads []domain.ImageWorkload
	err := r.db.WithContext(ctx).
		Where("state IN ?", []domain.ImageWorkloadState{
			domain.ImageWorkloadStateBooting,
			domain.ImageWorkloadStateRunning,
		}).
		Order("created_at ASC").
		Find(&workloads).Error
	return workloads, err
}

func (r *imageWorkloadRepository) Delete(ctx context.Context, id uuid.UUID) error {
	return r.db.WithContext(ctx).Where("id = ?", id).Delete(&domain.ImageWorkload{}).Error
}

// FindByIdempotencyKey resolves a job by the exact (owner_org, key) pair the
// unique index enforces. Scoping to owner_org is what stops one tenant's key
// from ever resolving to another tenant's handle (I28).
func (r *imageWorkloadRepository) FindByIdempotencyKey(ctx context.Context, ownerOrg, key string) (*domain.ImageWorkload, error) {
	var workload domain.ImageWorkload
	err := r.db.WithContext(ctx).
		Where("owner_org = ? AND idempotency_key = ?", ownerOrg, key).
		First(&workload).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, domain.ErrWorkloadNotFound
		}
		return nil, err
	}
	return &workload, nil
}

// IsDuplicateKey reports whether err is a Postgres unique-constraint violation
// (the losing side of a Create race on the same idempotency key).
func (r *imageWorkloadRepository) IsDuplicateKey(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation
}
