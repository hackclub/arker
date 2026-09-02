package utils

import (
	"fmt"

	"gorm.io/gorm"
)

// MigrateBrightDataUsage carries the retired Bright Data spend ledger into
// fallback_usages so the admin spend report and per-item cost blocks keep
// showing historical paid work after the provider swap.
//
// The fallback_usages table is created here explicitly rather than trusted to
// AutoMigrate for the same reason as EnsureCompletenessSchema: on the pinned
// driver versions AutoMigrate can fail before it reaches a new model, and
// startup deliberately continues past that error.
//
// The copy runs once: it is skipped as soon as any "brightdata" row exists in
// the new table. The old table is left in place untouched so the copy can be
// audited; dropping it is a manual operation.
func MigrateBrightDataUsage(db *gorm.DB) error {
	if db.Dialector.Name() != "postgres" {
		return nil
	}
	if err := db.Exec(`CREATE TABLE IF NOT EXISTS fallback_usages (
		id bigserial PRIMARY KEY,
		created_at timestamptz,
		updated_at timestamptz,
		deleted_at timestamptz,
		archive_item_id bigint,
		short_id text,
		url text,
		provider text,
		product text,
		resource_id text,
		operation_id text,
		records bigint,
		bytes_transferred bigint,
		cost_usd numeric,
		success boolean,
		detail text
	)`).Error; err != nil {
		return fmt.Errorf("create fallback_usages: %w", err)
	}
	for _, ddl := range []string{
		`CREATE INDEX IF NOT EXISTS idx_fallback_usages_deleted_at ON fallback_usages (deleted_at)`,
		`CREATE INDEX IF NOT EXISTS idx_fallback_usages_archive_item_id ON fallback_usages (archive_item_id)`,
		`CREATE INDEX IF NOT EXISTS idx_fallback_usages_short_id ON fallback_usages (short_id)`,
		`CREATE INDEX IF NOT EXISTS idx_fallback_usages_provider ON fallback_usages (provider)`,
		`CREATE INDEX IF NOT EXISTS idx_fallback_usages_product ON fallback_usages (product)`,
	} {
		if err := db.Exec(ddl).Error; err != nil {
			return fmt.Errorf("index fallback_usages: %w", err)
		}
	}
	var legacyExists bool
	if err := db.Raw(`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'bright_data_usages')`).Scan(&legacyExists).Error; err != nil {
		return fmt.Errorf("probe bright_data_usages: %w", err)
	}
	if !legacyExists {
		return nil
	}
	var copied int64
	if err := db.Raw(`SELECT COUNT(*) FROM fallback_usages WHERE provider = 'brightdata'`).Scan(&copied).Error; err != nil {
		return fmt.Errorf("count copied rows: %w", err)
	}
	if copied > 0 {
		return nil
	}
	result := db.Exec(`INSERT INTO fallback_usages
		(created_at, updated_at, deleted_at, archive_item_id, short_id, url, provider, product, resource_id, operation_id, records, bytes_transferred, cost_usd, success, detail)
		SELECT b.created_at, b.updated_at, b.deleted_at, b.archive_item_id, b.short_id, b.url, 'brightdata', b.product, b.dataset_id, b.snapshot_id, b.records, b.bytes_transferred, b.cost_usd, b.success, b.detail
		FROM bright_data_usages b ORDER BY b.id`)
	if result.Error != nil {
		return fmt.Errorf("copy bright_data_usages: %w", result.Error)
	}
	return nil
}
