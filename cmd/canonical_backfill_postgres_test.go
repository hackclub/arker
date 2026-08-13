package main

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"arker/internal/models"
)

// The SQLite tests cover the backfill's logic. This one covers the thing that
// only Postgres can answer: what actually happens to a production-shaped
// archived_urls table when this release boots. It builds the pre-change table,
// fills it, runs the real AutoMigrate and the real backfill, and times both.
//
//	ARKER_TEST_POSTGRES_DSN='postgres://user:pass@localhost:5432/arker?sslmode=disable' \
//	  go test ./cmd/ -run PostgresCanonical -v
//
// ARKER_TEST_BACKFILL_ROWS overrides the row count (default 5000) for a
// closer-to-production measurement.
func TestPostgresCanonicalURLMigrationOnLegacyTable(t *testing.T) {
	dsn := os.Getenv("ARKER_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set ARKER_TEST_POSTGRES_DSN to run the Postgres migration test")
	}
	rows := 5000
	if v := os.Getenv("ARKER_TEST_BACKFILL_ROWS"); v != "" {
		parsed, err := strconv.Atoi(v)
		if err != nil {
			t.Fatalf("ARKER_TEST_BACKFILL_ROWS=%q: %v", v, err)
		}
		rows = parsed
	}

	adminDB, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	// A dedicated database, not a schema: this test is about what happens to a
	// real production database on boot, and search_path games would change the
	// very migrator behavior under test.
	dbName := fmt.Sprintf("arker_migr_test_%d", time.Now().UnixNano()%1000000000)
	if err := adminDB.Exec(`CREATE DATABASE ` + dbName).Error; err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() { adminDB.Exec(`DROP DATABASE ` + dbName) })

	db, err := gorm.Open(postgres.Open(withDatabase(dsn, dbName)), &gorm.Config{})
	if err != nil {
		t.Fatalf("open scoped db: %v", err)
	}

	// Build the pre-change table the way production's was built: GORM
	// AutoMigrate from the model as it looked before canonical_url existed.
	// Hand-rolled DDL would only prove something about hand-rolled DDL.
	if err := db.AutoMigrate(&legacyArchivedURL{}); err != nil {
		t.Fatalf("create legacy table: %v", err)
	}
	var preColumns []string
	if err := db.Raw(`SELECT column_name FROM information_schema.columns
		WHERE table_name = 'archived_urls'`).Scan(&preColumns).Error; err != nil {
		t.Fatal(err)
	}
	if contains(preColumns, "canonical_url") {
		t.Fatal("precondition failed: the legacy table already has canonical_url")
	}

	// A production-shaped mix: mostly ordinary URLs, some social spellings that
	// will collapse onto shared identities.
	// Row count is interpolated, not bound: GORM rewrites ? placeholders and the
	// literal ? inside the watch?v= string below would be captured as one.
	seedSQL := fmt.Sprintf(`
		INSERT INTO archived_urls (created_at, updated_at, original)
		SELECT now(), now(),
		       CASE i %% 4
		         WHEN 0 THEN 'https://youtu.be/vid' || i || '?si=track'
		         WHEN 1 THEN 'https://www.youtube.com/watch?v=vid' || i
		         WHEN 2 THEN 'https://www.instagram.com/reel/code' || i || '/?igsh=x'
		         ELSE 'https://example.com/page/' || i
		       END
		FROM generate_series(1, %d) AS i`, rows)
	if err := db.Exec(seedSQL).Error; err != nil {
		t.Fatalf("seed rows: %v", err)
	}

	// Exactly the startup sequence in main.go. AutoMigrate is expected to fail
	// here — that is the bug this test exists to pin — so its error is logged
	// and tolerated, just as main.go does.
	migrateStart := time.Now()
	if err := db.AutoMigrate(&models.ArchivedURL{}); err != nil {
		t.Logf("AutoMigrate failed as expected on an existing table: %v", err)
	}
	if err := ensureCanonicalURLSchema(db); err != nil {
		t.Fatalf("ensureCanonicalURLSchema: %v", err)
	}
	migrateTook := time.Since(migrateStart)

	// The column and its index must exist, and nothing may have been dropped.
	var columns []string
	if err := db.Raw(`SELECT column_name FROM information_schema.columns
		WHERE table_name = 'archived_urls' ORDER BY column_name`).
		Scan(&columns).Error; err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"canonical_url", "created_at", "deleted_at", "id", "original", "updated_at"} {
		if !contains(columns, want) {
			t.Fatalf("column %q missing after migration; got %v", want, columns)
		}
	}
	var indexDefs []string
	if err := db.Raw(`SELECT indexdef FROM pg_indexes WHERE tablename = 'archived_urls'`).
		Scan(&indexDefs).Error; err != nil {
		t.Fatal(err)
	}
	canonicalIndexed := false
	for _, def := range indexDefs {
		if strings.Contains(def, "canonical_url") {
			canonicalIndexed = true
			// A UNIQUE index here would reject production data outright: several
			// spellings of one post legitimately share an identity.
			if strings.Contains(strings.ToUpper(def), "UNIQUE") {
				t.Fatalf("canonical_url index is UNIQUE (%s); duplicates are expected and must be allowed", def)
			}
		}
	}
	if !canonicalIndexed {
		t.Fatalf("no index on canonical_url; lookups would sequential-scan. indexes: %v", indexDefs)
	}

	// Existing rows survive the column addition untouched and unfilled.
	var unfilled int64
	db.Model(&models.ArchivedURL{}).Where("canonical_url IS NULL OR canonical_url = ''").Count(&unfilled)
	if unfilled != int64(rows) {
		t.Fatalf("%d rows unfilled straight after migration, want all %d", unfilled, rows)
	}

	// Second boot: the schema step must be idempotent.
	if err := ensureCanonicalURLSchema(db); err != nil {
		t.Fatalf("ensureCanonicalURLSchema is not idempotent: %v", err)
	}

	backfillStart := time.Now()
	if err := backfillCanonicalURLs(db); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	backfillTook := time.Since(backfillStart)

	db.Model(&models.ArchivedURL{}).Where("canonical_url IS NULL OR canonical_url = ''").Count(&unfilled)
	if unfilled != 0 {
		t.Fatalf("%d rows still unfilled after backfill", unfilled)
	}

	// The second boot must be a no-op, not a second full pass.
	rerunStart := time.Now()
	if err := backfillCanonicalURLs(db); err != nil {
		t.Fatalf("second backfill: %v", err)
	}
	rerunTook := time.Since(rerunStart)
	if rerunTook > backfillTook && backfillTook > 100*time.Millisecond {
		t.Errorf("re-run took %v vs first pass %v; it should be a single empty query", rerunTook, backfillTook)
	}

	// Ordinary URLs are stored as themselves; social spellings collapse.
	var ordinary models.ArchivedURL
	if err := db.Where("original = ?", "https://example.com/page/3").First(&ordinary).Error; err != nil {
		t.Fatal(err)
	}
	if ordinary.CanonicalURL != ordinary.Original {
		t.Errorf("ordinary URL rewritten: %q -> %q", ordinary.Original, ordinary.CanonicalURL)
	}
	var shared int64
	if err := db.Model(&models.ArchivedURL{}).
		Where("canonical_url = ?", "https://www.youtube.com/watch?v=vid4").Count(&shared).Error; err != nil {
		t.Fatal(err)
	}
	if shared != 1 {
		t.Errorf("expected exactly the youtu.be/vid4 row on that identity, got %d", shared)
	}

	t.Logf("rows=%d  AutoMigrate(add column+index)=%v  backfill=%v  re-run=%v",
		rows, migrateTook.Round(time.Millisecond), backfillTook.Round(time.Millisecond), rerunTook.Round(time.Millisecond))
}

// legacyArchivedURL is models.ArchivedURL as it existed before this change.
type legacyArchivedURL struct {
	gorm.Model
	Original string `gorm:"unique"`
}

func (legacyArchivedURL) TableName() string { return "archived_urls" }

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func withDatabase(dsn, dbName string) string {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		if parsed, err := url.Parse(dsn); err == nil {
			parsed.Path = "/" + dbName
			return parsed.String()
		}
	}
	return strings.TrimSpace(dsn) + " dbname=" + dbName
}
