package utils

import (
	"fmt"
	"os"
	"testing"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// Non-Postgres deployments get the column from AutoMigrate, which works there,
// so this must be a clean no-op rather than an error.
func TestEnsureCompletenessSchemaSkipsNonPostgres(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:completeness-skip?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := EnsureCompletenessSchema(db); err != nil {
		t.Fatalf("EnsureCompletenessSchema on sqlite: %v", err)
	}
}

// The case that matters: a table that already exists without the column, which
// is what production is. AutoMigrate cannot add it there — it fails while
// probing column types and startup continues past the error — so this explicit
// DDL is the only thing that actually creates it.
func TestPostgresEnsureCompletenessSchemaOnExistingTable(t *testing.T) {
	dsn := os.Getenv("ARKER_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set ARKER_TEST_POSTGRES_DSN to run Postgres integration test")
	}

	adminDB, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open postgres admin db: %v", err)
	}
	schema := fmt.Sprintf("arker_completeness_test_%d", time.Now().UnixNano())
	if err := adminDB.Exec(`CREATE SCHEMA ` + schema).Error; err != nil {
		t.Fatalf("create test schema: %v", err)
	}
	defer adminDB.Exec(`DROP SCHEMA ` + schema + ` CASCADE`)

	db, err := gorm.Open(postgres.Open(dsnWithSearchPath(dsn, schema)), &gorm.Config{})
	if err != nil {
		t.Fatalf("open postgres test schema db: %v", err)
	}

	// The pre-change table, with a row in it.
	if err := db.Exec(`CREATE TABLE archive_items (
		id bigserial PRIMARY KEY,
		created_at timestamptz,
		updated_at timestamptz,
		deleted_at timestamptz,
		capture_id bigint,
		type text,
		status text,
		storage_key text
	)`).Error; err != nil {
		t.Fatalf("create pre-change table: %v", err)
	}
	if err := db.Exec(`INSERT INTO archive_items (type, status, storage_key) VALUES ('gallery-dl', 'completed', 'abc12/gallery-dl-9f2c.zip')`).Error; err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}

	// Twice, because a restart or a rolling deploy will do exactly that.
	for attempt := 1; attempt <= 2; attempt++ {
		if err := EnsureCompletenessSchema(db); err != nil {
			t.Fatalf("EnsureCompletenessSchema attempt %d: %v", attempt, err)
		}
	}

	var count int64
	if err := db.Raw(`SELECT count(*) FROM information_schema.columns
		WHERE table_schema = ? AND table_name = 'archive_items' AND column_name = 'completeness'`, schema).
		Scan(&count).Error; err != nil {
		t.Fatalf("inspect columns: %v", err)
	}
	if count != 1 {
		t.Fatalf("completeness column count = %d, want exactly 1", count)
	}

	// The existing row must survive untouched, reading as legacy-unknown.
	var row struct {
		StorageKey   string
		Completeness *string
	}
	if err := db.Raw(`SELECT storage_key, completeness FROM archive_items`).Scan(&row).Error; err != nil {
		t.Fatalf("read row back: %v", err)
	}
	if row.StorageKey != "abc12/gallery-dl-9f2c.zip" {
		t.Fatalf("existing row was disturbed: %+v", row)
	}
	if row.Completeness != nil && *row.Completeness != "" {
		t.Fatalf("completeness = %v, want NULL/empty for a pre-existing row", *row.Completeness)
	}
}
