package apify

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"gorm.io/gorm"

	"arker/internal/archivers"
	"arker/internal/models"
	"arker/internal/utils"
)

// Instagram is served by data-slayer/instagram-post-details, which returns
// Instagram's own media item for a post: carousel_media[] in swipe order,
// image_versions/video_versions per item, metrics, and — the field no other
// source could produce — music_metadata with a downloadable track.
//
// Media URLs are signed but not IP-locked, so the bytes come straight from
// Instagram's CDN and only the resolution is paid for.

type instagramInput struct {
	PostURLs []string `json:"postUrls"`
}

// archiveInstagram is the Instagram fallback entry point.
func (c *Client) archiveInstagram(ctx context.Context, targetURL, itemType string, logWriter io.Writer, db *gorm.DB, itemID uint, shortID string) (archivers.Result, error) {
	usage := &models.FallbackUsage{ArchiveItemID: itemID, ShortID: shortID, URL: targetURL}
	record, _, err := c.resolveRecord(ctx, db, usage, ActorInstagram, instagramInput{PostURLs: []string{instagramCanonicalURL(targetURL)}}, logWriter, "code")
	if err != nil {
		return archivers.Result{}, err
	}
	logInstagramMetadata(logWriter, record)

	switch itemType {
	case utils.ArchiveTypeYtDlp:
		return c.instagramVideo(ctx, targetURL, record, usage, db, logWriter)
	case utils.ArchiveTypeGalleryDl:
		return c.instagramGallery(ctx, targetURL, record, usage, db, logWriter)
	default:
		return archivers.Result{}, fmt.Errorf("no Instagram fallback for archive type %s", itemType)
	}
}

// instagramVideo produces the muxed MP4 for a reel or a single-video post.
func (c *Client) instagramVideo(ctx context.Context, targetURL string, record map[string]any, usage *models.FallbackUsage, db *gorm.DB, logWriter io.Writer) (archivers.Result, error) {
	videoURL := instagramVideoURL(record)
	if videoURL == "" {
		// A carousel that leads with a video is still one video to yt-dlp.
		for _, entry := range instagramMediaEntries(record) {
			if entry.isVideo() {
				videoURL = entry.URL
				break
			}
		}
	}
	return c.archiveVideo(ctx, videoPlan{
		URL:          videoURL,
		Metadata:     instagramVideoMetadata(record, targetURL),
		Record:       record,
		ThumbnailURL: stringField(record, "thumbnail_url"),
		TempPattern:  "arker-apify-ig-*.mp4",
	}, usage, db, logWriter)
}

// instagramGallery produces the gallery ZIP for a feed post.
func (c *Client) instagramGallery(ctx context.Context, targetURL string, record map[string]any, usage *models.FallbackUsage, db *gorm.DB, logWriter io.Writer) (archivers.Result, error) {
	return c.archiveGallery(ctx, galleryPlan{
		Entries: instagramMediaEntries(record),
		Audio:   instagramAudio(record),
		Meta:    instagramGalleryMetadata(record, targetURL),
		Record:  record,
		// /p/ URLs use the gallery route even when the post is one video; the
		// record's poster is the only correct preview for an all-video bundle.
		PosterURL: stringField(record, "thumbnail_url"),
	}, usage, db, logWriter)
}

var instagramShortcodePattern = regexp.MustCompile(`(?i)instagram\.com/(?:[^/]+/)?(?:p|reel|reels|tv)/([A-Za-z0-9_-]+)`)

// instagramCanonicalURL strips tracking parameters and profile prefixes so
// the actor sees the plain post URL. Reels and posts resolve identically
// through /p/, so one form is enough.
func instagramCanonicalURL(rawURL string) string {
	if m := instagramShortcodePattern.FindStringSubmatch(rawURL); m != nil {
		return "https://www.instagram.com/p/" + m[1] + "/"
	}
	return rawURL
}

// instagramVideoURL picks the post's own video rendition. Every observed
// record lists the same URL under several version types; the first is the
// highest quality the endpoint offers.
func instagramVideoURL(record map[string]any) string {
	if versions := nestedList(record, "video_versions"); len(versions) > 0 {
		if v, ok := versions[0].(map[string]any); ok {
			if u := stringField(v, "url"); u != "" {
				return u
			}
		}
	}
	return stringField(record, "video_url")
}

// instagramImageURL picks the largest still of a media item; image_versions
// items are ordered largest-first.
func instagramImageURL(item map[string]any) string {
	items := nestedList(item, "image_versions", "items")
	if len(items) == 0 {
		items = nestedList(item, "image_versions2", "candidates")
	}
	best, bestArea := "", int64(-1)
	for _, raw := range items {
		candidate, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		u := stringField(candidate, "url")
		if u == "" {
			continue
		}
		area := int64(0)
		if w, h := intField(candidate, "width"), intField(candidate, "height"); w != nil && h != nil {
			area = *w * *h
		}
		if area > bestArea {
			best, bestArea = u, area
		}
	}
	return best
}

// instagramMediaEntries lists the post's media in swipe order: the carousel
// items for a carousel (media_type 8), the post itself otherwise.
func instagramMediaEntries(record map[string]any) []mediaEntry {
	items := nestedList(record, "carousel_media")
	if len(items) == 0 {
		items = []any{record}
	}
	var entries []mediaEntry
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if mediaType := intField(item, "media_type"); (mediaType != nil && *mediaType == 2) || item["is_video"] == true {
			if u := instagramVideoURL(item); u != "" {
				entries = append(entries, mediaEntry{URL: u, Type: "Video"})
				continue
			}
		}
		if u := instagramImageURL(item); u != "" {
			entries = append(entries, mediaEntry{URL: u, Type: "Photo"})
		}
	}
	return entries
}

// instagramAudio reads the post's soundtrack. Feed posts carry it as
// music_metadata, reels as clips_metadata; both hold either music_info (a
// licensed track, downloadable) or original_sound_info (the poster's own
// audio, attribution only).
func instagramAudio(record map[string]any) *galleryAudio {
	container := nestedObject(record, "music_metadata")
	if container == nil {
		container = nestedObject(record, "clips_metadata")
	}
	if container == nil {
		return nil
	}
	audio := &galleryAudio{Music: archivers.GalleryMusic{Status: archivers.GalleryMusicMetadataOnly}}
	if asset := nestedObject(container, "music_info", "music_asset_info"); asset != nil {
		audio.Music.Title = stringField(asset, "title")
		audio.Music.Artist = stringField(asset, "display_artist")
		audio.Music.ID = stringField(asset, "audio_id", "id")
		audio.URL = stringField(asset, "progressive_download_url", "fast_start_progressive_download_url")
		if ms := floatField(asset, "duration_in_ms"); ms != nil && *ms > 0 {
			seconds := *ms / 1000
			audio.Music.DurationSeconds = &seconds
		}
		original := false
		audio.Music.Original = &original
	} else if sound := nestedObject(container, "original_sound_info"); sound != nil {
		audio.Music.Title = stringField(sound, "original_audio_title")
		audio.Music.Artist = stringField(nestedObject(sound, "ig_artist"), "username", "full_name")
		audio.Music.ID = stringField(sound, "audio_asset_id", "audio_id")
		audio.URL = stringField(sound, "progressive_download_url")
		if ms := floatField(sound, "duration_in_ms"); ms != nil && *ms > 0 {
			seconds := *ms / 1000
			audio.Music.DurationSeconds = &seconds
		}
		original := true
		audio.Music.Original = &original
	}
	if audio.Music.ID == "" {
		audio.Music.ID = stringField(container, "audio_canonical_id")
	}
	if audio.URL == "" && audio.Music.Title == "" && audio.Music.Artist == "" {
		return nil
	}
	return audio
}

// instagramVideoMusic is the soundtrack attribution of a reel.
func instagramVideoMusic(record map[string]any) *archivers.VideoMusic {
	audio := instagramAudio(record)
	if audio == nil {
		return nil
	}
	return &archivers.VideoMusic{Title: audio.Music.Title, Artist: audio.Music.Artist, ID: audio.Music.ID, Original: audio.Music.Original}
}

func instagramCaption(record map[string]any) string {
	if caption := nestedObject(record, "caption"); caption != nil {
		return stringField(caption, "text")
	}
	return stringField(record, "caption")
}

func instagramHashtags(record map[string]any) []string {
	if caption := nestedObject(record, "caption"); caption != nil {
		if tags := stringSlice(caption, "hashtags"); len(tags) > 0 {
			for i, tag := range tags {
				tags[i] = strings.TrimPrefix(tag, "#")
			}
			return tags
		}
	}
	return hashtagsFromText(instagramCaption(record))
}

func instagramAuthor(record map[string]any) (username, id, fullName string) {
	user := nestedObject(record, "user")
	if user == nil {
		user = nestedObject(record, "owner")
	}
	if user == nil {
		return "", "", ""
	}
	return stringField(user, "username"), stringField(user, "id", "pk", "fbid_v2"), stringField(user, "full_name")
}

func instagramPostURL(record map[string]any) string {
	if code := stringField(record, "code"); code != "" {
		return "https://www.instagram.com/p/" + code + "/"
	}
	return ""
}

func instagramMetrics(record map[string]any) map[string]any {
	if metrics := nestedObject(record, "metrics"); metrics != nil {
		return metrics
	}
	return record
}

// instagramVideoTitle follows yt-dlp's naming for the same post: "Video by
// <uploader>". Only a post that is exactly one video gets it.
func instagramVideoTitle(record map[string]any) string {
	if title := stringField(record, "title"); title != "" {
		return title
	}
	entries := instagramMediaEntries(record)
	if len(entries) != 1 || !entries[0].isVideo() {
		return ""
	}
	username, _, _ := instagramAuthor(record)
	if username == "" {
		return ""
	}
	return "Video by " + username
}

func instagramVideoMetadata(record map[string]any, sourceURL string) *archivers.VideoMetadata {
	username, userID, _ := instagramAuthor(record)
	metrics := instagramMetrics(record)
	meta := &archivers.VideoMetadata{
		SourceURL:            archivers.SanitizeURL(sourceURL, nil),
		Platform:             "instagram",
		Extractor:            "instagram",
		PostID:               stringField(record, "code"),
		CanonicalURL:         archivers.SanitizeURL(firstNonEmptyString(instagramPostURL(record), sourceURL), nil),
		Title:                instagramVideoTitle(record),
		Description:          instagramCaption(record),
		Author:               username,
		AuthorID:             userID,
		Uploader:             username,
		UploaderID:           userID,
		PublicationTimestamp: timestampString(firstNonNil(record["taken_at_date"], record["taken_at"])),
		DurationSeconds:      positiveFloat(floatField(record, "video_duration")),
		Engagement: archivers.VideoEngagement{
			Views:    intField(metrics, "play_count", "ig_play_count", "view_count"),
			Likes:    intField(metrics, "like_count"),
			Comments: intField(metrics, "comment_count"),
			Reposts:  intField(metrics, "repost_count"),
		},
		Tags: instagramHashtags(record),
		Media: archivers.VideoMedia{
			Width:  intField(record, "original_width"),
			Height: intField(record, "original_height"),
		},
		Music:    instagramVideoMusic(record),
		Provider: "apify:" + strings.ReplaceAll(ActorInstagram, "~", "/"),
	}
	return meta
}

func instagramGalleryMetadata(record map[string]any, sourceURL string) *archivers.GalleryMetadata {
	username, _, fullName := instagramAuthor(record)
	metrics := instagramMetrics(record)
	meta := &archivers.GalleryMetadata{
		SourceURL:   sourceURL,
		Extractor:   "instagram",
		Subcategory: "apify",
		PostID:      stringField(record, "code"),
		PostURL:     instagramPostURL(record),
		Author:      username,
		AuthorName:  fullName,
		Title:       instagramVideoTitle(record),
		Description: instagramCaption(record),
		Date:        timestampString(firstNonNil(record["taken_at_date"], record["taken_at"])),
		Tags:        instagramHashtags(record),
		ToolVersion: "apify:" + strings.ReplaceAll(ActorInstagram, "~", "/"),
		ArchivedAt:  time.Now().UTC().Format(time.RFC3339),
	}
	meta.Likes = intField(metrics, "like_count")
	meta.Views = intField(metrics, "play_count", "ig_play_count", "view_count")
	meta.Comments = intField(metrics, "comment_count")
	return meta
}

func logInstagramMetadata(logWriter io.Writer, record map[string]any) {
	if username, _, _ := instagramAuthor(record); username != "" {
		fmt.Fprintf(logWriter, "Author: %s\n", username)
	}
	if desc := instagramCaption(record); desc != "" {
		fmt.Fprintf(logWriter, "Caption: %s\n", utils.TruncateForLog(desc, 300))
	}
	if date := timestampString(record["taken_at_date"]); date != "" {
		fmt.Fprintf(logWriter, "Posted: %s\n", date)
	}
	if likes := intField(instagramMetrics(record), "like_count"); likes != nil {
		fmt.Fprintf(logWriter, "Likes: %d\n", *likes)
	}
}

func firstNonNil(values ...any) any {
	for _, v := range values {
		if v != nil {
			return v
		}
	}
	return nil
}
