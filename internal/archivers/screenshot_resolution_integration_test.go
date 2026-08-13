package archivers

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"golang.org/x/image/webp"
)

// TestScreenshotArchiverHonorsRequestedResolution exercises the same
// ScreenshotArchiver path used in production and records the browser metrics
// alongside the stored artifact dimensions. It is opt-in because it launches a
// real Playwright browser.
func TestScreenshotArchiverHonorsRequestedResolution(t *testing.T) {
	if os.Getenv("ARKER_TEST_PLAYWRIGHT") != "1" {
		t.Skip("set ARKER_TEST_PLAYWRIGHT=1 to run the Playwright integration test")
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!doctype html><html><head><style>html,body{margin:0}</style></head><body><h1>resolution repro</h1></body></html>`)
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	var logs bytes.Buffer
	result, err := (&ScreenshotArchiver{}).Archive(ctx, server.URL, &logs, nil, 0)
	if result.Bundle != nil {
		defer result.Bundle.Cleanup()
	}
	if err != nil {
		t.Fatalf("archive failed: %v\n%s", err, logs.String())
	}

	page, err := result.Bundle.GetPage()
	if err != nil {
		t.Fatal(err)
	}
	metrics, err := page.Evaluate(`() => ({
		innerWidth: window.innerWidth,
		innerHeight: window.innerHeight,
		devicePixelRatio: window.devicePixelRatio
	})`)
	if err != nil {
		t.Fatal(err)
	}

	artifact, err := io.ReadAll(result.Data)
	if err != nil {
		t.Fatal(err)
	}
	img, err := webp.Decode(bytes.NewReader(artifact))
	if err != nil {
		t.Fatal(err)
	}

	bounds := img.Bounds()
	t.Logf("requested viewport=1500x1080 deviceScaleFactor=2")
	t.Logf("actual browser metrics=%v", metrics)
	t.Logf("actual stored screenshot=%dx%d (%d bytes, WebP)", bounds.Dx(), bounds.Dy(), len(artifact))

	browserMetrics, ok := metrics.(map[string]any)
	if !ok {
		t.Fatalf("browser metrics have unexpected type %T", metrics)
	}
	for key, want := range map[string]int{
		"innerWidth":       1500,
		"innerHeight":      1080,
		"devicePixelRatio": 2,
	} {
		if got := browserMetrics[key]; fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("browser metric %s=%v, want %d", key, got, want)
		}
	}

	if bounds.Dx() != 3000 || bounds.Dy() != 2160 {
		t.Errorf("stored screenshot=%dx%d, want 3000x2160", bounds.Dx(), bounds.Dy())
	}
}
