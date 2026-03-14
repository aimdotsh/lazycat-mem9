package db9

import (
	"database/sql"

	"github.com/f00700f/lazycat-mem9/internal/repository/postgres"
)

// NewTenantRepo creates the db9 tenant repository.
// Phase 1 delegates to PostgreSQL SQL implementation.
func NewTenantRepo(db *sql.DB) *postgres.TenantRepoImpl {
	return postgres.NewTenantRepo(db)
}
