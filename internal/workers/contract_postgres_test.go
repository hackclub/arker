package workers

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"arker/internal/models"
)

// postgresTestDB returns a gorm handle on a throwaway Postgres schema, or
// skips the test when no DSN is configured.
//
// Some of the contract is only expressible against Postgres: FindOrCreate
// serializes concurrent callers with pg_advisory_xact_lock, which queue.go
// deliberately skips on SQLite. Asserting concurrency safety on SQLite would
// assert something production does not do. This mirrors the gate and the
// per-run schema isolation already used by
// internal/utils/archive_item_logs_postgres_test.go, and CI sets the DSN.
func postgresTestDB(t *testing.T) (*gorm.DB, func()) {
	t.Helper()
	dsn := os.Getenv("ARKER_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set ARKER_TEST_POSTGRES_DSN to run the Postgres concurrency contract test")
	}

	adminDB, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open postgres admin db: %v", err)
	}

	schema := fmt.Sprintf("arker_contract_%d", time.Now().UnixNano())
	if err := adminDB.Exec(`CREATE SCHEMA ` + schema).Error; err != nil {
		t.Fatalf("create test schema: %v", err)
	}
	cleanup := func() { adminDB.Exec(`DROP SCHEMA ` + schema + ` CASCADE`) }

	db, err := gorm.Open(postgres.Open(dsnWithContractSearchPath(dsn, schema)), &gorm.Config{})
	if err != nil {
		cleanup()
		t.Fatalf("open postgres test schema: %v", err)
	}
	if err := db.AutoMigrate(&models.ArchivedURL{}, &models.Capture{}, &models.ArchiveItem{},
		&models.ArchiveItemLog{}, &models.FallbackUsage{}, &models.Config{}); err != nil {
		cleanup()
		t.Fatalf("migrate postgres test schema: %v", err)
	}
	return db, cleanup
}

func dsnWithContractSearchPath(dsn, schema string) string {
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
