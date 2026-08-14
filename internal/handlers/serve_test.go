package handlers

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"arker/internal/models"
	"arker/internal/storage"

	"github.com/gin-gonic/gin"
)

func TestServeArchiveFinalDirectResponseHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const body = "valid archive bytes"
	objectStore := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "19")
		w.Header().Set("X-Object-Request-Method", r.Method)

		// Public R2/custom-domain URLs ignore S3 response header override query
		// parameters. The authenticated S3 endpoint applies them when they are
		// part of the presigned request.
		if r.URL.Query().Get("X-Amz-Signature") == "" {
			w.Header().Set("Content-Type", "application/octet-stream")
		} else {
			w.Header().Set("X-Presigned-Request", "true")
			if contentType := r.URL.Query().Get("response-content-type"); contentType != "" {
				w.Header().Set("Content-Type", contentType)
			}
			if disposition := r.URL.Query().Get("response-content-disposition"); disposition != "" {
				w.Header().Set("Content-Disposition", disposition)
			}
		}

		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, body)
		}
	}))
	t.Cleanup(objectStore.Close)

	storageInstance, err := storage.NewS3Storage(context.Background(), storage.S3Config{
		Endpoint:        objectStore.URL,
		Region:          "us-east-1",
		AccessKeyID:     "test-access-key",
		SecretAccessKey: "test-secret-key",
		Bucket:          "archive-bucket",
		Prefix:          "arker",
		ForcePathStyle:  true,
		PublicBaseURL:   objectStore.URL + "/assets",
	})
	if err != nil {
		t.Fatalf("NewS3Storage: %v", err)
	}

	tests := []struct {
		name           string
		storedType     string
		requestedType  string
		extension      string
		wantType       string
		wantAttachment bool
	}{
		{name: "yt-dlp MP4", storedType: "yt-dlp", requestedType: "yt-dlp", extension: ".mp4", wantType: "video/mp4"},
		{name: "screenshot WebP", storedType: "screenshot", requestedType: "screenshot", extension: ".webp", wantType: "image/webp"},
		{name: "MHTML", storedType: "mhtml", requestedType: "mhtml", extension: ".mhtml", wantType: "multipart/related", wantAttachment: true},
		{name: "gallery-dl ZIP", storedType: "gallery-dl", requestedType: "gallery-dl", extension: ".zip", wantType: "application/zip", wantAttachment: true},
		{name: "legacy youtube alias", storedType: "youtube", requestedType: "youtube", extension: ".mp4", wantType: "video/mp4"},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := newHandlerLogTestDB(t)
			archivedURL := models.ArchivedURL{Original: "https://example.com/archive/" + tt.requestedType}
			if err := db.Create(&archivedURL).Error; err != nil {
				t.Fatalf("create archived URL: %v", err)
			}
			capture := models.Capture{
				ArchivedURLID: archivedURL.ID,
				ShortID:       "mime" + string(rune('a'+i)),
				Timestamp:     time.Date(2026, time.August, 13, 0, 0, 0, 0, time.UTC),
			}
			if err := db.Create(&capture).Error; err != nil {
				t.Fatalf("create capture: %v", err)
			}
			item := models.ArchiveItem{
				CaptureID:  capture.ID,
				Type:       tt.storedType,
				Status:     "completed",
				StorageKey: "artifacts/" + capture.ShortID + "/archive" + tt.extension,
				Extension:  tt.extension,
				FileSize:   int64(len(body)),
			}
			if err := db.Create(&item).Error; err != nil {
				t.Fatalf("create archive item: %v", err)
			}

			router := gin.New()
			router.GET("/archive/:shortid/:type", func(c *gin.Context) { ServeArchive(c, storageInstance, db) })
			router.HEAD("/archive/:shortid/:type", func(c *gin.Context) { ServeArchive(c, storageInstance, db) })
			archiveServer := httptest.NewServer(router)
			t.Cleanup(archiveServer.Close)

			for _, method := range []string{http.MethodGet, http.MethodHead} {
				t.Run(method, func(t *testing.T) {
					req, err := http.NewRequest(method, archiveServer.URL+"/archive/"+capture.ShortID+"/"+tt.requestedType, nil)
					if err != nil {
						t.Fatalf("NewRequest: %v", err)
					}
					resp, err := archiveServer.Client().Do(req)
					if err != nil {
						t.Fatalf("request archive: %v", err)
					}
					defer resp.Body.Close()

					if resp.StatusCode != http.StatusOK {
						t.Fatalf("final status = %d, want %d", resp.StatusCode, http.StatusOK)
					}
					if got := resp.Header.Get("Content-Type"); got != tt.wantType {
						t.Fatalf("final Content-Type = %q, want %q (URL %s)", got, tt.wantType, resp.Request.URL)
					}
					if got := resp.Header.Get("X-Presigned-Request"); got != "true" {
						t.Fatalf("final response did not use the presigned object endpoint (URL %s)", resp.Request.URL)
					}
					if got := resp.Header.Get("X-Object-Request-Method"); got != method {
						t.Fatalf("final object request method = %q, want %q", got, method)
					}

					disposition := resp.Header.Get("Content-Disposition")
					if !tt.wantAttachment {
						if disposition != "" {
							t.Fatalf("final Content-Disposition = %q, want inline response without attachment", disposition)
						}
					} else {
						mediaType, params, err := mime.ParseMediaType(disposition)
						if err != nil {
							t.Fatalf("parse final Content-Disposition %q: %v", disposition, err)
						}
						if mediaType != "attachment" || !strings.HasSuffix(params["filename"], tt.extension) {
							t.Fatalf("final Content-Disposition = %q, want attachment filename ending in %q", disposition, tt.extension)
						}
					}

					responseBody, err := io.ReadAll(resp.Body)
					if err != nil {
						t.Fatalf("read final response: %v", err)
					}
					if method == http.MethodGet && string(responseBody) != body {
						t.Fatalf("final body = %q, want %q", responseBody, body)
					}
					if method == http.MethodHead && len(responseBody) != 0 {
						t.Fatalf("HEAD final body length = %d, want 0", len(responseBody))
					}
				})
			}
		})
	}
}

func TestServeArchiveContentRedirectsToDirectStorageURL(t *testing.T) {
	storageInstance := &fakeDirectURLStorage{
		directURL: "https://objects.example.com/archive/Gxrbu/youtube.mp4?sig=abc",
	}
	c, recorder := newArchiveContentTestContext(http.MethodGet, "")
	item := models.ArchiveItem{
		Type:       "youtube",
		StorageKey: "archive/Gxrbu/youtube.mp4",
		Extension:  ".mp4",
		FileSize:   1391726026,
	}

	serveArchiveContent(c, storageInstance, item, models.Capture{
		ShortID: "Gxrbu",
	}, models.ArchivedURL{
		Original: "https://www.youtube.com/watch?v=esxk_nScxFQ",
	})

	if recorder.Code != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusTemporaryRedirect)
	}
	if got := recorder.Header().Get("Location"); got != storageInstance.directURL {
		t.Fatalf("Location = %q, want %q", got, storageInstance.directURL)
	}
	if got := recorder.Body.String(); got != "" {
		t.Fatalf("body = %q, want empty", got)
	}
	if storageInstance.directKey != item.StorageKey {
		t.Fatalf("DirectURL key = %q, want %q", storageInstance.directKey, item.StorageKey)
	}
	if storageInstance.directOptions.ContentType != "video/mp4" {
		t.Fatalf("DirectURL ContentType = %q, want %q", storageInstance.directOptions.ContentType, "video/mp4")
	}
	if storageInstance.directOptions.Method != http.MethodGet {
		t.Fatalf("DirectURL Method = %q, want %q", storageInstance.directOptions.Method, http.MethodGet)
	}
	if storageInstance.directOptions.ContentDisposition != "" {
		t.Fatalf("DirectURL ContentDisposition = %q, want empty for inline youtube video", storageInstance.directOptions.ContentDisposition)
	}
	if storageInstance.readerOpened || storageInstance.seekableReaderOpened {
		t.Fatal("redirect path opened a proxy reader")
	}
}

func TestServeArchiveContentHeadRedirectsToDirectStorageURL(t *testing.T) {
	storageInstance := &fakeDirectURLStorage{
		directURL: "https://objects.example.com/archive/Gxrbu/youtube.mp4?sig=abc",
	}
	c, recorder := newArchiveContentTestContext(http.MethodHead, "")

	serveArchiveContent(c, storageInstance, models.ArchiveItem{
		Type:       "youtube",
		StorageKey: "archive/Gxrbu/youtube.mp4",
		Extension:  ".mp4",
		FileSize:   1391726026,
	}, models.Capture{
		ShortID: "Gxrbu",
	}, models.ArchivedURL{
		Original: "https://www.youtube.com/watch?v=esxk_nScxFQ",
	})

	if recorder.Code != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusTemporaryRedirect)
	}
	if got := recorder.Header().Get("Location"); got != storageInstance.directURL {
		t.Fatalf("Location = %q, want %q", got, storageInstance.directURL)
	}
	if got := recorder.Body.Len(); got != 0 {
		t.Fatalf("body length = %d, want 0", got)
	}
	if storageInstance.readerOpened || storageInstance.seekableReaderOpened {
		t.Fatal("HEAD redirect path opened a proxy reader")
	}
	if storageInstance.directOptions.Method != http.MethodHead {
		t.Fatalf("DirectURL Method = %q, want %q", storageInstance.directOptions.Method, http.MethodHead)
	}
}

func TestServeArchiveContentDirectStorageURLIncludesAttachmentDisposition(t *testing.T) {
	storageInstance := &fakeDirectURLStorage{
		directURL: "https://objects.example.com/archive/test/mhtml.mhtml?sig=abc",
	}
	c, _ := newArchiveContentTestContext(http.MethodGet, "")

	serveArchiveContent(c, storageInstance, models.ArchiveItem{
		Type:       "mhtml",
		StorageKey: "archive/test/mhtml.mhtml",
		Extension:  ".mhtml",
		FileSize:   10,
	}, models.Capture{
		ShortID: "test",
	}, models.ArchivedURL{
		Original: "https://example.com/path",
	})

	if storageInstance.directOptions.ContentType != "multipart/related" {
		t.Fatalf("DirectURL ContentType = %q, want %q", storageInstance.directOptions.ContentType, "multipart/related")
	}
	if storageInstance.directOptions.ContentDisposition == "" {
		t.Fatal("DirectURL ContentDisposition is empty, want attachment filename")
	}
}

func TestServeArchiveContentRangeRequest(t *testing.T) {
	storageInstance := newTestFSStorage(t, "videos/test.mp4", "0123456789")
	c, recorder := newArchiveContentTestContext(http.MethodGet, "bytes=2-5")

	serveArchiveContent(c, storageInstance, models.ArchiveItem{
		Type:       "youtube",
		StorageKey: "videos/test.mp4",
		Extension:  ".mp4",
		FileSize:   10,
	}, models.Capture{
		ShortID: "test",
	}, models.ArchivedURL{
		Original: "https://www.youtube.com/watch?v=test",
	})

	if recorder.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusPartialContent)
	}
	if got := recorder.Body.String(); got != "2345" {
		t.Fatalf("body = %q, want %q", got, "2345")
	}
	if got := recorder.Header().Get("Content-Range"); got != "bytes 2-5/10" {
		t.Fatalf("Content-Range = %q, want %q", got, "bytes 2-5/10")
	}
	if got := recorder.Header().Get("Accept-Ranges"); got != "bytes" {
		t.Fatalf("Accept-Ranges = %q, want %q", got, "bytes")
	}
	if got := recorder.Header().Get("Content-Type"); got != "video/mp4" {
		t.Fatalf("Content-Type = %q, want %q", got, "video/mp4")
	}
}

func TestServeArchiveContentHeadRequest(t *testing.T) {
	storageInstance := newTestFSStorage(t, "videos/test.mp4", "0123456789")
	c, recorder := newArchiveContentTestContext(http.MethodHead, "")

	serveArchiveContent(c, storageInstance, models.ArchiveItem{
		Type:       "youtube",
		StorageKey: "videos/test.mp4",
		Extension:  ".mp4",
		FileSize:   10,
	}, models.Capture{
		ShortID: "test",
	}, models.ArchivedURL{
		Original: "https://www.youtube.com/watch?v=test",
	})

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if got := recorder.Body.Len(); got != 0 {
		t.Fatalf("body length = %d, want 0", got)
	}
	if got := recorder.Header().Get("Content-Length"); got != "10" {
		t.Fatalf("Content-Length = %q, want %q", got, "10")
	}
	if got := recorder.Header().Get("Accept-Ranges"); got != "bytes" {
		t.Fatalf("Accept-Ranges = %q, want %q", got, "bytes")
	}
}

func TestServeArchiveContentFallsBackWhenDirectURLGenerationFails(t *testing.T) {
	storageInstance := &fakeDirectURLStorage{
		directURLError: errors.New("signing failed"),
		data:           []byte("0123456789"),
	}
	c, recorder := newArchiveContentTestContext(http.MethodGet, "bytes=2-5")

	serveArchiveContent(c, storageInstance, models.ArchiveItem{
		Type:       "youtube",
		StorageKey: "videos/test.mp4",
		Extension:  ".mp4",
		FileSize:   10,
	}, models.Capture{
		ShortID: "test",
	}, models.ArchivedURL{
		Original: "https://www.youtube.com/watch?v=test",
	})

	if recorder.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusPartialContent)
	}
	if got := recorder.Body.String(); got != "2345" {
		t.Fatalf("body = %q, want %q", got, "2345")
	}
	if got := recorder.Header().Get("Content-Range"); got != "bytes 2-5/10" {
		t.Fatalf("Content-Range = %q, want %q", got, "bytes 2-5/10")
	}
	if !storageInstance.seekableReaderOpened {
		t.Fatal("fallback path did not open seekable reader")
	}
}

func TestServeArchiveContentNonSeekableStorageStreams(t *testing.T) {
	storageInstance := storage.NewMemoryStorage()
	writeTestStorageObject(t, storageInstance, "videos/test.mp4", "0123456789")
	c, recorder := newArchiveContentTestContext(http.MethodGet, "")

	serveArchiveContent(c, storageInstance, models.ArchiveItem{
		Type:       "youtube",
		StorageKey: "videos/test.mp4",
		Extension:  ".mp4",
		FileSize:   10,
	}, models.Capture{
		ShortID: "test",
	}, models.ArchivedURL{
		Original: "https://www.youtube.com/watch?v=test",
	})

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if got := recorder.Body.String(); got != "0123456789" {
		t.Fatalf("body = %q, want %q", got, "0123456789")
	}
	if got := recorder.Header().Get("Content-Length"); got != "10" {
		t.Fatalf("Content-Length = %q, want %q", got, "10")
	}
}

func TestContentTypeForArchiveUsesYoutubeExtension(t *testing.T) {
	tests := []struct {
		name       string
		extension  string
		wantType   string
		wantAttach bool
	}{
		{
			name:      "mp4",
			extension: ".mp4",
			wantType:  "video/mp4",
		},
		{
			name:      "webm",
			extension: ".webm",
			wantType:  "video/webm",
		},
		{
			name:      "uppercase webm",
			extension: ".WEBM",
			wantType:  "video/webm",
		},
		{
			name:      "unknown video extension defaults to mp4",
			extension: "",
			wantType:  "video/mp4",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotType, gotAttach := contentTypeForArchive("youtube", tt.extension)
			if gotType != tt.wantType {
				t.Fatalf("contentTypeForArchive(youtube, %q) type = %q, want %q", tt.extension, gotType, tt.wantType)
			}
			if gotAttach != tt.wantAttach {
				t.Fatalf("contentTypeForArchive(youtube, %q) attach = %t, want %t", tt.extension, gotAttach, tt.wantAttach)
			}
		})
	}
}

func TestContentTypeForArchiveKeepsDownloadsAttached(t *testing.T) {
	tests := []struct {
		name       string
		typ        string
		extension  string
		wantType   string
		wantAttach bool
	}{
		{
			name:       "mhtml",
			typ:        "mhtml",
			wantType:   "multipart/related",
			wantAttach: true,
		},
		{
			name:       "git",
			typ:        "git",
			wantType:   "application/x-tar",
			wantAttach: true,
		},
		{
			name:       "itch",
			typ:        "itch",
			wantType:   "application/zip",
			wantAttach: true,
		},
		{
			name:       "default",
			typ:        "unknown",
			wantType:   "application/octet-stream",
			wantAttach: true,
		},
		{
			name:       "youtube mp4",
			typ:        "youtube",
			extension:  ".mp4",
			wantType:   "video/mp4",
			wantAttach: false,
		},
		{
			name:       "youtube webm",
			typ:        "youtube",
			extension:  ".webm",
			wantType:   "video/webm",
			wantAttach: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotType, gotAttach := contentTypeForArchive(tt.typ, tt.extension)
			if gotType != tt.wantType {
				t.Fatalf("contentTypeForArchive(%q, %q) type = %q, want %q", tt.typ, tt.extension, gotType, tt.wantType)
			}
			if gotAttach != tt.wantAttach {
				t.Fatalf("contentTypeForArchive(%q, %q) attach = %t, want %t", tt.typ, tt.extension, gotAttach, tt.wantAttach)
			}
		})
	}
}

func newTestFSStorage(t *testing.T, key, data string) *storage.FSStorage {
	t.Helper()

	storageInstance := storage.NewFSStorage(t.TempDir())
	writeTestStorageObject(t, storageInstance, key, data)
	return storageInstance
}

func writeTestStorageObject(t *testing.T, storageInstance storage.Storage, key, data string) {
	t.Helper()

	writer, err := storageInstance.Writer(key)
	if err != nil {
		t.Fatalf("Writer(%q): %v", key, err)
	}
	if _, err := io.WriteString(writer, data); err != nil {
		t.Fatalf("WriteString(%q): %v", key, err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Close(%q): %v", key, err)
	}
}

func newArchiveContentTestContext(method, rangeHeader string) (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	request := httptest.NewRequest(method, "/archive/test/youtube", nil)
	if rangeHeader != "" {
		request.Header.Set("Range", rangeHeader)
	}
	c.Request = request

	return c, recorder
}

type fakeDirectURLStorage struct {
	directURL            string
	directURLError       error
	directKey            string
	directOptions        storage.DirectURLOptions
	data                 []byte
	readerOpened         bool
	seekableReaderOpened bool
}

func (s *fakeDirectURLStorage) DirectURL(_ context.Context, key string, opts storage.DirectURLOptions) (string, error) {
	s.directKey = key
	s.directOptions = opts
	if s.directURLError != nil {
		return "", s.directURLError
	}
	return s.directURL, nil
}

func (s *fakeDirectURLStorage) Writer(string) (io.WriteCloser, error) {
	return nil, errors.New("not implemented")
}

func (s *fakeDirectURLStorage) Reader(string) (io.ReadCloser, error) {
	s.readerOpened = true
	return io.NopCloser(bytes.NewReader(s.data)), nil
}

func (s *fakeDirectURLStorage) Exists(string) (bool, error) {
	return true, nil
}

func (s *fakeDirectURLStorage) Size(string) (int64, error) {
	return int64(len(s.data)), nil
}

func (s *fakeDirectURLStorage) SeekableReader(string) (storage.ReadSeekCloser, error) {
	s.seekableReaderOpened = true
	return &fakeReadSeekCloser{Reader: bytes.NewReader(s.data)}, nil
}

type fakeReadSeekCloser struct {
	*bytes.Reader
}

func (r *fakeReadSeekCloser) Close() error {
	return nil
}
