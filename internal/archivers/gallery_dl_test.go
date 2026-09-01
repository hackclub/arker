package archivers

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// writeGalleryFixture lays out a directory the way gallery-dl does with the
// flags the archiver passes: flat, numeric filenames, one JSON sidecar per file.
func writeGalleryFixture(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, contents := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dir
}

const instagramCarouselSidecar = `{
  "category": "instagram",
  "subcategory": "post",
  "post_id": "3955281333542808561",
  "post_shortcode": "DbktPO1Eopi",
  "post_url": "https://www.instagram.com/p/DbktPO1Eopi/",
  "username": "agentverseinsta",
  "fullname": "Agent Verse",
  "description": "the news community is buzzing",
  "post_date": "2026-08-04 18:12:03",
  "likes": 42,
  "tags": ["coding", "devops"],
  "sidecar_media_id": "3955281333542808561",
  "width": 1080,
  "height": 1350,
  "num": 1,
  "count": 3
}`

func TestCollectGalleryFilesSeparatesMediaFromSidecars(t *testing.T) {
	dir := writeGalleryFixture(t, map[string]string{
		"001.jpg":      "a",
		"001.jpg.json": "{}",
		"002.mp4":      "b",
		"002.mp4.json": "{}",
		"003.jpg":      "c",
	})

	media, sidecars, err := collectGalleryFiles(dir)
	if err != nil {
		t.Fatalf("collectGalleryFiles: %v", err)
	}

	want := []string{"001.jpg", "002.mp4", "003.jpg"}
	if len(media) != len(want) {
		t.Fatalf("media = %v, want %v", media, want)
	}
	for i := range want {
		if media[i] != want[i] {
			t.Fatalf("media = %v, want %v (must be sorted so slide order is stable)", media, want)
		}
	}
	if sidecars["001.jpg"] != "001.jpg.json" {
		t.Errorf("sidecars[001.jpg] = %q, want 001.jpg.json", sidecars["001.jpg"])
	}
	// A media file with no sidecar must still be listed, not dropped.
	if _, ok := sidecars["003.jpg"]; ok {
		t.Error("003.jpg should have no sidecar")
	}
}

func TestBuildGalleryMetadataFromInstagramCarousel(t *testing.T) {
	dir := writeGalleryFixture(t, map[string]string{
		"001.jpg":      "aaaa",
		"001.jpg.json": instagramCarouselSidecar,
		"002.mp4":      "bb",
		"002.mp4.json": `{"width": 720, "height": 1280}`,
	})

	media, sidecars, err := collectGalleryFiles(dir)
	if err != nil {
		t.Fatalf("collectGalleryFiles: %v", err)
	}
	meta := buildGalleryMetadata(dir, "https://www.instagram.com/p/DbktPO1Eopi/", "1.32.9", media, sidecars, io.Discard)

	if meta.Extractor != "instagram" {
		t.Errorf("Extractor = %q, want instagram", meta.Extractor)
	}
	if meta.Author != "agentverseinsta" {
		t.Errorf("Author = %q, want agentverseinsta", meta.Author)
	}
	if meta.AuthorName != "Agent Verse" {
		t.Errorf("AuthorName = %q, want Agent Verse", meta.AuthorName)
	}
	if meta.Description != "the news community is buzzing" {
		t.Errorf("Description = %q, want the caption text", meta.Description)
	}
	if meta.PostID != "3955281333542808561" {
		t.Errorf("PostID = %q, want 3955281333542808561", meta.PostID)
	}
	if meta.Date != "2026-08-04 18:12:03" {
		t.Errorf("Date = %q, want the post date", meta.Date)
	}
	if meta.Likes == nil || *meta.Likes != 42 {
		t.Errorf("Likes = %v, want 42", meta.Likes)
	}
	if len(meta.Tags) != 2 {
		t.Errorf("Tags = %v, want 2 entries", meta.Tags)
	}
	if meta.FileCount != 2 || len(meta.Files) != 2 {
		t.Fatalf("FileCount = %d / Files = %d, want 2 each", meta.FileCount, len(meta.Files))
	}

	image := meta.Files[0]
	if image.ContentType != "image/jpeg" || image.IsVideo {
		t.Errorf("file[0] = %+v, want a non-video image/jpeg", image)
	}
	if image.Size != 4 {
		t.Errorf("file[0].Size = %d, want 4", image.Size)
	}
	if image.Width != 1080 || image.Height != 1350 {
		t.Errorf("file[0] dimensions = %dx%d, want 1080x1350", image.Width, image.Height)
	}

	// A carousel can mix stills and video; the video slide must be marked so
	// the viewer renders a player instead of a broken image.
	video := meta.Files[1]
	if video.ContentType != "video/mp4" || !video.IsVideo {
		t.Errorf("file[1] = %+v, want a video/mp4 marked IsVideo", video)
	}
}

func TestBuildGalleryMetadataToleratesMissingAndMalformedSidecars(t *testing.T) {
	dir := writeGalleryFixture(t, map[string]string{
		"001.jpg":      "a",
		"001.jpg.json": "{not json",
		"002.png":      "b",
	})

	media, sidecars, err := collectGalleryFiles(dir)
	if err != nil {
		t.Fatalf("collectGalleryFiles: %v", err)
	}
	meta := buildGalleryMetadata(dir, "https://example.com/post/1", "1.32.9", media, sidecars, io.Discard)

	// Unparseable metadata must degrade to "no metadata", never lose the media.
	if meta.FileCount != 2 {
		t.Errorf("FileCount = %d, want 2", meta.FileCount)
	}
	if meta.SourceURL != "https://example.com/post/1" {
		t.Errorf("SourceURL = %q, want the source URL", meta.SourceURL)
	}
	if meta.Author != "" || meta.Description != "" {
		t.Errorf("expected empty post fields, got author=%q description=%q", meta.Author, meta.Description)
	}
}

// Some extractors nest the author in an object rather than a flat username.
// Unwrapping that is resolveGalleryAuthor's job, not galleryString's:
// galleryString stays a plain top-level string lookup so callers can control
// whether the top level or a container object wins.
func TestNestedAuthorObjectsAreResolvedByAuthorResolver(t *testing.T) {
	raw := map[string]interface{}{
		"user": map[string]interface{}{"name": "someone"},
	}
	if got := galleryString(raw, "user"); got != "" {
		t.Errorf("galleryString on an object = %q, want empty", got)
	}
	if handle, _ := resolveGalleryAuthor(raw); handle != "someone" {
		t.Errorf("resolveGalleryAuthor = %q, want someone", handle)
	}
}

func TestWriteGalleryZipContainsMetadataAndMedia(t *testing.T) {
	dir := writeGalleryFixture(t, map[string]string{
		"001.jpg":      "image-bytes",
		"001.jpg.json": instagramCarouselSidecar,
	})

	var buf bytes.Buffer
	zipWriter := zip.NewWriter(&buf)
	if err := writeGalleryZip(zipWriter, dir, []byte(`{"source_url":"x"}`), io.Discard); err != nil {
		t.Fatalf("writeGalleryZip: %v", err)
	}
	if err := zipWriter.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}

	reader, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("read zip: %v", err)
	}

	got := map[string]string{}
	for _, file := range reader.File {
		contents, err := file.Open()
		if err != nil {
			t.Fatalf("open %s: %v", file.Name, err)
		}
		data, err := io.ReadAll(contents)
		contents.Close()
		if err != nil {
			t.Fatalf("read %s: %v", file.Name, err)
		}
		got[file.Name] = string(data)
	}

	// Arker's metadata.json must be present alongside gallery-dl's raw sidecar,
	// so the archive is self-describing without re-running gallery-dl.
	for _, name := range []string{galleryMetadataFilename, "001.jpg", "001.jpg.json"} {
		if _, ok := got[name]; !ok {
			t.Errorf("zip is missing %s (has %v)", name, keys(got))
		}
	}
	if got["001.jpg"] != "image-bytes" {
		t.Errorf("001.jpg = %q, want image-bytes", got["001.jpg"])
	}
	var metadata map[string]interface{}
	if err := json.Unmarshal([]byte(got[galleryMetadataFilename]), &metadata); err != nil {
		t.Errorf("metadata.json is not valid JSON: %v", err)
	}
}

// A file that cannot be read must fail the whole archive rather than being
// silently skipped, or a partial ZIP gets stored as completed.
func TestWriteGalleryZipPropagatesFileError(t *testing.T) {
	dir := writeGalleryFixture(t, map[string]string{"001.jpg": "a"})
	if err := os.Chmod(filepath.Join(dir, "001.jpg"), 0o000); err != nil {
		t.Skipf("cannot chmod in this environment: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(dir, "001.jpg"), 0o600) })

	if os.Geteuid() == 0 {
		t.Skip("running as root; permission bits do not restrict reads")
	}

	zipWriter := zip.NewWriter(io.Discard)
	if err := writeGalleryZip(zipWriter, dir, []byte("{}"), io.Discard); err == nil {
		t.Fatal("expected writeGalleryZip to fail on an unreadable file, got nil")
	}
}

func TestGalleryContentType(t *testing.T) {
	tests := map[string]struct {
		contentType string
		isVideo     bool
	}{
		"001.jpg":  {"image/jpeg", false},
		"001.JPEG": {"image/jpeg", false},
		"002.png":  {"image/png", false},
		"003.webp": {"image/webp", false},
		"004.mp4":  {"video/mp4", true},
		"005.webm": {"video/webm", true},
		"006.mov":  {"video/quicktime", true},
		"007.bin":  {"application/octet-stream", false},
	}

	for name, want := range tests {
		got := galleryContentType(name)
		if got != want.contentType {
			t.Errorf("galleryContentType(%q) = %q, want %q", name, got, want.contentType)
		}
		if strings.HasPrefix(got, "video/") != want.isVideo {
			t.Errorf("galleryContentType(%q) video detection = %v, want %v", name, !want.isVideo, want.isVideo)
		}
	}
}

// gallery-dl's exit status is a bitmask, so a single run can report several
// causes at once. Decoding it as an enum would mislabel most real failures.
func TestDescribeGalleryDlExitDecodesBitmask(t *testing.T) {
	tests := []struct {
		code     int
		contains []string
	}{
		{64, []string{"no gallery-dl extractor"}},
		{4, []string{"extraction or download failed"}},
		{16, []string{"authentication"}},
		{8, []string{"anti-bot challenge"}},
		{20, []string{"extraction or download failed", "authentication"}},
		{68, []string{"extraction or download failed", "no gallery-dl extractor"}},
	}

	for _, tt := range tests {
		err := exitErrorWithCode(t, tt.code)
		got := describeGalleryDlExit(err)
		for _, want := range tt.contains {
			if !strings.Contains(got, want) {
				t.Errorf("describeGalleryDlExit(%d) = %q, want it to mention %q", tt.code, got, want)
			}
		}
	}

	if got := describeGalleryDlExit(errors.New("boom")); got != "gallery-dl did not run" {
		t.Errorf("describeGalleryDlExit(non-exit error) = %q, want %q", got, "gallery-dl did not run")
	}
}

// The archiver must never let a gallery-dl config file on the host change what
// it does, and must not let gallery-dl rewrite the shared cookie jar.
func TestGalleryDlDownloadArgsAreHermetic(t *testing.T) {
	args := galleryDlDownloadArgs("/tmp/out")
	joined := strings.Join(args, " ")

	for _, want := range []string{
		"--config-ignore",
		"-D /tmp/out",
		"cookies-update=false",
		"--write-metadata",
		"--no-part",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("gallery-dl args %v missing %q", args, want)
		}
	}

	// -d keeps gallery-dl's per-site subdirectory tree; only -D flattens it,
	// and a flat layout is what the ZIP and the viewer assume.
	for i, arg := range args {
		if arg == "-d" {
			t.Errorf("args[%d] uses -d, which preserves subdirectories; want -D", i)
		}
	}
}

func exitErrorWithCode(t *testing.T, code int) error {
	t.Helper()
	err := exec.Command("sh", "-c", "exit "+itoa(code)).Run()
	if err == nil {
		t.Fatalf("expected a non-zero exit for code %d", code)
	}
	return err
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

func keys(m map[string]string) []string {
	result := make([]string, 0, len(m))
	for key := range m {
		result = append(result, key)
	}
	return result
}

// JSON numbers decode to float64 by default, which silently rounds integers
// above 2^53. Real X and Instagram post IDs are in that range, so the ID
// recorded in metadata.json would not match the ID on the site.
func TestBuildGalleryMetadataPreservesLargeNumericIDs(t *testing.T) {
	dir := writeGalleryFixture(t, map[string]string{
		"001.jpg": "a",
		"001.jpg.json": `{
			"category": "twitter",
			"tweet_id": 1234567890123456789,
			"likes": 9007199254740995,
			"width": 1200,
			"height": 675
		}`,
	})

	media, sidecars, err := collectGalleryFiles(dir)
	if err != nil {
		t.Fatalf("collectGalleryFiles: %v", err)
	}
	meta := buildGalleryMetadata(dir, "https://x.com/a/status/1234567890123456789", "1.32.9", media, sidecars, io.Discard)

	if meta.PostID != "1234567890123456789" {
		t.Errorf("PostID = %q, want 1234567890123456789 exactly (float64 would give ...456768)", meta.PostID)
	}
	if meta.Likes == nil || *meta.Likes != 9007199254740995 {
		t.Errorf("Likes = %v, want 9007199254740995 exactly", meta.Likes)
	}
	if meta.Files[0].Width != 1200 || meta.Files[0].Height != 675 {
		t.Errorf("dimensions = %dx%d, want 1200x675", meta.Files[0].Width, meta.Files[0].Height)
	}
}

// Bluesky nests the poster in author{handle,displayName}, counts approval as
// likeCount, and — critically — uses "text" for the post body while
// "description" holds the image's alt text. Reading description as the caption
// put alt text on the page where the post should be.
const blueskySidecar = `{
  "category": "bluesky",
  "subcategory": "post",
  "post_id": "3mqafridzgk2e",
  "author": {"did": "did:plc:z72", "handle": "bsky.app", "displayName": "Bluesky"},
  "likeCount": 3485,
  "repostCount": 582,
  "text": "v1.127 is live! We're rolling out improvements to search",
  "description": "A rendering of the new Filters button",
  "date": "2026-07-09 19:50:16",
  "width": 2140,
  "height": 2000
}`

// Imgur describes the individual image at the top level and keeps the album's
// title, URL, uploader and vote counts in a nested object.
const imgurAlbumSidecar = `{
  "category": "imgur",
  "subcategory": "album",
  "id": "kKu3U5P",
  "url": "https://i.imgur.com/kKu3U5P.jpg",
  "title": "",
  "description": "",
  "width": 1537,
  "height": 2048,
  "date": "2026-08-07 14:32:03",
  "album": {
    "id": "zJjxIyO",
    "title": "The Baroness",
    "url": "https://imgur.com/a/zJjxIyO",
    "upvote_count": 366,
    "point_count": 342,
    "favorite_count": 0,
    "score": 0,
    "image_count": 5,
    "account": {"id": 27958845, "username": "somebody"}
  }
}`

func metaFrom(t *testing.T, sidecar string) *GalleryMetadata {
	t.Helper()
	dir := writeGalleryFixture(t, map[string]string{
		"001.jpg":      "x",
		"001.jpg.json": sidecar,
	})
	media, sidecars, err := collectGalleryFiles(dir)
	if err != nil {
		t.Fatalf("collectGalleryFiles: %v", err)
	}
	return buildGalleryMetadata(dir, "https://example.com/post", "1.32.9", media, sidecars, io.Discard)
}

func TestBuildGalleryMetadataFromBlueskyPost(t *testing.T) {
	meta := metaFrom(t, blueskySidecar)

	if meta.Author != "bsky.app" {
		t.Errorf("Author = %q, want bsky.app (from the nested author object)", meta.Author)
	}
	if meta.AuthorName != "Bluesky" {
		t.Errorf("AuthorName = %q, want Bluesky", meta.AuthorName)
	}
	if meta.Likes == nil || *meta.Likes != 3485 {
		t.Errorf("Likes = %v, want 3485 (likeCount)", meta.Likes)
	}
	// The regression that motivated this: alt text must not become the caption.
	if meta.Description != "v1.127 is live! We're rolling out improvements to search" {
		t.Errorf("Description = %q, want the post text, not the image alt text", meta.Description)
	}
}

func TestBuildGalleryMetadataFromImgurAlbum(t *testing.T) {
	meta := metaFrom(t, imgurAlbumSidecar)

	if meta.Title != "The Baroness" {
		t.Errorf("Title = %q, want The Baroness (from album.title)", meta.Title)
	}
	if meta.Author != "somebody" {
		t.Errorf("Author = %q, want somebody (from album.account.username)", meta.Author)
	}
	// The album carries upvote_count 366 and favorite_count 0 side by side;
	// a key order that checked favourites first reported 0 likes on a post
	// with hundreds.
	if meta.Likes == nil || *meta.Likes != 366 {
		t.Errorf("Likes = %v, want 366 (album.upvote_count, not favorite_count 0)", meta.Likes)
	}
	// The album identifies the post; the top-level id/url describe one image.
	if meta.PostID != "zJjxIyO" {
		t.Errorf("PostID = %q, want the album id zJjxIyO, not the image id", meta.PostID)
	}
	if meta.PostURL != "https://imgur.com/a/zJjxIyO" {
		t.Errorf("PostURL = %q, want the album URL, not the image CDN URL", meta.PostURL)
	}
}

// Instagram must keep working: it has no "text" key, flat username/fullname,
// and its caption genuinely lives in "description".
func TestBuildGalleryMetadataInstagramStillResolves(t *testing.T) {
	meta := metaFrom(t, instagramCarouselSidecar)

	if meta.Author != "agentverseinsta" || meta.AuthorName != "Agent Verse" {
		t.Errorf("author = %q / %q, want agentverseinsta / Agent Verse", meta.Author, meta.AuthorName)
	}
	if meta.Description != "the news community is buzzing" {
		t.Errorf("Description = %q, want the caption from description", meta.Description)
	}
	if meta.Likes == nil || *meta.Likes != 42 {
		t.Errorf("Likes = %v, want 42", meta.Likes)
	}
	if meta.PostID != "3955281333542808561" {
		t.Errorf("PostID = %q, want the post_id", meta.PostID)
	}
}

func TestResolveGalleryAuthorEdgeCases(t *testing.T) {
	// A bare string author.
	h, d := resolveGalleryAuthor(map[string]interface{}{"author": "someone"})
	if h != "someone" || d != "" {
		t.Errorf("bare string author = %q/%q, want someone/''", h, d)
	}
	// Only a display name available: it becomes the handle rather than
	// rendering an empty handle with a parenthesised name.
	h, d = resolveGalleryAuthor(map[string]interface{}{"fullname": "Some One"})
	if h != "Some One" || d != "" {
		t.Errorf("display-only = %q/%q, want Some One/''", h, d)
	}
	// Identical handle and display name must not render as "x (x)".
	h, d = resolveGalleryAuthor(map[string]interface{}{
		"author": map[string]interface{}{"handle": "x", "displayName": "x"},
	})
	if h != "x" || d != "" {
		t.Errorf("duplicate name = %q/%q, want x/''", h, d)
	}
	// Nothing at all.
	if h, d = resolveGalleryAuthor(map[string]interface{}{}); h != "" || d != "" {
		t.Errorf("empty = %q/%q, want empty", h, d)
	}
}

// writeTestImage writes a solid-colour JPEG so a test can identify which file
// the thumbnail was built from by its colour.
func writeTestImage(t *testing.T, path string, c color.RGBA) {
	t.Helper()
	m := image.NewRGBA(image.Rect(0, 0, 900, 900))
	for y := 0; y < 900; y++ {
		for x := 0; x < 900; x++ {
			m.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, m, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("encode: %v", err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func avgRGB(t *testing.T, data []byte) (r, g, b int) {
	t.Helper()
	img, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	bounds := img.Bounds()
	var sr, sg, sb, n int
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			cr, cg, cb, _ := img.At(x, y).RGBA()
			sr += int(cr >> 8)
			sg += int(cg >> 8)
			sb += int(cb >> 8)
			n++
		}
	}
	return sr / n, sg / n, sb / n
}

func TestGalleryThumbnailUsesFirstStillImage(t *testing.T) {
	dir := t.TempDir()
	writeTestImage(t, filepath.Join(dir, "001.jpg"), color.RGBA{220, 20, 20, 255})
	writeTestImage(t, filepath.Join(dir, "002.jpg"), color.RGBA{20, 20, 220, 255})

	meta := &GalleryMetadata{Files: []GalleryFile{
		{Name: "001.jpg", ContentType: "image/jpeg"},
		{Name: "002.jpg", ContentType: "image/jpeg"},
	}}

	got := galleryThumbnail(dir, meta, io.Discard)
	if got == nil {
		t.Fatal("expected a thumbnail")
	}
	if r, _, b := avgRGB(t, got.Data); r < 150 || b > 100 {
		t.Errorf("used the wrong slide: avg r=%d b=%d, want slide 1 (red)", r, b)
	}
}

// A carousel that opens with a video should fall through to its first photo
// rather than giving up.
func TestGalleryThumbnailSkipsLeadingVideo(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "001.mp4"), []byte("video bytes"), 0o600); err != nil {
		t.Fatalf("write video: %v", err)
	}
	writeTestImage(t, filepath.Join(dir, "002.jpg"), color.RGBA{20, 220, 20, 255})

	meta := &GalleryMetadata{Files: []GalleryFile{
		{Name: "001.mp4", ContentType: "video/mp4", IsVideo: true},
		{Name: "002.jpg", ContentType: "image/jpeg"},
	}}

	got := galleryThumbnail(dir, meta, io.Discard)
	if got == nil {
		t.Fatal("expected the photo slide to be used")
	}
	if _, g, _ := avgRGB(t, got.Data); g < 150 {
		t.Errorf("expected the green photo slide, got g=%d", g)
	}
}

func TestGalleryThumbnailReturnsNilWhenNoStillImage(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "001.mp4"), []byte("video"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	meta := &GalleryMetadata{Files: []GalleryFile{
		{Name: "001.mp4", ContentType: "video/mp4", IsVideo: true},
	}}
	if got := galleryThumbnail(dir, meta, io.Discard); got != nil {
		t.Errorf("all-video post produced a thumbnail: %+v", got)
	}
}

// A corrupt slide must not abort the whole preview; the next one should work.
func TestGalleryThumbnailSkipsCorruptImage(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "001.jpg"), []byte("not a jpeg"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	writeTestImage(t, filepath.Join(dir, "002.jpg"), color.RGBA{20, 220, 20, 255})

	meta := &GalleryMetadata{Files: []GalleryFile{
		{Name: "001.jpg", ContentType: "image/jpeg"},
		{Name: "002.jpg", ContentType: "image/jpeg"},
	}}

	got := galleryThumbnail(dir, meta, io.Discard)
	if got == nil {
		t.Fatal("expected fallback to the second slide")
	}
	if _, g, _ := avgRGB(t, got.Data); g < 150 {
		t.Errorf("expected the green second slide, got g=%d", g)
	}
}

func TestGalleryThumbnailToleratesNilMetadata(t *testing.T) {
	if got := galleryThumbnail(t.TempDir(), nil, io.Discard); got != nil {
		t.Errorf("expected nil, got %+v", got)
	}
}

// Alt text is often the only description an image-only post carries, so it is
// worth keeping — but only where the extractor genuinely means alt text.
func TestGalleryAltTextResolution(t *testing.T) {
	// Bluesky stores the post body in "text" and the image's alt text in
	// "description". Reading "description" generically would put Instagram's
	// caption on every slide as if the poster had described each image.
	blueskyMeta := metaFrom(t, blueskySidecar)
	if blueskyMeta.Files[0].AltText != "A rendering of the new Filters button" {
		t.Errorf("bluesky alt text = %q, want the image description", blueskyMeta.Files[0].AltText)
	}
	if blueskyMeta.Description != "v1.127 is live! We're rolling out improvements to search" {
		t.Errorf("the caption was disturbed: %q", blueskyMeta.Description)
	}

	instagramMeta := metaFrom(t, instagramCarouselSidecar)
	if instagramMeta.Files[0].AltText != "" {
		t.Errorf("instagram alt text = %q, want empty: its description is the post caption",
			instagramMeta.Files[0].AltText)
	}

	// An explicit alt field is unambiguous wherever it appears.
	explicit := metaFrom(t, `{"category":"twitter","tweet_id":1,"ext_alt_text":"a hand-drawn map","width":10,"height":10}`)
	if explicit.Files[0].AltText != "a hand-drawn map" {
		t.Errorf("explicit alt text = %q", explicit.Files[0].AltText)
	}
}
