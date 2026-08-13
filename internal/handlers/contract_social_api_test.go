package handlers

import (
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

	"gorm.io/gorm"

	"arker/internal/archivers"
	"arker/internal/models"
	"arker/internal/storage"
	"arker/internal/testfixtures"
	"arker/internal/utils"
)

// Social-archive contract tests for the unified API.
//
// The other suites prove the archivers and the pipeline behave; these prove
// the thing a caller actually sees. Where possible they seed from a *real*
// archiver run over a fixture rather than a hand-written row, so the response
// is built from bytes an extractor really produced.

// storeArchiverResult writes an archiver Result into storage the way
// saveArchiveResult does and updates the item, without depending on the
// workers package.
func storeArchiverResult(t *testing.T, store storage.Storage, db *gorm.DB,
	item *models.ArchiveItem, keyBase string, result archivers.Result) {
	t.Helper()

	data, err := io.ReadAll(result.Data)
	if err != nil {
		t.Fatalf("read artifact: %v", err)
	}
	if closer, ok := result.Data.(io.Closer); ok {
		_ = closer.Close()
	}
	key := keyBase + result.Extension
	storeTestObject(t, store, key, data)

	updates := map[string]any{
		"status": "completed", "storage_key": key, "extension": result.Extension,
		"file_size": int64(len(data)),
	}
	if result.Metadata != nil {
		metadataKey := keyBase + ".metadata.json"
		storeTestObject(t, store, metadataKey, result.Metadata.Data)
		updates["metadata_key"] = metadataKey
	}
	if result.RawMetadata != nil {
		rawKey := keyBase + ".raw-metadata.json"
		storeTestObject(t, store, rawKey, result.RawMetadata.Data)
		updates["raw_metadata_key"] = rawKey
	}
	if result.Source != "" {
		updates["source"] = result.Source
	}
	if err := db.Model(item).Updates(updates).Error; err != nil {
		t.Fatalf("update item: %v", err)
	}
}

// seedRealVideoCapture runs the real yt-dlp archiver over a fixture and stores
// what it produced against a new capture.
func seedRealVideoCapture(t *testing.T, db *gorm.DB, store storage.Storage,
	shortID, fixture string) models.Capture {
	t.Helper()
	c := testfixtures.Lookup(t, fixture)
	testfixtures.InstallFakeYtDlp(t, testfixtures.YtDlpFake{Fixture: fixture})

	capture := createVideoCapture(t, db, shortID, c.URL, map[string]string{utils.ArchiveTypeYtDlp: "processing"})
	var item models.ArchiveItem
	if err := db.Where("capture_id = ? AND type = ?", capture.ID, utils.ArchiveTypeYtDlp).First(&item).Error; err != nil {
		t.Fatalf("load item: %v", err)
	}

	archiver := &archivers.YtDlpArchiver{}
	result, err := archiver.Archive(context.Background(), c.URL, io.Discard, nil, item.ID)
	if err != nil {
		t.Fatalf("archive %s: %v", fixture, err)
	}
	storeArchiverResult(t, store, db, &item, shortID+"/yt-dlp-abcd", result)
	return capture
}

// seedRealGalleryCapture runs the real gallery-dl archiver over a fixture.
func seedRealGalleryCapture(t *testing.T, db *gorm.DB, store storage.Storage,
	shortID, fixture string, fake testfixtures.GalleryDlFake) models.Capture {
	t.Helper()
	c := testfixtures.Lookup(t, fixture)
	fake.Fixture = fixture
	testfixtures.InstallFakeGalleryDl(t, fake)

	capture := createVideoCapture(t, db, shortID, c.URL, map[string]string{utils.ArchiveTypeGalleryDl: "processing"})
	var item models.ArchiveItem
	if err := db.Where("capture_id = ? AND type = ?", capture.ID, utils.ArchiveTypeGalleryDl).First(&item).Error; err != nil {
		t.Fatalf("load item: %v", err)
	}

	archiver := &archivers.GalleryDLArchiver{}
	result, err := archiver.Archive(context.Background(), c.URL, io.Discard, nil, item.ID)
	if err != nil {
		t.Fatalf("archive %s: %v", fixture, err)
	}
	storeArchiverResult(t, store, db, &item, shortID+"/gallery-dl-abcd", result)
	return capture
}

// TestFulfilledVideoExposesTheWholeContract walks a real extraction all the
// way to the API. Contract #6: status, normalized post, Arker-hosted media
// URLs, raw metadata, provenance.
func TestFulfilledVideoExposesTheWholeContract(t *testing.T) {
	db := newHandlerLogTestDB(t)
	store := storage.NewMemoryStorage()
	seedRealVideoCapture(t, db, store, "vid01", "youtube_regular")

	code, body := getResult(t, resultRouter(db, store), "vid01")
	if code != 200 {
		t.Fatalf("status = %d", code)
	}
	social := socialOf(t, body)

	if social["fulfilled"] != true {
		t.Fatalf("fulfilled = %v, want true for a complete video archive: %#v", social["fulfilled"], social)
	}
	if social["status"] != "fulfilled" || social["terminal"] != true {
		t.Errorf("status/terminal = %v/%v, want fulfilled/true", social["status"], social["terminal"])
	}
	if social["platform"] != "youtube" {
		t.Errorf("platform = %v, want youtube", social["platform"])
	}

	post, ok := social["post"].(map[string]any)
	if !ok || post["id"] == "" || post["title"] == "" {
		t.Fatalf("post = %#v, want a normalized post with an id and title", social["post"])
	}
	if post["published_at"] == nil || post["published_at"] == "" {
		t.Error("post.published_at is empty")
	}

	media, ok := social["media"].([]any)
	if !ok || len(media) != 1 {
		t.Fatalf("media = %#v, want exactly one video entry", social["media"])
	}
	entry := media[0].(map[string]any)
	if entry["type"] != "video" {
		t.Errorf("media[0].type = %v, want video", entry["type"])
	}
	// Contract #6: Arker-hosted, not a hotlink to the platform.
	url, _ := entry["url"].(string)
	if url == "" || !hasPrefix(url, "https://archive.test/archive/vid01/") {
		t.Errorf("media[0].url = %q, want an Arker-hosted URL", url)
	}
	if size, _ := entry["size_bytes"].(float64); size <= 0 {
		t.Error("media[0].size_bytes is not set")
	}

	raw, ok := social["raw_metadata"].([]any)
	if !ok || len(raw) == 0 {
		t.Fatal("raw_metadata is empty; the sanitized provider record must stay retrievable")
	}
	if provenance, ok := social["provenance"].(map[string]any); !ok ||
		provenance["source"] != "native" || provenance["mode"] != "primary" {
		t.Errorf("provenance = %#v, want native/primary", social["provenance"])
	}
	if social["failure"] != nil {
		t.Errorf("failure = %#v, want null on a fulfilled archive", social["failure"])
	}
}

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

// TestRecognizedSocialURLWithOnlyPageCapturesIsNeverFulfilled is contract #5.
//
// This is the false-success case that matters most: a recognized social post
// whose only completed items are an MHTML page and a screenshot. A page
// snapshot of Instagram's login wall is not an archive of the post, and the
// API must say so rather than returning a green capture with no social_post.
func TestRecognizedSocialURLWithOnlyPageCapturesIsNeverFulfilled(t *testing.T) {
	socialURLs := map[string]string{
		"instagram post": "https://www.instagram.com/p/DbktPO1Eopi/",
		"instagram reel": "https://www.instagram.com/reel/DbktPO1Eopi/",
		"youtube":        "https://www.youtube.com/watch?v=aqz-KE-bpKQ",
		"tiktok":         "https://www.tiktok.com/@arkerfixture/video/7106594312292453675",
		"x status":       "https://x.com/arkerfixture/status/1929384756102938112",
		"reddit":         "https://www.reddit.com/r/aww/comments/1abcxyz/a_public_reddit_submission/",
		"bluesky":        "https://bsky.app/profile/bsky.app/post/3msqpuobiwk2t",
	}

	i := 0
	for name, sourceURL := range socialURLs {
		t.Run(name, func(t *testing.T) {
			db := newHandlerLogTestDB(t)
			store := storage.NewMemoryStorage()
			shortID := shortIDFor(i)
			i++

			// Only page-level captures completed. No yt-dlp, no gallery-dl.
			capture := createVideoCapture(t, db, shortID, sourceURL, map[string]string{
				"mhtml": "completed", "screenshot": "completed",
			})
			db.Model(&models.ArchiveItem{}).Where("capture_id = ?", capture.ID).
				Update("storage_key", shortID+"/page-abcd.mhtml")

			code, body := getResult(t, resultRouter(db, store), shortID)
			if code != 200 {
				t.Fatalf("status = %d", code)
			}

			// Contract #5: never silently non-social.
			if body["social_post"] == nil {
				t.Fatalf("social_post is null for the recognized social URL %s; "+
					"an MHTML-only capture of a social post must not read as an ordinary page", sourceURL)
			}
			social := socialOf(t, body)
			if social["fulfilled"] == true {
				t.Fatal("a capture with no social extractor item must never read fulfilled")
			}
			if social["status"] == "fulfilled" {
				t.Errorf("status = %v, want a non-fulfilled status", social["status"])
			}
			failure, ok := social["failure"].(map[string]any)
			if !ok {
				t.Fatalf("failure = %#v, want an explicit failure object", social["failure"])
			}
			if code, _ := failure["code"].(string); code == "" {
				t.Error("failure.code is empty; the reason must be machine-readable")
			}
		})
	}
}

func shortIDFor(i int) string {
	return string(rune('a'+i)) + "soc1"
}

// TestTikTokPhotoPostIsRecognizedAsSocial is G3a.
//
// A TikTok /photo/ URL matches no router rule: IsTikTokURL only accepts
// /video/ and the short-link hosts, and there is no galleryDLSites entry for
// tiktok.com. So the capture gets mhtml and screenshot only, and
// buildSocialPost returns nil because the URL is not recognized — the exact
// silent non-social success contract #5 forbids. A photo post is a post.
func TestTikTokPhotoPostIsRecognizedAsSocial(t *testing.T) {
	const sourceURL = "https://www.tiktok.com/@arkerfixture/photo/7301234567890123456"

	db := newHandlerLogTestDB(t)
	store := storage.NewMemoryStorage()
	capture := createVideoCapture(t, db, "ttp01", sourceURL, map[string]string{
		"mhtml": "completed", "screenshot": "completed",
	})
	db.Model(&models.ArchiveItem{}).Where("capture_id = ?", capture.ID).
		Update("storage_key", "ttp01/page-abcd.mhtml")

	code, body := getResult(t, resultRouter(db, store), "ttp01")
	if code != 200 {
		t.Fatalf("status = %d", code)
	}

	if body["social_post"] == nil {
		t.Fatal("G3a: social_post is null for a TikTok photo post, so a page snapshot reads as a complete archive")
	}
	social := socialOf(t, body)
	if social["fulfilled"] == true {
		t.Error("G3a: a TikTok photo post with no gallery-dl item must not read fulfilled")
	}
	if failure, ok := social["failure"].(map[string]any); !ok || failure["code"] == "" {
		t.Error("G3a: the failure reason must be explicit")
	}
}

// TestLegacyVideoCaptureIsExplicitLegacyAndNotFulfilled is contract #1's
// "unknown completeness never reads green", and G2.
//
// A capture made before structured metadata existed has a stored video and an
// empty MetadataKey. There is no normalized post behind it, so it cannot be
// fulfilled, and the reason must say it is an age problem rather than a
// failure.
func TestLegacyVideoCaptureIsExplicitLegacyAndNotFulfilled(t *testing.T) {
	db := newHandlerLogTestDB(t)
	store := storage.NewMemoryStorage()

	capture := createVideoCapture(t, db, "leg01", "https://www.youtube.com/watch?v=aqz-KE-bpKQ",
		map[string]string{utils.ArchiveTypeYtDlp: "completed"})
	// A stored artifact, but no sidecars: exactly the old shape.
	if err := db.Model(&models.ArchiveItem{}).Where("capture_id = ?", capture.ID).
		Updates(map[string]any{
			"storage_key": "leg01/yt-dlp-abcd.mp4", "file_size": 1024,
			"metadata_key": "", "raw_metadata_key": "",
		}).Error; err != nil {
		t.Fatalf("update item: %v", err)
	}

	_, body := getResult(t, resultRouter(db, store), "leg01")
	social := socialOf(t, body)

	if social["fulfilled"] == true {
		t.Fatal("a legacy capture with no normalized metadata must never read fulfilled")
	}
	failure, ok := social["failure"].(map[string]any)
	if !ok {
		t.Fatalf("failure = %#v, want an explicit failure", social["failure"])
	}
	if failure["code"] != "legacy_archive" {
		t.Errorf("failure.code = %v, want legacy_archive", failure["code"])
	}
	if failure["retryable"] != false {
		t.Errorf("failure.retryable = %v, want false: re-running cannot recover metadata "+
			"the original capture never wrote", failure["retryable"])
	}
	// The media is still there and still worth serving.
	if media, _ := social["media"].([]any); len(media) != 1 {
		t.Errorf("media = %#v, want the stored video to remain listed", social["media"])
	}
}

// TestCarouselFulfillmentRequiresEverySlide is G1 at the API boundary. This is
// where the false-green actually reaches a caller.
func TestCarouselFulfillmentRequiresEverySlide(t *testing.T) {
	t.Run("complete carousel is fulfilled", func(t *testing.T) {
		db := newHandlerLogTestDB(t)
		store := storage.NewMemoryStorage()
		seedRealGalleryCapture(t, db, store, "car01", "instagram_carousel", testfixtures.GalleryDlFake{})

		_, body := getResult(t, resultRouter(db, store), "car01")
		social := socialOf(t, body)
		if social["fulfilled"] != true {
			t.Fatalf("a complete 10-slide carousel must read fulfilled: %#v", social)
		}
		media, _ := social["media"].([]any)
		if len(media) != 10 {
			t.Errorf("media = %d entries, want all 10 slides", len(media))
		}
		videos := 0
		for _, entry := range media {
			if entry.(map[string]any)["type"] == "video" {
				videos++
			}
		}
		if videos != 1 {
			t.Errorf("media carries %d video entries, want the carousel's 1 video slide", videos)
		}
		if social["bundle_url"] == nil {
			t.Error("bundle_url is null; contract #6 requires the downloadable bundle")
		}
	})

	t.Run("partial carousel is not fulfilled", func(t *testing.T) {
		db := newHandlerLogTestDB(t)
		store := storage.NewMemoryStorage()
		seedRealGalleryCapture(t, db, store, "car02", "instagram_carousel",
			testfixtures.GalleryDlFake{Slides: 3, ExitCode: 4})

		_, body := getResult(t, resultRouter(db, store), "car02")
		social := socialOf(t, body)

		// Current behavior, and the bug: 3 of 10 slides reads fulfilled.
		media, _ := social["media"].([]any)
		if len(media) != 3 {
			t.Fatalf("media = %d entries, want the 3 slides that landed", len(media))
		}

		if social["fulfilled"] == true {
			t.Fatal("G1: a partial carousel must never read fulfilled")
		}
		if social["status"] != "partial" {
			t.Errorf("G1: status = %v, want partial", social["status"])
		}
		warnings, ok := social["warnings"].([]any)
		if !ok || len(warnings) == 0 {
			t.Fatal("G1: a partial capture must warn which slides are missing")
		}
	})
}

// TestCostAccountingSumsUsageRowsIncludingFailures is contract #3's reporting
// half. A failed paid attempt is billable, so it has to appear in the total —
// otherwise the API under-reports exactly the spend an operator most needs to
// see.
func TestCostAccountingSumsUsageRowsIncludingFailures(t *testing.T) {
	db := newHandlerLogTestDB(t)
	store := storage.NewMemoryStorage()

	capture := createVideoCapture(t, db, "cost1", "https://www.instagram.com/reel/DbktPO1Eopi/",
		map[string]string{utils.ArchiveTypeYtDlp: "completed"})
	var item models.ArchiveItem
	if err := db.Where("capture_id = ?", capture.ID).First(&item).Error; err != nil {
		t.Fatalf("load item: %v", err)
	}
	db.Model(&item).Updates(map[string]any{
		"storage_key": "cost1/yt-dlp-abcd.mp4", "source": models.ArchiveSourceBrightData,
	})

	rows := []models.BrightDataUsage{
		{ArchiveItemID: item.ID, ShortID: "cost1", Product: "web_scraper", Records: 3, CostUSD: 0.0045, Success: true},
		{ArchiveItemID: item.ID, ShortID: "cost1", Product: "web_scraper", Records: 1, CostUSD: 0.0015, Success: false},
		{ArchiveItemID: item.ID, ShortID: "cost1", Product: "browser_api", BytesTransferred: 5_000_000, CostUSD: 0.042, Success: false},
	}
	for i := range rows {
		if err := db.Create(&rows[i]).Error; err != nil {
			t.Fatalf("create usage row: %v", err)
		}
	}

	_, body := getResult(t, resultRouter(db, store), "cost1")
	cost, ok := body["cost"].(map[string]any)
	if !ok {
		t.Fatalf("cost = %#v", body["cost"])
	}

	if cost["currency"] != "USD" {
		t.Errorf("currency = %v, want USD", cost["currency"])
	}
	if cost["estimated"] != true {
		t.Error("estimated = false, but Bright Data costs are computed from configured rates")
	}
	total, _ := cost["total_usd"].(float64)
	const wantTotal = 0.0045 + 0.0015 + 0.042
	if diff := total - wantTotal; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("total_usd = %v, want %v (a failed attempt is still billed)", total, wantTotal)
	}

	breakdown, _ := cost["breakdown"].([]any)
	if len(breakdown) != 3 {
		t.Fatalf("breakdown = %d entries, want native + browser_api + web_scraper", len(breakdown))
	}
	byProduct := map[string]map[string]any{}
	for _, entry := range breakdown {
		row := entry.(map[string]any)
		key, _ := row["product"].(string)
		if key == "" {
			key = "native"
		}
		byProduct[key] = row
	}

	scraper, ok := byProduct["web_scraper"]
	if !ok {
		t.Fatal("no web_scraper breakdown row")
	}
	if ops, _ := scraper["operations"].(float64); ops != 2 {
		t.Errorf("web_scraper operations = %v, want 2 (successes and failures both count)", ops)
	}
	if successes, _ := scraper["successes"].(float64); successes != 1 {
		t.Errorf("web_scraper successes = %v, want 1", successes)
	}
	if records, _ := scraper["records"].(float64); records != 4 {
		t.Errorf("web_scraper records = %v, want 4", records)
	}

	browser, ok := byProduct["browser_api"]
	if !ok {
		t.Fatal("no browser_api breakdown row")
	}
	if successes, _ := browser["successes"].(float64); successes != 0 {
		t.Errorf("browser_api successes = %v, want 0", successes)
	}
	if bytes, _ := browser["bytes_transferred"].(float64); bytes != 5_000_000 {
		t.Errorf("browser_api bytes_transferred = %v, want 5000000", bytes)
	}

	native, ok := byProduct["native"]
	if !ok {
		t.Fatal("no native breakdown row")
	}
	if costUSD, _ := native["cost_usd"].(float64); costUSD != 0 {
		t.Errorf("native cost_usd = %v, want 0", costUSD)
	}
	// The item was rescued by Bright Data, so it is not a native success.
	if successes, _ := native["successes"].(float64); successes != 0 {
		t.Errorf("native successes = %v, want 0 for a Bright Data-sourced item", successes)
	}
}

// TestCostIsZeroAndNotEstimatedWithoutPaidOperations keeps the common case
// honest: a free archive must not report an estimate.
func TestCostIsZeroAndNotEstimatedWithoutPaidOperations(t *testing.T) {
	db := newHandlerLogTestDB(t)
	store := storage.NewMemoryStorage()
	seedRealVideoCapture(t, db, store, "free2", "youtube_shorts")

	_, body := getResult(t, resultRouter(db, store), "free2")
	cost := body["cost"].(map[string]any)

	if total, _ := cost["total_usd"].(float64); total != 0 {
		t.Errorf("total_usd = %v, want 0 for a free archive", total)
	}
	if cost["estimated"] != false {
		t.Error("estimated = true for an archive with no paid operations")
	}
	breakdown, _ := cost["breakdown"].([]any)
	if len(breakdown) != 1 {
		t.Errorf("breakdown = %d entries, want only the native row", len(breakdown))
	}
	native := breakdown[0].(map[string]any)
	if successes, _ := native["successes"].(float64); successes != 1 {
		t.Errorf("native successes = %v, want 1", successes)
	}
}

// TestBrightDataRescueIsReportedAsFallbackProvenance is contract #3's
// disclosure rule: a rescued artifact can differ in fidelity, so a caller has
// to be able to tell.
func TestBrightDataRescueIsReportedAsFallbackProvenance(t *testing.T) {
	db := newHandlerLogTestDB(t)
	store := storage.NewMemoryStorage()
	capture := seedRealVideoCapture(t, db, store, "bd001", "instagram_reel")
	if err := db.Model(&models.ArchiveItem{}).Where("capture_id = ?", capture.ID).
		Update("source", models.ArchiveSourceBrightData).Error; err != nil {
		t.Fatalf("update source: %v", err)
	}

	_, body := getResult(t, resultRouter(db, store), "bd001")
	social := socialOf(t, body)

	provenance, ok := social["provenance"].(map[string]any)
	if !ok {
		t.Fatalf("provenance = %#v", social["provenance"])
	}
	if provenance["source"] != "brightdata" || provenance["mode"] != "fallback" {
		t.Errorf("provenance = %#v, want brightdata/fallback", provenance)
	}
}

// TestAliasResolvesWithoutLosingEitherIdentifier pins contract #2's aliasing
// behavior at the API: the caller keeps the ID it asked for and also learns
// the canonical one.
func TestAliasResolvesWithoutLosingEitherIdentifier(t *testing.T) {
	db := newHandlerLogTestDB(t)
	store := storage.NewMemoryStorage()
	canonical := seedRealVideoCapture(t, db, store, "canon1", "youtube_regular")

	alias := models.Capture{
		ArchivedURLID: canonical.ArchivedURLID,
		Timestamp:     time.Now(),
		ShortID:       "alias1",
		AliasOfID:     &canonical.ID,
	}
	if err := db.Create(&alias).Error; err != nil {
		t.Fatalf("create alias: %v", err)
	}

	code, body := getResult(t, resultRouter(db, store), "alias1")
	if code != 200 {
		t.Fatalf("status = %d", code)
	}
	if body["short_id"] != "alias1" {
		t.Errorf("short_id = %v, want the requested alias1", body["short_id"])
	}
	if body["canonical_short_id"] != "canon1" {
		t.Errorf("canonical_short_id = %v, want canon1", body["canonical_short_id"])
	}
	// The alias owns no items; the response must show the canonical's work.
	social := socialOf(t, body)
	if social["fulfilled"] != true {
		t.Fatalf("an alias of a fulfilled capture must resolve to it: %#v", social)
	}
	items, _ := body["items"].([]any)
	if len(items) == 0 {
		t.Error("items is empty; the alias must expose the canonical capture's items")
	}
}

// TestSchemaVersionStaysOne guards contract #6's compatibility promise. New
// fields are additive; the version is not a place to record them.
func TestSchemaVersionStaysOne(t *testing.T) {
	db := newHandlerLogTestDB(t)
	store := storage.NewMemoryStorage()
	seedRealVideoCapture(t, db, store, "schm01", "youtube_regular")

	_, body := getResult(t, resultRouter(db, store), "schm01")
	if body["schema_version"] != "1" {
		t.Errorf("schema_version = %v, want \"1\"", body["schema_version"])
	}
	for _, field := range []string{
		"short_id", "canonical_short_id", "source_url", "archive_url",
		"submitted_at", "capture_done", "items", "cost", "social_post",
	} {
		if _, present := body[field]; !present {
			t.Errorf("response is missing the contracted field %q", field)
		}
	}
}

// TestApiNeverLeaksSecretsIntoTheResponse is contract #6's no-secrets rule at
// the boundary a caller actually reads.
func TestApiNeverLeaksSecretsIntoTheResponse(t *testing.T) {
	db := newHandlerLogTestDB(t)
	store := storage.NewMemoryStorage()
	seedRealVideoCapture(t, db, store, "sec001", "instagram_reel")

	_, body := getResult(t, resultRouter(db, store), "sec001")
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("re-encode response: %v", err)
	}
	for _, secret := range []string{"SYNTHETIC-OH", "SYNTHETIC-EFG", "SYNTHETIC-SIGNATURE", "203.0.113.7"} {
		if containsSubstring(string(encoded), secret) {
			t.Errorf("API response leaked %q", secret)
		}
	}
}

func containsSubstring(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
