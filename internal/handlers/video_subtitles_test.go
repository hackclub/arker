package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"arker/internal/archivers"
	"arker/internal/models"
	"arker/internal/storage"
)

func subtitleRouter(db *gorm.DB, store storage.Storage) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/api/v1/archive/:shortid", func(c *gin.Context) { ApiArchiveResult(c, store, db) })
	r.GET("/video/:shortid/transcript", func(c *gin.Context) { ServeVideoTranscript(c, store, db) })
	r.GET("/video/:shortid/subtitle/:name", func(c *gin.Context) { ServeVideoSubtitle(c, store, db) })
	return r
}

// seedCaptionedVideo stores a completed video capture whose metadata records a
// caption track and a derived transcript, with the track itself in storage.
func seedCaptionedVideo(t *testing.T, db *gorm.DB, store storage.Storage, shortID string, withSubtitles bool) {
	t.Helper()
	createVideoCapture(t, db, shortID, "https://www.youtube.com/watch?v="+shortID, map[string]string{"yt-dlp": "completed"})
	metadata := &archivers.VideoMetadata{
		SchemaVersion: archivers.VideoMetadataSchemaVersion,
		Platform:      "youtube",
		PostID:        shortID,
		Title:         "Me at the zoo",
		Media:         archivers.VideoMedia{Extension: ".mp4", ContentType: "video/mp4", SizeBytes: 8},
		Provider:      "yt-dlp",
	}
	base := shortID + "/yt-dlp-9f2c"
	if withSubtitles {
		key := base + ".sub.en.vtt"
		storeTestObject(t, store, key, []byte("WEBVTT\n\n00:00:01.000 --> 00:00:02.000\nall right so here we are\n"))
		metadata.Subtitles = []archivers.SubtitleTrack{{
			Lang: "en", Kind: archivers.SubtitleKindAuto, Format: "vtt",
			ArtifactSuffix: ".sub.en.vtt", StorageKey: key, SizeBytes: 62,
		}}
		metadata.Transcript = &archivers.Transcript{
			Lang: "en", Source: archivers.SubtitleKindAuto, Text: "all right so here we are",
		}
	}
	encoded, err := archivers.MarshalVideoMetadata(metadata)
	if err != nil {
		t.Fatalf("marshal metadata: %v", err)
	}
	storeTestObject(t, store, base+".metadata.json", encoded)
	storeTestObject(t, store, base+".raw-metadata.json", []byte(`{"id":"x"}`))
	db.Model(&models.ArchiveItem{}).Where("capture_id = (SELECT id FROM captures WHERE short_id = ?)", shortID).
		Updates(map[string]any{
			"storage_key": base + ".mp4", "extension": ".mp4", "file_size": 8,
			"metadata_key": base + ".metadata.json", "raw_metadata_key": base + ".raw-metadata.json",
			"completeness": "complete",
		})
}

func TestApiExposesSubtitlesAndTranscript(t *testing.T) {
	db := newHandlerLogTestDB(t)
	store := storage.NewMemoryStorage()
	seedCaptionedVideo(t, db, store, "cap01", true)

	_, body := getResult(t, subtitleRouter(db, store), "cap01")
	social := socialOf(t, body)
	if social["fulfilled"] != true {
		t.Fatalf("captioned video = %#v", social)
	}

	subtitles := social["subtitles"].([]any)
	if len(subtitles) != 1 {
		t.Fatalf("subtitles = %#v", subtitles)
	}
	track := subtitles[0].(map[string]any)
	if track["lang"] != "en" || track["kind"] != "auto" || track["format"] != "vtt" {
		t.Fatalf("track = %#v", track)
	}
	if !strings.HasSuffix(track["url"].(string), "/video/cap01/subtitle/en.vtt") {
		t.Fatalf("subtitle url = %v", track["url"])
	}

	transcript := social["transcript"].(map[string]any)
	if transcript["lang"] != "en" || transcript["source"] != "auto" {
		t.Fatalf("transcript = %#v", transcript)
	}
	if !strings.Contains(transcript["text"].(string), "all right so here we are") {
		t.Fatalf("transcript text = %v", transcript["text"])
	}
	if transcript["characters"] != float64(len("all right so here we are")) {
		t.Fatalf("characters = %v", transcript["characters"])
	}
}

// The contract explicitly does not require captions: a platform that exposes
// none must still produce a fulfilled archive, with the fields simply absent.
func TestVideoWithoutSubtitlesStaysFulfilled(t *testing.T) {
	db := newHandlerLogTestDB(t)
	store := storage.NewMemoryStorage()
	seedCaptionedVideo(t, db, store, "nsb01", false)

	_, body := getResult(t, subtitleRouter(db, store), "nsb01")
	social := socialOf(t, body)
	if social["fulfilled"] != true || social["status"] != "fulfilled" {
		t.Fatalf("a video without captions is not fulfilled: %#v", social)
	}
	if _, present := social["subtitles"]; present {
		t.Fatalf("absent captions produced a subtitles field: %#v", social["subtitles"])
	}
	if _, present := social["transcript"]; present {
		t.Fatalf("absent captions produced a transcript field: %#v", social["transcript"])
	}
	if codes := warningCodes(social); len(codes) != 0 {
		t.Fatalf("warnings = %v, want none: missing captions are not a defect", codes)
	}
}

func TestServeVideoSubtitleAndTranscript(t *testing.T) {
	db := newHandlerLogTestDB(t)
	store := storage.NewMemoryStorage()
	seedCaptionedVideo(t, db, store, "srv01", true)
	r := subtitleRouter(db, store)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/video/srv01/subtitle/en.vtt", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("subtitle status = %d, body %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/vtt") {
		t.Errorf("content type = %q, want text/vtt", ct)
	}
	if !strings.HasPrefix(rec.Body.String(), "WEBVTT") {
		t.Errorf("body = %q", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/video/srv01/transcript", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "all right so here we are") {
		t.Fatalf("transcript status = %d, body %q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-Transcript-Source") != "auto" {
		t.Errorf("missing transcript provenance headers: %v", rec.Header())
	}

	// A track this archive does not have is a 404, not a guess at a key.
	for _, path := range []string{"/video/srv01/subtitle/de.vtt", "/video/srv01/subtitle/en.srt"} {
		rec = httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, rec.Code)
		}
	}
}

// Only tracks the archive itself recorded can be served, so a crafted name
// cannot reach another object.
func TestServeVideoSubtitleRejectsUnlistedNames(t *testing.T) {
	db := newHandlerLogTestDB(t)
	store := storage.NewMemoryStorage()
	seedCaptionedVideo(t, db, store, "sec01", true)
	storeTestObject(t, store, "sec01/secret.json", []byte(`{"secret":true}`))
	r := subtitleRouter(db, store)

	for _, name := range []string{"..%2Fsecret.json", "secret.json", "en.vtt%00"} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/video/sec01/subtitle/"+name, nil))
		if rec.Code == http.StatusOK {
			t.Errorf("GET subtitle/%s returned 200: %s", name, rec.Body.String())
		}
	}
}

func TestServeVideoTranscriptWhenAbsent(t *testing.T) {
	db := newHandlerLogTestDB(t)
	store := storage.NewMemoryStorage()
	seedCaptionedVideo(t, db, store, "not01", false)

	rec := httptest.NewRecorder()
	subtitleRouter(db, store).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/video/not01/transcript", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["error"] == nil {
		t.Fatalf("body = %s, want an explicit error", rec.Body.String())
	}
}

// A track stored before its key was recorded still has to be servable, derived
// from the item's own key base.
func TestSubtitleStorageKeyFallsBackToTheItemKeyBase(t *testing.T) {
	item := &models.ArchiveItem{StorageKey: "abc12/yt-dlp-9f2c.mp4", Extension: ".mp4"}
	track := archivers.SubtitleTrack{Lang: "en", Format: "vtt", ArtifactSuffix: ".sub.en.vtt"}
	if got := subtitleStorageKey(item, track); got != "abc12/yt-dlp-9f2c.sub.en.vtt" {
		t.Fatalf("derived key = %q", got)
	}
	track.StorageKey = "explicit/key.vtt"
	if got := subtitleStorageKey(item, track); got != "explicit/key.vtt" {
		t.Fatalf("recorded key = %q, want it to win", got)
	}
}
