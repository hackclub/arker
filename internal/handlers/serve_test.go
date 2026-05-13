package handlers

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"arker/internal/models"
	"arker/internal/storage"

	"github.com/gin-gonic/gin"
)

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
