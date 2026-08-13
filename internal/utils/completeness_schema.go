package utils

import (
	"fmt"

	"gorm.io/gorm"
)

// EnsureCompletenessSchema creates archive_items.completeness explicitly.
//
// AutoMigrate cannot be relied on for this. With the gorm postgres driver and
// pgx versions pinned here it fails while probing an existing table's column
// types ("insufficient arguments") before emitting any ALTER, and startup
// deliberately continues past that error — so on a database that already has
// the table, which is to say production, the column would never appear. gorm
// still names every model column in its SELECTs, so a missing column does not
// degrade one feature: it breaks every query against archive_items.
//
// Fresh databases are unaffected either way, and AutoMigrate handles SQLite in
// tests, so this runs only where the failure is real.
//
// The DDL is idempotent by construction and safe on a populated table: adding a
// nullable column with no default is a catalog-only change in PostgreSQL, so it
// takes no table rewrite and existing rows read as empty — legacy-unknown,
// which is exactly how an archive captured before completeness tracking must be
// treated.
func EnsureCompletenessSchema(db *gorm.DB) error {
	if db.Dialector.Name() != "postgres" {
		return nil
	}
	if err := db.Exec(`ALTER TABLE archive_items ADD COLUMN IF NOT EXISTS completeness text`).Error; err != nil {
		return fmt.Errorf("add archive_items.completeness column: %w", err)
	}
	return nil
}
