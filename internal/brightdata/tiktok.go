package brightdata

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"gorm.io/gorm"

	"arker/internal/archivers"
	"arker/internal/models"
	"arker/internal/utils"
)

// archiveTikTok is the TikTok fallback entry point.
//
// TikTok is the one platform here whose media bytes cannot be bought with the
// dataset alone. The Posts dataset resolves the post and hands back video_url
// and cdn_link, but both are signed for Bright Data's own resolver and answer
// Arker's IP with 403 (verified) — the same shape as YouTube's googlevideo
// URLs. So the video is fetched inside a Bright Data Browser API session, with
// the in-page ranged fetch machinery in browser_fetch.go. The dataset and
// Browser API use separate workers, though: when the dataset URL is signed to
// its worker and answers the browser with 403, the browser refreshes the media
// URL from the already-loaded post page and consumes it in that same session.
//
// preview_image, by contrast, downloads fine from Arker's own connection, so
// the poster costs nothing extra.
func (c *Client) archiveTikTok(ctx context.Context, targetURL, itemType string, logWriter io.Writer, db *gorm.DB, itemID uint, shortID string) (archivers.Result, error) {
	switch {
	case utils.ArchiveTypesEqual(itemType, utils.ArchiveTypeYtDlp):
		return c.tiktokVideo(ctx, targetURL, logWriter, db, itemID, shortID)
	case utils.ArchiveTypesEqual(itemType, utils.ArchiveTypeGalleryDl):
		return c.tiktokPhotos(ctx, targetURL, logWriter, db, itemID, shortID)
	default:
		return archivers.Result{}, fmt.Errorf("no TikTok fallback for archive type %s", itemType)
	}
}

// tiktokVideo produces the MP4 for a TikTok video post.
func (c *Client) tiktokVideo(ctx context.Context, targetURL string, logWriter io.Writer, db *gorm.DB, itemID uint, shortID string) (archivers.Result, error) {
	scrapeUsage := &models.BrightDataUsage{ArchiveItemID: itemID, ShortID: shortID}
	record, err := c.resolveRecord(ctx, db, scrapeUsage, DatasetTikTokPosts, targetURL, logWriter)
	if err != nil {
		return archivers.Result{}, err
	}

	candidates := tiktokVideoURLs(record)
	if len(candidates) == 0 {
		err := fmt.Errorf("Bright Data record for %s has no video URL (post_type %q)",
			targetURL, stringField(record, "post_type"))
		scrapeUsage.Detail = truncate(err.Error(), 500)
		c.recordUsage(db, scrapeUsage)
		return archivers.Result{}, err
	}
	scrapeUsage.Success = true
	scrapeUsage.Detail = fmt.Sprintf("resolved %d candidate video URL(s)", len(candidates))
	c.recordUsage(db, scrapeUsage)

	logTikTokMetadata(logWriter, record)
	fmt.Fprintf(logWriter, "TikTok video URLs are IP-locked to Bright Data's resolver; fetching bytes inside a browser session...\n")

	browserUsage := &models.BrightDataUsage{
		ArchiveItemID: itemID,
		ShortID:       shortID,
		URL:           targetURL,
		Product:       "browser_api",
	}
	fetch, fetchErr := c.fetchThroughBrowser(ctx, browserFetchRequest{
		PageURL:     tiktokPageURL(record, targetURL),
		MediaURLs:   candidates,
		Countries:   tiktokSessionCountries(record),
		TempPattern: "arker-bd-tt-*.mp4",
		LogWriter:   logWriter,
		Retryable:   tiktokRetryableElsewhere,
		// Dataset URLs are resolved by a different Bright Data worker and can be
		// signed to that worker's IP. If TikTok refuses them, observe the URL the
		// post page loaded for this exact browser session and fetch it without
		// closing the session in between.
		RefreshMediaURLs: refreshTikTokMediaURLs,
	})
	finishBrowserUsage := func(success bool, detail string) {
		browserUsage.BytesTransferred, browserUsage.CostUSD = c.browserSessionCost(fetch.Size, fetch.Sessions)
		browserUsage.Success = success
		browserUsage.Detail = truncate(detail, 500)
		c.recordUsage(db, browserUsage)
	}
	if fetchErr != nil {
		finishBrowserUsage(false, fetchErr.Error())
		return archivers.Result{}, fetchErr
	}
	if err := verifyMP4(fetch.Path); err != nil {
		removeFile(fetch.Path)
		finishBrowserUsage(false, err.Error())
		return archivers.Result{}, err
	}
	fmt.Fprintf(logWriter, "Downloaded %d bytes through Bright Data browser session (%d billable)\n",
		fetch.StoredBytes, fetch.Size)

	// The stored size, not the billed one: a retried or partially-transferred
	// attempt inflates what the session cost without changing what the archive
	// holds, and the metadata has to describe the file.
	metadata, rawMetadata, err := buildBrightDataTikTokVideoArtifacts(record, targetURL, fetch.StoredBytes, time.Now())
	if err != nil {
		removeFile(fetch.Path)
		finishBrowserUsage(false, "metadata build failed: "+err.Error())
		return archivers.Result{}, fmt.Errorf("failed to build Bright Data video metadata: %w", err)
	}

	// The poster is a plain CDN image and downloads over Arker's own
	// connection, so it never touches the paid session.
	thumb := c.thumbnailFromURL(ctx, stringField(record, "preview_image"), logWriter)

	reader, err := openTempFileReader(fetch.Path)
	if err != nil {
		removeFile(fetch.Path)
		finishBrowserUsage(false, err.Error())
		return archivers.Result{}, err
	}
	finishBrowserUsage(true, fmt.Sprintf("video %d bytes stored, %d transferred", fetch.StoredBytes, fetch.Size))
	return archivers.Result{
		Data:        reader,
		Extension:   ".mp4",
		ContentType: "video/mp4",
		Thumbnail:   thumb,
		Source:      models.ArchiveSourceBrightData,
		Metadata:    metadata,
		RawMetadata: rawMetadata,
		// One TikTok video post is one video: the MP4 and both sidecars are
		// stored, so there is no second asset this could have missed.
		Completeness: archivers.CompletenessComplete,
	}, nil
}

// tiktokPhotos produces a gallery ZIP for a TikTok photo post (a slideshow of
// stills).
//
// The still URLs are ordinary signed CDN images, which is why each one is tried
// over Arker's own connection first; only the ones TikTok refuses fall back to
// a browser session, and that session is opened once and reused for the rest.
//
// The photo-post record shape is the one part of this platform that has not
// been verified against a live record, so the image list is read from several
// plausible fields rather than one assumed one, and a post that yields nothing
// fails explicitly instead of storing an empty bundle.
func (c *Client) tiktokPhotos(ctx context.Context, targetURL string, logWriter io.Writer, db *gorm.DB, itemID uint, shortID string) (archivers.Result, error) {
	scrapeUsage := &models.BrightDataUsage{ArchiveItemID: itemID, ShortID: shortID}
	record, err := c.resolveRecord(ctx, db, scrapeUsage, DatasetTikTokPosts, targetURL, logWriter)
	if err != nil {
		return archivers.Result{}, err
	}

	entries := tiktokImageEntries(record)
	if len(entries) == 0 {
		err := fmt.Errorf("Bright Data record for %s contains no images (post_type %q)",
			targetURL, stringField(record, "post_type"))
		scrapeUsage.Detail = truncate(err.Error(), 500)
		c.recordUsage(db, scrapeUsage)
		return archivers.Result{}, err
	}

	logTikTokMetadata(logWriter, record)

	fetcher := &tiktokMediaFetcher{
		client:    c,
		pageURL:   tiktokPageURL(record, targetURL),
		countries: tiktokSessionCountries(record),
		logWriter: logWriter,
	}
	defer fetcher.close()

	meta := tiktokGalleryMetadata(record, targetURL)
	result, completeness, totalBytes, err := c.buildGalleryArchive(ctx, entries, tiktokAudio(record), meta, record, fetcher.fetch, logWriter)

	if fetcher.sessions > 0 {
		// The session is a success only if it actually delivered media that was
		// archived. A session that opened and was refused everything still
		// spent its page load, which is what BytesTransferred records.
		browserUsage := &models.BrightDataUsage{
			ArchiveItemID: itemID,
			ShortID:       shortID,
			URL:           targetURL,
			Product:       "browser_api",
			Success:       err == nil && fetcher.browserFiles > 0,
			Detail:        truncate(fmt.Sprintf("browser fallback delivered %d image(s)", fetcher.browserFiles), 500),
		}
		browserUsage.BytesTransferred, browserUsage.CostUSD = c.browserSessionCost(fetcher.bytes, fetcher.sessions)
		c.recordUsage(db, browserUsage)
	}
	if err != nil {
		scrapeUsage.Detail = truncate(err.Error(), 500)
		c.recordUsage(db, scrapeUsage)
		return archivers.Result{}, err
	}

	scrapeUsage.Success = true
	scrapeUsage.Detail = fmt.Sprintf("gallery %d/%d file(s), %d bytes (%s)", len(meta.Files), len(entries), totalBytes, completeness.State)
	c.recordUsage(db, scrapeUsage)
	return result, nil
}

// tiktokMediaFetcher downloads TikTok stills over Arker's own connection and
// falls back to one shared Bright Data browser session for the ones TikTok
// refuses. The session is opened on first need and reused: a session per image
// would multiply the page-load overhead by the size of the slideshow.
type tiktokMediaFetcher struct {
	client    *Client
	pageURL   string
	countries []string
	logWriter io.Writer

	session      browserSession
	sessionErr   error
	sessions     int
	browserFiles int
	bytes        int64
}

func (f *tiktokMediaFetcher) fetch(ctx context.Context, entry mediaEntry, dest string) (int64, error) {
	size, directErr := f.client.downloadToPath(ctx, entry.URL, dest)
	if directErr == nil {
		return size, nil
	}
	if ctx.Err() != nil {
		return 0, directErr
	}

	page, err := f.browserPage(ctx)
	if err != nil {
		return 0, fmt.Errorf("direct download failed (%v) and no browser session is available: %w", directErr, err)
	}
	fmt.Fprintf(f.logWriter, "Direct download refused (%v); retrying through the browser session...\n", directErr)

	tmpPath, size, err := fetchURLThroughPage(ctx, page, entry.URL, 0, "arker-bd-tt-img-*", f.logWriter)
	f.bytes += size
	if err != nil {
		return 0, fmt.Errorf("direct download failed (%v); browser fetch failed: %w", directErr, err)
	}
	if err := renameTempFile(tmpPath, dest); err != nil {
		return 0, err
	}
	f.browserFiles++
	return size, nil
}

// browserPage opens the shared session on first use. A session that failed to
// open once is not retried per file: the failure is a credential or pool
// problem, not something the next image will resolve differently.
func (f *tiktokMediaFetcher) browserPage(ctx context.Context) (pageEvaluator, error) {
	if f.session != nil {
		return f.session, nil
	}
	if f.sessionErr != nil {
		return nil, f.sessionErr
	}
	country := ""
	if len(f.countries) > 0 {
		country = f.countries[0]
	}
	session, err := f.client.openBrowserSession(ctx, country, f.pageURL, f.logWriter)
	if err != nil {
		// A session that never connected is not billable; one that connected
		// and then failed to navigate loaded a page and is.
		if sessionConnected(err) {
			f.sessions++
		}
		f.sessionErr = err
		return nil, err
	}
	f.sessions++
	f.session = session
	return session, nil
}

func (f *tiktokMediaFetcher) close() {
	if f.session != nil {
		f.session.Close()
		f.session = nil
	}
}

// tiktokVideoURLs lists the signed video URLs the record offers, best first.
// The record carries two independently signed URLs for the same video, so a
// session that is refused one can try the other before the session is thrown
// away.
func tiktokVideoURLs(record map[string]any) []string {
	var urls []string
	seen := map[string]bool{}
	for _, key := range []string{"video_url", "cdn_link", "download_link", "play_url"} {
		u := stringField(record, key)
		if u == "" || seen[u] {
			continue
		}
		seen[u] = true
		urls = append(urls, u)
	}
	return urls
}

// resolveTikTokMediaURLsJS observes the video URL selected by the TikTok post
// page. The browser page and the subsequent ranged fetch share one Bright Data
// session and therefore one network position, unlike URLs returned by the
// separate Web Scraper dataset worker.
//
// TikTok sometimes exposes the URL directly on <video>, and sometimes only in
// a resource timing entry or the page's hydration JSON. The bounded poll gives
// its client-side app time to attach the player without leaving a paid browser
// session waiting indefinitely.
const resolveTikTokMediaURLsJS = `
async () => {
  const found = [];
  const seen = new Set();
  const looksLikeVideo = (raw) => {
    if (typeof raw !== 'string' || !/^https?:\/\//i.test(raw)) return false;
    try {
      const u = new URL(raw);
      const host = u.hostname.toLowerCase();
      const mediaHost = host.includes('tiktokcdn') ||
        host.endsWith('.byteoversea.com') || host.endsWith('.ibytedtos.com') ||
        host.endsWith('.muscdn.com') || host.endsWith('.akamaized.net') ||
        ((host.endsWith('.tiktok.com') || host.endsWith('.tiktokv.com')) &&
          (/^v\d/.test(host) || host.startsWith('video-')));
      if (!mediaHost) return false;
      const videoNamedHost = /^v\d/.test(host) || host.startsWith('video-');
      return u.searchParams.get('mime_type') === 'video_mp4' ||
        u.pathname.includes('/video/') ||
        (videoNamedHost && /\.mp4$/i.test(u.pathname));
    } catch (_) {
      return false;
    }
  };
  const add = (raw) => {
    if (looksLikeVideo(raw) && !seen.has(raw)) {
      seen.add(raw);
      found.push(raw);
    }
  };
  let hydrationScanned = false;
  const collect = () => {
    for (const video of document.querySelectorAll('video')) {
      add(video.currentSrc);
      add(video.src);
      for (const source of video.querySelectorAll('source')) add(source.src);
    }
    for (const entry of performance.getEntriesByType('resource')) add(entry.name);

    if (!hydrationScanned) {
      hydrationScanned = true;
      for (const script of document.querySelectorAll(
        'script#__UNIVERSAL_DATA_FOR_REHYDRATION__, script#SIGI_STATE, script[type="application/json"]')) {
        const text = script.textContent || '';
        if (!text || text.length > 8 * 1024 * 1024) continue;
        try {
          const stack = [JSON.parse(text)];
          let visited = 0;
          while (stack.length && visited++ < 50000) {
            const value = stack.pop();
            if (typeof value === 'string') {
              add(value);
            } else if (Array.isArray(value)) {
              for (const child of value) stack.push(child);
            } else if (value && typeof value === 'object') {
              for (const child of Object.values(value)) stack.push(child);
            }
          }
        } catch (_) {}
      }
    }
  };

  for (let attempt = 0; attempt < 20; attempt++) {
    collect();
    if (found.length) return JSON.stringify(found);
    await new Promise((resolve) => setTimeout(resolve, 500));
  }
  return JSON.stringify(found);
}
`

func refreshTikTokMediaURLs(page pageEvaluator) ([]string, error) {
	raw, err := page.Evaluate(resolveTikTokMediaURLsJS)
	if err != nil {
		return nil, fmt.Errorf("evaluate TikTok post media: %w", err)
	}
	text, ok := raw.(string)
	if !ok {
		return nil, fmt.Errorf("TikTok media refresh returned %T", raw)
	}
	var candidates []string
	if err := json.Unmarshal([]byte(text), &candidates); err != nil {
		return nil, fmt.Errorf("parse TikTok media refresh: %w", err)
	}
	return candidates, nil
}

// tiktokImageEntries resolves a photo post's stills.
//
// The live photo-post record shape is unverified, so every field the dataset
// plausibly uses is read, in both the flattened form Bright Data often
// produces (carousel_images: [url, ...]) and the nested form TikTok's own item
// struct uses (image_post_info.images[].display_image.url_list[]). Ordering
// follows the field list, which keeps the slideshow order stable.
func tiktokImageEntries(record map[string]any) []mediaEntry {
	var entries []mediaEntry
	seen := map[string]bool{}
	for _, key := range []string{"carousel_images", "images", "image_urls", "photos", "image_post_info"} {
		for _, still := range tiktokImageList(record[key]) {
			u := firstImageURL(still, 4)
			if u == "" || seen[u] {
				continue
			}
			seen[u] = true
			entries = append(entries, mediaEntry{URL: u, Type: "Photo"})
		}
	}
	return entries
}

// tiktokImageList normalizes a field that is either the list of stills itself
// or an object wrapping one.
func tiktokImageList(value any) []any {
	switch typed := value.(type) {
	case []any:
		return typed
	case map[string]any:
		for _, key := range []string{"images", "image_list", "url_list"} {
			if list, ok := typed[key].([]any); ok {
				return list
			}
		}
		return []any{typed}
	case string:
		return []any{typed}
	}
	return nil
}

// firstImageURL picks the one URL that represents a single still.
//
// It is deliberately first-match rather than collect-all: url_list is a list of
// CDN mirrors for the SAME image, so collecting every entry would turn a
// three-slide post into a nine-slide one and make the completeness count
// meaningless. Depth is bounded because a provider record is untrusted input.
func firstImageURL(value any, depth int) string {
	if depth <= 0 {
		return ""
	}
	switch typed := value.(type) {
	case string:
		trimmed := strings.TrimSpace(typed)
		if strings.HasPrefix(trimmed, "http://") || strings.HasPrefix(trimmed, "https://") {
			return trimmed
		}
	case []any:
		for _, child := range typed {
			if u := firstImageURL(child, depth-1); u != "" {
				return u
			}
		}
	case map[string]any:
		for _, key := range []string{"url", "url_list", "image_url", "display_image", "display_url", "thumbnail", "origin_cover"} {
			if u := firstImageURL(typed[key], depth-1); u != "" {
				return u
			}
		}
	}
	return ""
}

// tiktokPageURL is the page a browser session opens before fetching. An in-page
// fetch runs under that page's origin, so it has to be the post's own page for
// TikTok's CDN to answer it.
func tiktokPageURL(record map[string]any, targetURL string) string {
	return firstNonEmptyString(stringField(record, "url"), targetURL)
}

// tiktokSessionCountries orders the session geographies to try. The record's
// own region goes first: the media URL was signed by a resolver in that region
// and its CDN host is region-scoped, so a session there is the closest match
// available. An unpinned session follows as the fallback.
func tiktokSessionCountries(record map[string]any) []string {
	region := strings.ToLower(strings.TrimSpace(stringField(record, "region")))
	if region == "" || len(region) != 2 {
		return []string{""}
	}
	return []string{region, ""}
}

// tiktokRetryableElsewhere classifies failures a fresh session somewhere else
// could fix: Bright Data pool errors, and the refusals a signed URL gives a
// session it was not signed for.
func tiktokRetryableElsewhere(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, fragment := range []string{"no_peers", "no peer found", "status 403", "status 401", "fetch failed"} {
		if strings.Contains(msg, fragment) {
			return true
		}
	}
	return false
}

func tiktokGalleryMetadata(record map[string]any, sourceURL string) *archivers.GalleryMetadata {
	meta := &archivers.GalleryMetadata{
		SourceURL:   sourceURL,
		Extractor:   "tiktok",
		Subcategory: "brightdata",
		PostID:      stringField(record, "post_id", "shortcode"),
		PostURL:     stringField(record, "url"),
		Author:      stringField(record, "account_id", "profile_username"),
		AuthorName:  stringField(record, "profile_username"),
		Description: stringField(record, "description"),
		Date:        stringField(record, "create_time"),
		Tags:        stringSlice(record, "hashtags"),
		ToolVersion: "brightdata-web-scraper",
		ArchivedAt:  time.Now().UTC().Format(time.RFC3339),
	}
	if likes := intField(record, "digg_count"); likes != nil {
		meta.Likes = likes
	}
	return meta
}

func buildBrightDataTikTokVideoArtifacts(record map[string]any, sourceURL string, size int64, archivedAt time.Time) (*archivers.Sidecar, *archivers.Sidecar, error) {
	raw, err := json.Marshal(record)
	if err != nil {
		return nil, nil, fmt.Errorf("encode Bright Data TikTok record: %w", err)
	}
	sanitized, err := archivers.SanitizeJSON(raw, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("sanitize Bright Data TikTok record: %w", err)
	}

	var duration *float64
	if seconds := intField(record, "video_duration"); seconds != nil {
		value := float64(*seconds)
		duration = &value
	}

	metadataJSON, err := archivers.MarshalVideoMetadata(&archivers.VideoMetadata{
		SchemaVersion:        archivers.VideoMetadataSchemaVersion,
		SourceURL:            archivers.SanitizeURL(sourceURL, nil),
		Platform:             "tiktok",
		Extractor:            "tiktok",
		PostID:               stringField(record, "post_id", "shortcode"),
		CanonicalURL:         archivers.SanitizeURL(firstNonEmptyString(stringField(record, "url"), sourceURL), nil),
		Description:          stringField(record, "description"),
		Author:               stringField(record, "profile_username", "account_id"),
		AuthorID:             stringField(record, "profile_id"),
		Uploader:             stringField(record, "account_id", "profile_username"),
		UploaderID:           stringField(record, "profile_id"),
		PublicationTimestamp: normalizeProviderDate(stringField(record, "create_time")),
		DurationSeconds:      duration,
		Engagement: archivers.VideoEngagement{
			Views:    intField(record, "play_count"),
			Likes:    intField(record, "digg_count"),
			Comments: intField(record, "comment_count"),
			Reposts:  intField(record, "share_count", "num_share_count"),
		},
		Tags:  stringSlice(record, "hashtags"),
		Music: tiktokVideoMusic(record),
		Media: archivers.VideoMedia{
			Extension:    ".mp4",
			ContentType:  "video/mp4",
			SizeBytes:    size,
			Width:        intField(record, "width"),
			Height:       intField(record, "height"),
			QualityLabel: stringField(record, "ratio"),
		},
		ArchivedAt: archivedAt.UTC().Format(time.RFC3339),
		Provenance: models.ArchiveSourceBrightData,
		// The record comes from the Web Scraper API; only the bytes came
		// through the Browser API, which the usage rows record separately.
		Provider: "brightdata_web_scraper",
	})
	if err != nil {
		return nil, nil, err
	}
	return &archivers.Sidecar{Data: metadataJSON}, &archivers.Sidecar{Data: sanitized}, nil
}

func logTikTokMetadata(logWriter io.Writer, record map[string]any) {
	if author := stringField(record, "profile_username", "account_id"); author != "" {
		fmt.Fprintf(logWriter, "Author: %s\n", author)
	}
	if desc := stringField(record, "description"); desc != "" {
		fmt.Fprintf(logWriter, "Caption: %s\n", utils.TruncateForLog(desc, 300))
	}
	if date := stringField(record, "create_time"); date != "" {
		fmt.Fprintf(logWriter, "Posted: %s\n", date)
	}
	for _, counter := range []struct{ label, key string }{
		{"Plays", "play_count"}, {"Likes", "digg_count"}, {"Comments", "comment_count"}, {"Shares", "share_count"},
	} {
		if value := intField(record, counter.key); value != nil {
			fmt.Fprintf(logWriter, "%s: %d\n", counter.label, *value)
		}
	}
}

// tiktokVideoMusic reads the sound attribution off a TikTok Posts record for
// a video, where the audio is already mixed into the MP4.
func tiktokVideoMusic(record map[string]any) *archivers.VideoMusic {
	audio := tiktokAudio(record)
	if audio == nil || (audio.Music.Title == "" && audio.Music.Artist == "") {
		return nil
	}
	return &archivers.VideoMusic{
		Title:    audio.Music.Title,
		Artist:   audio.Music.Artist,
		ID:       audio.Music.ID,
		Original: audio.Music.Original,
	}
}
