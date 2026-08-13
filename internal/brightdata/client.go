// Package brightdata implements the paid fallback path for Instagram and
// YouTube media archiving.
//
// The native flows (yt-dlp, gallery-dl) run first and are preferred: they are
// free, fetch original-quality media, and work most of the time. But they fail
// in ways Arker cannot fix from its own network position — Instagram login
// walls and account throttles, YouTube geo-blocks and bot checks. For those
// cases this package buys the bytes through Bright Data instead:
//
//   - Instagram: the Web Scraper API (dataset trigger → poll → snapshot)
//     returns post metadata plus CDN media URLs, which download fine from
//     Arker's own IP because Instagram signs but does not IP-lock them.
//   - YouTube: the scraper's video URLs ARE IP-locked (Bright Data scrambles
//     the signed ip= parameter), so media must be fetched from the same
//     network position that resolved it. That is done inside a Bright Data
//     Browser API session: a remote Chrome on their unblocked network where
//     an in-page Innertube call resolves a progressive MP4 URL and in-page
//     ranged fetches pull the bytes out over CDP.
//
// Every operation writes a BrightDataUsage row so spend is visible in the
// admin API. Costs are estimates from configured rates: the scoped API key
// cannot read Bright Data's billing endpoints.
package brightdata

import (
	"bytes"
	"context"
	"encoding/json"
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

const apiBase = "https://api.brightdata.com"

// Dataset IDs for Bright Data's marketplace scrapers. These are global,
// stable identifiers published in Bright Data's docs, not account-specific.
const (
	DatasetInstagramReels = "gd_lyclm20il4r5helnj"
	DatasetInstagramPosts = "gd_lk5ns7kz21pck8jpis"
	DatasetTikTokPosts    = "gd_lu702nij2f790tmv9h"
	DatasetRedditPosts    = "gd_lvz8ah06191smkebj4"
	DatasetXPosts         = "gd_lwxkxvnf1cynvib9co"
)

// Config carries everything the fallback needs. Only APIKey is required:
// CustomerID and BrowserZonePassword are resolved from the API at startup when
// empty, and the cost rates default to Bright Data's published pay-as-you-go
// prices.
type Config struct {
	APIKey     string
	CustomerID string
	// BrowserZone is the Browser API zone name used for YouTube sessions.
	BrowserZone         string
	BrowserZonePassword string
	// ScraperCostPerRecord estimates Web Scraper API spend (USD per record).
	ScraperCostPerRecord float64
	// BrowserCostPerGB estimates Browser API spend (USD per GB transferred).
	BrowserCostPerGB float64
	// YouTubeClientName/Version identify the Innertube client the YouTube
	// media resolution impersonates. This is the only YouTube-versioned knob
	// in the fallback; when YouTube retires the version, updating the env var
	// fixes it without a code change.
	YouTubeClientName    string
	YouTubeClientVersion string
}

// Client is the shared Bright Data client. It is safe for concurrent use by
// multiple archive workers: it holds no per-job state.
type Client struct {
	cfg  Config
	http *http.Client
	// openBrowser opens one page in a Bright Data Browser API session. It is a
	// field rather than a plain method so tests can drive the whole in-page
	// fetch path — chunk loop, retries, EOF handling — against a fake page
	// without a remote browser or a live signed URL. Nil means the real
	// Browser API (see openBrowserSession).
	openBrowser func(ctx context.Context, country, pageURL string, logWriter io.Writer) (browserSession, error)

	// policyBlocked remembers hosts Bright Data's compliance layer refused this
	// process lifetime (account-level KYC gates, e.g. tiktok.com). River retries
	// a failed job three times; without this memory every retry would buy a new
	// browser session for a refusal that cannot change until the ACCOUNT does.
	// Cleared only by restart, which is also when a completed KYC would take
	// effect.
	policyBlockedMu sync.Mutex
	policyBlocked   map[string]bool
}

// markPolicyBlocked records a compliance refusal for a host.
func (c *Client) markPolicyBlocked(host string) {
	c.policyBlockedMu.Lock()
	defer c.policyBlockedMu.Unlock()
	if c.policyBlocked == nil {
		c.policyBlocked = make(map[string]bool)
	}
	c.policyBlocked[host] = true
}

// isPolicyBlocked reports whether a host was refused by the compliance layer
// earlier in this process's lifetime.
func (c *Client) isPolicyBlocked(host string) bool {
	c.policyBlockedMu.Lock()
	defer c.policyBlockedMu.Unlock()
	return c.policyBlocked[host]
}

// New builds a client and resolves the missing credentials it can. It never
// fails hard: a client without browser credentials still serves the Instagram
// dataset flow, and disabling the whole fallback over a transient startup error
// would silently reintroduce the failures the fallback exists to catch.
func New(ctx context.Context, cfg Config) *Client {
	if cfg.ScraperCostPerRecord <= 0 {
		cfg.ScraperCostPerRecord = 0.0015 // $1.50 per 1000 records, PAYG
	}
	if cfg.BrowserCostPerGB <= 0 {
		cfg.BrowserCostPerGB = 8.40 // $8.40 per GB, PAYG
	}
	if cfg.YouTubeClientName == "" {
		cfg.YouTubeClientName = "ANDROID"
	}
	if cfg.YouTubeClientVersion == "" {
		cfg.YouTubeClientVersion = "20.10.38"
	}

	c := &Client{
		cfg: cfg,
		// Generous timeout: snapshot polling and media downloads set their own
		// deadlines via request contexts; this is only the hard backstop.
		http: &http.Client{Timeout: 5 * time.Minute},
	}

	if cfg.APIKey == "" {
		return c
	}

	if c.cfg.CustomerID == "" {
		if id, err := c.fetchCustomerID(ctx); err == nil {
			c.cfg.CustomerID = id
			slog.Info("Bright Data customer ID resolved", "customer_id", id)
		} else {
			slog.Warn("Could not resolve Bright Data customer ID; YouTube browser fallback disabled", "error", err)
		}
	}
	if c.cfg.BrowserZone != "" && c.cfg.BrowserZonePassword == "" {
		if pw, err := c.fetchZonePassword(ctx, c.cfg.BrowserZone); err == nil {
			c.cfg.BrowserZonePassword = pw
			slog.Info("Bright Data browser zone password resolved", "zone", c.cfg.BrowserZone)
		} else {
			slog.Warn("Could not resolve Bright Data browser zone password; YouTube browser fallback disabled",
				"zone", c.cfg.BrowserZone, "error", err)
		}
	}

	return c
}

// Enabled reports whether the fallback can run at all.
func (c *Client) Enabled() bool {
	return c != nil && c.cfg.APIKey != ""
}

// BrowserReady reports whether the YouTube browser flow has the credentials it
// needs (Instagram needs only the API key).
func (c *Client) BrowserReady() bool {
	return c.Enabled() && c.cfg.CustomerID != "" && c.cfg.BrowserZone != "" && c.cfg.BrowserZonePassword != ""
}

// browserWSEndpoint builds the CDP connect URL for a Browser API session.
// A non-empty country pins the session's exit geography (the zone has the
// "country" permission); empty lets Bright Data pick any peer.
func (c *Client) browserWSEndpoint(country string) string {
	username := fmt.Sprintf("brd-customer-%s-zone-%s", c.cfg.CustomerID, c.cfg.BrowserZone)
	if country != "" {
		username += "-country-" + country
	}
	return fmt.Sprintf("wss://%s:%s@brd.superproxy.io:9222",
		username, url.QueryEscape(c.cfg.BrowserZonePassword))
}

func (c *Client) fetchCustomerID(ctx context.Context) (string, error) {
	var out struct {
		Customer string `json:"customer"`
	}
	if err := c.getJSON(ctx, apiBase+"/status", &out); err != nil {
		return "", err
	}
	if out.Customer == "" {
		return "", fmt.Errorf("Bright Data /status returned no customer")
	}
	return out.Customer, nil
}

func (c *Client) fetchZonePassword(ctx context.Context, zone string) (string, error) {
	var out struct {
		Passwords []string `json:"passwords"`
	}
	if err := c.getJSON(ctx, apiBase+"/zone/passwords?zone="+url.QueryEscape(zone), &out); err != nil {
		return "", err
	}
	if len(out.Passwords) == 0 {
		return "", fmt.Errorf("zone %s has no passwords", zone)
	}
	return out.Passwords[0], nil
}

// triggerDataset starts a Web Scraper API collection for one URL and returns
// the snapshot ID.
func (c *Client) triggerDataset(ctx context.Context, datasetID, targetURL string) (string, error) {
	payload, err := json.Marshal([]map[string]string{{"url": targetURL}})
	if err != nil {
		return "", err
	}

	endpoint := fmt.Sprintf("%s/datasets/v3/trigger?dataset_id=%s&include_errors=true", apiBase, url.QueryEscape(datasetID))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("dataset trigger returned %d: %s", resp.StatusCode, truncate(string(body), 300))
	}

	var out struct {
		SnapshotID string `json:"snapshot_id"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.SnapshotID == "" {
		return "", fmt.Errorf("dataset trigger returned no snapshot_id: %s", truncate(string(body), 300))
	}
	return out.SnapshotID, nil
}

// snapshotProgress is the /datasets/v3/progress response.
type snapshotProgress struct {
	Status  string `json:"status"`
	Records int    `json:"records"`
	Errors  int    `json:"errors"`
}

// waitForSnapshot polls until the snapshot is ready or the context expires.
// Collection is typically 20s–2min; the caller's context bounds the total wait.
func (c *Client) waitForSnapshot(ctx context.Context, snapshotID string, logWriter io.Writer) (*snapshotProgress, error) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		var progress snapshotProgress
		if err := c.getJSON(ctx, apiBase+"/datasets/v3/progress/"+url.PathEscape(snapshotID), &progress); err != nil {
			// Transient polling errors should not abandon a running (already
			// paid-for) collection; the context decides when to give up.
			fmt.Fprintf(logWriter, "Bright Data progress poll error (will retry): %v\n", err)
		} else {
			switch progress.Status {
			case "ready":
				return &progress, nil
			case "failed":
				return &progress, fmt.Errorf("Bright Data snapshot %s failed", snapshotID)
			default:
				fmt.Fprintf(logWriter, "Bright Data snapshot %s: %s\n", snapshotID, progress.Status)
			}
		}

		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("timed out waiting for Bright Data snapshot %s: %w", snapshotID, ctx.Err())
		case <-ticker.C:
		}
	}
}

// fetchSnapshotRecords downloads a ready snapshot as parsed JSON records.
func (c *Client) fetchSnapshotRecords(ctx context.Context, snapshotID string) ([]map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		apiBase+"/datasets/v3/snapshot/"+url.PathEscape(snapshotID)+"?format=json", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("snapshot download returned %d: %s", resp.StatusCode, truncate(string(body), 300))
	}

	var records []map[string]any
	if err := json.Unmarshal(body, &records); err != nil {
		return nil, fmt.Errorf("could not parse snapshot JSON: %w", err)
	}
	return records, nil
}

// runDataset triggers a collection, waits for it, records usage, and returns
// the records. The usage row is written even on failure: a triggered
// collection may be billable whether or not we managed to collect the result.
func (c *Client) runDataset(ctx context.Context, db *gorm.DB, usage *models.BrightDataUsage, datasetID, targetURL string, logWriter io.Writer) ([]map[string]any, error) {
	usage.Product = "web_scraper"
	usage.DatasetID = datasetID
	usage.URL = targetURL

	snapshotID, err := c.triggerDataset(ctx, datasetID, targetURL)
	if err != nil {
		usage.Detail = truncate("trigger failed: "+err.Error(), 500)
		c.recordUsage(db, usage)
		return nil, err
	}
	usage.SnapshotID = snapshotID
	fmt.Fprintf(logWriter, "Bright Data dataset %s triggered: snapshot %s\n", datasetID, snapshotID)

	progress, err := c.waitForSnapshot(ctx, snapshotID, logWriter)
	if progress != nil {
		usage.Records = progress.Records
		usage.CostUSD = float64(progress.Records) * c.cfg.ScraperCostPerRecord
	}
	if err != nil {
		usage.Detail = truncate("wait failed: "+err.Error(), 500)
		c.recordUsage(db, usage)
		return nil, err
	}

	records, err := c.fetchSnapshotRecords(ctx, snapshotID)
	if err != nil {
		usage.Detail = truncate("snapshot fetch failed: "+err.Error(), 500)
		c.recordUsage(db, usage)
		return nil, err
	}
	if usage.Records == 0 {
		usage.Records = len(records)
		usage.CostUSD = float64(len(records)) * c.cfg.ScraperCostPerRecord
	}

	// Success/Detail are finalized by the caller once it knows whether the
	// records were actually usable; record now so even a caller crash leaves a
	// spend row behind.
	c.recordUsage(db, usage)
	return records, nil
}

// recordUsage persists a usage row, creating or updating it in place. Usage
// tracking must never fail an archive: errors are logged and swallowed.
func (c *Client) recordUsage(db *gorm.DB, usage *models.BrightDataUsage) {
	if db == nil {
		return
	}
	if err := db.Save(usage).Error; err != nil {
		slog.Error("Failed to record Bright Data usage", "error", err, "url", usage.URL)
	}
}

func (c *Client) getJSON(ctx context.Context, endpoint string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s returned %d: %s", endpoint, resp.StatusCode, truncate(string(body), 200))
	}
	return json.Unmarshal(body, out)
}

// downloadToTemp streams a URL to a temp file and returns the path and size.
// Used for CDN media (Instagram, Reddit, X) that is fetched from Arker's own
// network.
func (c *Client) downloadToTemp(ctx context.Context, mediaURL, pattern string) (string, int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, mediaURL, nil)
	if err != nil {
		return "", 0, sanitizeTransportError(err)
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
		return "", 0, fmt.Errorf("media download failed after %d bytes: %w", n, copyErr)
	}
	if n == 0 {
		removeFile(f.Name())
		return "", 0, fmt.Errorf("media download returned an empty body")
	}
	return f.Name(), n, nil
}

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
// every transport-layer error.
//
// A DNS failure, a reset connection or a timeout produces a *url.Error whose
// message carries the full URL, query string included — net/http strips
// userinfo passwords and nothing else. That message reaches the persisted
// archive log, which applies no redaction of its own, and on the Instagram
// video path it also reaches BrightDataUsage.Detail. So a live signed CDN URL
// ends up stored in exactly the places the sanitizer exists to keep it out of,
// via the one path that never goes through SanitizeJSON.
//
// Only the transport layer is affected: an HTTP status failure carries no URL.
func sanitizeTransportError(err error) error {
	if err == nil {
		return nil
	}
	message := err.Error()
	sanitized := urlInErrorText.ReplaceAllStringFunc(message, func(rawURL string) string {
		// Trailing punctuation is part of the message, not the URL.
		trimmed := strings.TrimRight(rawURL, `:,.;`)
		return archivers.SanitizeURL(trimmed, nil) + rawURL[len(trimmed):]
	})
	if sanitized == message {
		return err
	}
	// Wrapped rather than replaced so errors.Is/As still see the cause.
	return &sanitizedError{message: sanitized, cause: err}
}

// sanitizedError presents a redacted message while preserving the error chain.
type sanitizedError struct {
	message string
	cause   error
}

func (e *sanitizedError) Error() string { return e.message }
func (e *sanitizedError) Unwrap() error { return e.cause }
