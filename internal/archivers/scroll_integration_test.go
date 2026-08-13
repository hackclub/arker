package archivers

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/mxschmitt/playwright-go"
)

// TestBrowserScrollDoesNotChaseInfiniteContent is the deterministic form of
// the Imgur QNsmD failure. Imgur appends recommendation/ad content as the page
// is scrolled; the old loop treated each new scrollHeight as a new target and
// could therefore run until the two-minute archive timeout on every retry.
//
// The regular CI image does not install Chromium, so the real-browser check is
// opt-in. It is exercised in the repository's development container with
// ARKER_BROWSER_INTEGRATION=1.
func TestBrowserScrollDoesNotChaseInfiniteContent(t *testing.T) {
	if os.Getenv("ARKER_BROWSER_INTEGRATION") != "1" {
		t.Skip("set ARKER_BROWSER_INTEGRATION=1 in the Playwright development container")
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<!doctype html>
<style>html,body{margin:0}.block{height:1200px}</style>
<div class="block">archivable post</div>
<script>
let additions = 0;
addEventListener('scroll', () => {
  if (scrollY + innerHeight < document.body.scrollHeight - 50) return;
  const block = document.createElement('div');
  block.className = 'block';
  block.textContent = 'infinite recommendation ' + (++additions);
  document.body.appendChild(block);
});
</script>`)
	}))
	defer server.Close()

	var logs bytes.Buffer
	bundle, page, err := setupBrowserForArchiving(&logs)
	if err != nil {
		t.Fatalf("start browser: %v", err)
	}
	defer bundle.Cleanup()
	if _, err := page.Goto(server.URL, playwright.PageGotoOptions{WaitUntil: playwright.WaitUntilStateLoad}); err != nil {
		t.Fatalf("load fixture: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	started := time.Now()
	if err := scrollToBottomAndWaitWithContext(ctx, page, &logs); err != nil {
		t.Fatalf("bounded scroll failed after %s: %v\n%s", time.Since(started).Round(time.Millisecond), err, logs.String())
	}
	if elapsed := time.Since(started); elapsed >= 4*time.Second {
		t.Fatalf("scroll chased appended content for %s", elapsed.Round(time.Millisecond))
	}
}
