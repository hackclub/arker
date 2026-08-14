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
