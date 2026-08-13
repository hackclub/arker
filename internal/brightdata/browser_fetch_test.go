package brightdata

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

const testMediaURL = "https://cdn.example/signed.mp4"

func readFetched(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fetched file: %v", err)
	}
	t.Cleanup(func() { os.Remove(path) })
	return data
}

// The EOF rule that matters: a cross-origin fetch cannot read Content-Range
// unless the CDN exposes it, so the loop has to recognize the end of the file
// from a short chunk rather than from a declared total.
func TestFetchURLThroughPageFindsEOFWithoutContentRange(t *testing.T) {
	page := newFakePage()
	body := bytes.Repeat([]byte("a"), 1024)
	page.media[testMediaURL] = body

	path, size, err := fetchURLThroughPage(context.Background(), page, testMediaURL, 0, "arker-test-*.bin", io.Discard)
	if err != nil {
		t.Fatalf("fetchURLThroughPage: %v", err)
	}
	if size != int64(len(body)) {
		t.Errorf("size = %d; want %d", size, len(body))
	}
	if got := readFetched(t, path); !bytes.Equal(got, body) {
		t.Errorf("fetched %d bytes; want the whole body", len(got))
	}
	if len(page.requests) != 1 {
		t.Errorf("made %d in-page requests for a one-chunk file", len(page.requests))
	}
}

// A file whose size is an exact multiple of the chunk size gives no short
// chunk to stop on: the next range starts at EOF and the server answers 416,
// which after bytes have flowed means "done", not "failed".
func TestFetchURLThroughPageTreats416AfterBytesAsEOF(t *testing.T) {
	page := newFakePage()
	body := bytes.Repeat([]byte("b"), mediaChunkBytes)
	page.media[testMediaURL] = body

	path, size, err := fetchURLThroughPage(context.Background(), page, testMediaURL, 0, "arker-test-*.bin", io.Discard)
	if err != nil {
		t.Fatalf("fetchURLThroughPage: %v", err)
	}
	if size != int64(len(body)) {
		t.Errorf("size = %d; want %d", size, len(body))
	}
	if got := readFetched(t, path); len(got) != len(body) {
		t.Errorf("fetched %d bytes; want %d", len(got), len(body))
	}
	if len(page.requests) != 2 {
		t.Errorf("made %d requests; want the full chunk then the 416", len(page.requests))
	}
}

// A 416 on the very first range is a refusal, not an EOF: nothing was stored,
// so the fetch has to fail rather than report an empty success.
func TestFetchURLThroughPageFailsWhenNothingIsServed(t *testing.T) {
	page := newFakePage()

	_, size, err := fetchURLThroughPage(context.Background(), page, testMediaURL, 0, "arker-test-*.bin", io.Discard)
	if err == nil {
		t.Fatal("expected an error when the page cannot fetch the URL")
	}
	if size != 0 {
		t.Errorf("size = %d; want 0", size)
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("error does not carry the refusal: %v", err)
	}
}

// When the CDN does expose Content-Range, the total is learned from the first
// chunk and the loop stops exactly there.
func TestFetchURLThroughPageUsesContentRangeTotal(t *testing.T) {
	page := newFakePage()
	page.exposeContentRange = true
	body := bytes.Repeat([]byte("c"), mediaChunkBytes+512)
	page.media[testMediaURL] = body

	var log strings.Builder
	path, size, err := fetchURLThroughPage(context.Background(), page, testMediaURL, 0, "arker-test-*.bin", &log)
	if err != nil {
		t.Fatalf("fetchURLThroughPage: %v", err)
	}
	if size != int64(len(body)) {
		t.Errorf("size = %d; want %d", size, len(body))
	}
	if got := readFetched(t, path); !bytes.Equal(got, body) {
		t.Error("fetched bytes differ from the served body")
	}
	if len(page.requests) != 2 {
		t.Errorf("made %d requests; want 2", len(page.requests))
	}
	if !strings.Contains(log.String(), "Media size from Content-Range") {
		t.Error("log does not record where the size came from")
	}
}

// A known total that never arrives is an incomplete download, not a short
// file: reporting it as success would archive a truncated video.
func TestFetchURLThroughPageRejectsTruncatedDownload(t *testing.T) {
	page := newFakePage()
	page.media[testMediaURL] = bytes.Repeat([]byte("d"), 1024)

	_, _, err := fetchURLThroughPage(context.Background(), page, testMediaURL, 8192, "arker-test-*.bin", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("error = %v; want an incomplete-download failure", err)
	}
}

func TestFetchURLThroughPageStopsOnCancelledContext(t *testing.T) {
	page := newFakePage()
	page.media[testMediaURL] = bytes.Repeat([]byte("e"), 1024)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := fetchURLThroughPage(ctx, page, testMediaURL, 0, "arker-test-*.bin", io.Discard); err == nil {
		t.Fatal("expected a cancellation error")
	}
	if len(page.requests) != 0 {
		t.Error("kept fetching after the job ran out of budget")
	}
}

// Sessions cost money, so a second one is only opened when the failure is the
// kind a different exit could fix.
func TestFetchThroughBrowserRetriesOnlyRetryableFailures(t *testing.T) {
	cases := []struct {
		name         string
		retryable    func(error) bool
		wantSessions int
	}{
		{"retryable", func(error) bool { return true }, 2},
		{"not retryable", func(error) bool { return false }, 1},
		{"no rule", nil, 1},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			client := &Client{}
			sessions := withFakeBrowser(client, newFakePage())

			result, err := client.fetchThroughBrowser(context.Background(), browserFetchRequest{
				PageURL:     "https://example.com/post",
				MediaURLs:   []string{testMediaURL},
				Countries:   []string{"us", ""},
				TempPattern: "arker-test-*.bin",
				LogWriter:   io.Discard,
				Retryable:   c.retryable,
			})
			if err == nil {
				t.Fatal("expected the fetch to fail")
			}
			if *sessions != c.wantSessions {
				t.Errorf("opened %d sessions; want %d", *sessions, c.wantSessions)
			}
			// Page-load overhead is billable even when nothing was fetched.
			if result.Sessions != c.wantSessions {
				t.Errorf("reported %d sessions for billing; want %d", result.Sessions, c.wantSessions)
			}
		})
	}
}

// Every candidate is tried inside one session before another is paid for.
func TestFetchThroughBrowserTriesEveryCandidateInOneSession(t *testing.T) {
	page := newFakePage()
	body := bytes.Repeat([]byte("f"), 256)
	page.media["https://cdn.example/second.mp4"] = body

	client := &Client{}
	sessions := withFakeBrowser(client, page)

	result, err := client.fetchThroughBrowser(context.Background(), browserFetchRequest{
		PageURL:     "https://example.com/post",
		MediaURLs:   []string{"https://cdn.example/first.mp4", "https://cdn.example/second.mp4"},
		Countries:   []string{""},
		TempPattern: "arker-test-*.bin",
		LogWriter:   io.Discard,
	})
	if err != nil {
		t.Fatalf("fetchThroughBrowser: %v", err)
	}
	if *sessions != 1 {
		t.Errorf("opened %d sessions; want 1", *sessions)
	}
	if result.MediaURL != "https://cdn.example/second.mp4" {
		t.Errorf("reported %q as the URL that answered", result.MediaURL)
	}
	if got := readFetched(t, result.Path); !bytes.Equal(got, body) {
		t.Error("fetched bytes differ from the served body")
	}
}

func TestBrowserSessionCost(t *testing.T) {
	client := &Client{cfg: Config{BrowserCostPerGB: 8.40}}

	bytesTransferred, cost := client.browserSessionCost(100*1024*1024, 2)
	wantBytes := int64(100*1024*1024) + 2*browserPageOverheadBytes
	if bytesTransferred != wantBytes {
		t.Errorf("bytes = %d; want %d", bytesTransferred, wantBytes)
	}
	if want := float64(wantBytes) / 1e9 * 8.40; cost != want {
		t.Errorf("cost = %v; want %v", cost, want)
	}
	// A session that fetched nothing still loaded a page.
	if bytesTransferred, _ := client.browserSessionCost(0, 1); bytesTransferred != browserPageOverheadBytes {
		t.Errorf("empty session bytes = %d; want the page overhead", bytesTransferred)
	}
}

func TestDeterministicChunkFailure(t *testing.T) {
	cases := map[string]bool{
		"status 416":                true,
		"status 403":                true,
		"status 404":                true,
		"status 500":                false,
		"fetch failed: TypeError":   false,
		"websocket connection lost": false,
	}
	for message, want := range cases {
		if got := deterministicChunkFailure(errors.New(message)); got != want {
			t.Errorf("deterministicChunkFailure(%q) = %v; want %v", message, got, want)
		}
	}
	if deterministicChunkFailure(nil) {
		t.Error("nil error classified as deterministic")
	}
}
