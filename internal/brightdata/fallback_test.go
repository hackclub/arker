package brightdata

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"arker/internal/archivers"
	"arker/internal/models"
	"arker/internal/utils"
)

type fakePrimary struct {
	err    error
	called bool
}

func (f *fakePrimary) Archive(ctx context.Context, url string, logWriter io.Writer, db *gorm.DB, itemID uint) (archivers.Result, error) {
	f.called = true
	if f.err != nil {
		return archivers.Result{}, f.err
	}
	return archivers.Result{Data: strings.NewReader("native"), Extension: ".mp4"}, nil
}

type fakeBackend struct {
	supports bool
	err      error
	called   bool
}

func (f *fakeBackend) Enabled() bool { return true }
func (f *fakeBackend) SupportsFallback(url, itemType string) bool {
	return f.supports
}
func (f *fakeBackend) ArchiveFallback(ctx context.Context, url, itemType string, logWriter io.Writer, db *gorm.DB, itemID uint) (archivers.Result, error) {
	f.called = true
	if f.err != nil {
		return archivers.Result{}, f.err
	}
	return archivers.Result{Data: strings.NewReader("fallback"), Extension: ".mp4", Source: "brightdata"}, nil
}

func TestFallbackNotUsedWhenNativeSucceeds(t *testing.T) {
	backend := &fakeBackend{supports: true}
	arch := &FallbackArchiver{Primary: &fakePrimary{}, Type: utils.ArchiveTypeYtDlp, Backend: backend}

	result, err := arch.Archive(context.Background(), "https://www.youtube.com/watch?v=abc123def45", io.Discard, nil, 1)
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if backend.called {
		t.Error("backend was called even though the native flow succeeded")
	}
	if result.Source != "" {
		t.Errorf("native result carries source %q", result.Source)
	}
}

func TestFallbackRescuesNativeFailure(t *testing.T) {
	backend := &fakeBackend{supports: true}
	arch := &FallbackArchiver{
		Primary: &fakePrimary{err: errors.New("yt-dlp cannot access video")},
		Type:    utils.ArchiveTypeYtDlp,
		Backend: backend,
	}

	var log strings.Builder
	result, err := arch.Archive(context.Background(), "https://www.instagram.com/reel/XYZ/", &log, nil, 1)
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if !backend.called {
		t.Fatal("backend was not called after native failure")
	}
	if result.Source != "brightdata" {
		t.Errorf("fallback result source = %q; want brightdata", result.Source)
	}
	if !strings.Contains(log.String(), "attempting Bright Data fallback") {
		t.Error("log does not mention the fallback attempt")
	}
}

func TestFallbackErrorKeepsNativeError(t *testing.T) {
	backend := &fakeBackend{supports: true, err: errors.New("dataset empty")}
	arch := &FallbackArchiver{
		Primary: &fakePrimary{err: errors.New("native boom")},
		Type:    utils.ArchiveTypeYtDlp,
		Backend: backend,
	}

	_, err := arch.Archive(context.Background(), "https://www.instagram.com/reel/XYZ/", io.Discard, nil, 1)
	if err == nil {
		t.Fatal("expected an error when both flows fail")
	}
	if !strings.Contains(err.Error(), "native boom") || !strings.Contains(err.Error(), "dataset empty") {
		t.Errorf("combined error missing a cause: %v", err)
	}
}

func TestFallbackSkippedForUnsupportedURL(t *testing.T) {
	backend := &fakeBackend{supports: false}
	arch := &FallbackArchiver{
		Primary: &fakePrimary{err: errors.New("native boom")},
		Type:    utils.ArchiveTypeYtDlp,
		Backend: backend,
	}

	_, err := arch.Archive(context.Background(), "https://vimeo.com/12345", io.Discard, nil, 1)
	if err == nil || backend.called {
		t.Fatalf("fallback ran for an unsupported URL (err=%v, called=%v)", err, backend.called)
	}
}

func TestFallbackSkippedWhenContextExpired(t *testing.T) {
	backend := &fakeBackend{supports: true}
	arch := &FallbackArchiver{
		Primary: &fakePrimary{err: errors.New("native timeout")},
		Type:    utils.ArchiveTypeYtDlp,
		Backend: backend,
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := arch.Archive(ctx, "https://www.instagram.com/reel/XYZ/", io.Discard, nil, 1)
	if err == nil {
		t.Fatal("expected the native error")
	}
	if backend.called {
		t.Error("fallback spent money on a job with no budget left")
	}
}

func TestFallbackSkippedWhenBudgetTooSmall(t *testing.T) {
	backend := &fakeBackend{supports: true}
	arch := &FallbackArchiver{
		Primary: &fakePrimary{err: errors.New("native boom")},
		Type:    utils.ArchiveTypeYtDlp,
		Backend: backend,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var log strings.Builder
	_, err := arch.Archive(ctx, "https://www.instagram.com/reel/XYZ/", &log, nil, 1)
	if err == nil {
		t.Fatal("expected the native error")
	}
	if backend.called {
		t.Error("fallback started with under 3 minutes of job budget")
	}
	if !strings.Contains(log.String(), "skipping Bright Data fallback") {
		t.Error("log does not explain the skipped fallback")
	}
}

func TestWithFallbackReturnsPrimaryWhenDisabled(t *testing.T) {
	primary := &fakePrimary{}
	if got := WithFallback(primary, utils.ArchiveTypeYtDlp, nil); got != archivers.Archiver(primary) {
		t.Error("nil backend should return the primary archiver unchanged")
	}
	disabled := &Client{}
	if got := WithFallback(primary, utils.ArchiveTypeYtDlp, disabled); got != archivers.Archiver(primary) {
		t.Error("disabled client should return the primary archiver unchanged")
	}
}

// The coverage table is load-bearing twice over: it decides when money may be
// spent, and routing consults it before creating a gallery item for a
// login-only site, so a row that changes here changes what Arker archives.
func TestClientSupportsFallback(t *testing.T) {
	datasetOnly := &Client{cfg: Config{APIKey: "k"}}
	full := &Client{cfg: Config{APIKey: "k", CustomerID: "c", BrowserZone: "z", BrowserZonePassword: "p"}}

	cases := []struct {
		name     string
		client   *Client
		url, typ string
		want     bool
	}{
		{"instagram reel", datasetOnly, "https://www.instagram.com/reel/X/", utils.ArchiveTypeYtDlp, true},
		{"instagram post", datasetOnly, "https://www.instagram.com/p/X/", utils.ArchiveTypeGalleryDl, true},

		// YouTube and TikTok video bytes are IP-locked to the resolver, so
		// they are only rescuable with browser credentials.
		{"youtube without browser", datasetOnly, "https://www.youtube.com/watch?v=abc123def45", utils.ArchiveTypeYtDlp, false},
		{"youtube with browser", full, "https://www.youtube.com/watch?v=abc123def45", utils.ArchiveTypeYtDlp, true},
		{"youtu.be", full, "https://youtu.be/abc123def45", utils.ArchiveTypeYtDlp, true},
		{"tiktok video without browser", datasetOnly, "https://www.tiktok.com/@u/video/1", utils.ArchiveTypeYtDlp, false},
		{"tiktok video with browser", full, "https://www.tiktok.com/@u/video/1", utils.ArchiveTypeYtDlp, true},
		{"tiktok short link", full, "https://vm.tiktok.com/ZMabcdef/", utils.ArchiveTypeYtDlp, true},

		// TikTok stills download directly; the browser is only the fallback.
		{"tiktok photo post", datasetOnly, "https://www.tiktok.com/@u/photo/1", utils.ArchiveTypeGalleryDl, true},

		// Reddit and X media download from Arker's own connection.
		{"reddit comments", datasetOnly, "https://www.reddit.com/r/aww/comments/abc/title/", utils.ArchiveTypeGalleryDl, true},
		{"redd.it short link", datasetOnly, "https://redd.it/abc123", utils.ArchiveTypeGalleryDl, true},
		{"x status", datasetOnly, "https://x.com/u/status/1", utils.ArchiveTypeGalleryDl, true},
		{"twitter status", datasetOnly, "https://twitter.com/u/status/1", utils.ArchiveTypeGalleryDl, true},

		// Wrong archive type for the platform, or no fallback at all.
		{"reddit as video", full, "https://www.reddit.com/r/aww/comments/abc/title/", utils.ArchiveTypeYtDlp, false},
		{"x as video", full, "https://x.com/u/status/1", utils.ArchiveTypeYtDlp, false},
		{"subreddit page", full, "https://www.reddit.com/r/aww/", utils.ArchiveTypeGalleryDl, false},
		{"x profile", full, "https://x.com/someone", utils.ArchiveTypeGalleryDl, false},
		{"vimeo", full, "https://vimeo.com/1234", utils.ArchiveTypeYtDlp, false},
		{"pinterest", full, "https://www.pinterest.com/pin/1/", utils.ArchiveTypeGalleryDl, false},
		{"mhtml", full, "https://www.youtube.com/watch?v=abc123def45", utils.ArchiveTypeMHTML, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.client.SupportsFallback(c.url, c.typ); got != c.want {
				t.Errorf("SupportsFallback(%s, %s) = %v; want %v", c.url, c.typ, got, c.want)
			}
		})
	}

	// A client with no API key never spends, whatever the URL.
	disabled := &Client{}
	if disabled.SupportsFallback("https://www.reddit.com/r/aww/comments/abc/x/", utils.ArchiveTypeGalleryDl) {
		t.Error("an unconfigured client claimed it could rescue a URL")
	}
}

func TestExtractYouTubeVideoID(t *testing.T) {
	cases := map[string]string{
		"https://www.youtube.com/watch?v=Px0L-_8_9fw":       "Px0L-_8_9fw",
		"https://www.youtube.com/watch?v=dQw4w9WgXcQ&t=10":  "dQw4w9WgXcQ",
		"https://youtu.be/dQw4w9WgXcQ":                      "dQw4w9WgXcQ",
		"https://www.youtube.com/shorts/6gdawrVffJc":        "6gdawrVffJc",
		"https://www.youtube.com/live/abcdefg1234":          "abcdefg1234",
		"https://www.youtube.com/embed/dQw4w9WgXcQ?rel=0":   "dQw4w9WgXcQ",
		"https://www.instagram.com/reel/NotYouTube/":        "",
		"https://www.youtube.com/@channel":                  "",
		"https://www.youtube.com/playlist?list=PLabcdef123": "",
	}
	for url, want := range cases {
		if got := ExtractYouTubeVideoID(url); got != want {
			t.Errorf("ExtractYouTubeVideoID(%s) = %q; want %q", url, got, want)
		}
	}
}

// One test that runs the whole public path: a native failure, the coverage
// check, the dispatch, the platform flow, and the usage row attributed to a
// real capture. The per-platform tests call the flows directly, so nothing
// else would catch SupportsFallback and ArchiveFallback disagreeing about a
// platform — a disagreement that reads as "native failed, and so did the
// rescue" in production.
func TestFallbackArchiverRescuesRedditThroughTheRealClient(t *testing.T) {
	record := loadRecords(t, "reddit_post.json")[0]
	network := newFakeNetwork(record)
	video := fakeMP4(2048)
	network.serve(redditMediaEntries(record)[0].URL, video)

	client, db := newTestClient(t, network)

	capture := models.Capture{ShortID: "rd001"}
	if err := db.Create(&capture).Error; err != nil {
		t.Fatalf("create capture: %v", err)
	}
	item := models.ArchiveItem{CaptureID: capture.ID, Type: utils.ArchiveTypeGalleryDl, Status: "processing"}
	if err := db.Create(&item).Error; err != nil {
		t.Fatalf("create item: %v", err)
	}

	arch := WithFallback(&fakePrimary{err: errors.New("gallery-dl: HTTP 403")}, utils.ArchiveTypeGalleryDl, client)
	var log strings.Builder
	result, err := arch.Archive(context.Background(), redditPostURL, &log, db, item.ID)
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if result.Source != models.ArchiveSourceBrightData || result.Extension != ".zip" {
		t.Errorf("result = %q %s; want a brightdata ZIP", result.Source, result.Extension)
	}
	readResult(t, result)

	rows := usageRows(t, db)
	if len(rows) != 1 || !rows[0].Success {
		t.Fatalf("usage rows = %+v", rows)
	}
	// The row has to be findable from the capture, which is how the API
	// reports what a rescue cost.
	if rows[0].ShortID != "rd001" || rows[0].ArchiveItemID != item.ID {
		t.Errorf("usage row not attributed to the capture: %+v", rows[0])
	}
	if !strings.Contains(log.String(), "Bright Data fallback succeeded") {
		t.Error("archive log does not record the rescue")
	}
}
