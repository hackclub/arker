package apify

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"arker/internal/archivers"
	"arker/internal/models"
)

// TestLive runs the real actors against real posts and spends real money.
// It is skipped unless APIFY_LIVE_TOKEN is set; APIFY_LIVE_ONLY narrows it to
// scenario names containing the substring, and APIFY_LIVE_OUT (default a
// temp dir) receives every artifact plus a summary.json for inspection.
//
// This exists because the offline suite proves the code against recorded
// records, and the recordings go stale the day an actor changes its output.
// Run it before a release that touches the fallback.
func TestLive(t *testing.T) {
	token := os.Getenv("APIFY_LIVE_TOKEN")
	if token == "" {
		t.Skip("APIFY_LIVE_TOKEN not set")
	}
	only := os.Getenv("APIFY_LIVE_ONLY")
	outDir := os.Getenv("APIFY_LIVE_OUT")
	if outDir == "" {
		outDir = t.TempDir()
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Logf("artifacts in %s", outDir)

	type scenario struct {
		name, url, itemType string
		mode                string // archive (default), thumbnail, refresh
	}
	scenarios := []scenario{
		{name: "instagram-reel", url: "https://www.instagram.com/reel/DUp5AL8kxuN/", itemType: "yt-dlp"},
		{name: "instagram-carousel", url: "https://www.instagram.com/p/DckIgOqFIr5/", itemType: "gallery-dl"},
		{name: "instagram-photo", url: "https://www.instagram.com/p/DcOX3hWFiey/", itemType: "gallery-dl"},
		{name: "instagram-reel-thumbnail", url: "https://www.instagram.com/reel/DUp5AL8kxuN/", itemType: "yt-dlp", mode: "thumbnail"},
		{name: "tiktok-video", url: "https://www.tiktok.com/@hackclub_latam/video/7653880597500218645", itemType: "yt-dlp"},
		{name: "tiktok-photo", url: "https://www.tiktok.com/@dct_deepcodethinking/photo/7656194593955859720", itemType: "gallery-dl"},
		{name: "tiktok-video-refresh", url: "https://www.tiktok.com/@hackclub_latam/video/7653880597500218645", itemType: "yt-dlp", mode: "refresh"},
		{name: "youtube-video", url: "https://www.youtube.com/watch?v=Px0L-_8_9fw", itemType: "yt-dlp"},
		{name: "youtube-thumbnail", url: "https://www.youtube.com/watch?v=Px0L-_8_9fw", itemType: "yt-dlp", mode: "thumbnail"},
		{name: "youtube-refresh", url: "https://www.youtube.com/watch?v=Px0L-_8_9fw", itemType: "yt-dlp", mode: "refresh"},
		{name: "facebook-video", url: "https://www.facebook.com/HackClubHQ/videos/1847881915262822/", itemType: "yt-dlp"},
		{name: "facebook-reel", url: "https://www.facebook.com/reel/1121155692481577/", itemType: "yt-dlp"},
		{name: "facebook-post", url: "https://www.facebook.com/HackClubHQ/posts/pfbid02kCzvfAri8zqTBvGg4qjBVfbhwtTkHuts4grQGnR7BVsSZvgyCN6Q8dYbypk335CWl", itemType: "gallery-dl"},
		{name: "reddit-gallery", url: "https://www.reddit.com/r/aww/comments/1vtkudd/neighbours_cat_came_into_our_house_and_slept_for/", itemType: "gallery-dl"},
		{name: "reddit-video", url: "https://www.reddit.com/r/aww/comments/1vf8x6i/my_cat_hiding_from_the_repair_guyin_my_pillowcase/", itemType: "gallery-dl"},
		{name: "x-photos", url: "https://x.com/icyelectronics/status/2063159442393248203", itemType: "gallery-dl"},
		{name: "x-video", url: "https://x.com/NASA/status/2094884934510956691", itemType: "gallery-dl"},
		{name: "pinterest-image", url: "https://www.pinterest.com/pin/2181499817705551/", itemType: "gallery-dl"},
		{name: "pinterest-video", url: "https://www.pinterest.com/pin/31103053674023753/", itemType: "gallery-dl"},
	}

	client := New(Config{Token: token, RunTimeout: 10 * time.Minute, MaxRunCostUSD: 0.5})
	summary := map[string]any{}
	var mu sync.Mutex
	defer func() {
		client.Close() // let pay-per-event costs settle into the rows
		for name, entry := range summary {
			if reread, ok := entry.(map[string]any)["reread"].(func() []models.FallbackUsage); ok {
				entry.(map[string]any)["usage"] = reread()
				delete(entry.(map[string]any), "reread")
				summary[name] = entry
			}
		}
		out, _ := json.MarshalIndent(summary, "", "  ")
		_ = os.WriteFile(filepath.Join(outDir, "summary.json"), out, 0o644)
	}()

	// The group finishes only after every parallel scenario has, so the
	// summary defer above sees them all.
	t.Run("scenarios", func(t *testing.T) {
		for _, sc := range scenarios {
			if only != "" && !strings.Contains(sc.name, only) {
				continue
			}
			sc := sc
			t.Run(sc.name, func(t *testing.T) {
				t.Parallel()
				db := newTestDB(t)
				var log bytes.Buffer
				ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
				defer cancel()
				entry := map[string]any{"url": sc.url, "item_type": sc.itemType, "mode": sc.mode}
				defer func() {
					_ = os.WriteFile(filepath.Join(outDir, sc.name+".log"), log.Bytes(), 0o644)
					var rows []models.FallbackUsage
					db.Find(&rows)
					entry["usage"] = rows
					entry["reread"] = func() []models.FallbackUsage {
						var settled []models.FallbackUsage
						db.Find(&settled)
						return settled
					}
					mu.Lock()
					summary[sc.name] = entry
					mu.Unlock()
				}()

				switch sc.mode {
				case "thumbnail":
					if !client.SupportsSocialThumbnail(sc.url, sc.itemType) {
						t.Fatalf("thumbnail not supported for %s", sc.url)
					}
					thumb, err := client.ResolveSocialThumbnail(ctx, sc.url, sc.itemType, &log, db, 7)
					if err != nil {
						t.Fatalf("thumbnail: %v\n%s", err, log.String())
					}
					if thumb == nil || len(thumb.Data) == 0 {
						t.Fatalf("empty thumbnail\n%s", log.String())
					}
					entry["thumbnail_bytes"] = len(thumb.Data)
					_ = os.WriteFile(filepath.Join(outDir, sc.name+".thumb.jpg"), thumb.Data, 0o644)
				case "refresh":
					if !client.SupportsStoredVideoMetadata(sc.url) {
						t.Fatalf("refresh not supported for %s", sc.url)
					}
					result, err := client.RefreshStoredVideoMetadata(ctx, sc.url, &log, db, 7, archivers.VideoMedia{SizeBytes: 1234, Extension: ".mp4"})
					if err != nil {
						t.Fatalf("refresh: %v\n%s", err, log.String())
					}
					writeSidecars(t, outDir, sc.name, result)
					entry["metadata"] = result.Metadata
				default:
					if !client.SupportsFallback(sc.url, sc.itemType) {
						t.Fatalf("fallback not supported for %s (%s)", sc.url, sc.itemType)
					}
					result, err := client.ArchiveFallback(ctx, sc.url, sc.itemType, &log, db, 7)
					if err != nil {
						t.Fatalf("archive: %v\n%s", err, log.String())
					}
					data, err := io.ReadAll(result.Data)
					if err != nil {
						t.Fatal(err)
					}
					if closer, ok := result.Data.(io.Closer); ok {
						closer.Close()
					}
					if len(data) == 0 {
						t.Fatalf("empty artifact\n%s", log.String())
					}
					artifact := filepath.Join(outDir, sc.name+result.Extension)
					if err := os.WriteFile(artifact, data, 0o644); err != nil {
						t.Fatal(err)
					}
					entry["artifact"] = artifact
					entry["bytes"] = len(data)
					entry["source"] = result.Source
					entry["completeness"] = result.Completeness
					entry["thumbnail"] = result.Thumbnail != nil
					writeSidecars(t, outDir, sc.name, result)
					if result.Source != models.ArchiveSourceApify {
						t.Errorf("source = %q", result.Source)
					}
					if result.Thumbnail == nil {
						t.Errorf("no thumbnail")
					}
					if result.Extension == ".zip" {
						zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
						if err != nil {
							t.Fatal(err)
						}
						var names []string
						for _, f := range zr.File {
							names = append(names, fmt.Sprintf("%s (%d)", f.Name, f.UncompressedSize64))
						}
						entry["zip"] = names
						if zr.File[0].Name != "metadata.json" {
							t.Errorf("first zip entry %q", zr.File[0].Name)
						}
					}
					entry["metadata"] = result.Metadata
				}
				var rows []models.FallbackUsage
				db.Find(&rows)
				// A run that spent money without yielding media (the
				// Facebook reel post-shape retry, an IPv6-only delivery
				// node) is booked unsuccessful with a reason; a silent
				// failure row is the only wrong shape.
				for _, row := range rows {
					if !row.Success && row.Detail == "" {
						t.Errorf("usage row failed without a reason: %+v", row)
					}
				}
				t.Logf("%s ok (%d usage rows)\n%s", sc.name, len(rows), log.String())
			})
		}
	})
}

// signedValue matches a CDN signature that survived sanitization; the
// redaction marker in its place is fine.
var signedValue = regexp.MustCompile(`(x-signature|oh|VSignature|signature|Signature|X-Amz-Signature)=(?:[^%&"\\]|%[0-9A-Fa-f]{2})+`)

func writeSidecars(t *testing.T, dir, name string, result archivers.Result) {
	t.Helper()
	for suffix, sc := range map[string]*archivers.Sidecar{".metadata.json": result.Metadata, ".raw.json": result.RawMetadata} {
		if sc == nil {
			continue
		}
		for _, match := range signedValue.FindAll(sc.Data, -1) {
			if bytes.Contains(match, []byte("REDACTED")) {
				continue
			}
			t.Errorf("%s sidecar leaks a signed URL: %s", name, match)
		}
		_ = os.WriteFile(filepath.Join(dir, name+suffix), sc.Data, 0o644)
	}
}
