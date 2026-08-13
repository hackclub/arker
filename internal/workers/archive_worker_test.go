package workers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"arker/internal/archivers"
	"arker/internal/models"
	"arker/internal/storage"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type stubArchiver struct{ err error }

func (s stubArchiver) Archive(ctx context.Context, url string, logWriter io.Writer, db *gorm.DB, itemID uint) (archivers.Result, error) {
	return archivers.Result{}, s.err
}

// dataArchiver is a stub that returns fixed content and no error.
type dataArchiver struct{ payload []byte }

func (d dataArchiver) Archive(ctx context.Context, url string, logWriter io.Writer, db *gorm.DB, itemID uint) (archivers.Result, error) {
	return archivers.Result{Data: bytes.NewReader(d.payload), Extension: ".mhtml", ContentType: "application/x-mhtml"}, nil
}

type videoDataArchiver struct{}

func (videoDataArchiver) Archive(ctx context.Context, url string, logWriter io.Writer, db *gorm.DB, itemID uint) (archivers.Result, error) {
	return archivers.Result{
		Data:        bytes.NewReader([]byte("mp4-data")),
		Extension:   ".mp4",
		ContentType: "video/mp4",
		Source:      models.ArchiveSourceNative,
		Metadata:    &archivers.Sidecar{Data: []byte(`{"schema_version":"1","title":"Fixture video"}`)},
		RawMetadata: &archivers.Sidecar{Data: []byte(`{"title":"Fixture video","cookie":"[REDACTED]"}`)},
	}, nil
}

func newWorkerTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&models.ArchivedURL{}, &models.Capture{}, &models.ArchiveItem{}, &models.ArchiveItemLog{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// TestProcessArchiveJobDoesNotMarkFailedOnRetryableError verifies a failed
// attempt leaves the item in "processing", not "failed" (regression: premature
// failed status during retry backoff).
func TestProcessArchiveJobDoesNotMarkFailedOnRetryableError(t *testing.T) {
	db := newWorkerTestDB(t)
	url := models.ArchivedURL{Original: "https://example.com"}
	db.Create(&url)
	capture := models.Capture{ArchivedURLID: url.ID, Timestamp: time.Now(), ShortID: "abc12"}
	db.Create(&capture)
	item := models.ArchiveItem{CaptureID: capture.ID, Type: "mhtml", Status: "processing"}
	db.Create(&item)

	m := map[string]archivers.Archiver{"mhtml": stubArchiver{err: errors.New("boom")}}
	args := ArchiveJobArgs{ShortID: "abc12", Type: "mhtml", URL: "https://example.com"}

	err := processArchiveJob(context.Background(), args, &item, storage.NewMemoryStorage(), db, m)
	if err == nil {
		t.Fatal("expected an error from the stub archiver")
	}

	var got models.ArchiveItem
	db.First(&got, item.ID)
	if got.Status == "failed" {
		t.Fatalf("item was marked failed on a retryable attempt; want processing, got %q", got.Status)
	}
}

func TestUploadNonceIsUnique(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 1000; i++ {
		n := uploadNonce()
		if n == "" {
			t.Fatal("uploadNonce returned empty string")
		}
		if seen[n] {
			t.Fatalf("uploadNonce collision: %q", n)
		}
		seen[n] = true
	}
}

func TestSaveArchiveDataMarksCompleted(t *testing.T) {
	db := newWorkerTestDB(t)
	url := models.ArchivedURL{Original: "https://example.com"}
	db.Create(&url)
	capture := models.Capture{ArchivedURLID: url.ID, Timestamp: time.Now(), ShortID: "sv001"}
	db.Create(&capture)
	item := models.ArchiveItem{CaptureID: capture.ID, Type: "mhtml", Status: "processing"}
	db.Create(&item)

	store := storage.NewMemoryStorage()
	payload := []byte("hello-archive")
	key := "sv001/mhtml-deadbeef.mhtml"
	if err := saveArchiveData(bytes.NewReader(payload), key, ".mhtml", "", store, db, &item); err != nil {
		t.Fatalf("saveArchiveData: %v", err)
	}

	// Storage got the bytes.
	if size, err := store.Size(key); err != nil || size != int64(len(payload)) {
		t.Fatalf("stored size = %d, err %v; want %d", size, err, len(payload))
	}
	// Item is completed with the right metadata.
	var got models.ArchiveItem
	db.First(&got, item.ID)
	if got.Status != "completed" || got.StorageKey != key || got.Extension != ".mhtml" || got.FileSize != int64(len(payload)) {
		t.Fatalf("item not finalized correctly: %+v", got)
	}
}

func TestProcessArchiveJobSuccessCompletes(t *testing.T) {
	db := newWorkerTestDB(t)
	url := models.ArchivedURL{Original: "https://example.com"}
	db.Create(&url)
	capture := models.Capture{ArchivedURLID: url.ID, Timestamp: time.Now(), ShortID: "sv002"}
	db.Create(&capture)
	item := models.ArchiveItem{CaptureID: capture.ID, Type: "mhtml", Status: "processing"}
	db.Create(&item)

	m := map[string]archivers.Archiver{"mhtml": dataArchiver{payload: []byte("body")}}
	args := ArchiveJobArgs{ShortID: "sv002", Type: "mhtml", URL: "https://example.com"}

	if err := processArchiveJob(context.Background(), args, &item, storage.NewMemoryStorage(), db, m); err != nil {
		t.Fatalf("processArchiveJob: %v", err)
	}
	var got models.ArchiveItem
	db.First(&got, item.ID)
	if got.Status != "completed" {
		t.Fatalf("status = %q, want completed", got.Status)
	}
	if got.StorageKey == "" {
		t.Fatal("storage key not set on completion")
	}
}

func TestProcessArchiveJobStoresVideoMetadataSidecars(t *testing.T) {
	db := newWorkerTestDB(t)
	url := models.ArchivedURL{Original: "https://www.youtube.com/watch?v=fixture"}
	db.Create(&url)
	capture := models.Capture{ArchivedURLID: url.ID, Timestamp: time.Now(), ShortID: "vid01"}
	db.Create(&capture)
	item := models.ArchiveItem{CaptureID: capture.ID, Type: "yt-dlp", Status: "processing"}
	db.Create(&item)

	store := storage.NewMemoryStorage()
	m := map[string]archivers.Archiver{"yt-dlp": videoDataArchiver{}}
	args := ArchiveJobArgs{ShortID: "vid01", Type: "yt-dlp", URL: url.Original}
	if err := processArchiveJob(context.Background(), args, &item, store, db, m); err != nil {
		t.Fatalf("processArchiveJob: %v", err)
	}

	var got models.ArchiveItem
	db.First(&got, item.ID)
	if got.Status != "completed" || got.StorageKey == "" || got.MetadataKey == "" || got.RawMetadataKey == "" {
		t.Fatalf("video item was not finalized with all artifact keys: %+v", got)
	}
	if got.Source != models.ArchiveSourceNative {
		t.Errorf("source = %q, want native", got.Source)
	}
	for key, want := range map[string]string{
		got.StorageKey:     "mp4-data",
		got.MetadataKey:    `{"schema_version":"1","title":"Fixture video"}`,
		got.RawMetadataKey: `{"title":"Fixture video","cookie":"[REDACTED]"}`,
	} {
		reader, err := store.Reader(key)
		if err != nil {
			t.Fatalf("read %s: %v", key, err)
		}
		data, err := io.ReadAll(reader)
		reader.Close()
		if err != nil || string(data) != want {
			t.Errorf("stored %s = %q, err %v; want %q", key, data, err, want)
		}
	}
}

// completenessArchiver reports a completeness verdict the way the social
// archivers do.
type completenessArchiver struct{ state string }

func (a completenessArchiver) Archive(ctx context.Context, url string, logWriter io.Writer, db *gorm.DB, itemID uint) (archivers.Result, error) {
	return archivers.Result{
		Data:         bytes.NewReader([]byte("zip-data")),
		Extension:    ".zip",
		ContentType:  "application/zip",
		Completeness: a.state,
	}, nil
}

// The completeness verdict has to reach the row, or the API cannot tell a
// salvaged partial capture from a whole one without opening the artifact.
func TestProcessArchiveJobStoresCompleteness(t *testing.T) {
	for name, tc := range map[string]struct{ reported, want string }{
		"partial":                   {archivers.CompletenessPartial, archivers.CompletenessPartial},
		"complete":                  {archivers.CompletenessComplete, archivers.CompletenessComplete},
		"unknown":                   {archivers.CompletenessUnknown, archivers.CompletenessUnknown},
		"unrecognized fails closed": {"totally-fine", archivers.CompletenessUnknown},
		// Archivers that cannot speak to completeness leave the column empty
		// rather than asserting anything about a non-social artifact.
		"silent archiver": {"", ""},
	} {
		t.Run(name, func(t *testing.T) {
			db := newWorkerTestDB(t)
			url := models.ArchivedURL{Original: "https://www.instagram.com/p/" + name + "/"}
			db.Create(&url)
			capture := models.Capture{ArchivedURLID: url.ID, Timestamp: time.Now(), ShortID: "c" + name[:4]}
			db.Create(&capture)
			item := models.ArchiveItem{CaptureID: capture.ID, Type: "gallery-dl", Status: "processing"}
			db.Create(&item)

			m := map[string]archivers.Archiver{"gallery-dl": completenessArchiver{state: tc.reported}}
			args := ArchiveJobArgs{ShortID: capture.ShortID, Type: "gallery-dl", URL: url.Original}
			if err := processArchiveJob(context.Background(), args, &item, storage.NewMemoryStorage(), db, m); err != nil {
				t.Fatalf("processArchiveJob: %v", err)
			}

			var got models.ArchiveItem
			db.First(&got, item.ID)
			if got.Status != "completed" {
				t.Fatalf("status = %q, want completed", got.Status)
			}
			if got.Completeness != tc.want {
				t.Errorf("completeness = %q, want %q", got.Completeness, tc.want)
			}
		})
	}
}

// closeTrackingReader reports whether the worker closed it.
type closeTrackingReader struct {
	io.Reader
	closed bool
}

func (r *closeTrackingReader) Close() error {
	r.closed = true
	return nil
}

type failingWriterStorage struct{ storage.Storage }

func (s failingWriterStorage) Writer(key string) (io.WriteCloser, error) {
	return nil, errors.New("storage unavailable")
}

// Archiver readers are backed by a live process or a goroutine writing into an
// io.Pipe; closing is what releases them. If saveArchiveData returns without
// closing, that goroutine blocks on Write forever and holds its temp directory
// — for gallery-dl, an entire downloaded post — for the life of the process.
func TestSaveArchiveDataClosesReaderWhenStorageWriterFails(t *testing.T) {
	data := &closeTrackingReader{Reader: strings.NewReader("payload")}

	err := saveArchiveData(data, "key", ".zip", "", failingWriterStorage{}, nil, &models.ArchiveItem{})
	if err == nil {
		t.Fatal("expected an error when the storage writer fails")
	}
	if !data.closed {
		t.Error("reader was not closed on the storage-writer failure path (leaks the archiver goroutine and its temp dir)")
	}
}

// subtitleArchiver returns a video plus caption tracks, the way the yt-dlp
// archiver does once a platform exposes them.
type subtitleArchiver struct{ failExtra bool }

func (a subtitleArchiver) Archive(ctx context.Context, url string, logWriter io.Writer, db *gorm.DB, itemID uint) (archivers.Result, error) {
	metadata := &archivers.VideoMetadata{
		SchemaVersion: archivers.VideoMetadataSchemaVersion,
		Title:         "Fixture video",
		Subtitles: []archivers.SubtitleTrack{{
			Lang: "en", Kind: archivers.SubtitleKindAuto, Format: "vtt",
			ArtifactSuffix: ".sub.en.vtt", SizeBytes: 12,
		}},
		Transcript: &archivers.Transcript{Lang: "en", Source: archivers.SubtitleKindAuto, Text: "hello there"},
	}
	encoded, err := archivers.MarshalVideoMetadata(metadata)
	if err != nil {
		return archivers.Result{}, err
	}
	return archivers.Result{
		Data:         bytes.NewReader([]byte("mp4-data")),
		Extension:    ".mp4",
		ContentType:  "video/mp4",
		Metadata:     &archivers.Sidecar{Data: encoded},
		RawMetadata:  &archivers.Sidecar{Data: []byte(`{"id":"x"}`)},
		Completeness: archivers.CompletenessComplete,
		Extras: []archivers.ExtraArtifact{{
			NameSuffix:  ".sub.en.vtt",
			ContentType: "text/vtt; charset=utf-8",
			Data:        []byte("WEBVTT\n\nhello"),
		}},
	}, nil
}

// A completed item must never advertise a subtitle track that is not stored, so
// extras go down before the row flips and their real keys are recorded in the
// metadata sidecar rather than left to be guessed.
func TestProcessArchiveJobStoresSubtitleExtras(t *testing.T) {
	db := newWorkerTestDB(t)
	url := models.ArchivedURL{Original: "https://www.youtube.com/watch?v=subs"}
	db.Create(&url)
	capture := models.Capture{ArchivedURLID: url.ID, Timestamp: time.Now(), ShortID: "sub01"}
	db.Create(&capture)
	item := models.ArchiveItem{CaptureID: capture.ID, Type: "yt-dlp", Status: "processing"}
	db.Create(&item)

	store := storage.NewMemoryStorage()
	m := map[string]archivers.Archiver{"yt-dlp": subtitleArchiver{}}
	args := ArchiveJobArgs{ShortID: "sub01", Type: "yt-dlp", URL: url.Original}
	if err := processArchiveJob(context.Background(), args, &item, store, db, m); err != nil {
		t.Fatalf("processArchiveJob: %v", err)
	}

	var got models.ArchiveItem
	db.First(&got, item.ID)
	if got.Status != "completed" || got.MetadataKey == "" {
		t.Fatalf("item was not finalized: %+v", got)
	}

	reader, err := store.Reader(got.MetadataKey)
	if err != nil {
		t.Fatalf("read metadata: %v", err)
	}
	raw, _ := io.ReadAll(reader)
	reader.Close()
	var metadata archivers.VideoMetadata
	if err := json.Unmarshal(raw, &metadata); err != nil {
		t.Fatalf("decode metadata: %v", err)
	}
	if len(metadata.Subtitles) != 1 {
		t.Fatalf("subtitles = %+v", metadata.Subtitles)
	}
	key := metadata.Subtitles[0].StorageKey
	if key == "" {
		t.Fatal("the stored metadata does not record where the subtitle track went")
	}
	// The key must sit under the same base as the video, and the object must
	// actually be there — the whole point of storing extras first.
	if !strings.HasSuffix(key, ".sub.en.vtt") {
		t.Errorf("subtitle key = %q", key)
	}
	if exists, err := store.Exists(key); err != nil || !exists {
		t.Fatalf("subtitle object %q missing (err %v)", key, err)
	}
	subtitle, err := store.Reader(key)
	if err != nil {
		t.Fatalf("read subtitle: %v", err)
	}
	data, _ := io.ReadAll(subtitle)
	subtitle.Close()
	if string(data) != "WEBVTT\n\nhello" {
		t.Errorf("stored subtitle = %q", data)
	}
	if metadata.Transcript == nil || metadata.Transcript.Text != "hello there" {
		t.Errorf("transcript = %+v", metadata.Transcript)
	}
}

// A video with no captions is the normal case and must finalize exactly as
// before: no extras, no subtitle fields, still completed and complete.
func TestProcessArchiveJobWithoutSubtitlesIsUnchanged(t *testing.T) {
	db := newWorkerTestDB(t)
	url := models.ArchivedURL{Original: "https://www.youtube.com/watch?v=nosubs"}
	db.Create(&url)
	capture := models.Capture{ArchivedURLID: url.ID, Timestamp: time.Now(), ShortID: "nos01"}
	db.Create(&capture)
	item := models.ArchiveItem{CaptureID: capture.ID, Type: "yt-dlp", Status: "processing"}
	db.Create(&item)

	store := storage.NewMemoryStorage()
	m := map[string]archivers.Archiver{"yt-dlp": videoDataArchiver{}}
	args := ArchiveJobArgs{ShortID: "nos01", Type: "yt-dlp", URL: url.Original}
	if err := processArchiveJob(context.Background(), args, &item, store, db, m); err != nil {
		t.Fatalf("processArchiveJob: %v", err)
	}

	var got models.ArchiveItem
	db.First(&got, item.ID)
	if got.Status != "completed" || got.StorageKey == "" || got.MetadataKey == "" || got.RawMetadataKey == "" {
		t.Fatalf("item without captions was not finalized: %+v", got)
	}
}
