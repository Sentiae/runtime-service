package domain

import (
	"time"

	"github.com/google/uuid"
)

// Route is an ingress route mapping an external host/path to a FleetApp. It
// doubles as the GORM model (see ImageWorkload). DDL is owned by golang-migrate
// (migrations/), not AutoMigrate.
type Route struct {
	ID           uuid.UUID `json:"id" gorm:"type:uuid;primary_key"`
	AppID        uuid.UUID `json:"app_id" gorm:"type:uuid;not null;index"`
	HostPattern  string    `json:"host_pattern" gorm:"type:varchar(255);not null"`
	PathPrefix   string    `json:"path_prefix" gorm:"type:varchar(255);not null;default:'/'"`
	CustomDomain string    `json:"custom_domain" gorm:"type:varchar(255);not null;default:''"`
	TLSCertRef   string    `json:"tls_cert_ref" gorm:"type:varchar(255);not null;default:''"`
	CreatedAt    time.Time `json:"created_at" gorm:"not null"`
	UpdatedAt    time.Time `json:"updated_at" gorm:"not null"`
}

// TableName specifies the GORM table name.
func (Route) TableName() string {
	return "fleet_routes"
}
