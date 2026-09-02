package apify

import (
	"archive/zip"
	"bytes"
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
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"arker/internal/archivers"
	"arker/internal/models"
)

// The platform fallbacks are exercised end to end against a fake network
// rather than mocked at the seams: one fake transport answers the Apify API
// (actor start, run status, dataset items, key-value-store records) and the
// platform CDNs. That keeps the run flow, the URL selection, the download
// loop, the ZIP layout, the sanitization and the usage accounting all under
// test while staying fully offline and spending nothing.

// newTestDB gives each test its own in-memory SQLite database.
func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	// Concurrent actor runs (YouTube) write usage rows from two goroutines;
	// a single connection keeps shared-cache SQLite from reporting locks.
	if sqlDB, err := db.DB(); err == nil {
		sqlDB.SetMaxOpenConns(1)
	}
	if err := db.AutoMigrate(&models.ArchivedURL{}, &models.Capture{}, &models.ArchiveItem{}, &models.FallbackUsage{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// cdnResponse is one canned answer from the fake network.
type cdnResponse struct {
	Status int
	Body   []byte
}

// actorRun is one scripted actor run: which actor it answers, what its
// dataset holds and how it ends.
type actorRun struct {
	Actor string
	Items []map[string]any
	// Status is the run's final status; SUCCEEDED when empty.
	Status        string
	StatusMessage string
	CostUSD       float64
	// StartStatus, when set, makes the start call itself fail with that code.
	StartStatus int
	// Pending makes the first status poll report RUNNING once before the
	// final status, exercising the poll loop.
	Pending bool

	id        string
	datasetID string
	kvsID     string
	polls     int
	input     map[string]any
}

// runFor scripts a successful run of an actor that returns the given items.
func runFor(actor string, items ...map[string]any) actorRun {
	return actorRun{Actor: actor, Items: items}
}

// fakeNetwork answers api.apify.com and the media CDNs. Any URL it was not
// told about returns 403, which is exactly how the platforms that IP-lock
// their media answer Arker.
type fakeNetwork struct {
	mu sync.Mutex

	// queued runs are consumed in order per actor as start calls arrive. An
	// actor with nothing queued gets an empty SUCCEEDED run.
	queued  []*actorRun
	started []*actorRun

	// media maps an exact URL to its response.
	media map[string]cdnResponse
	// failTransport marks URLs whose request fails below HTTP — DNS, TLS,
	// connection reset — which is how net/http produces a *url.Error carrying
	// the full signed URL.
	failTransport map[string]bool

	requests []string
}

func newFakeNetwork(runs ...actorRun) *fakeNetwork {
	f := &fakeNetwork{media: map[string]cdnResponse{}, failTransport: map[string]bool{}}
	for _, run := range runs {
		f.enqueue(run)
	}
	return f
}

func (f *fakeNetwork) enqueue(run actorRun) {
	f.mu.Lock()
	defer f.mu.Unlock()
	copied := run
	f.queued = append(f.queued, &copied)
}

func (f *fakeNetwork) serve(rawURL string, body []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.media[rawURL] = cdnResponse{Status: http.StatusOK, Body: body}
}

func (f *fakeNetwork) serveStatus(rawURL string, status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.media[rawURL] = cdnResponse{Status: status, Body: []byte("nope")}
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

// startedRuns returns the runs the client actually started, in order.
func (f *fakeNetwork) startedRuns() []*actorRun {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*actorRun(nil), f.started...)
}

// startedActors lists the actors started, in order.
func (f *fakeNetwork) startedActors() []string {
	var actors []string
	for _, run := range f.startedRuns() {
		actors = append(actors, run.Actor)
	}
	return actors
}

// inputFor returns the input the first run of an actor was started with.
func (f *fakeNetwork) inputFor(actor string) map[string]any {
	for _, run := range f.startedRuns() {
		if run.Actor == actor {
			return run.input
		}
	}
	return nil
}

func (f *fakeNetwork) RoundTrip(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	f.requests = append(f.requests, req.URL.String())
	f.mu.Unlock()

	if req.URL.Host == "api.apify.com" {
		if resp, handled := f.serveAPI(req); handled {
			return resp, nil
		}
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

func (f *fakeNetwork) serveAPI(req *http.Request) (*http.Response, bool) {
	if req.Header.Get("Authorization") != "Bearer test-token" {
		return httpResponse(req, http.StatusUnauthorized, []byte(`{"error":{"type":"token-not-provided","message":"Authentication token was not provided"}}`)), true
	}
	path := req.URL.Path
	segments := strings.Split(strings.Trim(path, "/"), "/")
	switch {
	case len(segments) == 4 && segments[0] == "v2" && segments[1] == "acts" && segments[3] == "runs" && req.Method == http.MethodPost:
		actor, _ := url.PathUnescape(segments[2])
		var input map[string]any
		body, _ := io.ReadAll(req.Body)
		json.Unmarshal(body, &input)
		run := f.startRun(actor, input)
		if run.StartStatus != 0 {
			return httpResponse(req, run.StartStatus, []byte(`{"error":{"type":"actor-not-found","message":"Actor was not found"}}`)), true
		}
		return httpResponse(req, http.StatusCreated, f.runJSON(run)), true
	case len(segments) == 3 && segments[0] == "v2" && segments[1] == "actor-runs":
		run := f.runByID(segments[2])
		if run == nil {
			return httpResponse(req, http.StatusNotFound, []byte(`{"error":{"type":"record-not-found","message":"Run was not found"}}`)), true
		}
		return httpResponse(req, http.StatusOK, f.runJSON(run)), true
	case len(segments) == 4 && segments[0] == "v2" && segments[1] == "actor-runs" && segments[3] == "abort":
		return httpResponse(req, http.StatusOK, []byte(`{"data":{}}`)), true
	case len(segments) == 4 && segments[0] == "v2" && segments[1] == "datasets" && segments[3] == "items":
		for _, run := range f.startedRuns() {
			if run.datasetID == segments[2] {
				body, _ := json.Marshal(run.Items)
				if run.Items == nil {
					body = []byte("[]")
				}
				return httpResponse(req, http.StatusOK, body), true
			}
		}
		return httpResponse(req, http.StatusNotFound, []byte(`{"error":{"type":"record-not-found","message":"Dataset was not found"}}`)), true
	}
	// Key-value-store records and anything else are served like a CDN.
	return nil, false
}

func (f *fakeNetwork) startRun(actor string, input map[string]any) *actorRun {
	f.mu.Lock()
	defer f.mu.Unlock()
	var run *actorRun
	for i, queued := range f.queued {
		if queued.Actor == actor || queued.Actor == "" {
			run = queued
			f.queued = append(f.queued[:i], f.queued[i+1:]...)
			break
		}
	}
	if run == nil {
		run = &actorRun{Actor: actor}
	}
	n := len(f.started) + 1
	run.id = fmt.Sprintf("run-%d", n)
	run.datasetID = fmt.Sprintf("dataset-%d", n)
	run.kvsID = fmt.Sprintf("kvs-%d", n)
	run.input = input
	f.started = append(f.started, run)
	return run
}

func (f *fakeNetwork) runByID(id string) *actorRun {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, run := range f.started {
		if run.id == id {
			return run
		}
	}
	return nil
}

func (f *fakeNetwork) runJSON(run *actorRun) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	status := run.Status
	if status == "" {
		status = "SUCCEEDED"
	}
	if run.Pending && run.polls == 0 {
		status = "RUNNING"
	}
	run.polls++
	body, _ := json.Marshal(map[string]any{"data": map[string]any{
		"id":                     run.id,
		"status":                 status,
		"statusMessage":          run.StatusMessage,
		"defaultKeyValueStoreId": run.kvsID,
		"defaultDatasetId":       run.datasetID,
		"usageTotalUsd":          run.CostUSD,
	}})
	return body
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

// newTestClient builds a client whose network is a fake and whose usage rows
// land in an in-memory database.
func newTestClient(t *testing.T, network *fakeNetwork) (*Client, *gorm.DB) {
	t.Helper()
	client := &Client{
		cfg:          Config{Token: "test-token", RunTimeout: 30 * time.Second, MaxRunCostUSD: 0.50},
		http:         &http.Client{Transport: network},
		pollInterval: time.Millisecond,
	}
	return client, newTestDB(t)
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

// loadRecord reads the first record of a fixture file.
func loadRecord(t *testing.T, name string) map[string]any {
	t.Helper()
	return loadRecords(t, name)[0]
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

// realMP4 renders a tiny genuine MP4 (with or without an audio track) so the
// ffmpeg-backed paths can be exercised. Tests calling it skip without ffmpeg.
func realMP4(t *testing.T, withAudio bool) []byte {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	dest := t.TempDir() + "/clip.mp4"
	args := []string{"-v", "error", "-y", "-f", "lavfi", "-i", "color=c=blue:s=64x48:d=0.5:r=10"}
	if withAudio {
		args = append(args, "-f", "lavfi", "-i", "sine=frequency=440:duration=0.5", "-c:a", "aac", "-shortest")
	}
	args = append(args, "-c:v", "libx264", "-pix_fmt", "yuv420p", dest)
	if out, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		t.Skipf("ffmpeg cannot render a test clip: %v: %s", err, out)
	}
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	return data
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

func usageRows(t *testing.T, db *gorm.DB) []models.FallbackUsage {
	t.Helper()
	var rows []models.FallbackUsage
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
