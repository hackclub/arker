package brightdata

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"arker/internal/archivers"
	"arker/internal/models"
)

// The platform fallbacks are exercised end to end against fakes rather than
// mocked at the seams: a fake HTTP transport answers both the Bright Data API
// and the media CDNs, and a fake browser session answers in-page fetches. That
// keeps the dataset flow, the URL selection, the download loop, the ZIP
// layout, the sanitization and the usage accounting all under test while
// staying fully offline and spending nothing.

// newTestDB gives each test its own in-memory SQLite database.
func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&models.ArchivedURL{}, &models.Capture{}, &models.ArchiveItem{}, &models.BrightDataUsage{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// cdnResponse is one canned answer from the fake network.
type cdnResponse struct {
	Status int
	Body   []byte
	Header http.Header
}

// fakeNetwork answers both api.brightdata.com and the media CDNs. Any URL it
// was not told about returns 403, which is exactly how the platforms that
// IP-lock their media answer Arker.
type fakeNetwork struct {
	mu sync.Mutex

	// records is the snapshot payload the dataset returns.
	records []map[string]any
	// triggerStatus overrides the dataset trigger response code.
	triggerStatus int
	// snapshotStatus is the progress status reported ("ready" by default).
	snapshotStatus string

	// media maps an exact URL to its response.
	media map[string]cdnResponse
	// failTransport marks URLs whose request fails below HTTP — DNS, TLS,
	// connection reset — which is how net/http produces a *url.Error carrying
	// the full signed URL.
	failTransport map[string]bool

	requests []string
}

func newFakeNetwork(records ...map[string]any) *fakeNetwork {
	return &fakeNetwork{records: records, media: map[string]cdnResponse{}, failTransport: map[string]bool{}}
}

func (f *fakeNetwork) serve(rawURL string, body []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.media[rawURL] = cdnResponse{Status: http.StatusOK, Body: body}
}

func (f *fakeNetwork) requestedURLs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

func (f *fakeNetwork) requested(rawURL string) bool {
	for _, seen := range f.requestedURLs() {
		if seen == rawURL {
			return true
		}
	}
	return false
}

func (f *fakeNetwork) RoundTrip(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	f.requests = append(f.requests, req.URL.String())
	f.mu.Unlock()

	if req.URL.Host == "api.brightdata.com" {
		return f.serveAPI(req)
	}

	f.mu.Lock()
	failTransport := f.failTransport[req.URL.String()]
	response, ok := f.media[req.URL.String()]
	f.mu.Unlock()
	if failTransport {
		// http.Client wraps this in a *url.Error carrying the request URL.
		return nil, fmt.Errorf("dial tcp 203.0.113.1:443: connect: connection refused")
	}
	if !ok {
		// The shape of an IP-locked CDN refusing a request from Arker's own
		// network position.
		return httpResponse(req, http.StatusForbidden, []byte("forbidden")), nil
	}
	return httpResponse(req, response.Status, response.Body), nil
}

func (f *fakeNetwork) serveAPI(req *http.Request) (*http.Response, error) {
	path := req.URL.Path
	switch {
	case strings.HasPrefix(path, "/datasets/v3/trigger"):
		if f.triggerStatus != 0 {
			return httpResponse(req, f.triggerStatus, []byte(`{"error":"dataset unavailable"}`)), nil
		}
		return httpResponse(req, http.StatusOK, []byte(`{"snapshot_id":"snap-test-1"}`)), nil
	case strings.HasPrefix(path, "/datasets/v3/progress/"):
		status := f.snapshotStatus
		if status == "" {
			status = "ready"
		}
		body := fmt.Sprintf(`{"status":%q,"records":%d,"errors":0}`, status, len(f.records))
		return httpResponse(req, http.StatusOK, []byte(body)), nil
	case strings.HasPrefix(path, "/datasets/v3/snapshot/"):
		body, err := json.Marshal(f.records)
		if err != nil {
			return nil, err
		}
		return httpResponse(req, http.StatusOK, body), nil
	}
	return httpResponse(req, http.StatusNotFound, []byte("{}")), nil
}

func httpResponse(req *http.Request, status int, body []byte) *http.Response {
	return &http.Response{
		StatusCode:    status,
		Body:          io.NopCloser(bytes.NewReader(body)),
		Header:        http.Header{},
		ContentLength: int64(len(body)),
		Request:       req,
	}
}

// fakePage answers in-page ranged fetches the way a real remote browser does,
// including the part that matters most: Content-Range is not exposed to a
// cross-origin fetch unless the server opts in, so by default the loop has to
// find EOF by itself.
type fakePage struct {
	media map[string][]byte
	// exposeContentRange mirrors a CDN that sets
	// Access-Control-Expose-Headers: Content-Range.
	exposeContentRange bool
	requests           []string
	closed             bool
}

func newFakePage() *fakePage {
	return &fakePage{media: map[string][]byte{}}
}

func (p *fakePage) Evaluate(expression string, arg ...any) (any, error) {
	if len(arg) == 0 {
		return nil, fmt.Errorf("no arguments")
	}
	args, ok := arg[0].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("unexpected argument %T", arg[0])
	}
	mediaURL, _ := args["url"].(string)
	p.requests = append(p.requests, mediaURL)

	body, served := p.media[mediaURL]
	if !served {
		return `{"error":"status 403"}`, nil
	}

	start, _ := strconv.ParseInt(args["start"].(string), 10, 64)
	end, _ := strconv.ParseInt(args["end"].(string), 10, 64)
	if start >= int64(len(body)) {
		return `{"error":"status 416"}`, nil
	}
	if end >= int64(len(body)) {
		end = int64(len(body)) - 1
	}
	chunk := body[start : end+1]

	contentRange := ""
	if p.exposeContentRange {
		contentRange = fmt.Sprintf("bytes %d-%d/%d", start, end, len(body))
	}
	result, err := json.Marshal(map[string]any{
		"b64":    base64.StdEncoding.EncodeToString(chunk),
		"range":  contentRange,
		"status": 206,
	})
	if err != nil {
		return nil, err
	}
	return string(result), nil
}

func (p *fakePage) Close() { p.closed = true }

// newTestClient builds a client whose network and browser are fakes and whose
// usage rows land in an in-memory database.
func newTestClient(t *testing.T, network *fakeNetwork) (*Client, *gorm.DB) {
	t.Helper()
	client := &Client{
		cfg: Config{
			APIKey:               "test-key",
			CustomerID:           "test-customer",
			BrowserZone:          "test-zone",
			BrowserZonePassword:  "test-password",
			ScraperCostPerRecord: 0.0015,
			BrowserCostPerGB:     8.40,
		},
		http: &http.Client{Transport: network},
	}
	return client, newTestDB(t)
}

// withFakeBrowser points the client's browser sessions at a fake page and
// reports how many sessions were opened.
func withFakeBrowser(client *Client, pages ...browserSession) *int {
	opened := 0
	client.openBrowser = func(ctx context.Context, country, pageURL string, logWriter io.Writer) (browserSession, error) {
		index := opened
		opened++
		if index >= len(pages) {
			index = len(pages) - 1
		}
		if index < 0 {
			return nil, fmt.Errorf("no browser session available")
		}
		return pages[index], nil
	}
	return &opened
}

// loadRecords reads every record from a fixture file.
func loadRecords(t *testing.T, name string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	var records []map[string]any
	if err := json.Unmarshal(data, &records); err != nil {
		t.Fatalf("parse fixture %s: %v", name, err)
	}
	if len(records) == 0 {
		t.Fatalf("fixture %s has no records", name)
	}
	return records
}

// fakeMP4 is a byte string that passes verifyMP4.
func fakeMP4(size int) []byte {
	if size < 12 {
		size = 12
	}
	body := make([]byte, 0, size)
	body = append(body, 0, 0, 0, 32)
	body = append(body, []byte("ftypisom")...)
	for len(body) < size {
		body = append(body, byte(len(body)%251))
	}
	return body[:size]
}

// fakePNG is a real (tiny) image so thumbnail generation exercises its real
// path rather than the "could not decode" branch.
func fakePNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for x := 0; x < 8; x++ {
		for y := 0; y < 8; y++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 30), G: uint8(y * 30), B: 200, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func fakeJPEG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for x := 0; x < 8; x++ {
		for y := 0; y < 8; y++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 30), G: uint8(y * 30), B: 200, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 85}); err != nil {
		t.Fatalf("encode jpeg: %v", err)
	}
	return buf.Bytes()
}

// resultZip reads an archive result's ZIP payload into memory and closes it.
func resultZip(t *testing.T, result archivers.Result) *zip.Reader {
	t.Helper()
	data := readResult(t, result)
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("open result zip: %v", err)
	}
	return reader
}

func readResult(t *testing.T, result archivers.Result) []byte {
	t.Helper()
	if result.Data == nil {
		t.Fatal("result has no data")
	}
	data, err := io.ReadAll(result.Data)
	if err != nil {
		t.Fatalf("read result: %v", err)
	}
	if closer, ok := result.Data.(io.Closer); ok {
		closer.Close()
	}
	return data
}

func zipEntry(t *testing.T, reader *zip.Reader, name string) []byte {
	t.Helper()
	for _, file := range reader.File {
		if file.Name == name {
			rc, err := file.Open()
			if err != nil {
				t.Fatalf("open %s: %v", name, err)
			}
			defer rc.Close()
			data, err := io.ReadAll(rc)
			if err != nil {
				t.Fatalf("read %s: %v", name, err)
			}
			return data
		}
	}
	t.Fatalf("zip has no entry %s (has %v)", name, zipNames(reader))
	return nil
}

func zipNames(reader *zip.Reader) []string {
	names := make([]string, 0, len(reader.File))
	for _, file := range reader.File {
		names = append(names, file.Name)
	}
	return names
}

func galleryMetadataFromZip(t *testing.T, reader *zip.Reader) archivers.GalleryMetadata {
	t.Helper()
	var meta archivers.GalleryMetadata
	if err := json.Unmarshal(zipEntry(t, reader, "metadata.json"), &meta); err != nil {
		t.Fatalf("parse metadata.json: %v", err)
	}
	return meta
}

func videoMetadataFromSidecar(t *testing.T, sidecar *archivers.Sidecar) archivers.VideoMetadata {
	t.Helper()
	if sidecar == nil {
		t.Fatal("result has no normalized metadata sidecar")
	}
	var meta archivers.VideoMetadata
	if err := json.Unmarshal(sidecar.Data, &meta); err != nil {
		t.Fatalf("parse video metadata: %v", err)
	}
	return meta
}

func usageRows(t *testing.T, db *gorm.DB) []models.BrightDataUsage {
	t.Helper()
	var rows []models.BrightDataUsage
	if err := db.Order("id asc").Find(&rows).Error; err != nil {
		t.Fatalf("load usage rows: %v", err)
	}
	return rows
}

// syntheticSecret is the marker the sanitized fixtures use wherever a real
// signed-URL credential was scrubbed. Nothing carrying it may survive into a
// stored raw record.
const syntheticSecret = "SYNTHETIC-NOT-A-REAL-SECRET"

// assertNoSignedParamsAtRest fails when a stored record still carries the
// credentials of a signed media URL.
func assertNoSignedParamsAtRest(t *testing.T, what string, stored []byte) {
	t.Helper()
	if bytes.Contains(stored, []byte(syntheticSecret)) {
		t.Errorf("%s stores a signed-URL credential at rest", what)
	}
}

// intValue dereferences an optional counter for assertions.
func intValue(value *int64) int64 {
	if value == nil {
		return -1
	}
	return *value
}

// mustParseQuery is a small helper for asserting a load-bearing query
// parameter survived into a download request.
func mustParseQuery(t *testing.T, rawURL string) url.Values {
	t.Helper()
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse %s: %v", rawURL, err)
	}
	return parsed.Query()
}
