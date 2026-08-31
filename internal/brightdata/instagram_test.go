package brightdata

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"arker/internal/archivers"
	"arker/internal/models"
	"arker/internal/utils"
)

// loadRecord loads a captured real Bright Data snapshot record. These fixtures
// are verbatim API output from the datasets this package depends on, so the
// mapping tests exercise the real field names, not an idealized schema.
func loadRecord(t *testing.T, name string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	var records []map[string]any
	if err := json.Unmarshal(data, &records); err != nil {
		t.Fatalf("parse fixture %s: %v", name, err)
	}
	if len(records) == 0 {
		t.Fatalf("fixture %s has no records", name)
	}
	return records[0]
}

func TestReelRecordHasDirectVideoURL(t *testing.T) {
	record := loadRecord(t, "instagram_reel.json")

	videoURL := stringField(record, "video_url")
	if !strings.Contains(videoURL, "cdninstagram.com") {
		t.Fatalf("reel video_url = %q; want a cdninstagram URL", videoURL)
	}
	if stringField(record, "thumbnail") == "" {
		t.Error("reel record has no thumbnail URL")
	}
	if stringField(record, "user_posted") == "" {
		t.Error("reel record has no author")
	}
}

func TestPostMediaEntriesFromCarousel(t *testing.T) {
	record := loadRecord(t, "instagram_post.json")

	entries := postMediaEntries(record)
	if len(entries) != 10 {
		t.Fatalf("carousel entries = %d; want 10", len(entries))
	}
	for i, entry := range entries {
		if !strings.Contains(entry.URL, "cdninstagram.com") {
			t.Errorf("entry %d URL = %q; want a cdninstagram URL", i, entry.URL)
		}
		if entry.isVideo() {
			t.Errorf("entry %d is marked video in an all-photo carousel", i)
		}
		if entry.extension() != ".jpg" {
			t.Errorf("entry %d extension = %q; want .jpg", i, entry.extension())
		}
	}
}

func TestPostMediaEntriesPrefersPostContentOrder(t *testing.T) {
	record := map[string]any{
		"post_content": []any{
			map[string]any{"index": float64(0), "type": "Photo", "url": "https://cdn.example/a.jpg"},
			map[string]any{"index": float64(1), "type": "Video", "url": "https://cdn.example/b.mp4"},
		},
		"photos": []any{"https://cdn.example/should-not-be-used.jpg"},
	}
	entries := postMediaEntries(record)
	if len(entries) != 2 {
		t.Fatalf("entries = %d; want 2", len(entries))
	}
	if !entries[1].isVideo() {
		t.Error("second entry should be a video")
	}
	if entries[1].extension() != ".mp4" {
		t.Errorf("video extension = %q; want .mp4", entries[1].extension())
	}
}

func TestInstagramGalleryFallbackCanonicalizesMisleadingHEICNames(t *testing.T) {
	targetURL := "https://www.instagram.com/p/L9YP9/"
	mediaURLs := []string{
		"https://scontent.example/first.heic",
		"https://scontent.example/second.heic",
	}
	record := map[string]any{
		"shortcode":   "L9YP9",
		"url":         targetURL,
		"user_posted": "fixture-user",
		"post_content": []any{
			map[string]any{"index": float64(0), "type": "Photo", "url": mediaURLs[0]},
			map[string]any{"index": float64(1), "type": "Photo", "url": mediaURLs[1]},
		},
	}
	network := newFakeNetwork(record)
	jpegBytes := fakeJPEG(t)
	for _, mediaURL := range mediaURLs {
		network.serve(mediaURL, jpegBytes)
	}
	client, db := newTestClient(t, network)

	result, err := client.ArchiveFallback(context.Background(), targetURL, utils.ArchiveTypeGalleryDl, io.Discard, db, 42)
	if err != nil {
		t.Fatalf("ArchiveFallback: %v", err)
	}
	if result.Source != models.ArchiveSourceBrightData {
		t.Errorf("result source = %q, want %q", result.Source, models.ArchiveSourceBrightData)
	}
	if result.Completeness != archivers.CompletenessComplete {
		t.Errorf("result completeness = %q, want complete", result.Completeness)
	}

	reader := resultZip(t, result)
	names := zipNames(reader)
	for _, want := range []string{"metadata.json", "brightdata.json", "001.jpg", "002.jpg"} {
		if !containsString(names, want) {
			t.Errorf("ZIP entries = %v, missing %s", names, want)
		}
	}
	for _, unwanted := range []string{"001.heic", "002.heic"} {
		if containsString(names, unwanted) {
			t.Errorf("ZIP retains misleading filename %s: %v", unwanted, names)
		}
	}
	if body := zipEntry(t, reader, "001.jpg"); !bytes.HasPrefix(body, []byte{0xff, 0xd8, 0xff}) {
		t.Errorf("001.jpg does not contain JPEG bytes: %x", body[:min(8, len(body))])
	}

	meta := galleryMetadataFromZip(t, reader)
	if meta.FileCount != 2 || len(meta.Files) != 2 {
		t.Fatalf("metadata files = %d/%d, want 2/2", meta.FileCount, len(meta.Files))
	}
	for i, file := range meta.Files {
		wantName := []string{"001.jpg", "002.jpg"}[i]
		if file.Name != wantName || file.ContentType != "image/jpeg" || file.IsVideo {
			t.Errorf("metadata file %d = %+v, want %s as image/jpeg", i, file, wantName)
		}
	}
	if meta.Completeness == nil || meta.Completeness.State != archivers.CompletenessComplete ||
		meta.Completeness.Expected == nil || *meta.Completeness.Expected != 2 || meta.Completeness.Stored != 2 {
		t.Errorf("metadata completeness = %+v, want complete 2 of 2", meta.Completeness)
	}

	// brightdata.json is the sanitized provider record, not an internal file
	// manifest. It keeps the provider's original .heic URLs while metadata.json
	// and ZIP entry names consistently describe the JPEG bytes Arker stored.
	raw := zipEntry(t, reader, "brightdata.json")
	if !bytes.Contains(raw, []byte(".heic")) {
		t.Errorf("raw provider record lost its original media URLs: %s", raw)
	}
}

// /p/ URLs take the gallery fallback even when Instagram says the post is a
// Reel. The video endpoint later projects a complete one-video gallery onto
// the video contract, so the ZIP's per-file metadata must carry the intrinsic
// duration and dimensions read from the downloaded MP4. This is the exact path
// that left capture 5pfAm without duration_seconds.
func TestInstagramGalleryFallbackRecordsVideoIntrinsics(t *testing.T) {
	targetURL := "https://www.instagram.com/p/DcO8-weBHGS/"
	mediaURL := "https://scontent.example/reel.mp4"
	record := map[string]any{
		"shortcode":    "DcO8-weBHGS",
		"url":          targetURL,
		"content_type": "Reel",
		"user_posted":  "anna.codes.stuff",
		"alt_text":     "Video by Anna Codes on August 19, 2026. May be an image of text.",
		"description":  "A fixture caption",
		"likes":        float64(177),
		"num_comments": float64(5),
		"post_content": []any{
			map[string]any{"index": float64(0), "type": "Video", "url": mediaURL},
		},
		"videos_duration": []any{
			map[string]any{"url": mediaURL, "video_duration": float64(48.087074)},
		},
	}
	network := newFakeNetwork(record)
	network.serve(mediaURL, fakeMP4(1024))
	installInstagramGalleryFFprobe(t)
	client, db := newTestClient(t, network)

	result, err := client.ArchiveFallback(context.Background(), targetURL, utils.ArchiveTypeGalleryDl, io.Discard, db, 42)
	if err != nil {
		t.Fatalf("ArchiveFallback: %v", err)
	}
	meta := galleryMetadataFromZip(t, resultZip(t, result))
	if len(meta.Files) != 1 {
		t.Fatalf("metadata files = %+v; want one video", meta.Files)
	}
	video := meta.Files[0]
	if !video.IsVideo || video.ContentType != "video/mp4" {
		t.Fatalf("stored file = %+v; want video/mp4", video)
	}
	if video.DurationSeconds == nil || *video.DurationSeconds != 48.087074 {
		t.Errorf("duration_seconds = %v; want 48.087074 from stored bytes", video.DurationSeconds)
	}
	if video.Width != 1080 || video.Height != 1920 {
		t.Errorf("dimensions = %dx%d; want 1080x1920 from stored bytes", video.Width, video.Height)
	}
	if meta.Likes == nil || *meta.Likes != 177 || meta.Comments == nil || *meta.Comments != 5 {
		t.Errorf("engagement = likes %v, comments %v; want 177/5", meta.Likes, meta.Comments)
	}
	if meta.Title != "Video by Anna Codes" {
		t.Errorf("title = %q; want a stable single-video title", meta.Title)
	}
}

// Instagram /p/ URLs use the gallery route even when the post is a single
// video. The provider record carries that video's authored cover separately;
// capture it rather than letting /thumb fall through to the browser screenshot
// of Instagram's page chrome (the production failure demonstrated by mrQQB).
func TestInstagramVideoPostGalleryUsesPublishedThumbnail(t *testing.T) {
	targetURL := "https://www.instagram.com/p/DcRwIs0oE2s/"
	mediaURL := "https://scontent.example/reel.mp4"
	thumbnailURL := "https://scontent.example/reel-cover.png"
	poster := fakePNG(t)
	record := map[string]any{
		"shortcode":   "DcRwIs0oE2s",
		"url":         targetURL,
		"user_posted": "irtaza.dev.stuff",
		"thumbnail":   thumbnailURL,
		"post_content": []any{
			map[string]any{"index": float64(0), "type": "Video", "url": mediaURL},
		},
	}
	network := newFakeNetwork(record)
	network.serve(mediaURL, fakeMP4(1024))
	network.serve(thumbnailURL, poster)
	installInstagramGalleryFFprobe(t)
	client, db := newTestClient(t, network)

	result, err := client.ArchiveFallback(context.Background(), targetURL, utils.ArchiveTypeGalleryDl, io.Discard, db, 42)
	if err != nil {
		t.Fatalf("ArchiveFallback: %v", err)
	}
	defer closeResultData(result)
	if result.Thumbnail == nil {
		t.Fatal("video post did not capture its published thumbnail")
	}
	if !bytes.Equal(result.Thumbnail.Data, poster) {
		t.Error("published thumbnail was cropped, scaled, or re-encoded")
	}
	if result.Thumbnail.Width != 8 || result.Thumbnail.Height != 8 {
		t.Errorf("thumbnail dimensions = %dx%d, want provider image's 8x8", result.Thumbnail.Width, result.Thumbnail.Height)
	}
	if !network.requested(thumbnailURL) {
		t.Error("provider thumbnail URL was never fetched")
	}
}

func installInstagramGalleryFFprobe(t *testing.T) {
	t.Helper()
	bin := t.TempDir()
	script := `#!/bin/sh
for last; do :; done
[ -r "$last" ] || exit 90
cat <<'JSON'
{"streams":[{"codec_name":"h264","codec_type":"video","width":1080,"height":1920,"r_frame_rate":"30/1"}],"format":{"duration":"48.087074"}}
JSON
`
	if err := os.WriteFile(filepath.Join(bin, "ffprobe"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestFirstVideoFromPost(t *testing.T) {
	record := map[string]any{
		"post_content": []any{
			map[string]any{"type": "Photo", "url": "https://cdn.example/a.jpg"},
			map[string]any{"type": "Video", "url": "https://cdn.example/b.mp4"},
		},
	}
	if got := firstVideoFromPost(record); got != "https://cdn.example/b.mp4" {
		t.Fatalf("firstVideoFromPost = %q", got)
	}
	if got := firstVideoFromPost(map[string]any{"photos": []any{"https://cdn.example/a.jpg"}}); got != "" {
		t.Fatalf("photo-only post returned video %q", got)
	}
}

// The reels dataset calls its duration "length" (a float: 68.475647 for a
// 68-second reel) and the posts dataset "videos_duration"; neither matches the
// yt-dlp-style video_duration/duration keys this mapping originally read,
// which is how brightdata-provenance video captures shipped with no
// duration_seconds at all (capture 5pfAm, flagged 2026-08-31). This pins the
// mapping to the real field names.
func TestBrightDataInstagramVideoMetadataCarriesDuration(t *testing.T) {
	record := loadRecord(t, "instagram_reel.json")

	sidecar, _, err := buildBrightDataInstagramVideoArtifacts(record, "https://www.instagram.com/reel/DPAid-WDi67/", 1234, fixedArchiveTime(t))
	if err != nil {
		t.Fatalf("buildBrightDataInstagramVideoArtifacts: %v", err)
	}
	var metadata archivers.VideoMetadata
	if err := json.Unmarshal(sidecar.Data, &metadata); err != nil {
		t.Fatalf("decode normalized metadata: %v", err)
	}
	if metadata.DurationSeconds == nil || *metadata.DurationSeconds < 68.47 || *metadata.DurationSeconds > 68.48 {
		t.Errorf("duration_seconds = %v; want the reel record's length 68.475647", metadata.DurationSeconds)
	}
	if metadata.Title != "Video by bcydc" {
		t.Errorf("title = %q; want a stable single-video title", metadata.Title)
	}
	if metadata.Engagement.Likes == nil || *metadata.Engagement.Likes != 40 {
		t.Errorf("likes = %v; want 40", metadata.Engagement.Likes)
	}
	if metadata.Engagement.Comments == nil || *metadata.Engagement.Comments != 7 {
		t.Errorf("comments = %v; want 7", metadata.Engagement.Comments)
	}
}

func fixedArchiveTime(t *testing.T) time.Time {
	t.Helper()
	stamp, err := time.Parse(time.RFC3339, "2026-08-31T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	return stamp
}

// floatField has to read every shape the datasets actually serve: bare floats,
// stringified numbers, and the posts dataset's one-element videos_duration
// list of {url, video_duration} objects, while refusing ambiguous lists and
// unparseable values.
func TestFloatFieldReadsProviderShapes(t *testing.T) {
	cases := []struct {
		name   string
		record map[string]any
		want   *float64
	}{
		{"bare float", map[string]any{"length": 68.475647}, floatPtr(68.475647)},
		{"string number", map[string]any{"videos_duration": "18.994"}, floatPtr(18.994)},
		{"single-element object list", map[string]any{"videos_duration": []any{map[string]any{"url": "https://cdn.example/video.mp4", "video_duration": 18.994}}}, floatPtr(18.994)},
		{"multi-element list is ambiguous", map[string]any{"videos_duration": []any{18.994, 3.2}}, nil},
		{"absent", map[string]any{}, nil},
		{"null", map[string]any{"length": nil}, nil},
		{"human-formatted string", map[string]any{"length": "1.2K"}, nil},
	}
	for _, c := range cases {
		got := floatField(c.record, "length", "videos_duration")
		switch {
		case (got == nil) != (c.want == nil):
			t.Errorf("%s: floatField = %v; want %v", c.name, got, c.want)
		case got != nil && *got != *c.want:
			t.Errorf("%s: floatField = %v; want %v", c.name, *got, *c.want)
		}
	}
}

func floatPtr(v float64) *float64 { return &v }

func TestGalleryMetadataFromRecord(t *testing.T) {
	record := loadRecord(t, "instagram_post.json")

	meta := galleryMetadataFromRecord(record, "https://www.instagram.com/p/DbktPO1Eopi/")
	if meta.Extractor != "instagram" {
		t.Errorf("extractor = %q", meta.Extractor)
	}
	if meta.PostID != "DbktPO1Eopi" {
		t.Errorf("post id = %q; want DbktPO1Eopi", meta.PostID)
	}
	if meta.Author != "kinematronics" {
		t.Errorf("author = %q; want kinematronics", meta.Author)
	}
	if meta.Description == "" {
		t.Error("description is empty")
	}
	if meta.Likes == nil || *meta.Likes != 27662 {
		t.Errorf("likes = %v; want 27662", meta.Likes)
	}
	if meta.Comments == nil || *meta.Comments != 22 {
		t.Errorf("comments = %v; want 22 (num_comments)", meta.Comments)
	}
	if meta.Date == "" {
		t.Error("date is empty")
	}
}

func TestBuildGalleryZipLayout(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "001.jpg"), []byte("jpegbytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	meta := &archivers.GalleryMetadata{
		SourceURL: "https://www.instagram.com/p/x/",
		FileCount: 1,
		Files: []archivers.GalleryFile{
			{Name: "001.jpg", Size: 9, ContentType: "image/jpeg"},
		},
	}
	record := map[string]any{"shortcode": "x"}

	zipPath, err := buildGalleryZip(dir, meta, record)
	if err != nil {
		t.Fatalf("buildGalleryZip: %v", err)
	}
	defer os.Remove(zipPath)

	reader, err := zip.OpenReader(zipPath)
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}
	defer reader.Close()

	if len(reader.File) != 3 {
		t.Fatalf("zip has %d entries; want 3", len(reader.File))
	}
	// metadata.json must be first so streaming readers find it immediately,
	// matching the native gallery-dl artifact layout.
	if reader.File[0].Name != "metadata.json" {
		t.Errorf("first entry = %q; want metadata.json", reader.File[0].Name)
	}
	names := map[string]bool{}
	for _, f := range reader.File {
		names[f.Name] = true
	}
	for _, want := range []string{"metadata.json", "brightdata.json", "001.jpg"} {
		if !names[want] {
			t.Errorf("zip missing %s", want)
		}
	}
	// Media must be stored uncompressed, like the native flow.
	for _, f := range reader.File {
		if f.Name == "001.jpg" && f.Method != zip.Store {
			t.Errorf("media stored with method %d; want Store", f.Method)
		}
	}
}

func TestVerifyMP4(t *testing.T) {
	dir := t.TempDir()

	good := filepath.Join(dir, "good.mp4")
	header := append([]byte{0, 0, 0, 32}, []byte("ftypisom....")...)
	if err := os.WriteFile(good, header, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := verifyMP4(good); err != nil {
		t.Errorf("valid mp4 rejected: %v", err)
	}

	bad := filepath.Join(dir, "bad.mp4")
	if err := os.WriteFile(bad, []byte("<html><body>login required</body></html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := verifyMP4(bad); err == nil {
		t.Error("HTML error page accepted as MP4")
	}
}

func TestMediaEntryExtensionFromURL(t *testing.T) {
	cases := []struct {
		url, typ, want string
	}{
		{"https://cdn.example/photo.jpg?x=1", "Photo", ".jpg"},
		{"https://cdn.example/clip.mp4?sig=abc", "Video", ".mp4"},
		{"https://cdn.example/o1/v/t2/f2/m367/AQNoExtension?efg=x", "Video", ".mp4"},
		{"https://cdn.example/no-ext", "Photo", ".jpg"},
		{"https://cdn.example/pic.webp", "Photo", ".webp"},
	}
	for _, c := range cases {
		got := mediaEntry{URL: c.url, Type: c.typ}.extension()
		if got != c.want {
			t.Errorf("extension(%s, %s) = %q; want %q", c.url, c.typ, got, c.want)
		}
	}
}
