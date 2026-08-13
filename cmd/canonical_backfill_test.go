package main

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"arker/internal/models"
	"arker/internal/utils"
)

var backfillDBSeq atomic.Uint64

func newBackfillTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := fmt.Sprintf("file:%s-%d?mode=memory&cache=shared", t.Name(), backfillDBSeq.Add(1))
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&models.ArchivedURL{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// insertLegacyRow writes a row the way a pre-canonical_url release would have:
// original set, canonical_url absent.
func insertLegacyRow(t *testing.T, db *gorm.DB, original string) models.ArchivedURL {
	t.Helper()
	row := models.ArchivedURL{Original: original}
	if err := db.Create(&row).Error; err != nil {
		t.Fatalf("create legacy row %q: %v", original, err)
	}
	if err := db.Model(&models.ArchivedURL{}).Where("id = ?", row.ID).
		UpdateColumn("canonical_url", nil).Error; err != nil {
		t.Fatalf("clear canonical_url: %v", err)
	}
	return row
}

func canonicalOf(t *testing.T, db *gorm.DB, id uint) string {
	t.Helper()
	var row models.ArchivedURL
	if err := db.First(&row, id).Error; err != nil {
		t.Fatalf("reload row %d: %v", id, err)
	}
	return row.CanonicalURL
}

func TestBackfillCanonicalURLs(t *testing.T) {
	db := newBackfillTestDB(t)

	social := insertLegacyRow(t, db, "https://youtu.be/dQw4w9WgXcQ?si=abc")
	otherSpelling := insertLegacyRow(t, db, "https://www.youtube.com/watch?v=dQw4w9WgXcQ")
	ordinary := insertLegacyRow(t, db, "https://example.com/page?b=2&a=1")

	if err := backfillCanonicalURLs(db); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	want := "https://www.youtube.com/watch?v=dQw4w9WgXcQ"
	if got := canonicalOf(t, db, social.ID); got != want {
		t.Errorf("social row canonical_url = %q, want %q", got, want)
	}
	// Duplicates across rows are expected, not an error: this is exactly why the
	// column is indexed rather than unique.
	if got := canonicalOf(t, db, otherSpelling.ID); got != want {
		t.Errorf("second spelling canonical_url = %q, want %q", got, want)
	}
	// An ordinary URL canonicalizes to itself: no rewriting, ever.
	if got := canonicalOf(t, db, ordinary.ID); got != "https://example.com/page?b=2&a=1" {
		t.Errorf("ordinary row canonical_url = %q, want the original unchanged", got)
	}
}

// TestBackfillCanonicalURLsIsIdempotent is the property that makes it safe to
// leave in the startup path forever: a second run must touch nothing.
func TestBackfillCanonicalURLsIsIdempotent(t *testing.T) {
	db := newBackfillTestDB(t)
	row := insertLegacyRow(t, db, "https://www.instagram.com/reel/CxYz-123_/?igsh=xyz")

	if err := backfillCanonicalURLs(db); err != nil {
		t.Fatalf("first backfill: %v", err)
	}
	first := canonicalOf(t, db, row.ID)

	// Mark the row so a second write would be visible.
	var before models.ArchivedURL
	db.First(&before, row.ID)

	if err := backfillCanonicalURLs(db); err != nil {
		t.Fatalf("second backfill: %v", err)
	}
	var after models.ArchivedURL
	db.First(&after, row.ID)

	if after.CanonicalURL != first {
		t.Errorf("canonical_url changed on re-run: %q -> %q", first, after.CanonicalURL)
	}
	if !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Error("re-run rewrote updated_at; the backfill must not touch already-populated rows")
	}
}

// TestBackfillCanonicalURLsPreservesUpdatedAt: operators read updated_at as
// "when did this URL last change". A migration that bumps it across the whole
// table destroys that.
func TestBackfillCanonicalURLsPreservesUpdatedAt(t *testing.T) {
	db := newBackfillTestDB(t)
	row := insertLegacyRow(t, db, "https://x.com/jack/status/20?s=20")
	longAgo := time.Now().Add(-365 * 24 * time.Hour)
	if err := db.Model(&models.ArchivedURL{}).Where("id = ?", row.ID).
		UpdateColumn("updated_at", longAgo).Error; err != nil {
		t.Fatal(err)
	}

	if err := backfillCanonicalURLs(db); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	var after models.ArchivedURL
	db.First(&after, row.ID)
	if after.UpdatedAt.Sub(longAgo).Abs() > time.Second {
		t.Errorf("updated_at moved from %v to %v", longAgo, after.UpdatedAt)
	}
	if after.CanonicalURL != "https://x.com/i/web/status/20" {
		t.Errorf("canonical_url = %q", after.CanonicalURL)
	}
}

// TestBackfillCanonicalURLsSpansManyBatches walks past the batch boundary, which
// is the part that has to be right for a 50k-row production table: the id
// cursor must advance and every row must be covered exactly once.
func TestBackfillCanonicalURLsSpansManyBatches(t *testing.T) {
	db := newBackfillTestDB(t)

	const rows = canonicalURLBackfillBatch*2 + 7
	batch := make([]models.ArchivedURL, 0, rows)
	for i := 0; i < rows; i++ {
		batch = append(batch, models.ArchivedURL{Original: fmt.Sprintf("https://youtu.be/vid%08d", i)})
	}
	if err := db.CreateInBatches(&batch, 500).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := db.Model(&models.ArchivedURL{}).Where("1 = 1").
		UpdateColumn("canonical_url", nil).Error; err != nil {
		t.Fatalf("clear canonical_url: %v", err)
	}

	if err := backfillCanonicalURLs(db); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	var remaining int64
	if err := db.Model(&models.ArchivedURL{}).
		Where("canonical_url IS NULL OR canonical_url = ''").Count(&remaining).Error; err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("%d rows left unfilled across %d batches", remaining, rows/canonicalURLBackfillBatch+1)
	}

	var sample models.ArchivedURL
	if err := db.Where("original = ?", "https://youtu.be/vid00002000").First(&sample).Error; err != nil {
		t.Fatal(err)
	}
	if sample.CanonicalURL != "https://www.youtube.com/watch?v=vid00002000" {
		t.Fatalf("row past the second batch boundary has canonical_url %q", sample.CanonicalURL)
	}
}

// TestBackfillCanonicalURLsResumesAfterPartialRun: a boot that dies mid-backfill
// (deploy, OOM, restart) must be recoverable by simply running again.
func TestBackfillCanonicalURLsResumesAfterPartialRun(t *testing.T) {
	db := newBackfillTestDB(t)
	done := insertLegacyRow(t, db, "https://youtu.be/alreadyDone")
	pending := insertLegacyRow(t, db, "https://youtu.be/notYetDone")

	// Simulate the first half having completed before the crash.
	if err := db.Model(&models.ArchivedURL{}).Where("id = ?", done.ID).
		UpdateColumn("canonical_url", utils.CanonicalizeArchiveURL("https://youtu.be/alreadyDone")).Error; err != nil {
		t.Fatal(err)
	}

	if err := backfillCanonicalURLs(db); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if got := canonicalOf(t, db, pending.ID); got != "https://www.youtube.com/watch?v=notYetDone" {
		t.Errorf("row after the crash point was not filled: %q", got)
	}
}

// TestBackfillCanonicalURLsTerminatesOnEmptyCanonical: a row whose canonical
// value is itself empty would be re-selected forever by the IS NULL filter
// alone. The id cursor is what stops it, and this is the regression test for
// that being removed.
func TestBackfillCanonicalURLsTerminatesOnEmptyCanonical(t *testing.T) {
	db := newBackfillTestDB(t)
	if err := db.Exec(`INSERT INTO archived_urls (created_at, updated_at, original, canonical_url) VALUES (?, ?, '', NULL)`,
		time.Now(), time.Now()).Error; err != nil {
		t.Fatal(err)
	}
	insertLegacyRow(t, db, "https://youtu.be/afterTheEmptyRow")

	finished := make(chan error, 1)
	go func() { finished <- backfillCanonicalURLs(db) }()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatalf("backfill: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("backfill did not terminate: a row with an empty canonical value loops forever")
	}
}
