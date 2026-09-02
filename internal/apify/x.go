package apify

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"
	"time"

	"gorm.io/gorm"

	"arker/internal/archivers"
	"arker/internal/models"
	"arker/internal/utils"
)

// X goes through kaitoeasyapi's pay-per-result tweet scraper, addressed by
// tweet ID. Its datasets are padded with "mock_tweet" advertisement rows, so
// the post is matched by type and ID rather than taken as the first item; a
// missing or private tweet is silently absent.

type xInput struct {
	TweetIDs []string `json:"tweetIDs"`
}

var xStatusIDPattern = regexp.MustCompile(`(?i)/status(?:es)?/(\d+)`)

// ExtractXStatusID pulls the numeric tweet ID from a status URL.
func ExtractXStatusID(rawURL string) string {
	if m := xStatusIDPattern.FindStringSubmatch(rawURL); m != nil {
		return m[1]
	}
	return ""
}

func (c *Client) archiveX(ctx context.Context, targetURL, itemType string, logWriter io.Writer, db *gorm.DB, itemID uint, shortID string) (archivers.Result, error) {
	if !utils.ArchiveTypesEqual(itemType, utils.ArchiveTypeGalleryDl) {
		return archivers.Result{}, fmt.Errorf("no X fallback for archive type %s", itemType)
	}
	tweetID := ExtractXStatusID(targetURL)
	if tweetID == "" {
		return archivers.Result{}, fmt.Errorf("could not extract a status ID from %s", targetURL)
	}

	usage := &models.FallbackUsage{ArchiveItemID: itemID, ShortID: shortID, URL: targetURL}
	finished, err := c.runActor(ctx, db, usage, ActorX, xInput{TweetIDs: []string{tweetID}}, logWriter)
	if err != nil {
		return archivers.Result{}, err
	}
	record := xFindTweet(finished.Items, tweetID)
	if record == nil {
		err := fmt.Errorf("%w: X actor returned no tweet %s (deleted, private, or suspended account)", errNotFound, tweetID)
		usage.Detail = truncate(err.Error(), 500)
		c.recordUsage(db, usage)
		return archivers.Result{}, err
	}

	entries := xMediaEntries(record)
	if len(entries) == 0 {
		err := fmt.Errorf("X record for %s contains no media (a text-only post has nothing to archive)", targetURL)
		usage.Detail = truncate(err.Error(), 500)
		c.recordUsage(db, usage)
		return archivers.Result{}, err
	}
	logXMetadata(logWriter, record)

	meta := xGalleryMetadata(record, targetURL)
	result, err := c.archiveGallery(ctx, galleryPlan{
		Entries:   entries,
		Meta:      meta,
		Record:    record,
		PosterURL: xPosterURL(record),
	}, usage, db, logWriter)
	if err != nil {
		return archivers.Result{}, err
	}
	if len(entries) == 1 && entries[0].isVideo() {
		if err := attachSingleVideoMetadata(&result, meta, record, xVideoMetadata(record, targetURL, entries[0].URL), ActorX); err != nil {
			closeResultData(result)
			return archivers.Result{}, fmt.Errorf("failed to build X video metadata: %w", err)
		}
	}
	return result, nil
}

// xFindTweet picks the real tweet out of a dataset padded with mock rows.
func xFindTweet(items []map[string]any, tweetID string) map[string]any {
	for _, item := range items {
		if stringField(item, "type") != "tweet" {
			continue
		}
		if stringField(item, "id") == tweetID {
			return item
		}
	}
	return nil
}

// xMediaEntries resolves the tweet's own media from extendedEntities.media,
// in display order. Photos are requested at original size; videos and
// animated GIFs (which X serves as MP4) take the highest-bitrate MP4 variant.
// Quoted and retweeted tweets' media is deliberately excluded: it belongs to
// another post.
func xMediaEntries(record map[string]any) []mediaEntry {
	var entries []mediaEntry
	for _, item := range nestedList(record, "extendedEntities", "media") {
		media, _ := item.(map[string]any)
		switch stringField(media, "type") {
		case "photo":
			if u := xOriginalImageURL(stringField(media, "media_url_https")); u != "" {
				entries = append(entries, mediaEntry{URL: u, Type: "Photo"})
			}
		case "video", "animated_gif":
			if u := xBestVideoVariant(media); u != "" {
				entries = append(entries, mediaEntry{URL: u, Type: "Video"})
			}
		}
	}
	return entries
}

// xOriginalImageURL asks pbs.twimg.com for the full-resolution rendition.
func xOriginalImageURL(rawURL string) string {
	if rawURL == "" {
		return ""
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	q := parsed.Query()
	q.Set("name", "orig")
	parsed.RawQuery = q.Encode()
	return parsed.String()
}

func xBestVideoVariant(media map[string]any) string {
	var best string
	var bestBitrate float64 = -1
	for _, item := range nestedList(media, "video_info", "variants") {
		variant, _ := item.(map[string]any)
		if stringField(variant, "content_type") != "video/mp4" {
			continue
		}
		bitrate := 0.0
		if v := floatField(variant, "bitrate"); v != nil {
			bitrate = *v
		}
		if u := stringField(variant, "url"); u != "" && bitrate > bestBitrate {
			best, bestBitrate = u, bitrate
		}
	}
	return best
}

func xPosterURL(record map[string]any) string {
	for _, item := range nestedList(record, "extendedEntities", "media") {
		media, _ := item.(map[string]any)
		if u := stringField(media, "media_url_https"); u != "" {
			return u
		}
	}
	return ""
}

func xHashtags(record map[string]any) []string {
	tags := objectStrings(nestedList(record, "entities", "hashtags"), "text")
	if len(tags) == 0 {
		tags = hashtagsFromText(stringField(record, "text"))
	}
	return tags
}

func xGalleryMetadata(record map[string]any, sourceURL string) *archivers.GalleryMetadata {
	author := nestedObject(record, "author")
	return &archivers.GalleryMetadata{
		SourceURL:   sourceURL,
		Extractor:   "twitter",
		Subcategory: "apify",
		PostID:      stringField(record, "id"),
		PostURL:     firstNonEmptyString(stringField(record, "url"), stringField(record, "twitterUrl")),
		Author:      stringField(author, "userName"),
		AuthorName:  stringField(author, "name"),
		Description: stringField(record, "text"),
		Date:        timestampString(record["createdAt"]),
		Likes:       intField(record, "likeCount"),
		Views:       intField(record, "viewCount"),
		Comments:    intField(record, "replyCount"),
		Tags:        xHashtags(record),
		ToolVersion: "apify:" + strings.ReplaceAll(ActorX, "~", "/"),
		ArchivedAt:  time.Now().UTC().Format(time.RFC3339),
	}
}

func xVideoMetadata(record map[string]any, sourceURL, videoURL string) *archivers.VideoMetadata {
	author := nestedObject(record, "author")
	var duration *float64
	for _, item := range nestedList(record, "extendedEntities", "media") {
		media, _ := item.(map[string]any)
		if ms := floatField(nestedObject(media, "video_info"), "duration_millis"); ms != nil && *ms > 0 {
			seconds := *ms / 1000
			duration = &seconds
			break
		}
	}
	return &archivers.VideoMetadata{
		SourceURL:            archivers.SanitizeURL(sourceURL, nil),
		Platform:             "twitter",
		Extractor:            "twitter",
		PostID:               stringField(record, "id"),
		CanonicalURL:         archivers.SanitizeURL(firstNonEmptyString(stringField(record, "url"), sourceURL), nil),
		Title:                xVideoTitle(record),
		Description:          stringField(record, "text"),
		Author:               stringField(author, "name"),
		AuthorID:             stringField(author, "id"),
		Uploader:             stringField(author, "userName"),
		UploaderID:           stringField(author, "id"),
		PublicationTimestamp: timestampString(record["createdAt"]),
		DurationSeconds:      duration,
		Engagement: archivers.VideoEngagement{
			Views:    intField(record, "viewCount"),
			Likes:    intField(record, "likeCount"),
			Comments: intField(record, "replyCount"),
			Reposts:  intField(record, "retweetCount"),
		},
		Tags: xHashtags(record),
		Media: archivers.VideoMedia{
			QualityLabel: videoQualityLabel(videoURL),
		},
	}
}

func logXMetadata(logWriter io.Writer, record map[string]any) {
	author := nestedObject(record, "author")
	if handle := stringField(author, "userName"); handle != "" {
		fmt.Fprintf(logWriter, "Author: @%s (%s)\n", handle, stringField(author, "name"))
	}
	if text := stringField(record, "text"); text != "" {
		fmt.Fprintf(logWriter, "Text: %s\n", truncate(text, 200))
	}
	if date := timestampString(record["createdAt"]); date != "" {
		fmt.Fprintf(logWriter, "Posted: %s\n", date)
	}
	if likes := intField(record, "likeCount"); likes != nil {
		fmt.Fprintf(logWriter, "Likes: %d\n", *likes)
	}
}

// xVideoTitle names a video tweet the way yt-dlp does: the first line of the
// text, or the author when the tweet has none.
func xVideoTitle(record map[string]any) string {
	author := ""
	if handle := stringField(nestedObject(record, "author"), "userName"); handle != "" {
		author = "@" + handle
	}
	return titleFromText(stringField(record, "text"), author)
}
