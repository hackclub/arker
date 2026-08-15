package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/gin-gonic/gin"

	"arker/internal/archivers"
	"arker/internal/models"
	"arker/internal/storage"
	"arker/internal/testfixtures"
)

func manifestMetadata(t *testing.T, router http.Handler, shortID string) map[string]interface{} {
	t.Helper()
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/video/"+shortID+"/manifest", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("manifest status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var manifest struct {
		MetadataAvailable bool            `json:"metadata_available"`
		Metadata          json.RawMessage `json:"metadata"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &manifest); err != nil {
		t.Fatal(err)
	}
	if !manifest.MetadataAvailable {
		t.Fatalf("manifest reported no metadata: %s", rec.Body.String())
	}
	var normalized map[string]interface{}
	if err := json.Unmarshal(manifest.Metadata, &normalized); err != nil {
		t.Fatal(err)
	}
	return normalized
}

// TestVideoManifestReportsDeliveryFormat drives the real archiver, worker and
// handler end to end for the three cases the consuming pipeline actually sees:
// a YouTube Short, a long-form YouTube video, and an Instagram Reel whose
// provider names no delivery format at all.
//
// Shorts and long-form videos retain viewers completely differently, so a
// consumer that cannot tell them apart from the manifest publishes materially
// wrong watch-hour figures. Inferring it from duration and aspect ratio is a
// guess; a second request to /raw per archive is the cost this removes.
func TestVideoManifestReportsDeliveryFormat(t *testing.T) {
	video, err := os.ReadFile("../archivers/testdata/muxed_video_audio_sample.mp4")
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		fixture string
		shortID string
		want    string
		present bool
	}{
		{fixture: "youtube_shorts", shortID: "mtyts", want: "short", present: true},
		{fixture: "youtube_regular", shortID: "mtytv", want: "video", present: true},
		{fixture: "instagram_reel", shortID: "mtigr", present: false},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			installArtifactCheckingFFprobe(t, video, fakeFFprobeOutput)
			fixture := testfixtures.Lookup(t, tc.fixture)
			testfixtures.InstallFakeYtDlp(t, testfixtures.YtDlpFake{Fixture: fixture.Name, VideoBytes: video})

			db := newHandlerLogTestDB(t)
			store := storage.NewMemoryStorage()
			runArchiveWorkerForProbeTest(t, db, store, tc.shortID, fixture.URL, &archivers.YtDlpArchiver{})

			router := gin.New()
			router.GET("/video/:shortid/manifest", func(c *gin.Context) { ServeVideoManifest(c, store, db) })
			normalized := manifestMetadata(t, router, tc.shortID)

			value, present := normalized["media_type"]
			if present != tc.present {
				t.Fatalf("media_type present = %v, want %v (metadata: %v)", present, tc.present, normalized)
			}
			if tc.present && value != tc.want {
				t.Errorf("media_type = %v, want %q", value, tc.want)
			}

			// The unified API serves the same normalized record, and a fact
			// available in one place but not the other is the gap this closes.
			_, body := getResult(t, resultRouter(db, store), tc.shortID)
			post := socialOf(t, body)["post"].(map[string]any)
			unified, unifiedPresent := post["media_type"]
			if unifiedPresent != tc.present {
				t.Fatalf("unified post media_type present = %v, want %v (post: %v)", unifiedPresent, tc.present, post)
			}
			if tc.present && unified != tc.want {
				t.Errorf("unified post media_type = %v, want %q", unified, tc.want)
			}
		})
	}
}

// TestVideoManifestLegacyArchiveHasNoDeliveryFormat is the compatibility half.
// Sidecars written before the field existed cannot be rewritten — the bucket
// forbids overwrites — so a legacy archive must serve exactly as before, with
// the field simply absent rather than null, guessed, or an error.
func TestVideoManifestLegacyArchiveHasNoDeliveryFormat(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newHandlerLogTestDB(t)
	store := storage.NewMemoryStorage()
	createVideoCapture(t, db, "mtleg", "https://www.youtube.com/shorts/wAGwnFMBdHk", map[string]string{"yt-dlp": "completed"})

	metadataKey := "mtleg/yt-dlp-legacy.metadata.json"
	legacy := []byte(`{"schema_version":"1","source_url":"https://www.youtube.com/shorts/wAGwnFMBdHk","platform":"youtube","post_id":"wAGwnFMBdHk","canonical_url":"https://www.youtube.com/watch?v=wAGwnFMBdHk","title":"Legacy Short","duration_seconds":59.921,"engagement":{},"media":{"extension":".mp4","content_type":"video/mp4","size_bytes":8,"width":1080,"height":1468},"archived_at":"2026-08-11T22:00:00Z","provenance":"native","provider":"yt-dlp"}`)
	storeTestObject(t, store, metadataKey, legacy)
	if err := db.Model(&models.ArchiveItem{}).Where("type = ?", "yt-dlp").
		Updates(map[string]interface{}{"storage_key": "mtleg/yt-dlp-legacy.mp4", "metadata_key": metadataKey}).Error; err != nil {
		t.Fatal(err)
	}

	router := gin.New()
	router.GET("/video/:shortid/manifest", func(c *gin.Context) { ServeVideoManifest(c, store, db) })
	normalized := manifestMetadata(t, router, "mtleg")

	if _, present := normalized["media_type"]; present {
		t.Errorf("legacy manifest gained a media_type it never captured: %v", normalized)
	}
	if normalized["duration_seconds"] != 59.921 || normalized["title"] != "Legacy Short" {
		t.Errorf("legacy manifest contract changed: %v", normalized)
	}
}
