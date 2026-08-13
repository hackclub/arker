package workers

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"arker/internal/models"
	"arker/internal/utils"
)

// The SQLite suite proves the find-or-create logic and that it is serialized on
// the canonical identity, but SQLite has no advisory locks, so the deployed
// mechanism — pg_advisory_xact_lock(hashtext(canonical)) — is unreachable
// there. These tests close that gap against a real Postgres. Run with:
//
//	ARKER_TEST_POSTGRES_DSN='postgres://user:pass@localhost:5432/arker?sslmode=disable' \
//	  go test ./internal/workers/ -run Postgres
//
// Each test works inside its own schema and drops it afterwards.

func newPostgresTestDB(t *testing.T) (*gorm.DB, string) {
	t.Helper()
	dsn := os.Getenv("ARKER_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set ARKER_TEST_POSTGRES_DSN to run Postgres integration tests")
	}

	adminDB, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open postgres admin db: %v", err)
	}
	schema := fmt.Sprintf("arker_queue_test_%d", time.Now().UnixNano())
	if err := adminDB.Exec(`CREATE SCHEMA ` + schema).Error; err != nil {
		t.Fatalf("create test schema: %v", err)
	}
	t.Cleanup(func() { adminDB.Exec(`DROP SCHEMA ` + schema + ` CASCADE`) })

	scopedDSN := postgresDSNWithSearchPath(dsn, schema)
	db, err := gorm.Open(postgres.Open(scopedDSN), &gorm.Config{})
	if err != nil {
		t.Fatalf("open postgres test schema db: %v", err)
	}
	if err := db.AutoMigrate(&models.ArchivedURL{}, &models.Capture{}, &models.ArchiveItem{}, &models.Config{}); err != nil {
		t.Fatalf("auto migrate: %v", err)
	}
	return db, scopedDSN
}

func postgresDSNWithSearchPath(dsn, schema string) string {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		if parsed, err := url.Parse(dsn); err == nil {
			query := parsed.Query()
			query.Set("search_path", schema)
			parsed.RawQuery = query.Encode()
			return parsed.String()
		}
	}
	return strings.TrimSpace(dsn) + " search_path=" + schema
}

// TestPostgresAdvisoryLockOnCanonicalIdentity exercises the lock primitive
// itself, on two separate connections, with the exact statement the production
// path runs.
//
// This is the assertion the in-process mutex cannot make: within one process
// that mutex would serialize the two goroutines before Postgres ever saw them,
// so the advisory lock has to be tested directly to know it works at all.
// It proves three things: the statement is valid Postgres (hashtext accepts a
// bound text parameter), two different spellings of one post contend because
// they hash to the same key, and two different posts do not.
func TestPostgresAdvisoryLockOnCanonicalIdentity(t *testing.T) {
	db, dsn := newPostgresTestDB(t)
	_ = db

	// Unique per run so concurrent runs cannot collide on a cluster-global key.
	videoID := fmt.Sprintf("t%d", time.Now().UnixNano()%100000000)
	spellingA := "https://youtu.be/" + videoID + "?si=abc"
	spellingB := "https://www.youtube.com/watch?v=" + videoID
	otherPost := "https://www.youtube.com/watch?v=" + videoID + "x"

	canonicalA := utils.CanonicalizeArchiveURL(spellingA)
	canonicalB := utils.CanonicalizeArchiveURL(spellingB)
	if canonicalA != canonicalB {
		t.Fatalf("precondition failed: %q and %q canonicalize differently", spellingA, spellingB)
	}

	conn := func() *gorm.DB {
		c, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
		if err != nil {
			t.Fatalf("open connection: %v", err)
		}
		return c
	}
	lock := func(tx *gorm.DB, identity string) error {
		return tx.Exec("SELECT pg_advisory_xact_lock(hashtext(?))", identity).Error
	}

	// Holder takes the lock for spelling A and keeps its transaction open.
	holder := conn().Begin()
	if err := lock(holder, canonicalA); err != nil {
		holder.Rollback()
		t.Fatalf("holder lock: %v", err)
	}

	// A different post must not contend.
	free := conn().Begin()
	freeDone := make(chan error, 1)
	go func() { freeDone <- lock(free, utils.CanonicalizeArchiveURL(otherPost)) }()
	select {
	case err := <-freeDone:
		if err != nil {
			t.Fatalf("unrelated identity lock: %v", err)
		}
	case <-time.After(5 * time.Second):
		holder.Rollback()
		free.Rollback()
		t.Fatal("a different post blocked on an unrelated identity: the lock key is too coarse")
	}
	free.Rollback()

	// The other spelling of the same post must block until the holder commits.
	waiter := conn().Begin()
	waiterDone := make(chan error, 1)
	go func() { waiterDone <- lock(waiter, canonicalB) }()
	select {
	case err := <-waiterDone:
		holder.Rollback()
		waiter.Rollback()
		t.Fatalf("the other spelling acquired the lock while it was held (err=%v): concurrent submissions would both archive", err)
	case <-time.After(300 * time.Millisecond):
	}

	if err := holder.Commit().Error; err != nil {
		t.Fatalf("commit holder: %v", err)
	}
	select {
	case err := <-waiterDone:
		if err != nil {
			t.Fatalf("waiter lock after release: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiter never acquired the lock after the holder committed")
	}
	waiter.Rollback()
}

// TestPostgresFindOrCreateConcurrentSpellings runs the real thing end to end on
// Postgres: AutoMigrate must produce the canonical_url column and its index, the
// identity lookup must work with Postgres' IN semantics, and concurrent
// submissions of four spellings must yield exactly one capture.
func TestPostgresFindOrCreateConcurrentSpellings(t *testing.T) {
	db, _ := newPostgresTestDB(t)

	videoID := fmt.Sprintf("p%d", time.Now().UnixNano()%100000000)
	spellings := []string{
		"https://youtu.be/" + videoID + "?si=Tr4ck",
		"https://www.youtube.com/watch?v=" + videoID,
		"https://m.youtube.com/watch?v=" + videoID + "&t=42s",
		"https://youtube.com/watch?v=" + videoID + "&feature=share",
	}

	const perSpelling = 4
	results := make([]FindOrCreateResult, len(spellings)*perSpelling)
	errs := make([]error, len(results))
	var start, done sync.WaitGroup
	start.Add(1)
	for i := range results {
		done.Add(1)
		go func(i int) {
			defer done.Done()
			start.Wait()
			results[i], errs[i] = FindOrCreateCapture(t.Context(), db, nil, spellings[i%len(spellings)], []string{"mhtml"}, nil)
		}(i)
	}
	start.Done()
	done.Wait()

	created := 0
	for i, res := range results {
		if errs[i] != nil {
			t.Fatalf("goroutine %d: %v", i, errs[i])
		}
		if res.ShortID != results[0].ShortID {
			t.Fatalf("goroutine %d got %q, want %q", i, res.ShortID, results[0].ShortID)
		}
		if res.Action == FindOrCreateCreated {
			created++
		}
	}
	if created != 1 {
		t.Fatalf("%d goroutines reported 'created', want exactly 1", created)
	}
	var captures int64
	db.Model(&models.Capture{}).Count(&captures)
	if captures != 1 {
		t.Fatalf("capture count = %d, want 1", captures)
	}

	// Every spelling kept its own row, each carrying the shared identity.
	var rows []models.ArchivedURL
	if err := db.Order("id").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("archived_urls rows = %d, want 1 (only the winner creates a row)", len(rows))
	}
	if rows[0].CanonicalURL != utils.CanonicalizeArchiveURL(rows[0].Original) {
		t.Fatalf("row canonical_url = %q, want %q", rows[0].CanonicalURL, utils.CanonicalizeArchiveURL(rows[0].Original))
	}
}

// TestPostgresFindOrCreateAcrossIdentityRows pins the multi-row case on the
// database that actually runs it: two archived_urls rows sharing one canonical
// identity, and the newest *completed* capture wins across both.
func TestPostgresFindOrCreateAcrossIdentityRows(t *testing.T) {
	db, _ := newPostgresTestDB(t)

	videoID := fmt.Sprintf("m%d", time.Now().UnixNano()%100000000)
	older := "https://youtu.be/" + videoID
	newer := "https://www.youtube.com/watch?v=" + videoID

	for i, spec := range []struct {
		url         string
		shortID     string
		completedAt time.Time
	}{
		{older, "pgold", time.Now().Add(-2 * time.Hour)},
		{newer, "pgnew", time.Now().Add(-30 * time.Minute)},
	} {
		row := models.ArchivedURL{Original: spec.url, CanonicalURL: utils.CanonicalizeArchiveURL(spec.url)}
		if err := db.Create(&row).Error; err != nil {
			t.Fatal(err)
		}
		// The row created second is the *older* capture, so "newest completed"
		// cannot be satisfied by row order.
		capture := models.Capture{ArchivedURLID: row.ID, Timestamp: time.Now().Add(-time.Duration(i) * 24 * time.Hour), ShortID: spec.shortID}
		if err := db.Create(&capture).Error; err != nil {
			t.Fatal(err)
		}
		item := models.ArchiveItem{CaptureID: capture.ID, Type: "mhtml", Status: "completed"}
		if err := db.Create(&item).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Model(&models.ArchiveItem{}).Where("id = ?", item.ID).
			UpdateColumn("updated_at", spec.completedAt).Error; err != nil {
			t.Fatal(err)
		}
	}

	got, err := FindOrCreateCapture(t.Context(), db, nil, "https://m.youtube.com/watch?v="+videoID, []string{"mhtml"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Action != FindOrCreateFound || got.ShortID != "pgnew" {
		t.Fatalf("result = %+v, want the capture that completed most recently across both rows", got)
	}
}
