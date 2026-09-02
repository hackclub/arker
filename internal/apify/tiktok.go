package apify

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"gorm.io/gorm"

	"arker/internal/archivers"
	"arker/internal/models"
	"arker/internal/utils"
)

// TikTok is served by clockworks/tiktok-video-scraper. TikTok's video CDN is
// IP-locked to whoever resolved the post, so the actor is asked to store the
// MP4 (and the slideshow stills) in its key-value store, and the bytes are
// pulled from there with the token. The stills' own CDN URLs are tried first
// because they are usually public; the store copy is the fallback.
//
// The soundtrack's playUrl is an ordinary CDN object (M4A or MP3, sniffed)
// and downloads directly.

type tiktokInput struct {
	PostURLs                     []string `json:"postURLs"`
	ResultsPerPage               int      `json:"resultsPerPage"`
	ShouldDownloadVideos         bool     `json:"shouldDownloadVideos"`
	ShouldDownloadCovers         bool     `json:"shouldDownloadCovers"`
	ShouldDownloadSlideshowImage bool     `json:"shouldDownloadSlideshowImages"`
}

func tiktokRunInput(targetURL string) tiktokInput {
	return tiktokInput{
		PostURLs:                     []string{targetURL},
		ResultsPerPage:               1,
		ShouldDownloadVideos:         true,
		ShouldDownloadCovers:         true,
		ShouldDownloadSlideshowImage: true,
	}
}

// archiveTikTok is the TikTok fallback entry point.
func (c *Client) archiveTikTok(ctx context.Context, targetURL, itemType string, logWriter io.Writer, db *gorm.DB, itemID uint, shortID string) (archivers.Result, error) {
	usage := &models.FallbackUsage{ArchiveItemID: itemID, ShortID: shortID, URL: targetURL}
	record, _, err := c.resolveRecord(ctx, db, usage, ActorTikTok, tiktokRunInput(targetURL), logWriter, "id")
	if err != nil {
		return archivers.Result{}, err
	}
	logTikTokMetadata(logWriter, record)

	switch {
	case utils.ArchiveTypesEqual(itemType, utils.ArchiveTypeYtDlp):
		return c.tiktokVideo(ctx, targetURL, record, usage, db, logWriter)
	case utils.ArchiveTypesEqual(itemType, utils.ArchiveTypeGalleryDl):
		return c.tiktokPhotos(ctx, targetURL, record, usage, db, logWriter)
	default:
		return archivers.Result{}, fmt.Errorf("no TikTok fallback for archive type %s", itemType)
	}
}

// tiktokVideo produces the MP4 for a TikTok video post.
func (c *Client) tiktokVideo(ctx context.Context, targetURL string, record map[string]any, usage *models.FallbackUsage, db *gorm.DB, logWriter io.Writer) (archivers.Result, error) {
	videoURL := tiktokVideoURL(record)
	if videoURL == "" {
		if isSlideshow := boolField(record, "isSlideshow"); isSlideshow != nil && *isSlideshow {
			// A photo post reached the yt-dlp route (a /video/ URL that is
			// really a slideshow). There is no MP4 to store; the gallery item
			// for the same capture holds the stills.
			err := fmt.Errorf("TikTok post %s is a photo slideshow, not a video", targetURL)
			usage.Detail = truncate(err.Error(), 500)
			c.recordUsage(db, usage)
			return archivers.Result{}, err
		}
	}
	return c.archiveVideo(ctx, videoPlan{
		URL:          videoURL,
		Metadata:     tiktokVideoMetadata(record, targetURL),
		Record:       record,
		ThumbnailURL: tiktokPosterURL(record),
		TempPattern:  "arker-apify-tt-*.mp4",
	}, usage, db, logWriter)
}

// tiktokPhotos produces a gallery ZIP for a TikTok photo post.
func (c *Client) tiktokPhotos(ctx context.Context, targetURL string, record map[string]any, usage *models.FallbackUsage, db *gorm.DB, logWriter io.Writer) (archivers.Result, error) {
	entries, alternates := tiktokImageEntries(record)
	if len(entries) == 0 {
		err := fmt.Errorf("Apify record for %s contains no slideshow images", targetURL)
		usage.Detail = truncate(err.Error(), 500)
		c.recordUsage(db, usage)
		return archivers.Result{}, err
	}
	return c.archiveGallery(ctx, galleryPlan{
		Entries:   entries,
		Audio:     tiktokAudio(record),
		Meta:      tiktokGalleryMetadata(record, targetURL),
		Record:    record,
		PosterURL: tiktokPosterURL(record),
		Fetch:     c.alternateFetch(usage, alternates, logWriter),
	}, usage, db, logWriter)
}

// alternateFetch downloads an entry from its own URL and, when the platform
// refuses, from the actor's stored copy. Key-value-store bytes count against
// the usage row either way.
func (c *Client) alternateFetch(usage *models.FallbackUsage, alternates map[string]string, logWriter io.Writer) mediaFetcher {
	counting := c.kvsCountingFetch(usage)
	return func(ctx context.Context, entry mediaEntry, dest string) (int64, error) {
		size, err := counting(ctx, entry, dest)
		if err == nil || ctx.Err() != nil {
			return size, err
		}
		alt := alternates[entry.URL]
		if alt == "" {
			return 0, err
		}
		fmt.Fprintf(logWriter, "Direct download refused (%v); using the actor's stored copy...\n", err)
		return counting(ctx, mediaEntry{URL: alt, Type: entry.Type}, dest)
	}
}

// tiktokVideoURL is the MP4 the actor stored: downloadAddr and mediaUrls[0]
// name the same record.
func tiktokVideoURL(record map[string]any) string {
	if u := stringField(nestedObject(record, "videoMeta"), "downloadAddr"); u != "" {
		return u
	}
	if urls := stringSlice(record, "mediaUrls"); len(urls) > 0 {
		return urls[0]
	}
	return ""
}

// tiktokImageEntries lists a slideshow's stills in order. The TikTok CDN
// link leads; the key-value-store copy is the alternate for that URL.
func tiktokImageEntries(record map[string]any) ([]mediaEntry, map[string]string) {
	var entries []mediaEntry
	alternates := map[string]string{}
	seen := map[string]bool{}
	for _, raw := range nestedList(record, "slideshowImageLinks") {
		slide, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		direct := stringField(slide, "tiktokLink")
		stored := stringField(slide, "downloadLink")
		primary := firstNonEmptyString(direct, stored)
		if primary == "" || seen[primary] {
			continue
		}
		seen[primary] = true
		if direct != "" && stored != "" {
			alternates[direct] = stored
		}
		entries = append(entries, mediaEntry{URL: primary, Type: "Photo"})
	}
	return entries, alternates
}

// tiktokPosterURL prefers the platform's own cover over the stored copy so
// the poster costs no key-value-store transfer.
func tiktokPosterURL(record map[string]any) string {
	meta := nestedObject(record, "videoMeta")
	return firstNonEmptyString(stringField(meta, "originalCoverUrl"), stringField(meta, "coverUrl"))
}

// tiktokAudio reads the sound attribution and its downloadable track.
func tiktokAudio(record map[string]any) *galleryAudio {
	music := nestedObject(record, "musicMeta")
	if music == nil {
		return nil
	}
	audio := &galleryAudio{
		URL: stringField(music, "playUrl"),
		Music: archivers.GalleryMusic{
			Title:    stringField(music, "musicName"),
			Artist:   stringField(music, "musicAuthor"),
			ID:       stringField(music, "musicId"),
			Original: boolField(music, "musicOriginal"),
			Status:   archivers.GalleryMusicMetadataOnly,
		},
	}
	if audio.URL == "" && audio.Music.Title == "" && audio.Music.Artist == "" {
		return nil
	}
	return audio
}

// tiktokVideoMusic is the sound attribution for a video, whose audio is
// already mixed into the MP4.
func tiktokVideoMusic(record map[string]any) *archivers.VideoMusic {
	audio := tiktokAudio(record)
	if audio == nil || (audio.Music.Title == "" && audio.Music.Artist == "") {
		return nil
	}
	return &archivers.VideoMusic{
		Title:    audio.Music.Title,
		Artist:   audio.Music.Artist,
		Album:    stringField(nestedObject(record, "musicMeta"), "musicAlbum"),
		ID:       audio.Music.ID,
		Original: audio.Music.Original,
	}
}

func tiktokAuthor(record map[string]any) (username, id, nickname string) {
	author := nestedObject(record, "authorMeta")
	return stringField(author, "name"), stringField(author, "id"), stringField(author, "nickName")
}

func tiktokPostURL(record map[string]any) string {
	return stringField(record, "webVideoUrl", "submittedVideoUrl")
}

func tiktokHashtags(record map[string]any) []string {
	tags := objectStrings(nestedList(record, "hashtags"), "name")
	if len(tags) == 0 {
		return hashtagsFromText(stringField(record, "text"))
	}
	return tags
}

func tiktokDate(record map[string]any) string {
	return timestampString(firstNonNil(record["createTimeISO"], record["createTime"]))
}

func tiktokGalleryMetadata(record map[string]any, sourceURL string) *archivers.GalleryMetadata {
	username, _, nickname := tiktokAuthor(record)
	meta := &archivers.GalleryMetadata{
		SourceURL:   sourceURL,
		Extractor:   "tiktok",
		Subcategory: "apify",
		PostID:      stringField(record, "id"),
		PostURL:     tiktokPostURL(record),
		Author:      username,
		AuthorName:  nickname,
		Description: stringField(record, "text"),
		Date:        tiktokDate(record),
		Tags:        tiktokHashtags(record),
		ToolVersion: "apify:" + strings.ReplaceAll(ActorTikTok, "~", "/"),
		ArchivedAt:  time.Now().UTC().Format(time.RFC3339),
	}
	meta.Likes = intField(record, "diggCount")
	meta.Views = intField(record, "playCount")
	meta.Comments = intField(record, "commentCount")
	return meta
}

func tiktokVideoMetadata(record map[string]any, sourceURL string) *archivers.VideoMetadata {
	username, userID, nickname := tiktokAuthor(record)
	videoMeta := nestedObject(record, "videoMeta")
	return &archivers.VideoMetadata{
		SourceURL:            archivers.SanitizeURL(sourceURL, nil),
		Platform:             "tiktok",
		Extractor:            "tiktok",
		PostID:               stringField(record, "id"),
		CanonicalURL:         archivers.SanitizeURL(firstNonEmptyString(tiktokPostURL(record), sourceURL), nil),
		Title:                titleFromText(stringField(record, "text"), "@"+username),
		Description:          stringField(record, "text"),
		Author:               username,
		AuthorID:             userID,
		Uploader:             username,
		UploaderID:           userID,
		Channel:              nickname,
		PublicationTimestamp: tiktokDate(record),
		DurationSeconds:      positiveFloat(floatField(videoMeta, "duration")),
		Engagement: archivers.VideoEngagement{
			Views:    intField(record, "playCount"),
			Likes:    intField(record, "diggCount"),
			Comments: intField(record, "commentCount"),
			Reposts:  intField(record, "shareCount"),
		},
		Tags:  tiktokHashtags(record),
		Music: tiktokVideoMusic(record),
		Media: archivers.VideoMedia{
			Width:        positiveInt(intField(videoMeta, "width")),
			Height:       positiveInt(intField(videoMeta, "height")),
			QualityLabel: stringField(videoMeta, "definition"),
		},
		Provider: "apify:" + strings.ReplaceAll(ActorTikTok, "~", "/"),
	}
}

func logTikTokMetadata(logWriter io.Writer, record map[string]any) {
	if username, _, _ := tiktokAuthor(record); username != "" {
		fmt.Fprintf(logWriter, "Author: %s\n", username)
	}
	if desc := stringField(record, "text"); desc != "" {
		fmt.Fprintf(logWriter, "Caption: %s\n", utils.TruncateForLog(desc, 300))
	}
	if date := tiktokDate(record); date != "" {
		fmt.Fprintf(logWriter, "Posted: %s\n", date)
	}
	for _, counter := range []struct{ label, key string }{
		{"Plays", "playCount"}, {"Likes", "diggCount"}, {"Comments", "commentCount"}, {"Shares", "shareCount"},
	} {
		if value := intField(record, counter.key); value != nil {
			fmt.Fprintf(logWriter, "%s: %d\n", counter.label, *value)
		}
	}
}

// positiveInt drops the zero a record uses for "unknown".
func positiveInt(value *int64) *int64 {
	if value == nil || *value <= 0 {
		return nil
	}
	return value
}
