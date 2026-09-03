// Package apify implements the paid fallback path for social media posts
// whose native archivers (yt-dlp, gallery-dl) are refused from Arker's own
// network position: login walls, throttles, geo-blocks, datacenter bans and
// IP-locked media.
//
// Each platform is served by one Apify Store actor. The client starts an
// actor run, waits for it to finish, reads its dataset items and, where the
// platform signs media against the resolving IP (YouTube, TikTok), pulls the
// bytes the actor stored in its key-value store. Everything else is fetched
// straight from the platform CDN, which is what the actors' records point at.
//
// Every run writes a FallbackUsage row carrying the usageTotalUsd the platform
// reports for the run, so spend is visible in the admin UI. Rows are written
// for failures too: a failed run is still billed for its actor start.
package apify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"

	"arker/internal/archivers"
	"arker/internal/models"
)

const apiBase = "https://api.apify.com/v2"

// Actor IDs, in the "owner~name" form the REST API accepts in paths.
const (
	ActorInstagram = "data-slayer~instagram-post-details"
	ActorTikTok    = "clockworks~tiktok-video-scraper"
	// YouTube is two actors: the downloader stores the muxed MP4 in its
	// key-value store (googlevideo URLs are IP-locked); the scraper is the
	// cheap metadata source.
	ActorYouTube         = "epctex~youtube-video-downloader"
	ActorYouTubeMetadata = "streamers~youtube-scraper"
	ActorFacebook        = "apify~facebook-posts-scraper"
	ActorReddit          = "harshmaur~reddit-scraper"
	ActorX               = "kaitoeasyapi~twitter-x-data-tweet-scraper-pay-per-result-cheapest"
	ActorPinterest       = "silentflow~pinterest-scraper-ppr"
)

// Config holds the credentials and tunables for the client.
type Config struct {
	// Token is the Apify API token.
	Token string
	// RunTimeout bounds one actor run from start to finished dataset. Runs
	// still executing at the deadline are aborted so they stop billing.
	RunTimeout time.Duration
	// MaxRunCostUSD is a per-run guard: a run whose platform-reported cost
	// exceeds it is logged loudly. It does not stop the archive — the money is
	// already spent — but it keeps a misconfigured actor from going unnoticed.
	MaxRunCostUSD float64
}

// Client talks to the Apify REST API.
type Client struct {
	cfg  Config
	http *http.Client
	// pollInterval is how long to sleep between run status polls when the
	// API returns before the run finishes; shortened in tests.
	pollInterval time.Duration
	// resolveHost and ipv6 back the delivery-host reachability check; nil
	// means the real resolver and the process-wide IPv6 probe.
	resolveHost hostResolver
	ipv6        func() bool
	// costSettleDelays schedules the re-reads of a finished run's cost (see
	// settleCost). Empty disables settlement.
	costSettleDelays []time.Duration
	// background tracks settlement goroutines so Close can wait for them.
	background sync.WaitGroup
}

// New builds a client. An empty token yields a disabled client so callers can
// wire it unconditionally.
func New(cfg Config) *Client {
	if cfg.RunTimeout <= 0 {
		cfg.RunTimeout = 10 * time.Minute
	}
	if cfg.MaxRunCostUSD <= 0 {
		cfg.MaxRunCostUSD = 0.50
	}
	return &Client{
		cfg:              cfg,
		http:             &http.Client{Timeout: 5 * time.Minute},
		pollInterval:     2 * time.Second,
		costSettleDelays: defaultCostSettleDelays,
	}
}

// defaultCostSettleDelays: pay-per-event charges land in stages after a run
// finishes — the start fee within a few seconds, the per-result fee some
// seconds later (measured: both within ~10s, but a slow ledger has been seen
// past 15s) — so the cost is re-read at each of these offsets after the run
// finishes and the row keeps the latest figure.
var defaultCostSettleDelays = []time.Duration{6 * time.Second, 20 * time.Second, 60 * time.Second, 3 * time.Minute}

// Close waits for background cost settlement to finish. It is only needed
// where the process is about to exit or a test is about to tear down.
func (c *Client) Close() {
	if c != nil {
		c.background.Wait()
	}
}

// Enabled reports whether the client has credentials.
func (c *Client) Enabled() bool {
	return c != nil && c.cfg.Token != ""
}

// run is what an actor run leaves behind once it has finished.
type run struct {
	ID              string
	Status          string
	KeyValueStoreID string
	DatasetID       string
	CostUSD         float64
	Items           []map[string]any
}

// runActor starts an actor with the given input, waits for it to finish and
// returns its dataset items. The usage row is created before the run starts
// and updated as facts arrive, so even a crash mid-run leaves a spend row.
// Success/Detail are finalized by the caller once it knows whether the items
// were actually usable.
func (c *Client) runActor(ctx context.Context, db *gorm.DB, usage *models.FallbackUsage, actorID string, input any, logWriter io.Writer) (*run, error) {
	usage.Provider = models.FallbackProviderApify
	usage.Product = strings.ReplaceAll(actorID, "~", "/")
	c.recordUsage(db, usage)

	// The run itself is bounded independently of the job context so an
	// abandoned run is aborted rather than left billing in the background.
	runCtx, cancel := context.WithTimeout(ctx, c.cfg.RunTimeout)
	defer cancel()

	started, err := c.startRun(runCtx, actorID, input)
	if err != nil {
		usage.Detail = truncate("start failed: "+err.Error(), 500)
		c.recordUsage(db, usage)
		return nil, err
	}
	usage.OperationID = started.ID
	usage.ResourceID = started.KeyValueStoreID
	c.recordUsage(db, usage)
	fmt.Fprintf(logWriter, "Apify actor %s started: run %s\n", usage.Product, started.ID)

	finished, err := c.waitForRun(runCtx, started.ID, logWriter)
	if finished != nil {
		usage.CostUSD = finished.CostUSD
	}
	if err != nil {
		if runCtx.Err() != nil {
			// Out of time: stop the meter. A detached context so the abort
			// itself is not cancelled by the deadline that triggered it.
			c.abortRun(started.ID)
		}
		usage.Detail = truncate("run failed: "+err.Error(), 500)
		c.recordUsage(db, usage)
		return nil, err
	}

	items, err := c.datasetItems(runCtx, finished.DatasetID)
	if err != nil {
		usage.Detail = truncate("dataset fetch failed: "+err.Error(), 500)
		c.recordUsage(db, usage)
		return nil, err
	}
	finished.Items = items
	usage.Records = len(items)
	c.recordUsage(db, usage)
	fmt.Fprintf(logWriter, "Apify run %s finished: %d item(s), $%.4f\n", finished.ID, len(items), finished.CostUSD)
	c.checkRunCost(finished.ID, usage.Product, finished.CostUSD)
	if finished.CostUSD == 0 {
		fmt.Fprintf(logWriter, "Run cost not yet reported by the platform; the usage row is updated once it settles\n")
		c.settleCost(db, usage.ID, finished.ID, usage.Product)
	}
	return finished, nil
}

func (c *Client) checkRunCost(runID, product string, costUSD float64) {
	if costUSD > c.cfg.MaxRunCostUSD {
		slog.Warn("Apify run cost exceeded the per-run guard", "run", runID, "actor", product, "cost_usd", costUSD, "guard_usd", c.cfg.MaxRunCostUSD)
	}
}

// settleCost re-reads a run whose cost was still zero when it finished.
// Pay-per-event actors are charged asynchronously, so the run object reports
// $0 for a few seconds after SUCCEEDED and the ledger would otherwise
// under-count every such run. The re-reads happen in the background so the
// archive does not wait on billing, and touch only the cost column so they
// cannot clobber the Success/Detail the caller finalizes meanwhile.
func (c *Client) settleCost(db *gorm.DB, usageID uint, runID, product string) {
	if db == nil || usageID == 0 || len(c.costSettleDelays) == 0 {
		return
	}
	c.background.Add(1)
	go func() {
		defer c.background.Done()
		recorded := 0.0
		for i, delay := range c.costSettleDelays {
			if i == 0 {
				time.Sleep(delay)
			} else {
				time.Sleep(delay - c.costSettleDelays[i-1])
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			var resp struct {
				Data runObject `json:"data"`
			}
			err := c.doJSON(ctx, http.MethodGet, fmt.Sprintf("%s/actor-runs/%s", apiBase, url.PathEscape(runID)), nil, &resp)
			cancel()
			if err != nil {
				slog.Warn("Could not re-read Apify run cost", "run", runID, "error", err)
				return
			}
			cost := resp.Data.UsageTotalUSD
			if cost == 0 || cost == recorded {
				continue
			}
			if err := db.Model(&models.FallbackUsage{}).Where("id = ?", usageID).Update("cost_usd", cost).Error; err != nil {
				slog.Error("Failed to record settled Apify run cost", "run", runID, "error", err)
				return
			}
			recorded = cost
			c.checkRunCost(runID, product, cost)
		}
		if recorded == 0 {
			slog.Warn("Apify run cost never settled; ledger row keeps $0", "run", runID, "actor", product)
		}
	}()
}

// runObject is the subset of the API's run resource the client reads.
type runObject struct {
	ID                     string  `json:"id"`
	Status                 string  `json:"status"`
	DefaultKeyValueStoreID string  `json:"defaultKeyValueStoreId"`
	DefaultDatasetID       string  `json:"defaultDatasetId"`
	UsageTotalUSD          float64 `json:"usageTotalUsd"`
	StatusMessage          string  `json:"statusMessage"`
}

func (r runObject) toRun() *run {
	return &run{ID: r.ID, Status: r.Status, KeyValueStoreID: r.DefaultKeyValueStoreID, DatasetID: r.DefaultDatasetID, CostUSD: r.UsageTotalUSD}
}

func (c *Client) startRun(ctx context.Context, actorID string, input any) (*run, error) {
	body, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	// The API's own wait-for-finish cap is 60s; most runs finish inside it
	// and never need a poll.
	endpoint := fmt.Sprintf("%s/acts/%s/runs?waitForFinish=60", apiBase, url.PathEscape(actorID))
	var resp struct {
		Data runObject `json:"data"`
	}
	if err := c.doJSON(ctx, http.MethodPost, endpoint, body, &resp); err != nil {
		return nil, err
	}
	if resp.Data.ID == "" {
		return nil, fmt.Errorf("actor start returned no run ID")
	}
	return resp.Data.toRun(), nil
}

// terminalRunStatuses are the run states after which nothing more happens.
var terminalRunStatuses = map[string]bool{"SUCCEEDED": true, "FAILED": true, "ABORTED": true, "TIMED-OUT": true, "TIMING-OUT": true, "ABORTING": true}

func (c *Client) waitForRun(ctx context.Context, runID string, logWriter io.Writer) (*run, error) {
	endpoint := fmt.Sprintf("%s/actor-runs/%s?waitForFinish=60", apiBase, url.PathEscape(runID))
	for {
		var resp struct {
			Data runObject `json:"data"`
		}
		if err := c.doJSON(ctx, http.MethodGet, endpoint, nil, &resp); err != nil {
			return nil, err
		}
		current := resp.Data.toRun()
		switch {
		case current.Status == "SUCCEEDED":
			return current, nil
		case terminalRunStatuses[current.Status]:
			detail := resp.Data.StatusMessage
			if detail == "" {
				detail = "no status message"
			}
			return current, fmt.Errorf("actor run %s ended %s: %s", runID, current.Status, truncate(detail, 200))
		}
		fmt.Fprintf(logWriter, "Apify run %s still %s; waiting...\n", runID, current.Status)
		select {
		case <-ctx.Done():
			return current, ctx.Err()
		case <-time.After(c.pollInterval):
		}
	}
}

func (c *Client) abortRun(runID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	endpoint := fmt.Sprintf("%s/actor-runs/%s/abort", apiBase, url.PathEscape(runID))
	if err := c.doJSON(ctx, http.MethodPost, endpoint, nil, nil); err != nil {
		slog.Warn("Could not abort Apify run", "run", runID, "error", err)
	}
}

func (c *Client) datasetItems(ctx context.Context, datasetID string) ([]map[string]any, error) {
	if datasetID == "" {
		return nil, fmt.Errorf("run has no dataset")
	}
	endpoint := fmt.Sprintf("%s/datasets/%s/items?clean=true&format=json", apiBase, url.PathEscape(datasetID))
	var items []map[string]any
	if err := c.doJSON(ctx, http.MethodGet, endpoint, nil, &items); err != nil {
		return nil, err
	}
	return items, nil
}

// doJSON performs an authenticated API call. Bodies are capped at 64 MiB: a
// dataset of one post is kilobytes, and anything larger is not a post.
func (c *Client) doJSON(ctx context.Context, method, endpoint string, body []byte, out any) error {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return sanitizeTransportError(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s returned %d: %s", method, redactToken(endpoint), resp.StatusCode, truncate(apiErrorMessage(data), 300))
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	return json.Unmarshal(data, out)
}

// apiErrorMessage pulls the human message out of an API error envelope.
func apiErrorMessage(data []byte) string {
	var envelope struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &envelope) == nil && envelope.Error.Message != "" {
		return envelope.Error.Type + ": " + envelope.Error.Message
	}
	return string(data)
}

func redactToken(endpoint string) string {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return endpoint
	}
	query := parsed.Query()
	if query.Has("token") {
		query.Set("token", "REDACTED")
		parsed.RawQuery = query.Encode()
	}
	return parsed.String()
}

// recordUsage persists a usage row, creating or updating it in place. Usage
// tracking must never fail an archive: errors are logged and swallowed.
func (c *Client) recordUsage(db *gorm.DB, usage *models.FallbackUsage) {
	if db == nil {
		return
	}
	if usage.ID == 0 {
		if err := db.Create(usage).Error; err != nil {
			slog.Error("Failed to record Apify usage", "error", err, "url", usage.URL)
		}
		return
	}

	// Cost settlement runs in the background. A caller can still be finalizing
	// Success/Detail when that goroutine records the charge, so saving this
	// in-memory struct wholesale would race and could put its stale $0 back over
	// the settled database value. Ordinary updates therefore leave cost alone;
	// a nonzero cost reported synchronously by the run is written explicitly.
	cost := usage.CostUSD
	if err := db.Omit("cost_usd").Save(usage).Error; err != nil {
		slog.Error("Failed to record Apify usage", "error", err, "url", usage.URL)
		return
	}
	if cost > 0 {
		if err := db.Model(&models.FallbackUsage{}).Where("id = ?", usage.ID).UpdateColumn("cost_usd", cost).Error; err != nil {
			slog.Error("Failed to record Apify usage cost", "error", err, "url", usage.URL)
		}
	}
}

// isApifyHost reports whether a media URL points at Apify's own API — a
// key-value-store record — which needs the token to read.
func isApifyHost(rawURL string) bool {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	return strings.EqualFold(parsed.Host, "api.apify.com")
}

// newMediaRequest builds a media download request. Platform CDN URLs go out
// bare; key-value-store URLs carry the token, since record reads on a private
// store are refused anonymously.
func (c *Client) newMediaRequest(ctx context.Context, mediaURL string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, mediaURL, nil)
	if err != nil {
		return nil, sanitizeTransportError(err)
	}
	if isApifyHost(mediaURL) {
		req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	} else {
		req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0 Safari/537.36")
	}
	return req, nil
}

// downloadToTemp streams a URL to a temp file and returns the path and size.
func (c *Client) downloadToTemp(ctx context.Context, mediaURL, pattern string) (string, int64, error) {
	req, err := c.newMediaRequest(ctx, mediaURL)
	if err != nil {
		return "", 0, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", 0, sanitizeTransportError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("media download returned %d", resp.StatusCode)
	}

	f, err := createTempFile(pattern)
	if err != nil {
		return "", 0, err
	}
	n, copyErr := io.Copy(f, resp.Body)
	closeErr := f.Close()
	if copyErr != nil || closeErr != nil {
		removeFile(f.Name())
		if copyErr == nil {
			copyErr = closeErr
		}
		return "", 0, fmt.Errorf("media download failed after %d bytes: %w", n, sanitizeTransportError(copyErr))
	}
	if n == 0 {
		removeFile(f.Name())
		return "", 0, fmt.Errorf("media download returned an empty body")
	}
	return f.Name(), n, nil
}

// downloadToPath streams a URL into dest, writing through a temp file so a
// partial download never sits at the final name.
func (c *Client) downloadToPath(ctx context.Context, mediaURL, dest string) (int64, error) {
	tmpPath, size, err := c.downloadToTemp(ctx, mediaURL, "apify-media-*")
	if err != nil {
		return 0, err
	}
	if err := renameTempFile(tmpPath, dest); err != nil {
		removeFile(tmpPath)
		return 0, err
	}
	return size, nil
}

// errNotFound marks a post the actor reports as missing, private or removed:
// the run worked, the post is gone. Callers use it to stop retrying.
var errNotFound = errors.New("post not found")

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// urlInErrorText matches an absolute URL embedded in an error message. Go
// formats transport errors as: Get "https://host/path?sig=…": dial tcp …
var urlInErrorText = regexp.MustCompile(`https?://[^\s"]+`)

// sanitizeTransportError redacts the signed media URL that net/http embeds in
// every transport-layer error. A DNS failure, a reset connection or a timeout
// produces a *url.Error whose message carries the full URL, query string
// included, and that message reaches the persisted archive log and the usage
// row's Detail — exactly the places the sanitizer exists to keep it out of.
func sanitizeTransportError(err error) error {
	if err == nil {
		return nil
	}
	message := err.Error()
	sanitized := urlInErrorText.ReplaceAllStringFunc(message, func(rawURL string) string {
		trimmed := strings.TrimRight(rawURL, `:,.;`)
		return archivers.SanitizeURL(trimmed, nil) + rawURL[len(trimmed):]
	})
	if sanitized == message {
		return err
	}
	return &sanitizedError{message: sanitized, cause: err}
}

// sanitizedError presents a redacted message while preserving the error chain.
type sanitizedError struct {
	message string
	cause   error
}

func (e *sanitizedError) Error() string { return e.message }
func (e *sanitizedError) Unwrap() error { return e.cause }
