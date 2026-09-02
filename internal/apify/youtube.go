package apify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"gorm.io/gorm"

	"arker/internal/archivers"
	"arker/internal/models"
)

// YouTube takes two actors, run side by side:
//
//   - epctex/youtube-video-downloader stores the muxed MP4 in its key-value
//     store (googlevideo URLs are IP-locked, so nothing else can fetch them
//     from Arker). It is billed per second of video at the requested tier and
//     returns almost no metadata.
//   - streamers/youtube-scraper returns the post facts: title, channel,
//     publish date, counts, subtitles. It is cheap but slow (a couple of
//     minutes), which is why the two run concurrently instead of in sequence.
//
// The bytes are the archive; the scraper is best-effort. If it fails, the
// public oEmbed endpoint still gives a title and channel so the video is
// never stored anonymous.

const (
	// youtubeQuality is the tier requested from the downloader. 1080p is the
	// highest tier that does not double the per-second price.
	youtubeQuality = "1080"
)

type youtubeDownloadInput struct {
	StartURLs   []string `json:"startUrls"`
	Quality     string   `json:"quality"`
	StorageType string   `json:"storageType"`
}

type youtubeScrapeInput struct {
	StartURLs         []youtubeStartURL `json:"startUrls"`
	MaxResults        int               `json:"maxResults"`
	MaxResultsShorts  int               `json:"maxResultsShorts"`
	DownloadSubtitles bool              `json:"downloadSubtitles"`
	SaveSubsToKVS     bool              `json:"saveSubsToKVS"`
	SubtitlesFormat   string            `json:"subtitlesFormat,omitempty"`
}

type youtubeStartURL struct {
	URL string `json:"url"`
}

// archiveYouTube downloads a YouTube video through Apify.
func (c *Client) archiveYouTube(ctx context.Context, targetURL string, logWriter io.Writer, db *gorm.DB, itemID uint, shortID string) (archivers.Result, error) {
	videoID := ExtractYouTubeVideoID(targetURL)
	if videoID == "" {
		return archivers.Result{}, fmt.Errorf("could not extract a YouTube video ID from %s", targetURL)
	}
	watchURL := youtubeWatchURL(videoID)

	// The scraper runs concurrently with the downloader and writes its log
	// lines into a buffer so the two runs do not interleave in the item log.
	scrapeCtx, cancelScrape := context.WithCancel(ctx)
	defer cancelScrape()
	scrape := make(chan youtubeScrape, 1)
	go func() {
		scrape <- c.youtubeScrape(scrapeCtx, watchURL, db, itemID, shortID)
	}()

	usage := &models.FallbackUsage{ArchiveItemID: itemID, ShortID: shortID, URL: targetURL}
	record, err := c.youtubeDownloadRecord(ctx, db, usage, watchURL, logWriter)
	if err != nil {
		return archivers.Result{}, err
	}

	facts := <-scrape
	io.Copy(logWriter, &facts.log)
	if facts.record == nil {
		facts.record = c.youtubeOEmbed(ctx, watchURL, logWriter)
	}
	logYouTubeMetadata(logWriter, facts.record)

	return c.archiveVideo(ctx, videoPlan{
		URL:          stringField(nestedObject(record, "output"), "url"),
		Metadata:     youtubeVideoMetadata(facts.record, record, targetURL, videoID),
		Record:       youtubeRawRecord(record, facts.record),
		ThumbnailURL: youtubePosterURL(facts.record, videoID),
		TempPattern:  "arker-apify-yt-*.mp4",
	}, usage, db, logWriter)
}

// youtubeDownloadRecord runs the downloader and returns its one row. The
// actor reports an unavailable video as an empty dataset with a SUCCEEDED
// run, so "no rows" is the not-found signal here.
func (c *Client) youtubeDownloadRecord(ctx context.Context, db *gorm.DB, usage *models.FallbackUsage, watchURL string, logWriter io.Writer) (map[string]any, error) {
	input := youtubeDownloadInput{StartURLs: []string{watchURL}, Quality: youtubeQuality, StorageType: "apify"}
	finished, err := c.runActor(ctx, db, usage, ActorYouTube, input, logWriter)
	if err != nil {
		return nil, err
	}
	for _, record := range finished.Items {
		if msg := stringField(record, "error"); msg != "" {
			err := fmt.Errorf("%w: YouTube downloader reported: %s", errNotFound, truncate(msg, 200))
			usage.Detail = truncate(err.Error(), 500)
			c.recordUsage(db, usage)
			return nil, err
		}
		if status := strings.ToLower(stringField(record, "status")); status != "" && status != "succeeded" {
			err := fmt.Errorf("YouTube downloader ended with status %q", status)
			usage.Detail = truncate(err.Error(), 500)
			c.recordUsage(db, usage)
			return nil, err
		}
		if stringField(nestedObject(record, "output"), "url") == "" {
			continue
		}
		return record, nil
	}
	err = fmt.Errorf("%w: YouTube downloader returned no video for %s (unavailable, private, or age-restricted)", errNotFound, watchURL)
	usage.Detail = truncate(err.Error(), 500)
	c.recordUsage(db, usage)
	return nil, err
}

// youtubeScrape is the metadata run's outcome. record is nil on any failure;
// the failure is already in the usage row and in log.
type youtubeScrape struct {
	record map[string]any
	log    bytes.Buffer
}

func (c *Client) youtubeScrape(ctx context.Context, watchURL string, db *gorm.DB, itemID uint, shortID string) youtubeScrape {
	var out youtubeScrape
	usage := &models.FallbackUsage{ArchiveItemID: itemID, ShortID: shortID, URL: watchURL}
	input := youtubeScrapeInput{
		StartURLs:         []youtubeStartURL{{URL: watchURL}},
		MaxResults:        1,
		MaxResultsShorts:  1,
		DownloadSubtitles: true,
		SaveSubsToKVS:     true,
		SubtitlesFormat:   "srt",
	}
	record, _, err := c.resolveRecord(ctx, db, usage, ActorYouTubeMetadata, input, &out.log, "id")
	if err != nil {
		fmt.Fprintf(&out.log, "YouTube metadata scrape failed (%v); falling back to oEmbed for title and channel\n", err)
		return out
	}
	usage.Success = true
	usage.Detail = "metadata: " + truncate(stringField(record, "title"), 200)
	c.recordUsage(db, usage)
	out.record = record
	return out
}

// youtubeOEmbed is the free last resort for title and channel. It is a public
// endpoint that answers datacenter ranges, unlike the watch page itself.
func (c *Client) youtubeOEmbed(ctx context.Context, watchURL string, logWriter io.Writer) map[string]any {
	endpoint := "https://www.youtube.com/oembed?format=json&url=" + url.QueryEscape(watchURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil
	}
	resp, err := c.http.Do(req)
	if err != nil {
		fmt.Fprintf(logWriter, "oEmbed lookup failed: %v\n", sanitizeTransportError(err))
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(logWriter, "oEmbed lookup returned %d\n", resp.StatusCode)
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil
	}
	var oembed map[string]any
	if err := json.Unmarshal(body, &oembed); err != nil {
		return nil
	}
	// Reshape into the scraper's field names so one metadata builder serves
	// both. oEmbed has no id/date/counts; those stay empty.
	return map[string]any{
		"title":        oembed["title"],
		"channelName":  oembed["author_name"],
		"channelUrl":   oembed["author_url"],
		"thumbnailUrl": oembed["thumbnail_url"],
		"_source":      "oembed",
	}
}

// youtubeRawRecord is the stored provider record: the scraper's post facts
// plus the downloader's delivery row, under one roof.
func youtubeRawRecord(download, facts map[string]any) map[string]any {
	raw := map[string]any{"download": download}
	if facts != nil {
		raw["video"] = facts
	}
	return raw
}

func youtubeWatchURL(videoID string) string {
	return "https://www.youtube.com/watch?v=" + url.QueryEscape(videoID)
}

func youtubePosterURL(facts map[string]any, videoID string) string {
	if u := stringField(facts, "thumbnailUrl"); u != "" {
		return u
	}
	return "https://i.ytimg.com/vi/" + videoID + "/maxresdefault.jpg"
}

// youtubeDurationSeconds parses the scraper's "HH:MM:SS" duration.
func youtubeDurationSeconds(value string) *float64 {
	parts := strings.Split(strings.TrimSpace(value), ":")
	if len(parts) == 0 || value == "" {
		return nil
	}
	var total float64
	for _, part := range parts {
		n, err := strconv.ParseFloat(part, 64)
		if err != nil {
			return nil
		}
		total = total*60 + n
	}
	if total <= 0 {
		return nil
	}
	return &total
}

func youtubeChannelUsername(facts map[string]any) string {
	if username := stringField(facts, "channelUsername"); username != "" {
		return username
	}
	// oEmbed only has the channel URL: https://www.youtube.com/@handle
	channelURL := stringField(facts, "channelUrl")
	if at := strings.LastIndex(channelURL, "/@"); at >= 0 {
		return channelURL[at+2:]
	}
	return ""
}

func youtubeVideoMetadata(facts, download map[string]any, sourceURL, videoID string) *archivers.VideoMetadata {
	var duration *float64
	if seconds := floatField(download, "durationSeconds"); seconds != nil && *seconds > 0 {
		duration = seconds
	} else {
		duration = youtubeDurationSeconds(stringField(facts, "duration"))
	}
	channelName := stringField(facts, "channelName")
	username := youtubeChannelUsername(facts)
	extractor := "youtube"
	if stringField(facts, "_source") == "oembed" {
		extractor = "youtube:oembed"
	}
	return &archivers.VideoMetadata{
		SourceURL:            archivers.SanitizeURL(sourceURL, nil),
		Platform:             "youtube",
		Extractor:            extractor,
		PostID:               videoID,
		CanonicalURL:         youtubeWatchURL(videoID),
		Title:                stringField(facts, "title"),
		Description:          stringField(facts, "text"),
		Author:               channelName,
		AuthorID:             stringField(facts, "channelId"),
		Uploader:             firstNonEmptyString(username, channelName),
		UploaderID:           stringField(facts, "channelId"),
		Channel:              channelName,
		ChannelID:            stringField(facts, "channelId"),
		PublicationTimestamp: timestampString(facts["date"]),
		DurationSeconds:      duration,
		Engagement: archivers.VideoEngagement{
			Views:    intField(facts, "viewCount"),
			Likes:    intField(facts, "likes"),
			Comments: intField(facts, "commentsCount"),
		},
		Tags: stringSlice(facts, "hashtags"),
		Media: archivers.VideoMedia{
			QualityLabel: youtubeQualityLabel(stringField(download, "quality")),
		},
		Provider: "apify:" + strings.ReplaceAll(ActorYouTube, "~", "/"),
	}
}

// youtubeQualityLabel renders the downloader's requested tier ("1080") the
// way yt-dlp names formats ("1080p"). It is the tier asked for, which the
// actor caps at what the video actually has; ffprobe's dimensions on the
// stored bytes are the fact.
func youtubeQualityLabel(quality string) string {
	if quality == "" {
		return ""
	}
	if _, err := strconv.Atoi(quality); err == nil {
		return quality + "p"
	}
	return quality
}

func logYouTubeMetadata(logWriter io.Writer, facts map[string]any) {
	if facts == nil {
		return
	}
	if title := stringField(facts, "title"); title != "" {
		fmt.Fprintf(logWriter, "Title: %s\n", title)
	}
	if channel := stringField(facts, "channelName"); channel != "" {
		fmt.Fprintf(logWriter, "Channel: %s\n", channel)
	}
	if date := timestampString(facts["date"]); date != "" {
		fmt.Fprintf(logWriter, "Published: %s\n", date)
	}
	if views := intField(facts, "viewCount"); views != nil {
		fmt.Fprintf(logWriter, "Views: %d\n", *views)
	}
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
