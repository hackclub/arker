package brightdata

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"arker/internal/archivers"
	"arker/internal/models"
	"arker/internal/utils"
)

const tiktokVideoURL = "https://www.tiktok.com/@tiktok/video/7673169793343622430"

func TestTikTokVideoURLsFromFixture(t *testing.T) {
	record := loadRecords(t, "tiktok_post.json")[0]

	urls := tiktokVideoURLs(record)
	if len(urls) != 2 {
		t.Fatalf("candidate URLs = %d; want video_url and cdn_link", len(urls))
	}
	if urls[0] != stringField(record, "video_url") || urls[1] != stringField(record, "cdn_link") {
		t.Errorf("candidate order = %v; want video_url first", urls)
	}
}

func TestArchiveTikTokVideo(t *testing.T) {
	record := loadRecords(t, "tiktok_post.json")[0]
	network := newFakeNetwork(record)
	// preview_image downloads from Arker's own connection; the video does not.
	previewURL := stringField(record, "preview_image")
	network.serve(previewURL, fakePNG(t))

	video := fakeMP4(64 * 1024)
	page := newFakePage()
	page.media[stringField(record, "video_url")] = video

	client, db := newTestClient(t, network)
	sessions := withFakeBrowser(client, page)

	var log strings.Builder
	result, err := client.archiveTikTok(context.Background(), tiktokVideoURL, utils.ArchiveTypeYtDlp, &log, db, 21, "tt001")
	if err != nil {
		t.Fatalf("archiveTikTok: %v", err)
	}

	if result.Extension != ".mp4" || result.ContentType != "video/mp4" {
		t.Errorf("result = %s/%s; want an MP4", result.Extension, result.ContentType)
	}
	if result.Completeness != archivers.CompletenessComplete {
		t.Errorf("completeness = %q", result.Completeness)
	}
	if result.Source != models.ArchiveSourceBrightData {
		t.Errorf("source = %q", result.Source)
	}
	if got := readResult(t, result); len(got) != len(video) {
		t.Errorf("stored %d bytes; want %d", len(got), len(video))
	}
	if result.Thumbnail == nil {
		t.Error("no thumbnail from preview_image")
	}
	if *sessions != 1 {
		t.Errorf("opened %d browser sessions; want 1", *sessions)
	}
	if !page.closed {
		t.Error("browser session was not closed; an open session keeps billing")
	}

	// The video bytes must never be attempted over Arker's own connection:
	// those URLs are signed for Bright Data's resolver and answer us with 403.
	for _, requested := range network.requestedURLs() {
		if strings.Contains(requested, "v16-webapp-prime") {
			t.Errorf("video URL fetched over Arker's own connection: %s", requested)
		}
	}
	if !network.requested(previewURL) {
		t.Error("preview image was not downloaded directly")
	}

	meta := videoMetadataFromSidecar(t, result.Metadata)
	if meta.Platform != "tiktok" || meta.PostID != "7673169793343622430" {
		t.Errorf("platform/post id = %q/%q", meta.Platform, meta.PostID)
	}
	if meta.PublicationTimestamp != "2026-08-12T15:37:54Z" {
		t.Errorf("publication timestamp = %q", meta.PublicationTimestamp)
	}
	if meta.Author != "TikTok" {
		t.Errorf("author = %q", meta.Author)
	}
	if meta.DurationSeconds == nil || *meta.DurationSeconds != 64 {
		t.Errorf("duration = %v; want 64", meta.DurationSeconds)
	}
	if got := intValue(meta.Engagement.Views); got != 60300 {
		t.Errorf("views = %d", got)
	}
	if got := intValue(meta.Engagement.Likes); got != 1807 {
		t.Errorf("likes = %d", got)
	}
	if got := intValue(meta.Engagement.Comments); got != 392 {
		t.Errorf("comments = %d", got)
	}
	if got := intValue(meta.Engagement.Reposts); got != 99 {
		t.Errorf("shares = %d", got)
	}
	if meta.Media.SizeBytes != int64(len(video)) || meta.Media.QualityLabel != "720p" {
		t.Errorf("media = %d bytes %q", meta.Media.SizeBytes, meta.Media.QualityLabel)
	}

	// Everything TikTok signs must be redacted before it is stored.
	assertNoSignedParamsAtRest(t, "tiktok raw metadata sidecar", result.RawMetadata.Data)
	raw := string(result.RawMetadata.Data)
	if strings.Contains(raw, "3Rg+tXmt") {
		t.Error("tt_chain_token stored at rest")
	}
	if strings.Contains(raw, "x-signature=hHDdOA") {
		t.Error("preview image signature stored at rest")
	}

	// Two billable operations: the dataset record, then the browser session.
	rows := usageRows(t, db)
	if len(rows) != 2 {
		t.Fatalf("usage rows = %d; want 2 (web_scraper + browser_api)", len(rows))
	}
	scrape, browser := rows[0], rows[1]
	if scrape.Product != "web_scraper" || scrape.DatasetID != DatasetTikTokPosts || !scrape.Success {
		t.Errorf("scrape usage row = %+v", scrape)
	}
	if scrape.CostUSD != 0.0015 {
		t.Errorf("scrape cost = %v; want 0.0015", scrape.CostUSD)
	}
	if browser.Product != "browser_api" || !browser.Success {
		t.Errorf("browser usage row = %+v", browser)
	}
	wantBytes := int64(len(video)) + browserPageOverheadBytes
	if browser.BytesTransferred != wantBytes {
		t.Errorf("browser bytes = %d; want %d (media + one page overhead)", browser.BytesTransferred, wantBytes)
	}
	wantCost := float64(wantBytes) / 1e9 * 8.40
	if browser.CostUSD != wantCost {
		t.Errorf("browser cost = %v; want %v", browser.CostUSD, wantCost)
	}
	if browser.ArchiveItemID != 21 || browser.ShortID != "tt001" {
		t.Errorf("browser usage row not attributed to the item: %+v", browser)
	}
}

// The record carries two independently signed URLs for the same video, so a
// session refused the first should try the second before being thrown away.
func TestArchiveTikTokVideoFallsBackToSecondURL(t *testing.T) {
	record := loadRecords(t, "tiktok_post.json")[0]
	network := newFakeNetwork(record)
	video := fakeMP4(1024)

	page := newFakePage()
	page.media[stringField(record, "cdn_link")] = video

	client, db := newTestClient(t, network)
	sessions := withFakeBrowser(client, page)

	result, err := client.archiveTikTok(context.Background(), tiktokVideoURL, utils.ArchiveTypeYtDlp, io.Discard, db, 22, "tt002")
	if err != nil {
		t.Fatalf("archiveTikTok: %v", err)
	}
	if got := readResult(t, result); len(got) != len(video) {
		t.Errorf("stored %d bytes; want %d", len(got), len(video))
	}
	if *sessions != 1 {
		t.Errorf("opened %d sessions; the second candidate should reuse the first session", *sessions)
	}
	if len(page.requests) < 2 {
		t.Errorf("in-page requests = %v; want the refused URL then the fallback", page.requests)
	}
}

// Dataset media URLs are signed in the scraper's network position, not the
// separate Browser API session that later downloads them. A current TikTok
// record can therefore be perfectly valid while every dataset URL answers the
// browser with 403 at byte zero. The post page loaded in that same session can
// refresh the media URL; the refreshed URL must be fetched before abandoning
// the otherwise usable metadata record.
func TestArchiveTikTokVideoRefreshesMediaURLAfterDatasetURLsReturn403(t *testing.T) {
	record := loadRecords(t, "tiktok_post.json")[0]
	refreshedURL := strings.SplitN(stringField(record, "video_url"), "?", 2)[0] + "?signature=fresh-for-browser-session"
	video := fakeMP4(32 * 1024)
	page := &refreshingTikTokPage{
		fakePage:     newFakePage(),
		refreshedURL: refreshedURL,
	}
	page.media[refreshedURL] = video

	client, db := newTestClient(t, newFakeNetwork(record))
	sessions := withFakeBrowser(client, page)

	result, err := client.archiveTikTok(context.Background(), tiktokVideoURL, utils.ArchiveTypeYtDlp, io.Discard, db, 30, "tt010")
	if err != nil {
		t.Fatalf("archiveTikTok: %v", err)
	}
	if got := readResult(t, result); len(got) != len(video) {
		t.Fatalf("stored %d bytes; want %d", len(got), len(video))
	}
	if result.Completeness != archivers.CompletenessComplete {
		t.Errorf("completeness = %q; want complete", result.Completeness)
	}
	if result.Metadata == nil || !json.Valid(result.Metadata.Data) {
		t.Error("successful capture lost normalized metadata")
	}
	if result.RawMetadata == nil || !json.Valid(result.RawMetadata.Data) {
		t.Error("successful capture lost raw provider metadata")
	}
	if *sessions != 1 {
		t.Errorf("opened %d sessions; want the refreshed URL fetched in the original session", *sessions)
	}
	wantRequests := []string{
		stringField(record, "video_url"),
		stringField(record, "cdn_link"),
		refreshedURL,
	}
	if len(page.requests) != len(wantRequests) {
		t.Fatalf("in-page requests = %v; want the two refused dataset URLs then one refreshed URL", page.requests)
	}
	for i, want := range wantRequests {
		if page.requests[i] != want {
			t.Errorf("in-page request %d = %q; want %q", i, page.requests[i], want)
		}
	}
}

// Error and malformed records must remain failures. In particular, the media
// refresh path must not turn a provider error into an MP4-only "success" that
// has no trustworthy provider metadata to preserve and normalize.
func TestArchiveTikTokVideoRejectsErrorAndMalformedRecords(t *testing.T) {
	tests := []struct {
		name   string
		record map[string]any
		want   string
	}{
		{
			name: "provider error",
			record: map[string]any{
				"error":      "Item doesn't exist",
				"error_code": "tiktok_item_not_found",
				"input":      map[string]any{"url": tiktokVideoURL},
			},
			want: "Item doesn't exist",
		},
		{
			name: "malformed media fields",
			record: map[string]any{
				"post_id":   float64(123),
				"post_type": "video",
				"video_url": map[string]any{"unexpected": true},
				"cdn_link":  []any{false},
			},
			want: "no video URL",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, db := newTestClient(t, newFakeNetwork(tt.record))
			sessions := withFakeBrowser(client, newFakePage())

			result, err := client.archiveTikTok(context.Background(), tiktokVideoURL, utils.ArchiveTypeYtDlp, io.Discard, db, 31, "tt011")
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v; want a failure containing %q", err, tt.want)
			}
			if result.Data != nil || result.Metadata != nil || result.RawMetadata != nil || result.Completeness != "" {
				t.Fatalf("failed record produced archive artifacts: %+v", result)
			}
			if *sessions != 0 {
				t.Errorf("opened %d browser sessions for an unusable provider record", *sessions)
			}
			rows := usageRows(t, db)
			if len(rows) != 1 || rows[0].Success {
				t.Errorf("usage rows = %+v; want one unsuccessful dataset operation", rows)
			}
		})
	}
}

// refreshingTikTokPage models one remote Browser API session. The dataset URLs
// are intentionally absent from fakePage.media and therefore return 403. A
// second page-evaluation shape returns the URL freshly observed in that page.
type refreshingTikTokPage struct {
	*fakePage
	refreshedURL string
}

func (p *refreshingTikTokPage) Evaluate(expression string, arg ...any) (any, error) {
	if expression != fetchMediaChunkJS {
		encoded, err := json.Marshal([]string{p.refreshedURL})
		return string(encoded), err
	}
	return p.fakePage.Evaluate(expression, arg...)
}

// A browser fetch that never succeeds still spent session time, so the usage
// row is written with the sessions counted and success=false.
func TestArchiveTikTokVideoBrowserFailureRecordsUsage(t *testing.T) {
	record := loadRecords(t, "tiktok_post.json")[0]
	client, db := newTestClient(t, newFakeNetwork(record))
	// The page serves nothing, so every in-page fetch is refused with 403.
	sessions := withFakeBrowser(client, newFakePage())

	_, err := client.archiveTikTok(context.Background(), tiktokVideoURL, utils.ArchiveTypeYtDlp, io.Discard, db, 23, "tt003")
	if err == nil {
		t.Fatal("expected an error when the browser cannot fetch the video")
	}

	// region "US" is tried first, then an unpinned peer: a 403 is exactly the
	// failure a different exit could fix.
	if *sessions != 2 {
		t.Errorf("opened %d sessions; want 2 (region, then any peer)", *sessions)
	}

	rows := usageRows(t, db)
	if len(rows) != 2 {
		t.Fatalf("usage rows = %d; want 2", len(rows))
	}
	browser := rows[1]
	if browser.Product != "browser_api" || browser.Success {
		t.Errorf("browser usage row = %+v; want an unsuccessful browser_api row", browser)
	}
	wantBytes := int64(2 * browserPageOverheadBytes)
	if browser.BytesTransferred != wantBytes {
		t.Errorf("browser bytes = %d; want %d (two page loads, no media)", browser.BytesTransferred, wantBytes)
	}
	if browser.CostUSD != float64(wantBytes)/1e9*8.40 {
		t.Errorf("browser cost = %v", browser.CostUSD)
	}
	if !strings.Contains(browser.Detail, "403") {
		t.Errorf("browser usage detail does not name the refusal: %q", browser.Detail)
	}
}

// A photo post is a slideshow of ordinary CDN stills: each is tried directly
// first, and only the refused ones cost a browser session — one session for the
// whole post, not one per slide.
func TestArchiveTikTokPhotoPost(t *testing.T) {
	image := fakePNG(t)
	record := map[string]any{
		"post_id":          "7412345678901234567",
		"url":              "https://www.tiktok.com/@someone/photo/7412345678901234567",
		"post_type":        "photo",
		"description":      "three stills",
		"profile_username": "someone",
		"account_id":       "someone",
		"create_time":      "2026-08-01T12:00:00.000Z",
		"digg_count":       float64(42),
		"region":           "US",
		"carousel_images": []any{
			"https://p16-sign.tiktokcdn-us.com/one.image",
			"https://p16-sign.tiktokcdn-us.com/two.image",
			"https://p16-sign.tiktokcdn-us.com/three.image",
		},
	}
	network := newFakeNetwork(record)
	network.serve("https://p16-sign.tiktokcdn-us.com/one.image", image)
	network.serve("https://p16-sign.tiktokcdn-us.com/three.image", image)

	page := newFakePage()
	page.media["https://p16-sign.tiktokcdn-us.com/two.image"] = image

	client, db := newTestClient(t, network)
	sessions := withFakeBrowser(client, page)

	result, err := client.archiveTikTok(context.Background(),
		"https://www.tiktok.com/@someone/photo/7412345678901234567", utils.ArchiveTypeGalleryDl, io.Discard, db, 24, "tt004")
	if err != nil {
		t.Fatalf("archiveTikTok: %v", err)
	}
	if result.Completeness != archivers.CompletenessComplete {
		t.Errorf("completeness = %q; want complete (the browser rescued the refused still)", result.Completeness)
	}
	if *sessions != 1 {
		t.Errorf("opened %d sessions; want 1 shared session", *sessions)
	}

	reader := resultZip(t, result)
	for _, name := range []string{"001.jpg", "002.jpg", "003.jpg"} {
		if got := zipEntry(t, reader, name); len(got) != len(image) {
			t.Errorf("%s is %d bytes; want %d", name, len(got), len(image))
		}
	}
	meta := galleryMetadataFromZip(t, reader)
	if meta.Extractor != "tiktok" || meta.FileCount != 3 {
		t.Errorf("metadata = %q, %d files", meta.Extractor, meta.FileCount)
	}
	if meta.Completeness == nil || meta.Completeness.Expected == nil || *meta.Completeness.Expected != 3 {
		t.Errorf("expected count = %+v; want 3", meta.Completeness)
	}

	rows := usageRows(t, db)
	if len(rows) != 2 {
		t.Fatalf("usage rows = %d; want web_scraper + browser_api", len(rows))
	}
	browser := rows[0]
	if browser.Product != "browser_api" {
		browser = rows[1]
	}
	if browser.Product != "browser_api" || !browser.Success {
		t.Fatalf("browser usage row = %+v", browser)
	}
	wantBytes := int64(len(image)) + browserPageOverheadBytes
	if browser.BytesTransferred != wantBytes {
		t.Errorf("browser bytes = %d; want %d (one still + one page load)", browser.BytesTransferred, wantBytes)
	}
}

// A photo post whose stills all download directly must not open a browser
// session at all: the session is the fallback, not the path.
func TestArchiveTikTokPhotoPostSkipsBrowserWhenDirectWorks(t *testing.T) {
	image := fakePNG(t)
	record := map[string]any{
		"post_id":   "1",
		"url":       "https://www.tiktok.com/@someone/photo/1",
		"post_type": "photo",
		"images":    []any{"https://p16-sign.tiktokcdn-us.com/only.image"},
	}
	network := newFakeNetwork(record)
	network.serve("https://p16-sign.tiktokcdn-us.com/only.image", image)

	client, db := newTestClient(t, network)
	sessions := withFakeBrowser(client, newFakePage())

	result, err := client.archiveTikTok(context.Background(), "https://www.tiktok.com/@someone/photo/1", utils.ArchiveTypeGalleryDl, io.Discard, db, 25, "tt005")
	if err != nil {
		t.Fatalf("archiveTikTok: %v", err)
	}
	readResult(t, result)
	if *sessions != 0 {
		t.Errorf("opened %d browser sessions for a post that downloaded directly", *sessions)
	}
	if rows := usageRows(t, db); len(rows) != 1 || rows[0].Product != "web_scraper" {
		t.Errorf("usage rows = %+v; want only the dataset record", rows)
	}
}

// The photo-post record shape is unverified against a live record, so the
// image list is read from several plausible fields and object shapes.
func TestTikTokImageEntriesDefensiveShapes(t *testing.T) {
	cases := []struct {
		name   string
		record map[string]any
		want   int
	}{
		{"carousel_images strings", map[string]any{"carousel_images": []any{"https://cdn/a.jpg", "https://cdn/b.jpg"}}, 2},
		{"images objects", map[string]any{"images": []any{map[string]any{"url": "https://cdn/a.jpg"}}}, 1},
		{"image_urls", map[string]any{"image_urls": []any{"https://cdn/a.jpg"}}, 1},
		{"display_image object", map[string]any{"images": []any{map[string]any{"display_image": "https://cdn/a.jpg"}}}, 1},
		// TikTok's own item struct nests two levels deep, and url_list is a
		// mirror list for one image rather than several images.
		{"nested image_post_info", map[string]any{"image_post_info": map[string]any{"images": []any{
			map[string]any{"display_image": map[string]any{"url_list": []any{"https://cdn/a-mirror1.jpg", "https://cdn/a-mirror2.jpg"}}},
			map[string]any{"display_image": map[string]any{"url_list": []any{"https://cdn/b-mirror1.jpg", "https://cdn/b-mirror2.jpg"}}},
		}}}, 2},
		{"url_list mirrors count once", map[string]any{"images": []any{
			map[string]any{"url_list": []any{"https://cdn/a1.jpg", "https://cdn/a2.jpg", "https://cdn/a3.jpg"}},
		}}, 1},
		{"duplicates collapse", map[string]any{
			"carousel_images": []any{"https://cdn/a.jpg"},
			"images":          []any{"https://cdn/a.jpg"},
		}, 1},
		{"video post has no stills", map[string]any{"post_type": "video", "carousel_images": nil}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := len(tiktokImageEntries(c.record)); got != c.want {
				t.Errorf("entries = %d; want %d", got, c.want)
			}
		})
	}
}

// A video-only record routed to the gallery flow (or the reverse) must fail
// with a message that names what happened, not store an empty bundle.
func TestArchiveTikTokMismatchedPostTypeFailsExplicitly(t *testing.T) {
	record := loadRecords(t, "tiktok_post.json")[0]
	client, db := newTestClient(t, newFakeNetwork(record))
	withFakeBrowser(client, newFakePage())

	_, err := client.archiveTikTok(context.Background(), tiktokVideoURL, utils.ArchiveTypeGalleryDl, io.Discard, db, 26, "tt006")
	if err == nil || !strings.Contains(err.Error(), "no images") {
		t.Fatalf("error = %v; want an explicit no-images failure", err)
	}
	if rows := usageRows(t, db); len(rows) != 1 || rows[0].Success {
		t.Errorf("usage rows = %+v; want one unsuccessful dataset row", rows)
	}
}

func TestTikTokSessionCountries(t *testing.T) {
	cases := []struct {
		region string
		want   []string
	}{
		{"US", []string{"us", ""}},
		{"", []string{""}},
		{"unknown", []string{""}},
	}
	for _, c := range cases {
		got := tiktokSessionCountries(map[string]any{"region": c.region})
		if len(got) != len(c.want) {
			t.Fatalf("region %q -> %v; want %v", c.region, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("region %q -> %v; want %v", c.region, got, c.want)
			}
		}
	}
}

// Without browser credentials a refused still cannot be rescued, and the
// archive must not be billed for the session that could not be opened.
func TestArchiveTikTokPhotoPostWithoutBrowserCredentials(t *testing.T) {
	image := fakePNG(t)
	record := map[string]any{
		"post_id":   "1",
		"url":       "https://www.tiktok.com/@someone/photo/1",
		"post_type": "photo",
		"carousel_images": []any{
			"https://p16-sign.tiktokcdn-us.com/one.image",
			"https://p16-sign.tiktokcdn-us.com/two.image",
		},
	}
	network := newFakeNetwork(record)
	network.serve("https://p16-sign.tiktokcdn-us.com/one.image", image)

	client, db := newTestClient(t, network)
	client.openBrowser = func(ctx context.Context, country, pageURL string, logWriter io.Writer) (browserSession, error) {
		return nil, errors.New("Bright Data browser credentials are not configured")
	}

	result, err := client.archiveTikTok(context.Background(), "https://www.tiktok.com/@someone/photo/1", utils.ArchiveTypeGalleryDl, io.Discard, db, 27, "tt007")
	if err != nil {
		t.Fatalf("archiveTikTok: %v", err)
	}
	readResult(t, result)
	// One of two stills is a partial archive, and partial never reads green.
	if result.Completeness != archivers.CompletenessPartial {
		t.Errorf("completeness = %q; want partial", result.Completeness)
	}
	rows := usageRows(t, db)
	if len(rows) != 1 || rows[0].Product != "web_scraper" {
		t.Fatalf("usage rows = %+v; want only the dataset record", rows)
	}
}

// A session that opened and was refused everything is not a success, even when
// the archive as a whole survives as partial: the browser row has to say the
// paid work delivered nothing.
func TestArchiveTikTokPhotoPostBrowserDeliveredNothing(t *testing.T) {
	image := fakePNG(t)
	record := map[string]any{
		"post_id":   "1",
		"url":       "https://www.tiktok.com/@someone/photo/1",
		"post_type": "photo",
		"carousel_images": []any{
			"https://p16-sign.tiktokcdn-us.com/one.image",
			"https://p16-sign.tiktokcdn-us.com/two.image",
		},
	}
	network := newFakeNetwork(record)
	network.serve("https://p16-sign.tiktokcdn-us.com/one.image", image)

	client, db := newTestClient(t, network)
	// The session opens but serves nothing, so the refused still stays lost.
	withFakeBrowser(client, newFakePage())

	result, err := client.archiveTikTok(context.Background(), "https://www.tiktok.com/@someone/photo/1", utils.ArchiveTypeGalleryDl, io.Discard, db, 28, "tt008")
	if err != nil {
		t.Fatalf("archiveTikTok: %v", err)
	}
	readResult(t, result)
	if result.Completeness != archivers.CompletenessPartial {
		t.Errorf("completeness = %q; want partial", result.Completeness)
	}

	rows := usageRows(t, db)
	if len(rows) != 2 {
		t.Fatalf("usage rows = %d; want 2", len(rows))
	}
	var browser models.BrightDataUsage
	for _, row := range rows {
		if row.Product == "browser_api" {
			browser = row
		}
	}
	if browser.Product != "browser_api" {
		t.Fatal("no browser usage row for the session that opened")
	}
	if browser.Success {
		t.Error("a session that delivered nothing was recorded as a success")
	}
	if browser.BytesTransferred != browserPageOverheadBytes {
		t.Errorf("browser bytes = %d; want the page overhead it did spend", browser.BytesTransferred)
	}
}

// share_count arrives as a string in the live record while every other counter
// is a number, so the normalized metadata has to read both without relying on
// the num_share_count duplicate happening to exist.
func TestTikTokShareCountIsReadFromTheStringField(t *testing.T) {
	record := loadRecords(t, "tiktok_post.json")[0]
	if _, isString := record["share_count"].(string); !isString {
		t.Skip("fixture no longer carries share_count as a string")
	}
	delete(record, "num_share_count")

	_, _, err := buildBrightDataTikTokVideoArtifacts(record, tiktokVideoURL, 1, time.Now())
	if err != nil {
		t.Fatalf("buildBrightDataTikTokVideoArtifacts: %v", err)
	}
	if got := intValue(intField(record, "share_count")); got != 99 {
		t.Errorf("share_count = %d; want 99 read from the string value", got)
	}
}

// A first candidate that transfers bytes and then dies inflates what the
// session cost without changing what the archive holds. The usage row must
// carry the larger number and the metadata the smaller one; swapping them
// either under-reports spend or claims the archive holds bytes it does not.
func TestArchiveTikTokVideoBillsTransferredButDescribesStored(t *testing.T) {
	record := loadRecords(t, "tiktok_post.json")[0]
	network := newFakeNetwork(record)
	network.serve(stringField(record, "preview_image"), fakePNG(t))

	stored := fakeMP4(4096)
	page := &partialThenGoodPage{
		fakePage:  newFakePage(),
		failAfter: stringField(record, "video_url"),
		partial:   mediaChunkBytes, // one full chunk, then the URL dies
	}
	page.media[stringField(record, "video_url")] = fakeMP4(3 * mediaChunkBytes)
	page.media[stringField(record, "cdn_link")] = stored

	client, db := newTestClient(t, network)
	withFakeBrowser(client, page)

	result, err := client.archiveTikTok(context.Background(), tiktokVideoURL, utils.ArchiveTypeYtDlp, io.Discard, db, 29, "tt009")
	if err != nil {
		t.Fatalf("archiveTikTok: %v", err)
	}
	if got := readResult(t, result); len(got) != len(stored) {
		t.Fatalf("stored %d bytes; want %d", len(got), len(stored))
	}

	meta := videoMetadataFromSidecar(t, result.Metadata)
	if meta.Media.SizeBytes != int64(len(stored)) {
		t.Errorf("metadata reports %d media bytes; storage holds %d", meta.Media.SizeBytes, len(stored))
	}

	var browser models.BrightDataUsage
	for _, row := range usageRows(t, db) {
		if row.Product == "browser_api" {
			browser = row
		}
	}
	wantBilled := int64(mediaChunkBytes) + int64(len(stored)) + browserPageOverheadBytes
	if browser.BytesTransferred != wantBilled {
		t.Errorf("billed %d bytes; want %d (abandoned chunk + stored file + page load)",
			browser.BytesTransferred, wantBilled)
	}
}

// partialThenGoodPage serves failAfter's bytes up to partial and then refuses,
// the shape of a signed URL whose window closes mid-download.
type partialThenGoodPage struct {
	*fakePage
	failAfter string
	partial   int
	delivered int
}

func (p *partialThenGoodPage) Evaluate(expression string, arg ...any) (any, error) {
	args, _ := arg[0].(map[string]interface{})
	if mediaURL, _ := args["url"].(string); mediaURL == p.failAfter {
		if p.delivered >= p.partial {
			p.requests = append(p.requests, mediaURL)
			return `{"error":"status 403"}`, nil
		}
		p.delivered += mediaChunkBytes
	}
	return p.fakePage.Evaluate(expression, arg...)
}
