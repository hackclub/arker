package utils

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"arker/internal/models"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func TestPostgresArchiveItemLogSchemaAndBackfill(t *testing.T) {
	dsn := os.Getenv("ARKER_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set ARKER_TEST_POSTGRES_DSN to run Postgres integration test")
	}

	adminDB, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open postgres admin db: %v", err)
	}

	schema := fmt.Sprintf("arker_log_test_%d", time.Now().UnixNano())
	if err := adminDB.Exec(`CREATE SCHEMA ` + schema).Error; err != nil {
		t.Fatalf("create test schema: %v", err)
	}
	defer adminDB.Exec(`DROP SCHEMA ` + schema + ` CASCADE`)

	db, err := gorm.Open(postgres.Open(dsnWithSearchPath(dsn, schema)), &gorm.Config{})
	if err != nil {
		t.Fatalf("open postgres test schema db: %v", err)
	}

	var currentSchema string
	if err := db.Raw(`SELECT current_schema()`).Scan(&currentSchema).Error; err != nil {
		t.Fatalf("read current schema: %v", err)
	}
	if currentSchema != schema {
		t.Fatalf("current schema = %q, want %q", currentSchema, schema)
	}

	if err := db.AutoMigrate(&models.ArchivedURL{}, &models.Capture{}, &models.ArchiveItem{}, &models.ArchiveItemLog{}); err != nil {
		t.Fatalf("auto migrate: %v", err)
	}
	if err := ConfigureArchiveItemLogSchema(db); err != nil {
		t.Fatalf("configure archive item log schema: %v", err)
	}

	archivedURL := models.ArchivedURL{Original: "https://example.com/postgres-log-test"}
	if err := db.Create(&archivedURL).Error; err != nil {
		t.Fatalf("create archived url: %v", err)
	}
	capture := models.Capture{
		ArchivedURLID: archivedURL.ID,
		Timestamp:     time.Now(),
		ShortID:       "pglog",
	}
	if err := db.Create(&capture).Error; err != nil {
		t.Fatalf("create capture: %v", err)
	}

	legacy := "postgres legacy\n" + strings.Repeat("x", MaxArchiveLogChunkBytes+25) + "\n"
	item := models.ArchiveItem{CaptureID: capture.ID, Type: "web", Status: "failed", Logs: legacy}
	if err := db.Create(&item).Error; err != nil {
		t.Fatalf("create legacy archive item: %v", err)
	}

	if err := BackfillLegacyArchiveItemLogs(db); err != nil {
		t.Fatalf("backfill legacy logs: %v", err)
	}

	var reloaded models.ArchiveItem
	if err := db.First(&reloaded, item.ID).Error; err != nil {
		t.Fatalf("reload archive item: %v", err)
	}
	if reloaded.Logs != "" {
		t.Fatalf("legacy logs were not cleared: %q", reloaded.Logs)
	}

	got, err := ArchiveItemLogString(db, item.ID, reloaded.Logs)
	if err != nil {
		t.Fatalf("reconstruct archive item logs: %v", err)
	}
	if got != legacy {
		t.Fatalf("reconstructed logs mismatch: got %q want %q", got, legacy)
	}

	var maxChunkBytes int
	if err := db.Raw(`SELECT COALESCE(MAX(octet_length(chunk)), 0) FROM archive_item_logs`).Scan(&maxChunkBytes).Error; err != nil {
		t.Fatalf("read max chunk size: %v", err)
	}
	if maxChunkBytes > MaxArchiveLogChunkBytes {
		t.Fatalf("max chunk size = %d, want <= %d", maxChunkBytes, MaxArchiveLogChunkBytes)
	}

	var constraintCount int
	if err := db.Raw(`
		SELECT count(*)
		FROM pg_constraint
		WHERE conname = 'archive_item_logs_chunk_max_bytes'
		  AND conrelid = 'archive_item_logs'::regclass
	`).Scan(&constraintCount).Error; err != nil {
		t.Fatalf("read check constraint: %v", err)
	}
	if constraintCount != 1 {
		t.Fatalf("constraint count = %d, want 1", constraintCount)
	}

	var storageMode string
	if err := db.Raw(`
		SELECT attstorage
		FROM pg_attribute
		WHERE attrelid = 'archive_item_logs'::regclass
		  AND attname = 'chunk'
	`).Scan(&storageMode).Error; err != nil {
		t.Fatalf("read chunk storage mode: %v", err)
	}
	if storageMode != "p" {
		t.Fatalf("chunk storage mode = %q, want PLAIN ('p')", storageMode)
	}
}

func dsnWithSearchPath(dsn, schema string) string {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		parsed, err := url.Parse(dsn)
		if err == nil {
			query := parsed.Query()
			query.Set("search_path", schema)
			parsed.RawQuery = query.Encode()
			return parsed.String()
		}
	}
	return strings.TrimSpace(dsn) + " search_path=" + schema
}
