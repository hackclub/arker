package canary

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"arker/internal/archivers"
	"arker/internal/models"
	"arker/internal/storage"
	"arker/internal/utils"
)

// newTestDB gives each test its own in-memory SQLite database, matching the
// pattern used elsewhere in the tree (internal/workers/archive_worker_test.go).
func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&models.ArchivedURL{}, &models.Capture{}, &models.ArchiveItem{}, &models.ArchiveItemLog{}, &models.BrightDataUsage{}, &models.CanaryRun{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func videoProbe() Probe {
	return Probe{
		Platform: "youtube", PostType: "video",
		URL:           "https://www.youtube.com/watch?v=jNQXAC9IVRw",
		ExpectedType:  utils.ArchiveTypeYtDlp,
		ExpectedMedia: MediaKindVideo,
		MinMediaBytes: 1024,
	}
}

func galleryProbe() Probe {
	return Probe{
		Platform: "reddit", PostType: "gallery",
		URL:           "https://www.reddit.com/r/pics/comments/haucpf/",
		ExpectedType:  utils.ArchiveTypeGalleryDl,
		ExpectedMedia: MediaKindImage,
		MinMediaBytes: 1024,
	}
}

// videoFixture writes a plausible completed yt-dlp archive into storage and
// returns the matching archive item. The tweaks callback mutates the metadata
// before it is written, so a test can express exactly one defect.
func videoFixture(t *testing.T, store storage.Storage, tweaks func(meta *archivers.VideoMetadata, item *models.ArchiveItem)) *models.ArchiveItem {
	t.Helper()
	payload := bytes.Repeat([]byte("v"), 4096)
	meta := archivers.VideoMetadata{
		SchemaVersion: archivers.VideoMetadataSchemaVersion,
		SourceURL:     "https://www.youtube.com/watch?v=jNQXAC9IVRw",
		Platform:      "youtube",
		PostID:        "jNQXAC9IVRw",
		CanonicalURL:  "https://www.youtube.com/watch?v=jNQXAC9IVRw",
		Title:         "Me at the zoo",
		ArchivedAt:    time.Now().UTC().Format(time.RFC3339),
		Provenance:    "native",
		Media: archivers.VideoMedia{
			Extension: ".mp4", ContentType: "video/mp4", SizeBytes: int64(len(payload)),
		},
	}
	item := &models.ArchiveItem{
		Type: utils.ArchiveTypeYtDlp, Status: "completed",
		StorageKey: "abc12/yt-dlp-0001.mp4", Extension: ".mp4",
		FileSize:       int64(len(payload)),
		MetadataKey:    "abc12/yt-dlp-0001.metadata.json",
		RawMetadataKey: "abc12/yt-dlp-0001.raw-metadata.json",
		Source:         models.ArchiveSourceNative,
	}
	if tweaks != nil {
		tweaks(&meta, item)
	}
	writeObject(t, store, item.StorageKey, payload)
	if item.MetadataKey != "" {
		writeObject(t, store, item.MetadataKey, mustJSON(t, meta))
	}
	if item.RawMetadataKey != "" {
		writeObject(t, store, item.RawMetadataKey, []byte(`{"id":"jNQXAC9IVRw","title":"Me at the zoo"}`))
	}
	return item
}

// galleryFile is one entry in a fixture gallery bundle.
type galleryFile struct {
	name string
	data []byte
}

// galleryFixture writes a gallery-dl style ZIP (normalized metadata.json, media
// files, raw per-file sidecars) and returns the matching archive item.
func galleryFixture(t *testing.T, store storage.Storage, meta archivers.GalleryMetadata, files []galleryFile) *models.ArchiveItem {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	if meta.ArchivedAt != "" || meta.PostID != "" || meta.PostURL != "" || meta.Extractor != "" {
		w, err := zw.Create("metadata.json")
		if err != nil {
			t.Fatalf("create metadata.json: %v", err)
		}
		if _, err := w.Write(mustJSON(t, meta)); err != nil {
			t.Fatalf("write metadata.json: %v", err)
		}
	}
	for _, f := range files {
		w, err := zw.Create(f.name)
		if err != nil {
			t.Fatalf("create %s: %v", f.name, err)
		}
		if _, err := w.Write(f.data); err != nil {
			t.Fatalf("write %s: %v", f.name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	key := "def34/gallery-dl-0001.zip"
	writeObject(t, store, key, buf.Bytes())
	return &models.ArchiveItem{
		Type: utils.ArchiveTypeGalleryDl, Status: "completed",
		StorageKey: key, Extension: ".zip", FileSize: int64(buf.Len()),
		Source: models.ArchiveSourceNative,
	}
}

// defaultGalleryMeta is a healthy two-image gallery post.
func defaultGalleryMeta() archivers.GalleryMetadata {
	return archivers.GalleryMetadata{
		SourceURL: "https://www.reddit.com/r/pics/comments/haucpf/",
		Extractor: "reddit", PostID: "haucpf",
		PostURL:    "https://www.reddit.com/r/pics/comments/haucpf/",
		ArchivedAt: time.Now().UTC().Format(time.RFC3339),
		FileCount:  2,
		Files: []archivers.GalleryFile{
			{Name: "001.jpg", Size: 2048, ContentType: "image/jpeg"},
			{Name: "002.jpg", Size: 2048, ContentType: "image/jpeg"},
		},
	}
}

func defaultGalleryFiles() []galleryFile {
	return []galleryFile{
		{name: "001.jpg", data: bytes.Repeat([]byte("a"), 2048)},
		{name: "001.jpg.json", data: []byte(`{"id":"1","title":"raw"}`)},
		{name: "002.jpg", data: bytes.Repeat([]byte("b"), 2048)},
		{name: "002.jpg.json", data: []byte(`{"id":"2","title":"raw"}`)},
	}
}

func writeObject(t *testing.T, store storage.Storage, key string, data []byte) {
	t.Helper()
	w, err := store.Writer(key)
	if err != nil {
		t.Fatalf("writer for %s: %v", key, err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatalf("write %s: %v", key, err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close %s: %v", key, err)
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return data
}
