package workers

import (
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"

	"arker/internal/models"
	"arker/internal/utils"
)

// Spellings of one YouTube video. Every test below submits one of these and
// expects Arker to behave as though the others had been submitted.
const (
	spellingShort    = "https://youtu.be/dQw4w9WgXcQ?si=Tr4ck1ng"
	spellingWatch    = "https://www.youtube.com/watch?v=dQw4w9WgXcQ"
	spellingMobile   = "https://m.youtube.com/watch?v=dQw4w9WgXcQ&t=42s"
	spellingBareHost = "https://youtube.com/watch?v=dQw4w9WgXcQ&feature=share"
)

// seedCanonicalCapture creates a capture under an exact spelling, storing the
// canonical_url the way the backfill and the create path do.
func seedCanonicalCapture(t *testing.T, db *gorm.DB, url, shortID string, age time.Duration, items map[string]string) models.Capture {
	t.Helper()
	var u models.ArchivedURL
	if err := db.Where("original = ?", url).First(&u).Error; err != nil {
		u = models.ArchivedURL{Original: url, CanonicalURL: utils.CanonicalizeArchiveURL(url)}
		if err := db.Create(&u).Error; err != nil {
			t.Fatalf("create url: %v", err)
		}
	}
	capture := models.Capture{ArchivedURLID: u.ID, Timestamp: time.Now().Add(-age), ShortID: shortID}
	if err := db.Create(&capture).Error; err != nil {
		t.Fatalf("create capture: %v", err)
	}
	for typ, status := range items {
		if err := db.Create(&models.ArchiveItem{CaptureID: capture.ID, Type: typ, Status: status}).Error; err != nil {
			t.Fatalf("create item: %v", err)
		}
	}
	return capture
}

func countCaptures(t *testing.T, db *gorm.DB) int64 {
	t.Helper()
	var n int64
	if err := db.Model(&models.Capture{}).Count(&n).Error; err != nil {
		t.Fatalf("count captures: %v", err)
	}
	return n
}

// TestFindOrCreateMatchesAcrossSpellings is G5 in one assertion: an archive made
// under the share-link spelling answers a request made under the watch spelling.
func TestFindOrCreateMatchesAcrossSpellings(t *testing.T) {
	db := newQueueTestDB(t)
	seedCanonicalCapture(t, db, spellingShort, "shared", 90*24*time.Hour, map[string]string{"mhtml": "completed"})

	got, err := FindOrCreateCapture(t.Context(), db, nil, spellingWatch, []string{"mhtml"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Action != FindOrCreateFound || got.ShortID != "shared" {
		t.Fatalf("result = %+v, want the youtu.be capture", got)
	}
	if n := countCaptures(t, db); n != 1 {
		t.Fatalf("capture count = %d, want 1 (no duplicate archive)", n)
	}
}

// TestFindOrCreateNewestCompletionWinsAcrossIdentityRows is the core of the
// contract: newest *completed* qualifying archive regardless of age, chosen by
// completion time, across every archived_urls row sharing one canonical
// identity. The winner here is the older capture, on a different row, that
// finished more recently.
func TestFindOrCreateNewestCompletionWinsAcrossIdentityRows(t *testing.T) {
	db := newQueueTestDB(t)
	seedCanonicalCapture(t, db, spellingShort, "createdlate", 24*time.Hour, map[string]string{"mhtml": "completed"})
	seedCanonicalCapture(t, db, spellingMobile, "finishlate", 365*24*time.Hour, map[string]string{"mhtml": "completed"})
	setItemUpdatedAt(t, db, "createdlate", "mhtml", time.Now().Add(-20*time.Hour))
	setItemUpdatedAt(t, db, "finishlate", "mhtml", time.Now().Add(-1*time.Hour))

	var rows int64
	db.Model(&models.ArchivedURL{}).Count(&rows)
	if rows != 2 {
		t.Fatalf("archived_urls rows = %d, want 2 sharing one identity", rows)
	}

	got, err := FindOrCreateCapture(t.Context(), db, nil, spellingBareHost, []string{"mhtml"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Action != FindOrCreateFound || got.ShortID != "finishlate" || got.Status != "completed" {
		t.Fatalf("result = %+v, want the capture that completed most recently", got)
	}
}

// TestFindOrCreateJoinsInFlightAcrossSpellings: work already underway under one
// spelling is joined, not duplicated, by a request under another.
func TestFindOrCreateJoinsInFlightAcrossSpellings(t *testing.T) {
	db := newQueueTestDB(t)
	seedCanonicalCapture(t, db, spellingShort, "inflight", time.Minute, map[string]string{"mhtml": "completed", "screenshot": "processing"})

	got, err := FindOrCreateCapture(t.Context(), db, nil, spellingWatch, []string{"mhtml", "screenshot"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Action != FindOrCreateInProgress || got.ShortID != "inflight" || got.Status != "processing" {
		t.Fatalf("result = %+v, want to join the in-flight capture", got)
	}
	if n := countCaptures(t, db); n != 1 {
		t.Fatalf("capture count = %d, want 1", n)
	}
}

// TestFindOrCreateCreatesExactlyOnceAcrossSpellings walks the three-way rule in
// sequence: create, then join, then reuse — each under a different spelling.
func TestFindOrCreateCreatesExactlyOnceAcrossSpellings(t *testing.T) {
	db := newQueueTestDB(t)

	first, err := FindOrCreateCapture(t.Context(), db, nil, spellingShort, []string{"mhtml"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.Action != FindOrCreateCreated {
		t.Fatalf("first result = %+v, want created", first)
	}

	second, err := FindOrCreateCapture(t.Context(), db, nil, spellingWatch, []string{"mhtml"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if second.Action != FindOrCreateInProgress || second.ShortID != first.ShortID {
		t.Fatalf("second result = %+v, want to join %s", second, first.ShortID)
	}

	if err := db.Model(&models.ArchiveItem{}).
		Where("capture_id = (SELECT id FROM captures WHERE short_id = ?)", first.ShortID).
		Update("status", "completed").Error; err != nil {
		t.Fatal(err)
	}

	third, err := FindOrCreateCapture(t.Context(), db, nil, spellingMobile, []string{"mhtml"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if third.Action != FindOrCreateFound || third.ShortID != first.ShortID {
		t.Fatalf("third result = %+v, want to reuse %s", third, first.ShortID)
	}
	if n := countCaptures(t, db); n != 1 {
		t.Fatalf("capture count = %d, want exactly 1 across three spellings", n)
	}
}

// TestFindOrCreateFailedCapturesNeverQualifyAcrossSpellings: a failed item
// disqualifies its capture no matter which spelling it was archived under.
func TestFindOrCreateFailedCapturesNeverQualifyAcrossSpellings(t *testing.T) {
	db := newQueueTestDB(t)
	seedCanonicalCapture(t, db, spellingShort, "hasfailed", time.Hour, map[string]string{"mhtml": "completed", "yt-dlp": "failed"})

	got, err := FindOrCreateCapture(t.Context(), db, nil, spellingWatch, []string{"mhtml", "yt-dlp"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Action != FindOrCreateCreated {
		t.Fatalf("result = %+v, want a new capture", got)
	}
	if got.ShortID == "hasfailed" {
		t.Fatal("returned the capture with a failed item")
	}
}

// TestFindOrCreateIgnoresAliasesAcrossSpellings: alias captures own no items and
// must never be handed back as candidates, including when the alias hangs off a
// different row of the same identity.
func TestFindOrCreateIgnoresAliasesAcrossSpellings(t *testing.T) {
	db := newQueueTestDB(t)
	canonical := seedCanonicalCapture(t, db, spellingShort, "realone", 48*time.Hour, map[string]string{"mhtml": "completed"})

	aliasRow := models.ArchivedURL{Original: spellingMobile, CanonicalURL: utils.CanonicalizeArchiveURL(spellingMobile)}
	if err := db.Create(&aliasRow).Error; err != nil {
		t.Fatal(err)
	}
	alias := models.Capture{ArchivedURLID: aliasRow.ID, Timestamp: time.Now(), ShortID: "aliasone", AliasOfID: &canonical.ID}
	if err := db.Create(&alias).Error; err != nil {
		t.Fatal(err)
	}

	got, err := FindOrCreateCapture(t.Context(), db, nil, spellingWatch, []string{"mhtml"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Action != FindOrCreateFound || got.ShortID != "realone" {
		t.Fatalf("result = %+v, want the canonical capture, never the alias", got)
	}
}

// TestFindOrCreateLegacyTypeNameStillQualifies: rows still carrying the retired
// "youtube" type (the startup rename is best-effort) must keep matching a
// request for "yt-dlp", including across spellings.
func TestFindOrCreateLegacyTypeNameStillQualifies(t *testing.T) {
	db := newQueueTestDB(t)
	seedCanonicalCapture(t, db, spellingShort, "legacyyt", 200*24*time.Hour,
		map[string]string{"mhtml": "completed", "screenshot": "completed", "youtube": "completed"})

	// Empty types means "derive from the URL", which for a YouTube link is
	// mhtml + screenshot + yt-dlp.
	got, err := FindOrCreateCapture(t.Context(), db, nil, spellingWatch, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Action != FindOrCreateFound || got.ShortID != "legacyyt" {
		t.Fatalf("result = %+v, want the legacy-named capture to qualify", got)
	}
}

// TestFindOrCreateOrdinaryURLsAreUnaffected: an ordinary URL canonicalizes to
// itself, so two similar-but-distinct ordinary URLs stay distinct. This is the
// compatibility half of the change.
func TestFindOrCreateOrdinaryURLsAreUnaffected(t *testing.T) {
	db := newQueueTestDB(t)
	seedCanonicalCapture(t, db, "https://example.com/page?b=2&a=1", "ord", time.Hour, map[string]string{"mhtml": "completed"})

	for _, other := range []string{
		"https://example.com/page?a=1&b=2", // reordered params: a different page as far as we know
		"https://example.com/page/",        // trailing slash
		"https://www.example.com/page?b=2&a=1",
		"http://example.com/page?b=2&a=1",
	} {
		got, err := FindOrCreateCapture(t.Context(), db, nil, other, []string{"mhtml"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got.Action != FindOrCreateCreated {
			t.Errorf("%q returned %+v, want a new capture (ordinary URLs are never rewritten)", other, got)
		}
	}
}

// TestFindOrCreateStoresOriginalUntouched: the submitted URL is what gets
// stored, alongside the canonical identity that ties spellings together.
func TestFindOrCreateStoresOriginalUntouched(t *testing.T) {
	db := newQueueTestDB(t)
	if _, err := FindOrCreateCapture(t.Context(), db, nil, spellingShort, []string{"mhtml"}, nil); err != nil {
		t.Fatal(err)
	}

	var row models.ArchivedURL
	if err := db.Where("original = ?", spellingShort).First(&row).Error; err != nil {
		t.Fatalf("the submitted URL must be stored verbatim: %v", err)
	}
	if want := utils.CanonicalizeArchiveURL(spellingShort); row.CanonicalURL != want {
		t.Fatalf("canonical_url = %q, want %q", row.CanonicalURL, want)
	}
	if row.CanonicalURL == row.Original {
		t.Fatal("expected the canonical identity to differ from this spelling")
	}
}

// TestFindOrCreateBackfillsCanonicalOnLegacyRow: a row written before the column
// existed is matched on original and repaired in place, so the next lookup can
// use the index.
func TestFindOrCreateBackfillsCanonicalOnLegacyRow(t *testing.T) {
	db := newQueueTestDB(t)
	legacy := models.ArchivedURL{Original: spellingWatch} // canonical_url empty
	if err := db.Create(&legacy).Error; err != nil {
		t.Fatal(err)
	}

	if _, err := FindOrCreateCapture(t.Context(), db, nil, spellingWatch, []string{"mhtml"}, nil); err != nil {
		t.Fatal(err)
	}

	var reloaded models.ArchivedURL
	if err := db.First(&reloaded, legacy.ID).Error; err != nil {
		t.Fatal(err)
	}
	if want := utils.CanonicalizeArchiveURL(spellingWatch); reloaded.CanonicalURL != want {
		t.Fatalf("canonical_url = %q, want %q", reloaded.CanonicalURL, want)
	}
	var rows int64
	db.Model(&models.ArchivedURL{}).Count(&rows)
	if rows != 1 {
		t.Fatalf("archived_urls rows = %d, want 1 (the legacy row was reused)", rows)
	}
}

// TestFindOrCreateMatchesLegacyRowStoredInCanonicalForm covers the other
// backfill-lag direction: the pre-existing row happens to hold the canonical
// spelling, and the request arrives under a different one.
func TestFindOrCreateMatchesLegacyRowStoredInCanonicalForm(t *testing.T) {
	db := newQueueTestDB(t)
	legacy := models.ArchivedURL{Original: utils.CanonicalizeArchiveURL(spellingShort)} // canonical_url empty
	if err := db.Create(&legacy).Error; err != nil {
		t.Fatal(err)
	}
	capture := models.Capture{ArchivedURLID: legacy.ID, Timestamp: time.Now().Add(-time.Hour), ShortID: "legacyrow"}
	if err := db.Create(&capture).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&models.ArchiveItem{CaptureID: capture.ID, Type: "mhtml", Status: "completed"}).Error; err != nil {
		t.Fatal(err)
	}

	got, err := FindOrCreateCapture(t.Context(), db, nil, spellingShort, []string{"mhtml"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Action != FindOrCreateFound || got.ShortID != "legacyrow" {
		t.Fatalf("result = %+v, want the un-backfilled row to still match", got)
	}
}

// TestQueueCaptureAliasesAcrossSpellings: the QueueCapture freshness-window
// alias path is keyed on the same identity, so a repeat submission of a post
// under a different spelling aliases instead of re-archiving.
func TestQueueCaptureAliasesAcrossSpellings(t *testing.T) {
	db := newQueueTestDB(t)
	canonical := seedCanonicalCapture(t, db, spellingShort, "freshone", time.Minute, map[string]string{"mhtml": "completed"})

	shortID, aliasOf, createdItems, err := createCapture(db, spellingWatch, []string{"mhtml"}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if aliasOf == nil || aliasOf.ID != canonical.ID {
		t.Fatalf("aliasOf = %v, want the capture archived under the other spelling", aliasOf)
	}
	if createdItems != 0 {
		t.Fatalf("alias created %d items, want 0", createdItems)
	}

	// The alias still records the URL the caller actually submitted.
	var row models.ArchivedURL
	if err := db.Joins("JOIN captures ON captures.archived_url_id = archived_urls.id").
		Where("captures.short_id = ?", shortID).First(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.Original != spellingWatch {
		t.Fatalf("alias stored original %q, want %q", row.Original, spellingWatch)
	}
}

// TestQueueCaptureOrdinaryURLUnchanged pins POST /api/v1/archive behavior for
// ordinary URLs: same row, same alias decision, no canonical rewriting.
func TestQueueCaptureOrdinaryURLUnchanged(t *testing.T) {
	db := newQueueTestDB(t)
	url := "https://example.com/ordinary?x=1"
	canonical := seedCanonicalCapture(t, db, url, "ordcanon", time.Minute, map[string]string{"mhtml": "completed", "screenshot": "completed"})

	_, aliasOf, _, err := createCapture(db, url, []string{"mhtml", "screenshot"}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if aliasOf == nil || aliasOf.ID != canonical.ID {
		t.Fatalf("aliasOf = %v, want the fresh ordinary capture", aliasOf)
	}

	var rows int64
	db.Model(&models.ArchivedURL{}).Count(&rows)
	if rows != 1 {
		t.Fatalf("archived_urls rows = %d, want 1", rows)
	}
	var row models.ArchivedURL
	db.First(&row)
	if row.Original != url || row.CanonicalURL != url {
		t.Fatalf("row = {original: %q, canonical: %q}, want both %q", row.Original, row.CanonicalURL, url)
	}
}

// TestFindOrCreateConcurrentSpellingsCreateExactlyOneCapture is the concurrency
// contract, and the reason withCaptureIdentityLock takes an in-process lock as
// well as a Postgres advisory one.
//
// Run under -race. Without serialization on the canonical identity, every
// goroutine's lookup misses and every goroutine creates a capture; the assertion
// is that all of them agree on one short ID and the table holds exactly one row.
//
// Honest limitation: this proves the logic given a working identity lock, and it
// proves the lock is keyed on the canonical identity rather than the raw URL —
// the four spellings share a lock only because they canonicalize to one string.
// It does not exercise pg_advisory_xact_lock itself, because the suite runs on
// SQLite. TestPostgresFindOrCreateConcurrentSpellings covers that against a real
// Postgres when ARKER_TEST_POSTGRES_DSN is set.
func TestFindOrCreateConcurrentSpellingsCreateExactlyOneCapture(t *testing.T) {
	db := newQueueTestDB(t)
	spellings := []string{spellingShort, spellingWatch, spellingMobile, spellingBareHost}

	const perSpelling = 4
	results := make([]FindOrCreateResult, len(spellings)*perSpelling)
	errs := make([]error, len(results))

	var start sync.WaitGroup
	var done sync.WaitGroup
	start.Add(1)
	for i := range results {
		done.Add(1)
		go func(i int) {
			defer done.Done()
			start.Wait() // release all goroutines at once
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
			t.Fatalf("goroutine %d got short ID %q, want %q — a second archive was started",
				i, res.ShortID, results[0].ShortID)
		}
		if res.Action == FindOrCreateCreated {
			created++
		}
	}
	if created != 1 {
		t.Fatalf("%d goroutines reported 'created', want exactly 1", created)
	}
	if n := countCaptures(t, db); n != 1 {
		t.Fatalf("capture count = %d, want exactly 1", n)
	}
	var items int64
	db.Model(&models.ArchiveItem{}).Count(&items)
	if items != 1 {
		t.Fatalf("archive item count = %d, want exactly 1", items)
	}
}

// TestCaptureIdentityLockSerializesSameIdentityOnly checks the lock set itself:
// same identity is mutually exclusive, different identities never block each
// other (a global lock would serialize all archiving), and entries are released.
func TestCaptureIdentityLockSerializesSameIdentityOnly(t *testing.T) {
	locks := &identityLockSet{locks: map[string]*identityLock{}}

	unlock := locks.acquire("identity-a")
	otherAcquired := make(chan struct{})
	go func() {
		release := locks.acquire("identity-b")
		close(otherAcquired)
		release()
	}()
	select {
	case <-otherAcquired:
	case <-time.After(2 * time.Second):
		t.Fatal("a different identity blocked on an unrelated lock")
	}

	sameAcquired := make(chan struct{})
	go func() {
		release := locks.acquire("identity-a")
		close(sameAcquired)
		release()
	}()
	select {
	case <-sameAcquired:
		t.Fatal("the same identity was acquired twice concurrently")
	case <-time.After(50 * time.Millisecond):
	}

	unlock()
	select {
	case <-sameAcquired:
	case <-time.After(2 * time.Second):
		t.Fatal("waiter never acquired the released lock")
	}

	// Wait for the waiter's release to land before asserting cleanup.
	for i := 0; i < 100; i++ {
		locks.mu.Lock()
		n := len(locks.locks)
		locks.mu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("lock entries were not released; the map would grow without bound")
}
