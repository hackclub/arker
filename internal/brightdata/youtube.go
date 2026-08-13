package brightdata

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"

	"arker/internal/archivers"
	"arker/internal/models"
	"arker/internal/utils"
)

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
	var totalBytes int64
	sessions := 0
	finishUsage := func(success bool, detail string) {
		usage.BytesTransferred, usage.CostUSD = c.browserSessionCost(totalBytes, sessions)
		usage.Success = success
		usage.Detail = truncate(detail, 500)
		c.recordUsage(db, usage)
	}

	// Session exit countries, in order. The first session lets Bright Data
	// pick any peer; if YouTube then refuses playback with a country block,
	// later sessions pin the exit elsewhere — a rights-holder block (the main
	// rescuable YouTube failure) rarely covers all of these at once. Pool
	// errors ("no_peers") also advance to the next session.
	countries := []string{"", "de", "gb"}
	var lastErr error
	for _, country := range countries {
		if lastErr != nil {
			fmt.Fprintf(logWriter, "Retrying with a different session geography (%s) after: %v\n",
				countryLabel(country), lastErr)
		}
		sessions++

		var info *youtubeMediaInfo
		var videoPath string
		var size int64
		info, videoPath, size, lastErr = c.youtubeSession(ctx, country, watchURL, videoID, logWriter)
		totalBytes += size
		if lastErr == nil {
			fmt.Fprintf(logWriter, "Downloaded %d bytes through Bright Data browser session\n", size)

			metadata, rawMetadata, err := buildBrightDataYouTubeArtifacts(info, targetURL, videoID, size, time.Now())
			if err != nil {
				removeFile(videoPath)
				finishUsage(false, "metadata build failed: "+err.Error())
				return archivers.Result{}, fmt.Errorf("failed to build Bright Data video metadata: %w", err)
			}

			// The poster image is fetched directly (i.ytimg.com is not blocked
			// for Arker), so it costs nothing through the session.
			thumb := c.thumbnailFromURL(ctx, info.ThumbnailURL, logWriter)

			reader, err := openTempFileReader(videoPath)
			if err != nil {
				removeFile(videoPath)
				finishUsage(false, err.Error())
				return archivers.Result{}, err
			}
			finishUsage(true, fmt.Sprintf("%s %s %d bytes", info.Title, info.QualityLabel, size))
			return archivers.Result{
				Data:        reader,
				Extension:   ".mp4",
				ContentType: "video/mp4",
				Thumbnail:   thumb,
				Source:      models.ArchiveSourceBrightData,
				Metadata:    metadata,
				RawMetadata: rawMetadata,
				// One video, stored with both sidecars. Fidelity is a separate
				// question from completeness: the fallback is capped at the
				// progressive stream, which the normalized metadata records as
				// its quality label.
				Completeness: archivers.CompletenessComplete,
			}, nil
		}

		if ctx.Err() != nil || !retryableInAnotherCountry(lastErr) {
			break
		}
	}

	finishUsage(false, lastErr.Error())
	return archivers.Result{}, lastErr
}

// youtubeSession runs one complete browser session attempt: connect, navigate,
// resolve, download. Returns the media info, temp file path, and bytes
// fetched (bytes are reported even on failure, for usage accounting).
func (c *Client) youtubeSession(ctx context.Context, country, watchURL, videoID string, logWriter io.Writer) (*youtubeMediaInfo, string, int64, error) {
	session, err := c.openBrowserSession(ctx, country, watchURL, logWriter)
	if err != nil {
		return nil, "", 0, err
	}
	defer session.Close()

	info, err := resolveYouTubeMedia(session, videoID, c.cfg)
	if err != nil {
		return nil, "", 0, err
	}
	fmt.Fprintf(logWriter, "Resolved %s (%s) by %s: %s, %d bytes\n",
		info.Title, info.QualityLabel, info.Author, info.MimeType, info.ContentLength)
	if info.LengthSeconds > 0 {
		fmt.Fprintf(logWriter, "Duration: %ds, views: %s\n", info.LengthSeconds, info.ViewCount)
	}
	if info.ShortDescription != "" {
		fmt.Fprintf(logWriter, "Description: %s\n", utils.TruncateForLog(info.ShortDescription, 300))
	}

	videoPath, size, err := fetchURLThroughPage(ctx, session, info.URL, info.ContentLength, "arker-bd-yt-*.mp4", logWriter)
	if err != nil {
		return nil, "", size, err
	}
	if err := verifyMP4(videoPath); err != nil {
		removeFile(videoPath)
		return nil, "", size, err
	}
	return info, videoPath, size, nil
}

// retryableInAnotherCountry classifies failures a fresh session elsewhere
// could fix: country-blocked playability and Bright Data pool errors.
func retryableInAnotherCountry(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not available in your country") ||
		strings.Contains(msg, "no_peers") ||
		strings.Contains(msg, "no peer found")
}

// youtubeMediaInfo is what the in-page resolution returns.
type youtubeMediaInfo struct {
	OK               bool            `json:"ok"`
	Reason           string          `json:"reason"`
	URL              string          `json:"url"`
	ContentLength    int64           `json:"contentLength,string"`
	MimeType         string          `json:"mimeType"`
	QualityLabel     string          `json:"qualityLabel"`
	Title            string          `json:"title"`
	Author           string          `json:"author"`
	LengthSeconds    int64           `json:"lengthSeconds,string"`
	ViewCount        string          `json:"viewCount"`
	ShortDescription string          `json:"shortDescription"`
	ThumbnailURL     string          `json:"thumbnailUrl"`
	FormatID         string          `json:"formatId"`
	Width            int64           `json:"width"`
	Height           int64           `json:"height"`
	FPS              float64         `json:"fps"`
	Raw              json.RawMessage `json:"raw"`
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
	formatId: String(best.itag || ''),
	width: best.width || 0,
	height: best.height || 0,
	fps: best.fps || 0,
	raw: response,
  });
}
`

func buildBrightDataYouTubeArtifacts(info *youtubeMediaInfo, sourceURL, videoID string, size int64, archivedAt time.Time) (*archivers.Sidecar, *archivers.Sidecar, error) {
	if info == nil {
		return nil, nil, fmt.Errorf("YouTube media info is nil")
	}
	if len(info.Raw) == 0 {
		return nil, nil, fmt.Errorf("Innertube response is missing")
	}

	var provider struct {
		VideoDetails struct {
			ChannelID string   `json:"channelId"`
			Keywords  []string `json:"keywords"`
		} `json:"videoDetails"`
		Microformat struct {
			Renderer struct {
				PublishDate string `json:"publishDate"`
				UploadDate  string `json:"uploadDate"`
			} `json:"playerMicroformatRenderer"`
		} `json:"microformat"`
	}
	if err := json.Unmarshal(info.Raw, &provider); err != nil {
		return nil, nil, fmt.Errorf("decode Innertube response: %w", err)
	}

	var duration *float64
	if info.LengthSeconds > 0 {
		value := float64(info.LengthSeconds)
		duration = &value
	}
	var views *int64
	if value, err := strconv.ParseInt(info.ViewCount, 10, 64); err == nil {
		views = &value
	}
	var width, height *int64
	if info.Width > 0 {
		value := info.Width
		width = &value
	}
	if info.Height > 0 {
		value := info.Height
		height = &value
	}
	var fps *float64
	if info.FPS > 0 {
		value := info.FPS
		fps = &value
	}
	contentType := strings.TrimSpace(strings.Split(info.MimeType, ";")[0])
	if contentType == "" {
		contentType = "video/mp4"
	}
	publishedAt := normalizeProviderDate(firstNonEmptyString(
		provider.Microformat.Renderer.PublishDate,
		provider.Microformat.Renderer.UploadDate,
	))

	metadataJSON, err := archivers.MarshalVideoMetadata(&archivers.VideoMetadata{
		SchemaVersion:        archivers.VideoMetadataSchemaVersion,
		SourceURL:            archivers.SanitizeURL(sourceURL, nil),
		Platform:             "youtube",
		Extractor:            "youtube:innertube",
		PostID:               videoID,
		CanonicalURL:         "https://www.youtube.com/watch?v=" + url.QueryEscape(videoID),
		Title:                info.Title,
		Description:          info.ShortDescription,
		Author:               info.Author,
		Uploader:             info.Author,
		Channel:              info.Author,
		ChannelID:            provider.VideoDetails.ChannelID,
		PublicationTimestamp: publishedAt,
		DurationSeconds:      duration,
		Engagement:           archivers.VideoEngagement{Views: views},
		Tags:                 provider.VideoDetails.Keywords,
		Media: archivers.VideoMedia{
			FormatID:     info.FormatID,
			Extension:    ".mp4",
			ContentType:  contentType,
			SizeBytes:    size,
			Width:        width,
			Height:       height,
			FPS:          fps,
			QualityLabel: info.QualityLabel,
		},
		ArchivedAt: archivedAt.UTC().Format(time.RFC3339),
		Provenance: models.ArchiveSourceBrightData,
		Provider:   "brightdata_browser_api",
	})
	if err != nil {
		return nil, nil, err
	}
	rawJSON, err := archivers.SanitizeJSON(info.Raw, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("sanitize Innertube response: %w", err)
	}
	return &archivers.Sidecar{Data: metadataJSON}, &archivers.Sidecar{Data: rawJSON}, nil
}

func normalizeProviderDate(value string) string {
	for _, layout := range []string{time.RFC3339, "2006-01-02", "2006-01-02 15:04:05"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.UTC().Format(time.RFC3339)
		}
	}
	return ""
}

func resolveYouTubeMedia(page pageEvaluator, videoID string, cfg Config) (*youtubeMediaInfo, error) {
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
