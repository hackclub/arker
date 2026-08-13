package brightdata

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	neturl "net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/mxschmitt/playwright-go"
)

// This file holds the machinery for fetching bytes from Bright Data's network
// position rather than Arker's.
//
// Some platforms sign their media URLs against the IP that resolved them.
// Arker can download Instagram, Reddit and X media over its own connection
// because those URLs are portable, but YouTube's googlevideo URLs and TikTok's
// video CDN URLs are not: they answer only the network position that asked for
// them, and from anywhere else return 403. For those the bytes have to be
// pulled inside a Bright Data Browser API session — a remote Chrome on their
// unblocked network — with in-page ranged fetches whose payloads cross back to
// Arker base64-chunked over CDP.
//
// YouTube adds its own resolution step on top (an in-page Innertube call, see
// youtube.go); TikTok just fetches the URL the dataset already handed us. The
// session handling, the chunk loop and the retry rules are the same for both
// and live here.

// browserPageOverheadBytes is the estimated non-media traffic of one browser
// session (HTML, JS, API calls). Counted into usage so the cost estimate errs
// high rather than silently low.
const browserPageOverheadBytes = 3 << 20

// mediaChunkBytes is the Range size for in-page media fetches. Each chunk
// crosses the CDP connection base64-encoded, so this trades round-trips
// against websocket message size.
const mediaChunkBytes = 6 << 20

// pageEvaluator is the slice of playwright.Page the in-page machinery uses.
// Narrowing it to the one method the fetch loop calls is what lets the loop be
// tested offline against a fake page instead of a live remote browser.
type pageEvaluator interface {
	Evaluate(expression string, arg ...any) (any, error)
}

// browserSession is one open page in a Bright Data remote browser. Close
// releases the remote session and the local Playwright driver; it must always
// be called, since an unclosed session keeps billing.
type browserSession interface {
	pageEvaluator
	Close()
}

// openBrowserSession connects to a Bright Data Browser API session, opens a
// page and navigates it to pageURL.
//
// The navigation is not decoration: an in-page fetch runs under the page's
// origin, so the page a session lands on decides whether the CDN will answer
// the request at all (CORS) and what Referer it sees. Callers pass the
// platform page that owns the media.
func (c *Client) openBrowserSession(ctx context.Context, country, pageURL string, logWriter io.Writer) (browserSession, error) {
	if c.openBrowser != nil {
		return c.openBrowser(ctx, country, pageURL, logWriter)
	}
	if !c.BrowserReady() {
		return nil, fmt.Errorf("Bright Data browser credentials are not configured")
	}

	fmt.Fprintf(logWriter, "Connecting to Bright Data browser session (%s)...\n", countryLabel(country))
	pw, err := playwright.Run()
	if err != nil {
		return nil, fmt.Errorf("failed to start Playwright: %w", err)
	}

	browser, err := pw.Chromium.ConnectOverCDP(c.browserWSEndpoint(country))
	if err != nil {
		pw.Stop()
		return nil, fmt.Errorf("failed to connect to Bright Data browser: %w", err)
	}
	closeSession := func() {
		browser.Close()
		pw.Stop()
	}

	page, err := browserPage(browser)
	if err != nil {
		closeSession()
		return nil, &sessionOpenError{connected: true, err: err}
	}

	fmt.Fprintf(logWriter, "Loading %s in remote browser...\n", pageURL)
	if err := gotoWithRetry(ctx, page, pageURL, logWriter); err != nil {
		closeSession()
		return nil, &sessionOpenError{connected: true, err: fmt.Errorf("remote browser navigation failed: %w", err)}
	}
	return &playwrightSession{Page: page, closeFn: closeSession}, nil
}

// sessionOpenError marks a session attempt that reached Bright Data's network
// before failing. Navigation is real traffic on a real session, so it is
// billable even though it produced nothing; a CDP connect that never landed is
// not. Without the distinction the usage row either invents spend for failed
// connects or hides it for failed page loads, and the table exists to be
// trusted in both directions.
type sessionOpenError struct {
	connected bool
	err       error
}

func (e *sessionOpenError) Error() string { return e.err.Error() }
func (e *sessionOpenError) Unwrap() error { return e.err }

// sessionConnected reports whether a failed session attempt transferred
// anything billable. Unmarked errors count as not connected: those are the
// pre-connect failures (no credentials, Playwright unavailable, CDP refused).
func sessionConnected(err error) bool {
	var open *sessionOpenError
	return errors.As(err, &open) && open.connected
}

// playwrightSession adapts a live Playwright page to the browserSession
// interface.
type playwrightSession struct {
	playwright.Page
	closeFn func()
}

func (s *playwrightSession) Close() { s.closeFn() }

// browserFetchRequest is one "get me these bytes from Bright Data's network
// position" job.
type browserFetchRequest struct {
	// PageURL is the page the session opens before fetching: it supplies the
	// origin and Referer the CDN checks.
	PageURL string
	// MediaURLs are candidate URLs for the same asset, tried in order within a
	// session. Platforms that hand out more than one signed URL for a video
	// (TikTok's video_url and cdn_link) get a second chance for free.
	MediaURLs []string
	// Countries are session exit geographies to try, in order; "" lets Bright
	// Data pick any peer. A second entry only costs money when the first
	// session fails in a way Retryable accepts.
	Countries []string
	// TotalHint is the expected byte count when the caller knows it, or 0.
	TotalHint   int64
	TempPattern string
	LogWriter   io.Writer
	// Retryable decides whether a failed session is worth repeating elsewhere.
	// Nil means never retry.
	Retryable func(error) bool
}

// browserFetchResult reports what a fetch cost as well as what it produced.
//
// Size and StoredBytes are deliberately separate and are NOT the same number.
// Size is every byte that crossed the session — including the partial transfer
// of a candidate URL that died halfway, and a whole session's worth of
// progress thrown away on a retry elsewhere — which is what Bright Data bills
// for. StoredBytes is the size of the file at Path, which is what the
// normalized metadata must report. Billing the stored size under-reports
// spend; describing the archive with the billed size claims it holds bytes it
// does not, which the canary validator catches as a metadata/storage mismatch.
type browserFetchResult struct {
	Path        string
	Size        int64
	StoredBytes int64
	Sessions    int
	// MediaURL is the candidate that actually answered.
	MediaURL string
}

// fetchThroughBrowser downloads one asset through Bright Data's browser,
// trying each session geography until one works. The result carries the
// session count and byte total even on the error path so the caller can still
// bill what was spent.
func (c *Client) fetchThroughBrowser(ctx context.Context, req browserFetchRequest) (browserFetchResult, error) {
	countries := req.Countries
	if len(countries) == 0 {
		countries = []string{""}
	}
	if len(req.MediaURLs) == 0 {
		return browserFetchResult{}, fmt.Errorf("no media URL to fetch through the browser")
	}

	// A compliance-policy refusal earlier in this process means every further
	// session against the same host is a guaranteed billed failure until the
	// account completes KYC and the process restarts. Skip before spending.
	if host := hostOf(req.PageURL); host != "" && c.isPolicyBlocked(host) {
		fmt.Fprintf(req.LogWriter, "Bright Data compliance policy already refused %s this process lifetime; "+
			"skipping the browser session (account KYC approval required: https://brightdata.com/cp/kyc)\n", host)
		return browserFetchResult{}, fmt.Errorf("bright data compliance policy blocks %s (account KYC approval required)", host)
	}

	var out browserFetchResult
	var lastErr error
	for _, country := range countries {
		if lastErr != nil {
			fmt.Fprintf(req.LogWriter, "Retrying with a different session geography (%s) after: %v\n",
				countryLabel(country), lastErr)
		}

		path, transferred, stored, mediaURL, opened, err := c.browserFetchSession(ctx, country, req)
		out.Size += transferred
		// Only a session that actually opened is billable: a connect that
		// never established one transferred nothing, and counting it would
		// invent page-load overhead in the spend estimate.
		if opened {
			out.Sessions++
		}
		if err == nil {
			out.Path, out.MediaURL, out.StoredBytes = path, mediaURL, stored
			return out, nil
		}
		lastErr = err
		if compliancePolicyError(err) {
			if host := hostOf(req.PageURL); host != "" {
				c.markPolicyBlocked(host)
			}
			break
		}
		if ctx.Err() != nil || req.Retryable == nil || !req.Retryable(err) {
			break
		}
	}
	return out, lastErr
}

// hostOf returns the lowercase hostname of a URL, or "" when unparseable.
func hostOf(rawURL string) string {
	parsed, err := neturl.Parse(rawURL)
	if err != nil {
		return ""
	}
	return strings.ToLower(parsed.Hostname())
}

// browserFetchSession runs one session: connect, navigate, then try each
// candidate URL until one yields bytes. It returns the transferred total (every
// candidate attempt, billable) and the stored size (the candidate that
// answered) separately, plus whether a remote session was actually
// established, which is what makes it billable at all.
func (c *Client) browserFetchSession(ctx context.Context, country string, req browserFetchRequest) (string, int64, int64, string, bool, error) {
	session, err := c.openBrowserSession(ctx, country, req.PageURL, req.LogWriter)
	if err != nil {
		// A session that reached Bright Data before failing still loaded a
		// page, so it is billed; one that never connected is not.
		return "", 0, 0, "", sessionConnected(err), err
	}
	defer session.Close()

	var fetched int64
	var lastErr error
	for _, mediaURL := range req.MediaURLs {
		path, size, err := fetchURLThroughPage(ctx, session, mediaURL, req.TotalHint, req.TempPattern, req.LogWriter)
		fetched += size
		if err == nil {
			return path, fetched, size, mediaURL, true, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			break
		}
		fmt.Fprintf(req.LogWriter, "In-page fetch failed for one candidate URL: %v\n", err)
	}
	return "", fetched, 0, "", true, lastErr
}

// fetchMediaChunkJS fetches one Range of the media URL inside the page and
// returns it base64-encoded. Running in the page is the point: the media URL
// only answers to the remote session's exit IP.
const fetchMediaChunkJS = `
async (args) => {
  let r;
  try {
    r = await fetch(args.url, {headers: {'Range': 'bytes=' + args.start + '-' + args.end}});
  } catch (e) {
    return JSON.stringify({error: 'fetch failed: ' + e});
  }
  if (r.status !== 206 && r.status !== 200) {
    return JSON.stringify({error: 'status ' + r.status});
  }
  const contentRange = r.headers.get('Content-Range') || '';
  const buf = new Uint8Array(await r.arrayBuffer());
  let binary = '';
  const step = 0x8000;
  for (let i = 0; i < buf.length; i += step) {
    binary += String.fromCharCode.apply(null, buf.subarray(i, i + step));
  }
  return JSON.stringify({b64: btoa(binary), range: contentRange, status: r.status});
}
`

var contentRangeTotal = regexp.MustCompile(`bytes \d+-\d+/(\d+)`)

// fetchURLThroughPage pulls a URL through the remote page in ranged chunks.
// Returns the temp file path and the number of bytes fetched (also on error,
// for usage accounting).
//
// totalHint is the expected size when the caller knows it up front; 0 means
// unknown, which is the normal case for a cross-origin fetch: Content-Range is
// not a CORS-exposed header, so the loop cannot read the total and instead
// stops when the server runs out of file.
func fetchURLThroughPage(ctx context.Context, page pageEvaluator, mediaURL string, totalHint int64, tempPattern string, logWriter io.Writer) (string, int64, error) {
	out, err := createTempFile(tempPattern)
	if err != nil {
		return "", 0, err
	}
	outPath := out.Name()
	ok := false
	defer func() {
		out.Close()
		if !ok {
			removeFile(outPath)
		}
	}()

	total := totalHint
	var offset int64
	for {
		select {
		case <-ctx.Done():
			return "", offset, fmt.Errorf("cancelled during media download: %w", ctx.Err())
		default:
		}

		requested := int64(mediaChunkBytes)
		if total > 0 && total-offset < requested {
			requested = total - offset
		}
		end := offset + requested - 1

		chunk, contentRange, status, err := fetchOneChunk(page, mediaURL, offset, end)
		if err != nil {
			// A 416 after bytes have flowed is the server saying the offset
			// equals the file size: the download is complete, we just did not
			// know the size up front (Content-Range is not a CORS-exposed
			// header, and some providers omit the length entirely).
			if offset > 0 && isRangeNotSatisfiable(err) {
				fmt.Fprintf(logWriter, "Range end at %d bytes; download complete\n", offset)
				break
			}
			return "", offset, fmt.Errorf("media chunk at offset %d failed: %w", offset, err)
		}
		if _, err := out.Write(chunk); err != nil {
			return "", offset, err
		}
		offset += int64(len(chunk))

		if total <= 0 {
			if m := contentRangeTotal.FindStringSubmatch(contentRange); m != nil {
				if parsed, err := strconv.ParseInt(m[1], 10, 64); err == nil {
					total = parsed
					fmt.Fprintf(logWriter, "Media size from Content-Range: %d bytes\n", total)
				}
			}
		}

		// Termination: a 200 means the server ignored Range and sent the whole
		// file; a short or empty chunk means the range ran past EOF; and once
		// the total is known, stopping at it avoids a pointless final request.
		if status == 200 || int64(len(chunk)) < requested || (total > 0 && offset >= total) {
			break
		}
		if total > 0 {
			fmt.Fprintf(logWriter, "Fetched %d / %d bytes...\n", offset, total)
		} else {
			fmt.Fprintf(logWriter, "Fetched %d bytes so far (total not disclosed by the CDN)...\n", offset)
		}
	}

	if offset == 0 {
		return "", 0, fmt.Errorf("media download produced no bytes")
	}
	if total > 0 && offset < total {
		return "", offset, fmt.Errorf("media download incomplete: %d of %d bytes", offset, total)
	}
	if err := out.Close(); err != nil {
		return "", offset, err
	}
	ok = true
	return outPath, offset, nil
}

// isRangeNotSatisfiable matches the in-page fetch error for HTTP 416.
func isRangeNotSatisfiable(err error) bool {
	return err != nil && strings.Contains(err.Error(), "status 416")
}

// deterministicChunkFailure reports failures another attempt cannot fix: the
// range starts past EOF (416), or the CDN refuses this session outright
// (401/403/404/410). Retrying those spends session time — which is billed by
// the byte and the second — to receive the same answer three times. A URL
// signed for a different network position fails this way, and the useful
// response is to try the next candidate or the next session, not to wait.
func deterministicChunkFailure(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	for _, status := range []string{"status 416", "status 401", "status 403", "status 404", "status 410"} {
		if strings.Contains(message, status) {
			return true
		}
	}
	return false
}

// fetchOneChunk runs one in-page ranged fetch with retries. Chunk fetches ride
// on a live remote session, so a transient failure is worth two more tries
// before abandoning the whole (already paid-for) session.
func fetchOneChunk(page pageEvaluator, mediaURL string, start, end int64) ([]byte, string, int, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if deterministicChunkFailure(lastErr) {
			break
		}
		if attempt > 0 {
			time.Sleep(2 * time.Second)
		}
		raw, err := page.Evaluate(fetchMediaChunkJS, map[string]interface{}{
			"url":   mediaURL,
			"start": fmt.Sprintf("%d", start),
			"end":   fmt.Sprintf("%d", end),
		})
		if err != nil {
			lastErr = err
			continue
		}
		text, ok := raw.(string)
		if !ok {
			lastErr = fmt.Errorf("chunk fetch returned %T", raw)
			continue
		}
		var result struct {
			Error  string `json:"error"`
			B64    string `json:"b64"`
			Range  string `json:"range"`
			Status int    `json:"status"`
		}
		if err := json.Unmarshal([]byte(text), &result); err != nil {
			lastErr = err
			continue
		}
		if result.Error != "" {
			lastErr = fmt.Errorf("%s", result.Error)
			continue
		}
		data, err := base64.StdEncoding.DecodeString(result.B64)
		if err != nil {
			lastErr = err
			continue
		}
		return data, result.Range, result.Status, nil
	}
	return nil, "", 0, lastErr
}

// gotoWithRetry navigates with a few retries. Bright Data's browser pool
// intermittently reports "No Peer Found (no_peers)" when no exit is free;
// their docs class it as transient, and a short wait usually clears it.
//
// Policy refusals are the opposite of transient: Bright Data gates some
// navigation targets (tiktok.com among them) behind account-level KYC
// approval, and every retry against that wall bills another attempt at the
// same guaranteed refusal. Verified live 2026-08-13: three retried sessions
// against a KYC-gated TikTok URL cost ~$0.08 to fail identically. Those
// errors abort immediately with a message naming the account-level fix.
func gotoWithRetry(ctx context.Context, page playwright.Page, targetURL string, logWriter io.Writer) error {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			fmt.Fprintf(logWriter, "Retrying navigation after transient error: %v\n", lastErr)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt) * 10 * time.Second):
			}
		}
		_, lastErr = page.Goto(targetURL, playwright.PageGotoOptions{
			Timeout:   playwright.Float(90000),
			WaitUntil: playwright.WaitUntilStateDomcontentloaded,
		})
		if lastErr == nil {
			return nil
		}
		if compliancePolicyError(lastErr) {
			fmt.Fprintf(logWriter, "Bright Data compliance policy refuses this navigation target; "+
				"not retrying. Fix is account-level: complete Bright Data's KYC approval "+
				"(https://brightdata.com/cp/kyc), after which this pathway works unchanged.\n")
			return fmt.Errorf("bright data compliance policy blocks this target (account KYC approval required): %w", lastErr)
		}
	}
	return lastErr
}

// compliancePolicyError reports whether a navigation error is Bright Data's
// permanent compliance-policy refusal rather than a transient pool problem.
func compliancePolicyError(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	return strings.Contains(message, "requires special permission") ||
		strings.Contains(message, "compliance policy")
}

// browserPage returns a fresh page in the remote browser's default context.
func browserPage(browser playwright.Browser) (playwright.Page, error) {
	contexts := browser.Contexts()
	if len(contexts) > 0 {
		return contexts[0].NewPage()
	}
	ctx, err := browser.NewContext()
	if err != nil {
		return nil, err
	}
	return ctx.NewPage()
}

// countryLabel names a session geography for the archive log.
func countryLabel(country string) string {
	if country == "" {
		return "any peer"
	}
	return "country " + country
}

// browserSessionCost estimates what a browser fetch cost: measured payload
// plus a flat page-load overhead per session opened.
func (c *Client) browserSessionCost(mediaBytes int64, sessions int) (int64, float64) {
	bytes := mediaBytes + int64(sessions)*browserPageOverheadBytes
	return bytes, float64(bytes) / 1e9 * c.cfg.BrowserCostPerGB
}
