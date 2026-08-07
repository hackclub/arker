package brightdata

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/mxschmitt/playwright-go"
	"gorm.io/gorm"

	"arker/internal/archivers"
	"arker/internal/models"
	"arker/internal/utils"
)

// browserPageOverheadBytes is the estimated non-media traffic of one YouTube
// watch-page session (HTML, player JS, API calls). Counted into usage so the
// cost estimate errs high rather than silently low.
const browserPageOverheadBytes = 3 << 20

// mediaChunkBytes is the Range size for in-page media fetches. Each chunk
// crosses the CDP connection base64-encoded, so this trades round-trips
// against websocket message size.
const mediaChunkBytes = 6 << 20

// archiveYouTube downloads a YouTube video through a Bright Data Browser API
// session.
//
// Why a remote browser and not the scraper dataset: the dataset's video_url is
// signed with an ip= parameter that Bright Data scrambles, so nothing outside
// their exit can fetch it. And why not a proxy: Bright Data's unlocker proxy
// refuses CONNECT to youtube.com outright. What remains is doing everything at
// their network position — resolve AND download inside the remote browser.
//
// Inside the page, an Innertube /player call with an Android client context
// returns progressive (muxed) format URLs without signature ciphering. Those
// URLs are bound to the session's own exit IP, so in-page ranged fetches can
// pull the media, which crosses back to Arker base64-chunked over CDP.
// Progressive streams top out at 360p/720p; that ceiling is the price of a
// video that could not be archived at all natively, and Source records the
// provenance for anyone auditing fidelity later.
func (c *Client) archiveYouTube(ctx context.Context, targetURL string, logWriter io.Writer, db *gorm.DB, itemID uint, shortID string) (archivers.Result, error) {
	if !c.BrowserReady() {
		return archivers.Result{}, fmt.Errorf("Bright Data browser credentials are not configured")
	}

	videoID := ExtractYouTubeVideoID(targetURL)
	if videoID == "" {
		return archivers.Result{}, fmt.Errorf("could not extract a YouTube video ID from %s", targetURL)
	}
	watchURL := "https://www.youtube.com/watch?v=" + url.QueryEscape(videoID)

	usage := &models.BrightDataUsage{
		ArchiveItemID: itemID,
		ShortID:       shortID,
		URL:           targetURL,
		Product:       "browser_api",
	}
	finishUsage := func(bytesTransferred int64, success bool, detail string) {
		usage.BytesTransferred = bytesTransferred + browserPageOverheadBytes
		usage.CostUSD = float64(usage.BytesTransferred) / 1e9 * c.cfg.BrowserCostPerGB
		usage.Success = success
		usage.Detail = truncate(detail, 500)
		c.recordUsage(db, usage)
	}

	fmt.Fprintf(logWriter, "Connecting to Bright Data browser session...\n")
	pw, err := playwright.Run()
	if err != nil {
		finishUsage(0, false, "playwright start failed: "+err.Error())
		return archivers.Result{}, fmt.Errorf("failed to start Playwright: %w", err)
	}
	defer pw.Stop()

	browser, err := pw.Chromium.ConnectOverCDP(c.browserWSEndpoint())
	if err != nil {
		finishUsage(0, false, "browser connect failed: "+err.Error())
		return archivers.Result{}, fmt.Errorf("failed to connect to Bright Data browser: %w", err)
	}
	defer browser.Close()

	page, err := browserPage(browser)
	if err != nil {
		finishUsage(0, false, "page create failed: "+err.Error())
		return archivers.Result{}, err
	}

	fmt.Fprintf(logWriter, "Loading %s in remote browser...\n", watchURL)
	if _, err := page.Goto(watchURL, playwright.PageGotoOptions{
		Timeout:   playwright.Float(90000),
		WaitUntil: playwright.WaitUntilStateDomcontentloaded,
	}); err != nil {
		finishUsage(0, false, "navigation failed: "+err.Error())
		return archivers.Result{}, fmt.Errorf("remote browser navigation failed: %w", err)
	}

	info, err := resolveYouTubeMedia(page, videoID, c.cfg)
	if err != nil {
		finishUsage(0, false, "resolve failed: "+err.Error())
		return archivers.Result{}, err
	}
	fmt.Fprintf(logWriter, "Resolved %s (%s) by %s: %s, %d bytes\n",
		info.Title, info.QualityLabel, info.Author, info.MimeType, info.ContentLength)
	if info.LengthSeconds > 0 {
		fmt.Fprintf(logWriter, "Duration: %ds, views: %s\n", info.LengthSeconds, info.ViewCount)
	}
	if info.ShortDescription != "" {
		fmt.Fprintf(logWriter, "Description: %s\n", utils.TruncateForLog(info.ShortDescription, 300))
	}

	videoPath, size, err := fetchMediaThroughPage(ctx, page, info, logWriter)
	if err != nil {
		finishUsage(size, false, "media fetch failed: "+err.Error())
		return archivers.Result{}, err
	}
	if err := verifyMP4(videoPath); err != nil {
		removeFile(videoPath)
		finishUsage(size, false, err.Error())
		return archivers.Result{}, err
	}

	finishUsage(size, true, fmt.Sprintf("%s %s %d bytes", info.Title, info.QualityLabel, size))
	fmt.Fprintf(logWriter, "Downloaded %d bytes through Bright Data browser session\n", size)

	// The poster image is fetched directly (i.ytimg.com is not blocked for
	// Arker), so it costs nothing through the session.
	thumb := c.thumbnailFromURL(ctx, info.ThumbnailURL, logWriter)

	reader, err := openTempFileReader(videoPath)
	if err != nil {
		removeFile(videoPath)
		return archivers.Result{}, err
	}
	return archivers.Result{
		Data:        reader,
		Extension:   ".mp4",
		ContentType: "video/mp4",
		Thumbnail:   thumb,
		Source:      models.ArchiveSourceBrightData,
	}, nil
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

// youtubeMediaInfo is what the in-page resolution returns.
type youtubeMediaInfo struct {
	OK               bool   `json:"ok"`
	Reason           string `json:"reason"`
	URL              string `json:"url"`
	ContentLength    int64  `json:"contentLength,string"`
	MimeType         string `json:"mimeType"`
	QualityLabel     string `json:"qualityLabel"`
	Title            string `json:"title"`
	Author           string `json:"author"`
	LengthSeconds    int64  `json:"lengthSeconds,string"`
	ViewCount        string `json:"viewCount"`
	ShortDescription string `json:"shortDescription"`
	ThumbnailURL     string `json:"thumbnailUrl"`
}

// resolveYouTubeMediaJS runs in the watch page. Deliberately, it does NOT
// scrape the rendered page: page markup churns constantly and anything reading
// it breaks on YouTube's schedule. Instead it calls the Innertube player
// endpoint — the same stable API every official YouTube client (and yt-dlp)
// uses — with an Android client context, which returns progressive format URLs
// without signature ciphering, and picks the best progressive (muxed
// audio+video) format. The page is only the origin for the request and the
// network position the media URL is bound to. The page's ytcfg is consulted
// opportunistically (API key, visitor ID) with hardcoded fallbacks, so a
// redesigned page cannot break resolution.
//
// The one YouTube-versioned surface is the Android client name/version pair,
// which is why it is injected from configuration rather than pinned here.
const resolveYouTubeMediaJS = `
async (args) => {
  const videoId = args.videoId;
  const cfg = (window.ytcfg && window.ytcfg.data_) ? window.ytcfg.data_ : {};
  const apiKey = cfg.INNERTUBE_API_KEY || 'AIzaSyAO_FJ2SlqU8Q4STEHLGCilw_Y9_11qcW8';
  const visitorData = (cfg.INNERTUBE_CONTEXT && cfg.INNERTUBE_CONTEXT.client && cfg.INNERTUBE_CONTEXT.client.visitorData) || undefined;
  const body = {
    context: { client: {
      clientName: args.clientName,
      clientVersion: args.clientVersion,
      androidSdkVersion: 30,
      osName: 'Android',
      osVersion: '11',
      hl: 'en',
      visitorData: visitorData,
      userAgent: 'com.google.android.youtube/' + args.clientVersion + ' (Linux; U; Android 11) gzip',
    }},
    videoId: videoId,
    contentCheckOk: true,
    racyCheckOk: true,
  };
  const headers = {'Content-Type': 'application/json'};
  if (visitorData) headers['X-Goog-Visitor-Id'] = visitorData;
  let response;
  try {
    const r = await fetch('https://www.youtube.com/youtubei/v1/player?key=' + apiKey + '&prettyPrint=false', {
      method: 'POST', headers: headers, body: JSON.stringify(body)
    });
    response = await r.json();
  } catch (e) {
    return JSON.stringify({ok: false, reason: 'innertube request failed: ' + e});
  }
  const status = response.playabilityStatus && response.playabilityStatus.status;
  if (status !== 'OK') {
    return JSON.stringify({ok: false, reason: 'playability ' + status + ': ' + ((response.playabilityStatus && response.playabilityStatus.reason) || 'no reason')});
  }
  const formats = ((response.streamingData && response.streamingData.formats) || []).filter(f => f.url);
  if (!formats.length) {
    return JSON.stringify({ok: false, reason: 'no progressive formats with direct URLs'});
  }
  formats.sort((a, b) => (b.height || 0) - (a.height || 0));
  const best = formats[0];
  const details = response.videoDetails || {};
  const thumbs = (details.thumbnail && details.thumbnail.thumbnails) || [];
  const bestThumb = thumbs.length ? thumbs[thumbs.length - 1].url : '';
  return JSON.stringify({
    ok: true,
    url: best.url,
    contentLength: String(best.contentLength || '0'),
    mimeType: best.mimeType || '',
    qualityLabel: best.qualityLabel || '',
    title: details.title || '',
    author: details.author || '',
    lengthSeconds: String(details.lengthSeconds || '0'),
    viewCount: String(details.viewCount || ''),
    shortDescription: details.shortDescription || '',
    thumbnailUrl: bestThumb,
  });
}
`

func resolveYouTubeMedia(page playwright.Page, videoID string, cfg Config) (*youtubeMediaInfo, error) {
	raw, err := page.Evaluate(resolveYouTubeMediaJS, map[string]interface{}{
		"videoId":       videoID,
		"clientName":    cfg.YouTubeClientName,
		"clientVersion": cfg.YouTubeClientVersion,
	})
	if err != nil {
		return nil, fmt.Errorf("in-page media resolution failed: %w", err)
	}
	text, ok := raw.(string)
	if !ok {
		return nil, fmt.Errorf("in-page media resolution returned %T", raw)
	}
	var info youtubeMediaInfo
	if err := json.Unmarshal([]byte(text), &info); err != nil {
		return nil, fmt.Errorf("could not parse media resolution result: %w", err)
	}
	if !info.OK {
		return nil, fmt.Errorf("YouTube refused playback through Bright Data browser: %s", info.Reason)
	}
	return &info, nil
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

// fetchMediaThroughPage pulls the media file through the remote page in ranged
// chunks. Returns the temp file path and the number of media bytes fetched
// (also on error, for usage accounting).
func fetchMediaThroughPage(ctx context.Context, page playwright.Page, info *youtubeMediaInfo, logWriter io.Writer) (string, int64, error) {
	out, err := createTempFile("arker-bd-yt-*.mp4")
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

	total := info.ContentLength
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

		chunk, contentRange, status, err := fetchOneChunk(page, info.URL, offset, end)
		if err != nil {
			// A 416 after bytes have flowed is the server saying the offset
			// equals the file size: the download is complete, we just did not
			// know the size up front (Content-Range is not a CORS-exposed
			// header, and some Innertube responses omit contentLength).
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
		fmt.Fprintf(logWriter, "Fetched %d / %d bytes...\n", offset, total)
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

// fetchOneChunk runs one in-page ranged fetch with retries. Chunk fetches ride
// on a live remote session, so a transient failure is worth two more tries
// before abandoning the whole (already paid-for) session.
func fetchOneChunk(page playwright.Page, mediaURL string, start, end int64) ([]byte, string, int, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		// 416 is deterministic, not transient: the range starts past EOF.
		if isRangeNotSatisfiable(lastErr) {
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

var youtubeIDPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)[?&]v=([A-Za-z0-9_-]{6,20})`),
	regexp.MustCompile(`(?i)youtu\.be/([A-Za-z0-9_-]{6,20})`),
	regexp.MustCompile(`(?i)youtube\.com/shorts/([A-Za-z0-9_-]{6,20})`),
	regexp.MustCompile(`(?i)youtube\.com/live/([A-Za-z0-9_-]{6,20})`),
	regexp.MustCompile(`(?i)youtube\.com/embed/([A-Za-z0-9_-]{6,20})`),
}

// ExtractYouTubeVideoID pulls the video ID out of the URL shapes Arker sees
// (watch, youtu.be, shorts, live, embed). Empty when none match.
func ExtractYouTubeVideoID(rawURL string) string {
	for _, pattern := range youtubeIDPatterns {
		if m := pattern.FindStringSubmatch(rawURL); m != nil {
			return m[1]
		}
	}
	return ""
}
