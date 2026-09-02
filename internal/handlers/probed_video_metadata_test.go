package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"gorm.io/gorm"

	"arker/internal/apify"
	"arker/internal/archivers"
	"arker/internal/models"
	"arker/internal/storage"
	"arker/internal/testfixtures"
	"arker/internal/utils"
	"arker/internal/workers"
)

const fakeFFprobeOutput = `{
  "streams": [
    {"codec_name":"h264","codec_type":"video","width":400,"height":252,"r_frame_rate":"30/1","bit_rate":"300000"},
    {"codec_name":"aac","codec_type":"audio","r_frame_rate":"0/0","bit_rate":"64000"}
  ],
  "format": {"format_name":"mov,mp4,m4a,3gp,3g2,mj2","duration":"7.000000","bit_rate":"400000"}
}`

const probeFacebookVideoURL = "https://www.facebook.com/watch/?v=m5CY3"

const probeTikTokVideoURL = "https://www.tiktok.com/@tiktok/video/7673169793343622430"

const conflictingTikTokRawMetadata = `{"video_duration":5,"width":576,"height":1920,"description":"Provider caption","profile_username":"TikTok","play_count":60300,"post_id":"7673169793343622430","create_time":"2026-08-12T15:37:54Z","url":"https://www.tiktok.com/@tiktok/video/7673169793343622430"}`

const conflictingTikTokProbeOutput = `{
  "streams": [
    {"codec_name":"hevc","codec_type":"video","width":1080,"height":1920,"r_frame_rate":"30000/1001","bit_rate":"1500000"},
    {"codec_name":"aac","codec_type":"audio","r_frame_rate":"0/0","bit_rate":"96000"}
  ],
  "format": {"format_name":"mov,mp4,m4a,3gp,3g2,mj2","duration":"5.478005","bit_rate":"1600000"}
}`

// installArtifactCheckingFFprobe makes the probe deterministic and also proves
// it was given the bytes that reached storage, rather than another provider
// field. The fake is a local process only; these regressions never call a live
// extractor, CDN or paid service.
func installArtifactCheckingFFprobe(t *testing.T, artifact []byte, output string) string {
	t.Helper()
	stage := t.TempDir()
	expected := filepath.Join(stage, "expected.mp4")
	actual := filepath.Join(stage, "actual.mp4")
	response := filepath.Join(stage, "probe.json")
	if err := os.WriteFile(expected, artifact, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(response, []byte(output), 0o600); err != nil {
		t.Fatal(err)
	}

	bin := t.TempDir()
	script := fmt.Sprintf(`#!/bin/sh
cat > %q
cmp -s %q %q || exit 91
cat %q
`, actual, expected, actual, response)
	if err := os.WriteFile(filepath.Join(bin, "ffprobe"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return actual
}

func storedVideoMetadata(t *testing.T, db *gorm.DB, store storage.Storage, itemID uint) archivers.VideoMetadata {
	t.Helper()
	var item models.ArchiveItem
	if err := db.First(&item, itemID).Error; err != nil {
		t.Fatal(err)
	}
	if item.Status != "completed" || item.MetadataKey == "" {
		t.Fatalf("video item was not completed with metadata: %+v", item)
	}
	reader, err := store.Reader(item.MetadataKey)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	var metadata archivers.VideoMetadata
	if err := json.NewDecoder(reader).Decode(&metadata); err != nil {
		t.Fatal(err)
	}
	return metadata
}

func runArchiveWorkerForProbeTest(t *testing.T, db *gorm.DB, store storage.Storage, shortID, sourceURL string, arch archivers.Archiver) models.ArchiveItem {
	t.Helper()
	capture := createVideoCapture(t, db, shortID, sourceURL, map[string]string{utils.ArchiveTypeYtDlp: "processing"})
	var item models.ArchiveItem
	if err := db.Where("capture_id = ? AND type = ?", capture.ID, utils.ArchiveTypeYtDlp).First(&item).Error; err != nil {
		t.Fatal(err)
	}
	worker := workers.NewArchiveWorker(store, db, map[string]archivers.Archiver{utils.ArchiveTypeYtDlp: arch})
	job := &river.Job[workers.ArchiveJobArgs]{
		JobRow: &rivertype.JobRow{ID: 1, Attempt: 1, MaxAttempts: 3},
		Args: workers.ArchiveJobArgs{
			CaptureID: capture.ID,
			ShortID:   shortID,
			Type:      utils.ArchiveTypeYtDlp,
			URL:       sourceURL,
		},
	}
	if err := worker.Work(context.Background(), job); err != nil {
		t.Fatalf("archive worker: %v", err)
	}
	return item
}

func assertUnifiedVideoFacts(t *testing.T, db *gorm.DB, store storage.Storage, shortID string, duration, width, height float64) {
	t.Helper()
	_, body := getResult(t, resultRouter(db, store), shortID)
	social := socialOf(t, body)
	post := social["post"].(map[string]any)
	media := social["media"].([]any)[0].(map[string]any)
	if post["duration_seconds"] != duration {
		t.Errorf("unified post duration = %v, want %v", post["duration_seconds"], duration)
	}
	if media["duration_seconds"] != duration || media["width"] != width || media["height"] != height {
		t.Errorf("unified media facts = %#v, want duration=%v width=%v height=%v", media, duration, width, height)
	}
}

// Native yt-dlp metadata can describe the provider's selected stream rather
// than the post-processed MP4. The shared worker path must use the exact stored
// artifact for every intrinsic media fact it can probe.
func TestNativeInstagramUsesStoredArtifactMediaFacts(t *testing.T) {
	video, err := os.ReadFile("../archivers/testdata/muxed_video_audio_sample.mp4")
	if err != nil {
		t.Fatal(err)
	}
	probeOutput := bytes.ReplaceAll([]byte(fakeFFprobeOutput), []byte(`"duration":"7.000000"`), []byte(`"duration":"2.554195"`))
	probedPath := installArtifactCheckingFFprobe(t, video, string(probeOutput))

	fixture := testfixtures.Lookup(t, "instagram_reel")
	var provider map[string]any
	if err := json.Unmarshal(fixture.InfoJSON(t), &provider); err != nil {
		t.Fatal(err)
	}
	delete(provider, "duration")
	rawInfo, err := json.Marshal(provider)
	if err != nil {
		t.Fatal(err)
	}
	testfixtures.InstallFakeYtDlp(t, testfixtures.YtDlpFake{Fixture: fixture.Name, InfoJSON: rawInfo, VideoBytes: video})

	db := newHandlerLogTestDB(t)
	store := storage.NewMemoryStorage()
	item := runArchiveWorkerForProbeTest(t, db, store, "igp01", fixture.URL, &archivers.YtDlpArchiver{})
	metadata := storedVideoMetadata(t, db, store, item.ID)
	if metadata.DurationSeconds == nil || *metadata.DurationSeconds != 2.554195 {
		t.Fatalf("normalized duration = %v, want exact probed duration 2.554195", metadata.DurationSeconds)
	}
	if metadata.Media.Width == nil || *metadata.Media.Width != 400 || metadata.Media.Height == nil || *metadata.Media.Height != 252 {
		t.Errorf("normalized dimensions = %+v, want stored-artifact 400x252", metadata.Media)
	}
	if metadata.Media.VideoCodec != "h264" || metadata.Media.AudioCodec != "aac" {
		t.Errorf("normalized codecs = %+v, want stored-artifact h264/aac", metadata.Media)
	}
	if metadata.Media.FPS == nil || *metadata.Media.FPS != 30 || metadata.Media.BitrateKbps == nil || *metadata.Media.BitrateKbps != 400 {
		t.Errorf("normalized frame rate/bitrate = %+v, want stored-artifact 30 fps / 400 kbps", metadata.Media)
	}
	if _, err := os.Stat(probedPath); err != nil {
		t.Fatalf("stored artifact was never probed: %v", err)
	}
	assertUnifiedVideoFacts(t, db, store, "igp01", 2.554195, 400, 252)
}

type refusedVideoArchiver struct{}

func (refusedVideoArchiver) Archive(context.Context, string, io.Writer, *gorm.DB, uint) (archivers.Result, error) {
	return archivers.Result{}, errors.New("native extraction refused")
}

type facebookProbeFallback struct{ video []byte }

func (f facebookProbeFallback) Enabled() bool { return true }

func (f facebookProbeFallback) SupportsFallback(string, string) bool { return true }

func (f facebookProbeFallback) ArchiveFallback(context.Context, string, string, io.Writer, *gorm.DB, uint) (archivers.Result, error) {
	metadata, err := archivers.MarshalVideoMetadata(&archivers.VideoMetadata{
		SchemaVersion: archivers.VideoMetadataSchemaVersion,
		SourceURL:     probeFacebookVideoURL,
		Platform:      "facebook",
		Extractor:     "facebook",
		PostID:        "m5CY3",
		CanonicalURL:  probeFacebookVideoURL,
		Title:         "Fallback fixture",
		Engagement:    archivers.VideoEngagement{},
		Media: archivers.VideoMedia{
			Extension:   ".mp4",
			ContentType: "video/mp4",
			SizeBytes:   int64(len(f.video)),
		},
		ArchivedAt: time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC).Format(time.RFC3339),
		Provenance: models.ArchiveSourceApify,
		Provider:   "apify:apify/facebook-posts-scraper",
	})
	if err != nil {
		return archivers.Result{}, err
	}
	return archivers.Result{
		Data:         bytes.NewReader(f.video),
		Extension:    ".mp4",
		ContentType:  "video/mp4",
		Source:       models.ArchiveSourceApify,
		Metadata:     &archivers.Sidecar{Data: metadata},
		RawMetadata:  &archivers.Sidecar{Data: []byte(`{"provider":"apify"}`)},
		Completeness: archivers.CompletenessComplete,
	}, nil
}

// Facebook's fallback record can omit every intrinsic media fact even
// though the fallback stored a valid MP4. Those facts must be recovered after
// storage without changing the fallback/completeness policy.
func TestFallbackFacebookBackfillsMissingMediaFactsFromStoredArtifact(t *testing.T) {
	video, err := os.ReadFile("../archivers/testdata/muxed_video_audio_sample.mp4")
	if err != nil {
		t.Fatal(err)
	}
	probedPath := installArtifactCheckingFFprobe(t, video, fakeFFprobeOutput)

	db := newHandlerLogTestDB(t)
	store := storage.NewMemoryStorage()
	backend := facebookProbeFallback{video: video}
	arch := apify.WithFallback(refusedVideoArchiver{}, utils.ArchiveTypeYtDlp, backend)
	item := runArchiveWorkerForProbeTest(t, db, store, "fbp01", probeFacebookVideoURL, arch)
	metadata := storedVideoMetadata(t, db, store, item.ID)
	if metadata.DurationSeconds == nil || *metadata.DurationSeconds != 7 {
		t.Fatalf("normalized duration = %v, want 7", metadata.DurationSeconds)
	}
	if metadata.Media.Width == nil || *metadata.Media.Width != 400 || metadata.Media.Height == nil || *metadata.Media.Height != 252 {
		t.Errorf("normalized dimensions = %+v, want 400x252", metadata.Media)
	}
	if metadata.Media.VideoCodec != "h264" || metadata.Media.AudioCodec != "aac" {
		t.Errorf("normalized codecs = %+v, want h264/aac", metadata.Media)
	}
	if metadata.Media.FPS == nil || *metadata.Media.FPS != 30 || metadata.Media.BitrateKbps == nil || *metadata.Media.BitrateKbps != 400 {
		t.Errorf("normalized frame rate/bitrate = %+v, want 30 fps / 400 kbps", metadata.Media)
	}
	if _, err := os.Stat(probedPath); err != nil {
		t.Fatalf("stored artifact was never probed: %v", err)
	}
	assertUnifiedVideoFacts(t, db, store, "fbp01", 7, 400, 252)
}

type tiktokProbeFallback struct{ video []byte }

func (f tiktokProbeFallback) Enabled() bool { return true }

func (f tiktokProbeFallback) SupportsFallback(string, string) bool { return true }

func (f tiktokProbeFallback) ArchiveFallback(context.Context, string, string, io.Writer, *gorm.DB, uint) (archivers.Result, error) {
	duration := 5.0
	width, height := int64(576), int64(1920)
	fps, bitrate := 24.0, 500.0
	views := int64(60300)
	metadata, err := archivers.MarshalVideoMetadata(&archivers.VideoMetadata{
		SchemaVersion:        archivers.VideoMetadataSchemaVersion,
		SourceURL:            probeTikTokVideoURL,
		Platform:             "tiktok",
		Extractor:            "tiktok",
		PostID:               "7673169793343622430",
		CanonicalURL:         probeTikTokVideoURL,
		Title:                "Provider title",
		Description:          "Provider caption",
		Author:               "TikTok",
		PublicationTimestamp: "2026-08-12T15:37:54Z",
		DurationSeconds:      &duration,
		Engagement:           archivers.VideoEngagement{Views: &views},
		Media: archivers.VideoMedia{
			Extension:   ".mp4",
			ContentType: "video/mp4",
			SizeBytes:   int64(len(f.video)),
			Width:       &width,
			Height:      &height,
			FPS:         &fps,
			VideoCodec:  "h264",
			AudioCodec:  "mp3",
			BitrateKbps: &bitrate,
		},
		ArchivedAt: time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC).Format(time.RFC3339),
		Provenance: models.ArchiveSourceApify,
		Provider:   "apify:apify/facebook-posts-scraper",
	})
	if err != nil {
		return archivers.Result{}, err
	}
	return archivers.Result{
		Data:         bytes.NewReader(f.video),
		Extension:    ".mp4",
		ContentType:  "video/mp4",
		Source:       models.ArchiveSourceApify,
		Metadata:     &archivers.Sidecar{Data: metadata},
		RawMetadata:  &archivers.Sidecar{Data: []byte(conflictingTikTokRawMetadata)},
		Completeness: archivers.CompletenessComplete,
	}, nil
}

// The provider described FJBKa as five seconds and 576x1920, while ffprobe of
// the exact archived MP4 reported 5.478005 seconds and 1080x1920. Intrinsic
// normalized facts must follow the stored artifact without rewriting any
// descriptive claim or the separately served raw provider record.
func TestFallbackTikTokStoredArtifactOverridesConflictingProviderVideoFacts(t *testing.T) {
	video, err := os.ReadFile("../archivers/testdata/muxed_video_audio_sample.mp4")
	if err != nil {
		t.Fatal(err)
	}
	probedPath := installArtifactCheckingFFprobe(t, video, conflictingTikTokProbeOutput)

	db := newHandlerLogTestDB(t)
	store := storage.NewMemoryStorage()
	backend := tiktokProbeFallback{video: video}
	arch := apify.WithFallback(refusedVideoArchiver{}, utils.ArchiveTypeYtDlp, backend)
	item := runArchiveWorkerForProbeTest(t, db, store, "FJBKa", probeTikTokVideoURL, arch)
	metadata := storedVideoMetadata(t, db, store, item.ID)

	if metadata.DurationSeconds == nil || *metadata.DurationSeconds != 5.478005 {
		t.Fatalf("normalized duration = %v, want stored-artifact duration 5.478005", metadata.DurationSeconds)
	}
	if metadata.Media.Width == nil || *metadata.Media.Width != 1080 || metadata.Media.Height == nil || *metadata.Media.Height != 1920 {
		t.Errorf("normalized dimensions = %+v, want stored-artifact 1080x1920", metadata.Media)
	}
	if metadata.Media.VideoCodec != "hevc" || metadata.Media.AudioCodec != "aac" {
		t.Errorf("normalized codecs = %+v, want stored-artifact hevc/aac", metadata.Media)
	}
	wantFPS := 30000.0 / 1001.0
	if metadata.Media.FPS == nil || *metadata.Media.FPS != wantFPS || metadata.Media.BitrateKbps == nil || *metadata.Media.BitrateKbps != 1600 {
		t.Errorf("normalized frame rate/bitrate = %+v, want %v fps / 1600 kbps", metadata.Media, wantFPS)
	}
	if metadata.Title != "Provider title" || metadata.Description != "Provider caption" || metadata.Author != "TikTok" ||
		metadata.PostID != "7673169793343622430" || metadata.CanonicalURL != probeTikTokVideoURL || metadata.PublicationTimestamp != "2026-08-12T15:37:54Z" ||
		metadata.Engagement.Views == nil || *metadata.Engagement.Views != 60300 || metadata.Provider != "apify:apify/facebook-posts-scraper" {
		t.Errorf("descriptive/provider facts changed: %+v", metadata)
	}
	if _, err := os.Stat(probedPath); err != nil {
		t.Fatalf("stored artifact was never probed: %v", err)
	}

	var storedItem models.ArchiveItem
	if err := db.First(&storedItem, item.ID).Error; err != nil {
		t.Fatal(err)
	}
	if storedItem.Completeness != archivers.CompletenessComplete {
		t.Errorf("completeness = %q, want unchanged complete", storedItem.Completeness)
	}

	router := resultRouter(db, store)
	router.GET("/video/:shortid/manifest", func(c *gin.Context) { ServeVideoManifest(c, store, db) })
	router.GET("/video/:shortid/raw", func(c *gin.Context) { ServeVideoRawMetadata(c, store, db) })
	assertUnifiedVideoFacts(t, db, store, "FJBKa", 5.478005, 1080, 1920)

	manifestRec := httptest.NewRecorder()
	router.ServeHTTP(manifestRec, httptest.NewRequest(http.MethodGet, "/video/FJBKa/manifest", nil))
	if manifestRec.Code != http.StatusOK {
		t.Fatalf("manifest status = %d, body = %s", manifestRec.Code, manifestRec.Body.String())
	}
	var manifest struct {
		Metadata json.RawMessage `json:"metadata"`
	}
	if err := json.Unmarshal(manifestRec.Body.Bytes(), &manifest); err != nil {
		t.Fatal(err)
	}
	var manifestMetadata archivers.VideoMetadata
	if err := json.Unmarshal(manifest.Metadata, &manifestMetadata); err != nil {
		t.Fatal(err)
	}
	if manifestMetadata.DurationSeconds == nil || *manifestMetadata.DurationSeconds != 5.478005 ||
		manifestMetadata.Media.Width == nil || *manifestMetadata.Media.Width != 1080 {
		t.Errorf("normalized manifest preserved provider video facts: %+v", manifestMetadata)
	}

	rawRec := httptest.NewRecorder()
	router.ServeHTTP(rawRec, httptest.NewRequest(http.MethodGet, "/video/FJBKa/raw", nil))
	if rawRec.Code != http.StatusOK || rawRec.Body.String() != conflictingTikTokRawMetadata {
		t.Errorf("raw provider metadata status/body = %d/%q, want unchanged %q", rawRec.Code, rawRec.Body.String(), conflictingTikTokRawMetadata)
	}
}

// ffprobe is an enrichment boundary, not an archive-success boundary. If it
// cannot inspect a valid stored video, the provider's normalized values remain
// the best available facts and the completed capture stays complete.
func TestVideoProbeFailureIsNonFatalAndRetainsProviderFacts(t *testing.T) {
	video, err := os.ReadFile("../archivers/testdata/muxed_video_audio_sample.mp4")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir())

	db := newHandlerLogTestDB(t)
	store := storage.NewMemoryStorage()
	backend := tiktokProbeFallback{video: video}
	arch := apify.WithFallback(refusedVideoArchiver{}, utils.ArchiveTypeYtDlp, backend)
	item := runArchiveWorkerForProbeTest(t, db, store, "ttf01", probeTikTokVideoURL, arch)
	metadata := storedVideoMetadata(t, db, store, item.ID)

	if metadata.DurationSeconds == nil || *metadata.DurationSeconds != 5 ||
		metadata.Media.Width == nil || *metadata.Media.Width != 576 ||
		metadata.Media.Height == nil || *metadata.Media.Height != 1920 ||
		metadata.Media.FPS == nil || *metadata.Media.FPS != 24 ||
		metadata.Media.VideoCodec != "h264" || metadata.Media.AudioCodec != "mp3" ||
		metadata.Media.BitrateKbps == nil || *metadata.Media.BitrateKbps != 500 {
		t.Errorf("provider video facts changed after probe failure: %+v", metadata)
	}
	if metadata.Title != "Provider title" || metadata.Description != "Provider caption" || metadata.PostID != "7673169793343622430" {
		t.Errorf("provider descriptive facts changed after probe failure: %+v", metadata)
	}
	var storedItem models.ArchiveItem
	if err := db.First(&storedItem, item.ID).Error; err != nil {
		t.Fatal(err)
	}
	if storedItem.Status != "completed" || storedItem.Completeness != archivers.CompletenessComplete {
		t.Errorf("probe failure changed archive outcome: %+v", storedItem)
	}
}
