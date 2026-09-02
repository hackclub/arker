package archivers

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestYtDlpDownloadArgsWriteRemuxedMP4File(t *testing.T) {
	outputTemplate := "/tmp/arker-video.%(ext)s"
	args := ytDlpDownloadArgs(outputTemplate)

	if hasArgPair(args, "-o", "-") {
		t.Fatal("yt-dlp download args write to stdout")
	}
	if !hasArgPair(args, "-o", outputTemplate) {
		t.Fatalf("yt-dlp download args do not include output template %q: %v", outputTemplate, args)
	}
	if !hasArgPair(args, "--merge-output-format", "mp4") {
		t.Fatalf("yt-dlp download args do not force MP4 merge output: %v", args)
	}
	if !hasArgPair(args, "--remux-video", "mp4") {
		t.Fatalf("yt-dlp download args do not remux final video to MP4: %v", args)
	}
}

func TestYtDlpDownloadArgsCaptureFullInfoJSON(t *testing.T) {
	args := ytDlpDownloadArgs("/tmp/arker-video.%(ext)s")
	for _, required := range []string{"--write-info-json", "--no-clean-infojson"} {
		found := false
		for _, arg := range args {
			if arg == required {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s missing from yt-dlp arguments: %v", required, args)
		}
	}
}

func TestBuildYtDlpVideoArtifactsFromFixture(t *testing.T) {
	raw, err := os.ReadFile("testdata/ytdlp_info.json")
	if err != nil {
		t.Fatal(err)
	}
	archivedAt := time.Date(2026, 8, 11, 21, 30, 0, 0, time.UTC)
	metadata, sanitized, err := BuildYtDlpVideoArtifacts(raw, "https://youtu.be/dQw4w9WgXcQ", "2026.08.11", VideoMedia{
		Extension:   ".mp4",
		ContentType: "video/mp4",
		SizeBytes:   987654321,
	}, archivedAt)
	if err != nil {
		t.Fatalf("BuildYtDlpVideoArtifacts: %v", err)
	}

	if metadata.SchemaVersion != VideoMetadataSchemaVersion || metadata.Platform != "youtube" || metadata.Extractor != "youtube" {
		t.Errorf("provider identity = version %q platform %q extractor %q", metadata.SchemaVersion, metadata.Platform, metadata.Extractor)
	}
	if metadata.PostID != "dQw4w9WgXcQ" || metadata.CanonicalURL != "https://www.youtube.com/watch?v=dQw4w9WgXcQ" {
		t.Errorf("post identity = id %q URL %q", metadata.PostID, metadata.CanonicalURL)
	}
	if metadata.Title != "Never Gonna Give You Up" || metadata.Author != "Rick Astley" || metadata.ChannelID != "UCuAXFkgsw1L7xaCfnd5JJOw" {
		t.Errorf("post fields not normalized: %+v", metadata)
	}
	if metadata.DurationSeconds == nil || *metadata.DurationSeconds != 212.25 {
		t.Errorf("duration = %v", metadata.DurationSeconds)
	}
	if metadata.Engagement.Views == nil || *metadata.Engagement.Views != 1600000000 || metadata.Engagement.Likes == nil || *metadata.Engagement.Likes != 18000000 {
		t.Errorf("engagement = %+v", metadata.Engagement)
	}
	if metadata.Media.SizeBytes != 987654321 || metadata.Media.Width == nil || *metadata.Media.Width != 1920 || metadata.Media.VideoCodec != "avc1.640028" {
		t.Errorf("media = %+v", metadata.Media)
	}
	if metadata.YtDlpVersion != "2026.08.11" || metadata.ArchivedAt != "2026-08-11T21:30:00Z" || metadata.Provenance != "native" {
		t.Errorf("capture fields not normalized: %+v", metadata)
	}

	var sanitizedObject map[string]interface{}
	if err := json.Unmarshal(sanitized, &sanitizedObject); err != nil {
		t.Fatalf("sanitized raw JSON is invalid: %v", err)
	}
	text := string(sanitized)
	for _, secret := range []string{
		"proxy-user", "proxy-pass", "private.proxy.example", "super-secret-cookie",
		"secret-access-token", "secret-signature", "10.20.30.40", "1999999999",
	} {
		if strings.Contains(text, secret) {
			t.Errorf("sanitized raw JSON leaked %q: %s", secret, text)
		}
	}
	if !strings.Contains(text, "[REDACTED]") || !strings.Contains(text, "yt-dlp test") {
		t.Errorf("sanitization removed useful data or omitted redaction marker: %s", text)
	}
}

func TestFindYtDlpInfoJSON(t *testing.T) {
	base := filepath.Join(t.TempDir(), "video")
	path := base + ".info.json"
	if err := os.WriteFile(path, []byte(`{"id":"abc"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := findYtDlpInfoJSON(base)
	if err != nil || got != path {
		t.Fatalf("findYtDlpInfoJSON = %q, %v; want %q", got, err, path)
	}
}

func TestFindDownloadedMP4PrefersFinalOutput(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "video")

	intermediatePath := base + ".fdash-123.mp4"
	finalPath := base + ".mp4"
	if err := os.WriteFile(intermediatePath, []byte("intermediate"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(finalPath, []byte("final"), 0644); err != nil {
		t.Fatal(err)
	}

	got, err := findDownloadedMP4(base)
	if err != nil {
		t.Fatalf("findDownloadedMP4 returned error: %v", err)
	}
	if got != finalPath {
		t.Fatalf("findDownloadedMP4 = %q, want %q", got, finalPath)
	}
}

func TestCreateTempVideoBaseUsesPrivateDirectory(t *testing.T) {
	base, err := createTempVideoBase()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(base)
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got&0o077 != 0 {
		t.Errorf("temp directory permissions = %o, want no group/other access", got)
	}
	if err := os.WriteFile(base+".info.json", []byte(`{"proxy":"secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cleanupTempVideoFiles(base)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("cleanupTempVideoFiles left private directory behind: %v", err)
	}
}

func TestTempVideoReaderRemovesFileOnClose(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "video.mp4")
	if err := os.WriteFile(path, []byte("video"), 0644); err != nil {
		t.Fatal(err)
	}

	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}

	reader := &tempVideoReader{File: file, path: path}
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "video" {
		t.Fatalf("tempVideoReader read %q, want video", data)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("tempVideoReader.Close returned error: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("tempVideoReader.Close did not remove file, stat err = %v", err)
	}
}

func TestCleanupTempVideoFilesExceptKeepsFinalOutput(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "video")
	keepPath := base + ".mp4"
	removePath := base + ".fdash-123.mp4"

	if err := os.WriteFile(keepPath, []byte("keep"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(removePath, []byte("remove"), 0644); err != nil {
		t.Fatal(err)
	}

	cleanupTempVideoFilesExcept(base, keepPath)

	if _, err := os.Stat(keepPath); err != nil {
		t.Fatalf("cleanupTempVideoFilesExcept removed kept file: %v", err)
	}
	if _, err := os.Stat(removePath); !os.IsNotExist(err) {
		t.Fatalf("cleanupTempVideoFilesExcept did not remove intermediate file, stat err = %v", err)
	}
}

func hasArgPair(args []string, key, value string) bool {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == key && args[i+1] == value {
			return true
		}
	}
	return false
}

// The platform's own poster image is the best available preview for a video,
// and it is free -- it used to be explicitly discarded via --no-write-thumbnail.
// This guards against that flag coming back.
func TestYtDlpDownloadArgsRequestThumbnail(t *testing.T) {
	args := ytDlpDownloadArgs("/tmp/arker-video-123.%(ext)s")

	var hasWrite bool
	for _, a := range args {
		if a == "--no-write-thumbnail" {
			t.Fatal("--no-write-thumbnail is back; it throws away the video's own poster image")
		}
		if a == "--write-thumbnail" {
			hasWrite = true
		}
	}
	if !hasWrite {
		t.Errorf("--write-thumbnail missing from %v", args)
	}
}

func TestFindDownloadedThumbnail(t *testing.T) {
	for _, tc := range []struct {
		name    string
		files   []string
		video   string
		wantExt string
	}{
		{"jpg beside video", []string{".mp4", ".jpg"}, ".mp4", ".jpg"},
		{"webp beside video", []string{".mp4", ".webp"}, ".mp4", ".webp"},
		{"png beside video", []string{".mp4", ".png"}, ".mp4", ".png"},
		{"no thumbnail written", []string{".mp4"}, ".mp4", ""},
		{"ignores non-image siblings", []string{".mp4", ".part", ".json"}, ".mp4", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := filepath.Join(t.TempDir(), "arker-video-abc")
			for _, ext := range tc.files {
				if err := os.WriteFile(base+ext, []byte("x"), 0o600); err != nil {
					t.Fatalf("write %s: %v", ext, err)
				}
			}
			got := findDownloadedThumbnail(base, base+tc.video)
			if tc.wantExt == "" {
				if got != "" {
					t.Errorf("found %q, want none", got)
				}
				return
			}
			if got != base+tc.wantExt {
				t.Errorf("found %q, want %q", got, base+tc.wantExt)
			}
		})
	}
}

// The video must never be mistaken for its own poster image.
func TestFindDownloadedThumbnailNeverReturnsTheVideo(t *testing.T) {
	base := filepath.Join(t.TempDir(), "arker-video-abc")
	if err := os.WriteFile(base+".mp4", []byte("video"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := findDownloadedThumbnail(base, base+".mp4"); got != "" {
		t.Errorf("returned %q, which is the video itself", got)
	}
}

// A missing or corrupt poster is not an archive failure.
func TestVideoThumbnailToleratesBadInput(t *testing.T) {
	base := filepath.Join(t.TempDir(), "arker-video-abc")
	if err := os.WriteFile(base+".mp4", []byte("video"), 0o600); err != nil {
		t.Fatalf("write video: %v", err)
	}

	if got := videoThumbnail(base, base+".mp4", io.Discard); got != nil {
		t.Errorf("expected nil when no thumbnail was written, got %+v", got)
	}

	if err := os.WriteFile(base+".jpg", []byte("not actually a jpeg"), 0o600); err != nil {
		t.Fatalf("write thumb: %v", err)
	}
	if got := videoThumbnail(base, base+".mp4", io.Discard); got != nil {
		t.Errorf("expected nil for undecodable poster, got %+v", got)
	}
}

// The happy path: the platform's real poster is retained byte-for-byte rather
// than being cropped and re-encoded into Arker's old 16:9 JPEG tile.
func TestVideoThumbnailFromRealImage(t *testing.T) {
	base := filepath.Join(t.TempDir(), "arker-video-abc")
	if err := os.WriteFile(base+".mp4", []byte("video"), 0o600); err != nil {
		t.Fatalf("write video: %v", err)
	}

	// Portrait, like a reel cover: red top, green middle, blue bottom.
	src := image.NewRGBA(image.Rect(0, 0, 1080, 1920))
	for y := 0; y < 1920; y++ {
		c := color.RGBA{255, 0, 0, 255}
		switch {
		case y >= 1280:
			c = color.RGBA{0, 0, 255, 255}
		case y >= 640:
			c = color.RGBA{0, 255, 0, 255}
		}
		for x := 0; x < 1080; x++ {
			src.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, src, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("encode: %v", err)
	}
	if err := os.WriteFile(base+".jpg", buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write thumb: %v", err)
	}

	got := videoThumbnail(base, base+".mp4", io.Discard)
	if got == nil {
		t.Fatal("expected a thumbnail")
	}
	if got.Width != 270 || got.Height != 480 {
		t.Errorf("size = %dx%d, want aspect-correct row preview 270x480", got.Width, got.Height)
	}
	if bytes.Equal(got.Data, buf.Bytes()) {
		t.Error("oversized video poster was not compacted")
	}
}
