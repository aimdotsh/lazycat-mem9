package db9

import (
	"database/sql"

	"github.com/f00700f/lazycat-mem9/internal/repository/postgres"
)

// NewDB creates a configured *sql.DB pool for db9.
// db9 is PostgreSQL-compatible at the driver/protocol layer.
func NewDB(dsn string) (*sql.DB, error) {
	return postgres.NewDB(dsn)
}
