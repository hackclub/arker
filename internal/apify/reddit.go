package apify

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	"gorm.io/gorm"

	"arker/internal/archivers"
	"arker/internal/models"
	"arker/internal/utils"
)

// Reddit goes through harshmaur/reddit-scraper, which returns the post as
// Reddit's own API describes it: mediaAssets[] for galleries, contentUrl for
// a single image, media.reddit_video for hosted video. A missing or removed
// post is simply absent from the dataset (and not billed), so "no item" is
// the not-found signal.

type redditInput struct {
	StartURLs      []redditStartURL `json:"startUrls"`
	MaxItems       int              `json:"maxItems"`
	SearchComments bool             `json:"searchComments"`
}

type redditStartURL struct {
	URL string `json:"url"`
}

func redditRunInput(targetURL string) redditInput {
	return redditInput{StartURLs: []redditStartURL{{URL: targetURL}}, MaxItems: 5, SearchComments: false}
}

func (c *Client) archiveReddit(ctx context.Context, targetURL, itemType string, logWriter io.Writer, db *gorm.DB, itemID uint, shortID string) (archivers.Result, error) {
	if !utils.ArchiveTypesEqual(itemType, utils.ArchiveTypeGalleryDl) {
		return archivers.Result{}, fmt.Errorf("no Reddit fallback for archive type %s", itemType)
	}

	usage := &models.FallbackUsage{ArchiveItemID: itemID, ShortID: shortID, URL: targetURL}
	record, _, err := c.resolveRecord(ctx, db, usage, ActorReddit, redditRunInput(targetURL), logWriter, "id")
	if err != nil {
		return archivers.Result{}, err
	}

	entries := redditMediaEntries(record)
	if len(entries) == 0 {
		err := fmt.Errorf("Reddit record for %s contains no media (a text or link post has nothing to archive)", targetURL)
		usage.Detail = truncate(err.Error(), 500)
		c.recordUsage(db, usage)
		return archivers.Result{}, err
	}
	logRedditMetadata(logWriter, record)

	meta := redditGalleryMetadata(record, targetURL)
	result, err := c.archiveGallery(ctx, galleryPlan{
		Entries:   entries,
		Meta:      meta,
		Record:    record,
		PosterURL: redditPosterURL(record),
		Fetch:     c.redditFetch(usage, record, logWriter),
	}, usage, db, logWriter)
	if err != nil {
		return archivers.Result{}, err
	}

	if len(entries) == 1 && entries[0].isVideo() {
		if err := attachSingleVideoMetadata(&result, meta, record, redditVideoMetadata(record, targetURL), ActorReddit); err != nil {
			closeResultData(result)
			return archivers.Result{}, fmt.Errorf("failed to build Reddit video metadata: %w", err)
		}
	}
	return result, nil
}

// redditFetch downloads stills directly and assembles hosted videos.
//
// Reddit's best rendition (fallback_url, up to 1080p) is video-only CMAF with
// the audio published as a sibling CMAF_AUDIO_128.mp4; the two are muxed
// locally. The signed HLS manifest is the backstop when either half fails: it
// is what the player uses, but it tops out at 720p.
func (c *Client) redditFetch(usage *models.FallbackUsage, record map[string]any, logWriter io.Writer) mediaFetcher {
	direct := c.kvsCountingFetch(usage)
	video := nestedObject(record, "media", "reddit_video")
	return func(ctx context.Context, entry mediaEntry, dest string) (int64, error) {
		if !entry.isVideo() || video == nil {
			return direct(ctx, entry, dest)
		}
		hasAudio := boolField(video, "has_audio")
		size, err := c.redditMuxedVideo(ctx, stringField(video, "fallback_url"), hasAudio == nil || *hasAudio, dest)
		if err == nil {
			return size, nil
		}
		fmt.Fprintf(logWriter, "Reddit CMAF download failed (%v); remuxing HLS instead\n", err)
		manifest := firstNonEmptyString(stringField(video, "hls_url"), entry.URL)
		if !isStreamManifest(manifest) {
			return 0, fmt.Errorf("no HLS manifest to fall back to")
		}
		variant, err := c.bestHLSVariant(ctx, manifest)
		if err != nil {
			return 0, err
		}
		return remuxStream(ctx, variant, dest)
	}
}

// redditMuxedVideo downloads the CMAF video rendition and its audio track
// and joins them.
func (c *Client) redditMuxedVideo(ctx context.Context, fallbackURL string, hasAudio bool, dest string) (int64, error) {
	if fallbackURL == "" {
		return 0, fmt.Errorf("record has no fallback_url")
	}
	videoTmp, _, err := c.downloadToTemp(ctx, fallbackURL, "apify-reddit-video-*.mp4")
	if err != nil {
		return 0, err
	}
	defer removeFile(videoTmp)
	if err := verifyMP4(videoTmp); err != nil {
		return 0, err
	}
	if !hasAudio {
		if err := renameTempFile(videoTmp, dest); err != nil {
			return 0, err
		}
		info, err := os.Stat(dest)
		if err != nil {
			return 0, err
		}
		return info.Size(), nil
	}
	audioURL, err := redditAudioURL(fallbackURL)
	if err != nil {
		return 0, err
	}
	audioTmp, _, err := c.downloadToTemp(ctx, audioURL, "apify-reddit-audio-*.mp4")
	if err != nil {
		return 0, fmt.Errorf("audio track: %w", err)
	}
	defer removeFile(audioTmp)
	if err := verifyMP4(audioTmp); err != nil {
		return 0, fmt.Errorf("audio track: %w", err)
	}
	return muxFiles(ctx, videoTmp, audioTmp, dest)
}

// redditAudioURL derives the audio track's URL from the video rendition's:
// https://v.redd.it/<id>/CMAF_1080.mp4 -> https://v.redd.it/<id>/CMAF_AUDIO_128.mp4
func redditAudioURL(fallbackURL string) (string, error) {
	parsed, err := url.Parse(fallbackURL)
	if err != nil {
		return "", err
	}
	dir := path.Dir(parsed.Path)
	if dir == "/" || dir == "." {
		return "", fmt.Errorf("cannot derive audio URL from %s", urlPath(fallbackURL))
	}
	parsed.Path = dir + "/CMAF_AUDIO_128.mp4"
	parsed.RawQuery = ""
	return parsed.String(), nil
}

// redditMediaEntries resolves the post's assets from the record: a gallery's
// slides, a single hosted video, or a single image, in that order of
// precedence. Link previews (external-preview.redd.it) are the linked page's
// thumbnail, not the post's media, and never count.
func redditMediaEntries(record map[string]any) []mediaEntry {
	var entries []mediaEntry
	for _, asset := range nestedList(record, "mediaAssets") {
		obj, _ := asset.(map[string]any)
		u := stringField(obj, "url")
		if u == "" {
			continue
		}
		kind := "Photo"
		if strings.HasPrefix(stringField(obj, "mimeType"), "video/") {
			kind = "Video"
		}
		entries = append(entries, mediaEntry{URL: u, Type: kind})
	}
	if len(entries) > 0 {
		return entries
	}
	if video := nestedObject(record, "media", "reddit_video"); video != nil {
		// fallback_url is the best rendition but video-only; redditFetch
		// pairs it with the audio track (or falls back to the HLS manifest).
		if u := firstNonEmptyString(stringField(video, "fallback_url"), stringField(video, "hls_url")); u != "" {
			return []mediaEntry{{URL: u, Type: "Video"}}
		}
	}
	if u := stringField(record, "contentUrl"); redditIsImageURL(u) {
		return []mediaEntry{{URL: u, Type: "Photo"}}
	}
	if strings.EqualFold(stringField(record, "postType"), "image") {
		for _, u := range stringSlice(record, "images") {
			if redditIsImageURL(u) {
				return []mediaEntry{{URL: u, Type: "Photo"}}
			}
		}
	}
	return nil
}

// redditIsImageURL accepts Reddit's own image hosts, where the URL is the
// post's media rather than a preview of something else.
func redditIsImageURL(rawURL string) bool {
	parsed, err := url.Parse(rawURL)
	if err != nil || rawURL == "" {
		return false
	}
	host := strings.ToLower(parsed.Host)
	if host != "i.redd.it" && host != "preview.redd.it" {
		return false
	}
	switch strings.ToLower(path.Ext(parsed.Path)) {
	case ".jpg", ".jpeg", ".png", ".gif", ".webp":
		return true
	}
	return false
}

func redditPosterURL(record map[string]any) string {
	for _, u := range stringSlice(record, "images") {
		if u != "" {
			return u
		}
	}
	return stringField(record, "thumbnail")
}

func redditPostID(record map[string]any) string {
	return firstNonEmptyString(stringField(record, "parsedId"), strings.TrimPrefix(stringField(record, "id"), "t3_"))
}

// subredditName renders the community as r/<name>, the form a reader expects.
func subredditName(record map[string]any) string {
	name := firstNonEmptyString(stringField(record, "communityName"), stringField(record, "parsedCommunityName"))
	if name == "" {
		return ""
	}
	if strings.HasPrefix(strings.ToLower(name), "r/") {
		return name
	}
	return "r/" + name
}

func redditTags(record map[string]any) []string {
	if flair := stringField(record, "flair"); flair != "" {
		return []string{flair}
	}
	return nil
}

func redditGalleryMetadata(record map[string]any, sourceURL string) *archivers.GalleryMetadata {
	meta := &archivers.GalleryMetadata{
		SourceURL:   sourceURL,
		Extractor:   "reddit",
		Subcategory: "apify",
		PostID:      redditPostID(record),
		PostURL:     stringField(record, "postUrl"),
		Author:      stringField(record, "authorName"),
		AuthorName:  subredditName(record),
		Title:       stringField(record, "title"),
		Description: stringField(record, "body"),
		Date:        timestampString(record["createdAt"]),
		Likes:       intField(record, "score", "upVotes"),
		Comments:    intField(record, "commentsCount"),
		Tags:        redditTags(record),
		ToolVersion: "apify:" + strings.ReplaceAll(ActorReddit, "~", "/"),
		ArchivedAt:  time.Now().UTC().Format(time.RFC3339),
	}
	return meta
}

func redditVideoMetadata(record map[string]any, sourceURL string) *archivers.VideoMetadata {
	video := nestedObject(record, "media", "reddit_video")
	return &archivers.VideoMetadata{
		SourceURL:            archivers.SanitizeURL(sourceURL, nil),
		Platform:             "reddit",
		Extractor:            "reddit",
		PostID:               redditPostID(record),
		CanonicalURL:         archivers.SanitizeURL(firstNonEmptyString(stringField(record, "postUrl"), sourceURL), nil),
		Title:                stringField(record, "title"),
		Description:          stringField(record, "body"),
		Author:               stringField(record, "authorName"),
		AuthorID:             firstNonEmptyString(stringField(record, "parsedAuthorId"), stringField(record, "authorId")),
		Uploader:             stringField(record, "authorName"),
		UploaderID:           firstNonEmptyString(stringField(record, "parsedAuthorId"), stringField(record, "authorId")),
		Channel:              subredditName(record),
		ChannelID:            firstNonEmptyString(stringField(record, "parsedCommunityId"), stringField(record, "communityId")),
		PublicationTimestamp: timestampString(record["createdAt"]),
		DurationSeconds:      positiveFloat(floatField(video, "duration")),
		Engagement: archivers.VideoEngagement{
			Likes:    intField(record, "score", "upVotes"),
			Comments: intField(record, "commentsCount"),
		},
		Tags: redditTags(record),
		Media: archivers.VideoMedia{
			Width:  intField(video, "width"),
			Height: intField(video, "height"),
		},
	}
}

func logRedditMetadata(logWriter io.Writer, record map[string]any) {
	if community := subredditName(record); community != "" {
		fmt.Fprintf(logWriter, "Community: %s\n", community)
	}
	if author := stringField(record, "authorName"); author != "" {
		fmt.Fprintf(logWriter, "Author: u/%s\n", author)
	}
	if title := stringField(record, "title"); title != "" {
		fmt.Fprintf(logWriter, "Title: %s\n", title)
	}
	if date := timestampString(record["createdAt"]); date != "" {
		fmt.Fprintf(logWriter, "Posted: %s\n", date)
	}
	if score := intField(record, "score", "upVotes"); score != nil {
		fmt.Fprintf(logWriter, "Score: %d\n", *score)
	}
}
