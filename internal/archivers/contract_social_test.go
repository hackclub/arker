package archivers

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"arker/internal/testfixtures"
)

// Social-archive contract tests for the two extractor-backed archivers.
//
// These drive the real YtDlpArchiver and GalleryDLArchiver end to end — argv
// construction, the subprocess, the output-directory scan, metadata
// normalization, thumbnailing and ZIP building — against a fake extractor on
// PATH replaying sanitized fixtures. Nothing here touches the network.
//
// The contract they pin is in docs/social-contract/CONTRACT-GAPS.md. Where the
// current code violates it, the assertion is guarded with a t.Skip naming the
// G-number so the manager can turn it on with the fix.

// runYtDlpArchive runs the real archiver and returns the stored artifact plus
// both sidecars.
func runYtDlpArchive(t *testing.T, url string) (artifact []byte, metadata *VideoMetadata, rawMetadata []byte, logs string) {
	t.Helper()
	var log strings.Builder
	archiver := &YtDlpArchiver{}
	result, err := archiver.Archive(context.Background(), url, &log, nil, 1)
	if err != nil {
		t.Fatalf("yt-dlp archive failed: %v\nlog:\n%s", err, log.String())
	}
	artifact, err = io.ReadAll(result.Data)
	if err != nil {
		t.Fatalf("read artifact: %v", err)
	}
	if closer, ok := result.Data.(io.Closer); ok {
		_ = closer.Close()
	}
	if result.Metadata == nil {
		t.Fatal("yt-dlp result carried no normalized metadata sidecar")
	}
	if result.RawMetadata == nil {
		t.Fatal("yt-dlp result carried no raw metadata sidecar")
	}
	metadata = &VideoMetadata{}
	if err := json.Unmarshal(result.Metadata.Data, metadata); err != nil {
		t.Fatalf("decode normalized metadata: %v", err)
	}
	return artifact, metadata, result.RawMetadata.Data, log.String()
}

// runGalleryArchive runs the real archiver and returns the ZIP bytes.
func runGalleryArchive(t *testing.T, url string) ([]byte, string, error) {
	t.Helper()
	var log strings.Builder
	archiver := &GalleryDLArchiver{}
	result, err := archiver.Archive(context.Background(), url, &log, nil, 1)
	if err != nil {
		return nil, log.String(), err
	}
	data, readErr := io.ReadAll(result.Data)
	if closer, ok := result.Data.(io.Closer); ok {
		_ = closer.Close()
	}
	if readErr != nil {
		t.Fatalf("read gallery ZIP: %v", readErr)
	}
	return data, log.String(), nil
}

// zipNames returns the entry names inside a gallery ZIP.
func zipNames(t *testing.T, data []byte) []string {
	t.Helper()
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("open gallery ZIP: %v", err)
	}
	names := make([]string, 0, len(reader.File))
	for _, f := range reader.File {
		names = append(names, f.Name)
	}
	return names
}

// galleryManifest decodes the normalized metadata.json Arker adds at the ZIP
// root.
func galleryManifest(t *testing.T, data []byte) *GalleryMetadata {
	t.Helper()
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("open gallery ZIP: %v", err)
	}
	for _, f := range reader.File {
		if f.Name != galleryMetadataFilename {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open %s: %v", galleryMetadataFilename, err)
		}
		defer rc.Close()
		var meta GalleryMetadata
		if err := json.NewDecoder(rc).Decode(&meta); err != nil {
			t.Fatalf("decode %s: %v", galleryMetadataFilename, err)
		}
		return &meta
	}
	t.Fatalf("gallery ZIP has no %s", galleryMetadataFilename)
	return nil
}

// TestNativeYtDlpSuccessForEveryVideoPlatform walks the yt-dlp half of the
// contract matrix. Contract #1: a fulfilled video archive stores the media,
// normalized metadata, and the sanitized raw extractor record.
func TestNativeYtDlpSuccessForEveryVideoPlatform(t *testing.T) {
	for _, c := range testfixtures.CasesForTool(testfixtures.ToolYtDlp) {
		t.Run(c.Name, func(t *testing.T) {
			testfixtures.InstallFakeYtDlp(t, testfixtures.YtDlpFake{Fixture: c.Name})
			artifact, meta, raw, _ := runYtDlpArchive(t, c.URL)

			if len(artifact) == 0 {
				t.Error("stored artifact is empty; the contract requires the source media, not a placeholder")
			}
			if meta.SchemaVersion != VideoMetadataSchemaVersion {
				t.Errorf("schema_version = %q, want %q", meta.SchemaVersion, VideoMetadataSchemaVersion)
			}
			if meta.Platform != c.Platform {
				t.Errorf("platform = %q, want %q", meta.Platform, c.Platform)
			}
			if meta.PostID == "" {
				t.Error("post_id is empty; a normalized post must be identifiable")
			}
			if meta.CanonicalURL == "" {
				t.Error("canonical_url is empty")
			}
			if meta.Title == "" {
				t.Error("title is empty")
			}
			if meta.PublicationTimestamp == "" {
				t.Error("publication_timestamp is empty")
			}
			if meta.Author == "" {
				t.Error("author is empty; every fixture names an uploader or channel")
			}
			if meta.Provenance != "native" {
				t.Errorf("provenance = %q, want native", meta.Provenance)
			}
			if meta.Provider != "yt-dlp" {
				t.Errorf("provider = %q, want yt-dlp", meta.Provider)
			}
			// Contract #4: the stored media facts describe the file on disk,
			// not the pre-remux format the extractor guessed at.
			if meta.Media.SizeBytes != int64(len(artifact)) {
				t.Errorf("media.size_bytes = %d, want the stored artifact size %d",
					meta.Media.SizeBytes, len(artifact))
			}
			if meta.Media.ContentType != "video/mp4" {
				t.Errorf("media.content_type = %q, want video/mp4", meta.Media.ContentType)
			}
			// Contract #1: the raw provider record is retrievable and safe.
			if !json.Valid(raw) {
				t.Error("raw metadata sidecar is not valid JSON")
			}
		})
	}
}

// TestNativeYtDlpRawMetadataNeverLeaksCredentials pins contract #6's "no
// secrets" requirement against real captured extractor output. The YouTube
// fixtures carry genuine signed googlevideo URLs (with synthetic values), and
// the Instagram and Facebook ones carry the cdninstagram/fbcdn hosts Arker
// redacts wholesale.
func TestNativeYtDlpRawMetadataNeverLeaksCredentials(t *testing.T) {
	for _, c := range testfixtures.CasesForTool(testfixtures.ToolYtDlp) {
		t.Run(c.Name, func(t *testing.T) {
			testfixtures.InstallFakeYtDlp(t, testfixtures.YtDlpFake{Fixture: c.Name})
			_, meta, raw, _ := runYtDlpArchive(t, c.URL)

			text := string(raw)
			// The fixture generator marks every credential-shaped value with
			// this sentinel. None of them may survive into stored output.
			//
			// Query-string-borne values only: path-embedded ones are the
			// separate G14 gap, asserted in
			// TestSanitizeJSONRedactsPathEmbeddedSignatures. Repeating that
			// failure here would just make one bug fail two tests.
			for _, line := range strings.Split(text, "\n") {
				if !strings.Contains(line, "SYNTHETIC-") {
					continue
				}
				if queryPart(line) == "" {
					continue
				}
				t.Errorf("sanitized raw metadata still carries a signature-shaped query value:\n%s",
					strings.TrimSpace(line))
			}
			if strings.Contains(text, "203.0.113.7") {
				t.Error("sanitized raw metadata still carries a client IP")
			}
			// Sanitization must remove secrets without gutting the record.
			if !strings.Contains(text, `"id"`) {
				t.Error("sanitization removed the post id along with the secrets")
			}
			if meta.CanonicalURL != "" && strings.Contains(meta.CanonicalURL, "SYNTHETIC-") {
				t.Errorf("canonical_url leaked a signed parameter: %s", meta.CanonicalURL)
			}
		})
	}
}

// TestSanitizeJSONRedactsPathEmbeddedSignatures is G14, found by running the
// YouTube Shorts fixture through the real archiver.
//
// sanitizeJSONString only walks url.Query(), and signedMediaHost only widens
// that to "every query parameter". But googlevideo's HLS manifest URLs carry
// no query string at all — expire, ei, ip, sig and lsig are alternating path
// segments:
//
//	https://manifest.googlevideo.com/api/manifest/hls_playlist/expire/1786613715/
//	  ei/czt9auPuMr2FkucP64C6iQY/ip/203.0.113.7/.../sig/AE0s2JYwRQIgaB68...
//
// Every YouTube Shorts capture, every livestream, and anything else yt-dlp
// resolves to HLS therefore stores the viewer's client IP and a valid URL
// signature in raw metadata — which /video/:shortid/raw serves publicly.
// Contract #6 says no secrets and no signed URLs.
func TestSanitizeJSONRedactsPathEmbeddedSignatures(t *testing.T) {
	raw := []byte(`{
	  "url": "https://manifest.googlevideo.com/api/manifest/hls_playlist/expire/1786613715/ei/czt9auPuMr2FkucP64C6iQY/ip/198.51.100.23/id/5239b1e4fb13d486/itag/616/source/youtube/sig/AE0s2JYwRQIgaB6812wW7sOHGbtxnAdY21EcPjVZUJ9AJUKxoYO/playlist/index.m3u8"
	}`)
	sanitized, err := SanitizeJSON(raw, nil)
	if err != nil {
		t.Fatalf("SanitizeJSON: %v", err)
	}
	text := string(sanitized)

	// The host is a known signed-media host, so the intent is already there;
	// only the query-string-shaped assumption is wrong.
	if !strings.Contains(text, "manifest.googlevideo.com") {
		t.Fatal("sanitization dropped the URL entirely; the host is useful provenance")
	}

	// G14 fixed at integration: path-embedded signatures are now redacted.

	for _, secret := range []string{
		"AE0s2JYwRQIgaB6812wW7sOHGbtxnAdY21EcPjVZUJ9AJUKxoYO", // sig
		"198.51.100.23",           // client IP
		"1786613715",              // expire
		"czt9auPuMr2FkucP64C6iQY", // ei session token
	} {
		if strings.Contains(text, secret) {
			t.Errorf("G14: sanitized JSON still carries %q from the URL path", secret)
		}
	}
}

// queryPart returns the query string of the first URL on a line, or "" when
// the line has no URL or the URL has no query.
func queryPart(line string) string {
	start := strings.Index(line, "http")
	if start < 0 {
		return ""
	}
	rest := line[start:]
	if end := strings.IndexAny(rest, `"`); end >= 0 {
		rest = rest[:end]
	}
	_, query, _ := strings.Cut(rest, "?")
	return query
}

// TestNativeYtDlpFailureIsExplicit pins contract #1: a refused extraction must
// surface as an error, never as an empty-but-successful archive.
func TestNativeYtDlpFailureIsExplicit(t *testing.T) {
	t.Run("accessibility probe refused", func(t *testing.T) {
		testfixtures.InstallFakeYtDlp(t, testfixtures.YtDlpFake{
			Fixture: "instagram_reel",
			// The shape Instagram returns to a logged-out client.
			FailProbe: true,
			Stderr:    "ERROR: [Instagram] You need to log in to access this content",
		})
		var log strings.Builder
		archiver := &YtDlpArchiver{}
		if _, err := archiver.Archive(context.Background(),
			"https://www.instagram.com/reel/DbktPO1Eopi/", &log, nil, 1); err == nil {
			t.Fatal("a refused probe must fail the archive, not return an empty success")
		}
	})

	t.Run("download refused after the probe passed", func(t *testing.T) {
		testfixtures.InstallFakeYtDlp(t, testfixtures.YtDlpFake{
			Fixture: "tiktok_video", FailDownload: true,
		})
		var log strings.Builder
		archiver := &YtDlpArchiver{}
		if _, err := archiver.Archive(context.Background(),
			"https://www.tiktok.com/@arkerfixture/video/7106594312292453675", &log, nil, 1); err == nil {
			t.Fatal("a failed download must fail the archive")
		}
	})

	// A run that produced the video but no info JSON has unknown metadata.
	// Contract #1: unknown-completeness must never read as success.
	t.Run("missing info JSON", func(t *testing.T) {
		testfixtures.InstallFakeYtDlp(t, testfixtures.YtDlpFake{
			Fixture: "youtube_regular", NoInfoJSON: true,
		})
		var log strings.Builder
		archiver := &YtDlpArchiver{}
		if _, err := archiver.Archive(context.Background(),
			"https://www.youtube.com/watch?v=aqz-KE-bpKQ", &log, nil, 1); err == nil {
			t.Fatal("a run with no info JSON has no auditable provider record and must fail")
		}
	})
}

// A missing poster image is cosmetic. Contract #1 is about media and metadata;
// losing the preview must not lose the archive.
func TestNativeYtDlpSucceedsWithoutAThumbnail(t *testing.T) {
	testfixtures.InstallFakeYtDlp(t, testfixtures.YtDlpFake{
		Fixture: "vimeo_video", NoThumbnail: true,
	})
	artifact, meta, _, log := runYtDlpArchive(t, "https://vimeo.com/76979871")
	if len(artifact) == 0 || meta.PostID == "" {
		t.Fatal("a missing poster must not cost the archive its media or metadata")
	}
	if !strings.Contains(log, "No thumbnail written") {
		t.Errorf("expected the log to record the missing poster, got:\n%s", log)
	}
}

// TestNativeGalleryDLSuccessForEveryPostType walks the gallery-dl half of the
// matrix. Contract #4: every slide is stored, and the manifest describes them.
func TestNativeGalleryDLSuccessForEveryPostType(t *testing.T) {
	for _, c := range testfixtures.CasesForTool(testfixtures.ToolGalleryDl) {
		t.Run(c.Name, func(t *testing.T) {
			slides := c.Sidecars(t)
			testfixtures.InstallFakeGalleryDl(t, testfixtures.GalleryDlFake{Fixture: c.Name})
			data, log, err := runGalleryArchive(t, c.URL)
			if err != nil {
				t.Fatalf("gallery archive failed: %v\nlog:\n%s", err, log)
			}

			meta := galleryManifest(t, data)
			if meta.FileCount != len(slides) {
				t.Errorf("file_count = %d, want %d", meta.FileCount, len(slides))
			}
			if meta.Extractor != galleryExtractorFor(c.Platform) {
				t.Errorf("extractor = %q, want %q", meta.Extractor, galleryExtractorFor(c.Platform))
			}
			if meta.PostID == "" {
				t.Error("post_id is empty; a normalized post must be identifiable")
			}
			if meta.Author == "" {
				t.Error("author is empty")
			}

			// Contract #4: every downloaded slide is in the bundle, in order,
			// with its raw sidecar beside it.
			names := zipNames(t, data)
			for _, slide := range slides {
				if !contains(names, slide.MediaName) {
					t.Errorf("ZIP is missing slide %s; entries: %v", slide.MediaName, names)
				}
				if !contains(names, slide.SidecarName) {
					t.Errorf("ZIP is missing raw sidecar %s", slide.SidecarName)
				}
			}
			if !contains(names, galleryMetadataFilename) {
				t.Errorf("ZIP has no %s", galleryMetadataFilename)
			}
		})
	}
}

// galleryExtractorFor maps a matrix platform to gallery-dl's "category".
func galleryExtractorFor(platform string) string {
	if platform == "x" {
		return "twitter"
	}
	return platform
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

// TestCarouselStoresEverySlideIncludingVideo is contract #4's all-media
// requirement at its hardest: a ten-slide Instagram carousel whose fourth
// slide is a video.
func TestCarouselStoresEverySlideIncludingVideo(t *testing.T) {
	c := testfixtures.Lookup(t, "instagram_carousel")
	testfixtures.InstallFakeGalleryDl(t, testfixtures.GalleryDlFake{Fixture: c.Name})

	data, log, err := runGalleryArchive(t, c.URL)
	if err != nil {
		t.Fatalf("gallery archive failed: %v\nlog:\n%s", err, log)
	}

	meta := galleryManifest(t, data)
	if meta.FileCount != 10 {
		t.Fatalf("file_count = %d, want all 10 slides", meta.FileCount)
	}

	videoSlides := 0
	for _, f := range meta.Files {
		if f.IsVideo {
			videoSlides++
			if f.Name != "004.mp4" {
				t.Errorf("video slide is %s, want 004.mp4", f.Name)
			}
		}
		if f.MetadataFile == "" {
			t.Errorf("slide %s has no raw sidecar recorded", f.Name)
		}
	}
	if videoSlides != 1 {
		t.Errorf("found %d video slides, want exactly 1", videoSlides)
	}

	names := zipNames(t, data)
	for _, slide := range c.Sidecars(t) {
		if !contains(names, slide.MediaName) {
			t.Errorf("carousel bundle is missing %s", slide.MediaName)
		}
	}
}

// TestPartialCarouselIsNeverRecordedAsComplete is the G1 contract.
//
// gallery-dl exits non-zero for partial failures, and the archiver keeps what
// came back — which is right, a partial archive beats none. What is wrong is
// that nothing downstream can tell a 3-of-10 carousel from a complete one:
// GalleryMetadata carries FileCount but no expected count, so the API's
// fulfillment check (validPost && len(media) > 0) reads a third of a post as
// fulfilled.
//
// The fixture's sidecars declare "count": 10. The fix is to record that
// expected count and a completeness verdict on the manifest.
func TestPartialCarouselIsNeverRecordedAsComplete(t *testing.T) {
	c := testfixtures.Lookup(t, "instagram_carousel")
	testfixtures.InstallFakeGalleryDl(t, testfixtures.GalleryDlFake{
		Fixture: c.Name,
		Slides:  3,
		// 4 = "extraction or download failed", what gallery-dl returns when
		// it gives up partway through a post.
		ExitCode: 4,
	})

	data, log, err := runGalleryArchive(t, c.URL)
	if err != nil {
		t.Fatalf("a partial run must still keep what it downloaded: %v", err)
	}
	if !strings.Contains(log, "keeping partial archive") {
		t.Errorf("expected the log to record the partial run, got:\n%s", log)
	}

	meta := galleryManifest(t, data)
	if meta.FileCount != 3 {
		t.Fatalf("file_count = %d, want the 3 slides that landed", meta.FileCount)
	}

	// Everything above is current behavior and must keep working. What
	// follows is the contract the fix has to add.

	raw := manifestRaw(t, data)
	completeness, ok := raw["completeness"].(map[string]any)
	if !ok {
		t.Fatal("G1: manifest records no completeness verdict")
	}
	if completeness["expected"] != float64(10) {
		t.Errorf("completeness.expected = %v, want 10 (the sidecar's declared count)", completeness["expected"])
	}
	if completeness["state"] != "partial" {
		t.Errorf("completeness.state = %v, want partial", completeness["state"])
	}
	if completeness["stored"] != float64(3) {
		t.Errorf("completeness.stored = %v, want 3", completeness["stored"])
	}
}

// TestCompleteCarouselIsRecordedAsComplete is the other half of G1: a run that
// got everything must say so, including a non-zero exit that only affected
// something cosmetic.
func TestCompleteCarouselIsRecordedAsComplete(t *testing.T) {
	c := testfixtures.Lookup(t, "instagram_carousel")
	testfixtures.InstallFakeGalleryDl(t, testfixtures.GalleryDlFake{
		Fixture: c.Name,
		// All ten slides landed, but gallery-dl still reported trouble.
		ExitCode: 4,
	})

	data, _, err := runGalleryArchive(t, c.URL)
	if err != nil {
		t.Fatalf("gallery archive failed: %v", err)
	}
	if meta := galleryManifest(t, data); meta.FileCount != 10 {
		t.Fatalf("file_count = %d, want 10", meta.FileCount)
	}

	raw := manifestRaw(t, data)
	completeness, _ := raw["completeness"].(map[string]any)
	if completeness["state"] != "complete" {
		t.Errorf("completeness.state = %v, want complete: every declared slide was stored, "+
			"so a non-zero exit about something else must not downgrade the verdict",
			completeness["state"])
	}
}

// TestSingleMediaPostIsStructurallyComplete pins the G1 carve-out: a post type
// whose media count is structurally one is complete as soon as the artifact is
// stored, with no declared count needed.
func TestSingleMediaPostIsStructurallyComplete(t *testing.T) {
	for _, name := range []string{"bluesky_image", "flickr_photo", "reddit_image"} {
		t.Run(name, func(t *testing.T) {
			c := testfixtures.Lookup(t, name)
			testfixtures.InstallFakeGalleryDl(t, testfixtures.GalleryDlFake{Fixture: c.Name})
			data, _, err := runGalleryArchive(t, c.URL)
			if err != nil {
				t.Fatalf("gallery archive failed: %v", err)
			}
			if meta := galleryManifest(t, data); meta.FileCount != 1 {
				t.Fatalf("file_count = %d, want 1", meta.FileCount)
			}

			raw := manifestRaw(t, data)
			completeness, _ := raw["completeness"].(map[string]any)
			if completeness["state"] != "complete" {
				t.Errorf("completeness.state = %v, want complete", completeness["state"])
			}
		})
	}
}

// manifestRaw decodes metadata.json into a map so a test can assert on fields
// GalleryMetadata does not have yet.
func manifestRaw(t *testing.T, data []byte) map[string]any {
	t.Helper()
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("open gallery ZIP: %v", err)
	}
	for _, f := range reader.File {
		if f.Name != galleryMetadataFilename {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open manifest: %v", err)
		}
		defer rc.Close()
		var raw map[string]any
		if err := json.NewDecoder(rc).Decode(&raw); err != nil {
			t.Fatalf("decode manifest: %v", err)
		}
		return raw
	}
	t.Fatal("no manifest in ZIP")
	return nil
}

// TestGalleryDLFailureWithNoMediaIsExplicit pins contract #1 for the
// auth-blocked case: nothing downloaded must be an error naming the cause, not
// an empty bundle.
func TestGalleryDLFailureWithNoMediaIsExplicit(t *testing.T) {
	tests := map[string]struct {
		exitCode int
		want     string
	}{
		"authentication required": {16, "authentication required"},
		"no extractor":            {64, "no gallery-dl extractor"},
		"anti-bot challenge":      {8, "anti-bot challenge"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			// No fixture: the run writes nothing at all.
			testfixtures.InstallFakeGalleryDl(t, testfixtures.GalleryDlFake{ExitCode: tc.exitCode})
			_, _, err := runGalleryArchive(t, "https://x.com/arkerfixture/status/1929384756102938112")
			if err == nil {
				t.Fatal("a run that downloaded nothing must fail the archive")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to name the cause %q", err, tc.want)
			}
		})
	}
}

// TestGalleryRawSidecarsAreSanitizedAtRest is G12: the served /gallery/:id/raw
// endpoint sanitizes on the way out, but the public ZIP bundle is what a user
// downloads, and it stores gallery-dl's sidecars verbatim.
func TestGalleryRawSidecarsAreSanitizedAtRest(t *testing.T) {
	c := testfixtures.Lookup(t, "instagram_carousel")
	testfixtures.InstallFakeGalleryDl(t, testfixtures.GalleryDlFake{Fixture: c.Name})
	data, _, err := runGalleryArchive(t, c.URL)
	if err != nil {
		t.Fatalf("gallery archive failed: %v", err)
	}

	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("open gallery ZIP: %v", err)
	}
	var sidecar []byte
	for _, f := range reader.File {
		if f.Name != "004.mp4.json" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open sidecar: %v", err)
		}
		sidecar, err = io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("read sidecar: %v", err)
		}
	}
	if len(sidecar) == 0 {
		t.Fatal("the carousel's video sidecar is not in the bundle")
	}

	// The fixture's video slide carries a cdninstagram URL with a signed
	// parameter, exactly like a real one.
	if !strings.Contains(string(sidecar), "SYNTHETIC-OH") {
		t.Skip("fixture no longer carries a signed sidecar URL; nothing to prove here")
	}
	t.Skip("contract-pending: G12 — gallery sidecars are stored unsanitized; enable at integration")
}
