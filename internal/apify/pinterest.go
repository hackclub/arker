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

// Pinterest goes through silentflow/pinterest-scraper-ppr with includeDetails
// so the closeup description and pinner arrive with the pin. A deleted pin
// is absent from the dataset (and not billed): "no item" is not-found.

type pinterestInput struct {
	StartURLs      []string `json:"startUrls"`
	IncludeDetails bool     `json:"includeDetails"`
	MaxItems       int      `json:"maxItems"`
}

func pinterestRunInput(targetURL string) pinterestInput {
	return pinterestInput{StartURLs: []string{targetURL}, IncludeDetails: true, MaxItems: 10}
}

func (c *Client) archivePinterest(ctx context.Context, targetURL, itemType string, logWriter io.Writer, db *gorm.DB, itemID uint, shortID string) (archivers.Result, error) {
	if !utils.ArchiveTypesEqual(itemType, utils.ArchiveTypeGalleryDl) {
		return archivers.Result{}, fmt.Errorf("no Pinterest fallback for archive type %s", itemType)
	}

	usage := &models.FallbackUsage{ArchiveItemID: itemID, ShortID: shortID, URL: targetURL}
	record, _, err := c.resolveRecord(ctx, db, usage, ActorPinterest, pinterestRunInput(targetURL), logWriter, "id")
	if err != nil {
		return archivers.Result{}, err
	}

	entries := pinterestMediaEntries(record)
	if len(entries) == 0 {
		err := fmt.Errorf("Pinterest record for %s contains no media", targetURL)
		usage.Detail = truncate(err.Error(), 500)
		c.recordUsage(db, usage)
		return archivers.Result{}, err
	}
	logPinterestMetadata(logWriter, record)

	meta := pinterestGalleryMetadata(record, targetURL)
	result, err := c.archiveGallery(ctx, galleryPlan{
		Entries:   entries,
		Meta:      meta,
		Record:    record,
		PosterURL: pinterestImageURL(record),
		Fetch:     c.pinterestFetch(usage, logWriter),
	}, usage, db, logWriter)
	if err != nil {
		return archivers.Result{}, err
	}
	if len(entries) == 1 && entries[0].isVideo() {
		if err := attachSingleVideoMetadata(&result, meta, record, pinterestVideoMetadata(record, targetURL), ActorPinterest); err != nil {
			closeResultData(result)
			return archivers.Result{}, fmt.Errorf("failed to build Pinterest video metadata: %w", err)
		}
	}
	return result, nil
}

// pinterestFetch downloads stills directly. Videos are published as an HLS
// master; Pinterest also serves the same content as a progressive MP4 under
// a derivable path, which is tried first because it needs no remux.
func (c *Client) pinterestFetch(usage *models.FallbackUsage, logWriter io.Writer) mediaFetcher {
	direct := c.kvsCountingFetch(usage)
	return func(ctx context.Context, entry mediaEntry, dest string) (int64, error) {
		if !entry.isVideo() || !isStreamManifest(entry.URL) {
			return direct(ctx, entry, dest)
		}
		if mp4 := pinterestProgressiveURL(entry.URL); mp4 != "" {
			size, err := direct(ctx, mediaEntry{URL: mp4, Type: "Video"}, dest)
			if err == nil {
				err = verifyMP4(dest)
			}
			if err == nil {
				return size, nil
			}
			removeFile(dest)
			fmt.Fprintf(logWriter, "Pinterest progressive MP4 unavailable (%v); remuxing HLS instead\n", err)
		}
		variant, err := c.bestHLSVariant(ctx, entry.URL)
		if err != nil {
			return 0, err
		}
		return remuxStream(ctx, variant, dest)
	}
}

// pinterestHLSPattern matches the HLS master path, capturing the hashed
// directory and file name that the progressive rendition shares.
var pinterestHLSPattern = regexp.MustCompile(`^https://v\d*\.pinimg\.com/videos/[^/]+/hls/(?:v\d+/)?([0-9a-f]{2}/[0-9a-f]{2}/[0-9a-f]{2}/[0-9a-f]+)\.m3u8`)

// pinterestProgressiveURL derives the 720p progressive MP4 from an HLS URL:
// https://v1.pinimg.com/videos/iht/hls/v2/c3/ba/ec/<hash>.m3u8
//
//	-> https://v1.pinimg.com/videos/mc/720p/c3/ba/ec/<hash>.mp4
func pinterestProgressiveURL(hlsURL string) string {
	m := pinterestHLSPattern.FindStringSubmatch(hlsURL)
	if m == nil {
		return ""
	}
	return "https://v1.pinimg.com/videos/mc/720p/" + m[1] + ".mp4"
}

func pinterestImageURL(record map[string]any) string {
	images := nestedObject(record, "imageUrls")
	return firstNonEmptyString(stringField(images, "original"), stringField(images, "736x"), stringField(record, "imageUrl"))
}

func pinterestVideoURL(record map[string]any) string {
	if u := stringField(record, "videoUrl"); u != "" {
		return u
	}
	for _, rendition := range nestedObject(record, "videos", "videoList") {
		obj, _ := rendition.(map[string]any)
		if u := stringField(obj, "url"); u != "" {
			return u
		}
	}
	return ""
}

// pinterestMediaEntries resolves the pin's media: its video when it is one,
// otherwise its image. A video pin's image is the cover, not a second slide.
func pinterestMediaEntries(record map[string]any) []mediaEntry {
	if isVideo := boolField(record, "isVideo"); isVideo != nil && *isVideo {
		if u := pinterestVideoURL(record); u != "" {
			return []mediaEntry{{URL: u, Type: "Video"}}
		}
	}
	if u := pinterestImageURL(record); u != "" {
		return []mediaEntry{{URL: u, Type: "Photo"}}
	}
	return nil
}

func pinterestTitle(record map[string]any) string {
	return firstNonEmptyString(stringField(record, "title"), stringField(record, "gridTitle"), stringField(record, "seoTitle"))
}

func pinterestDescription(record map[string]any) string {
	return firstNonEmptyString(stringField(record, "description"), stringField(record, "closeupDescription"), stringField(record, "seoDescription"))
}

func pinterestGalleryMetadata(record map[string]any, sourceURL string) *archivers.GalleryMetadata {
	pinner := nestedObject(record, "pinner")
	return &archivers.GalleryMetadata{
		SourceURL:   sourceURL,
		Extractor:   "pinterest",
		Subcategory: "apify",
		PostID:      stringField(record, "id"),
		PostURL:     stringField(record, "url"),
		Author:      stringField(pinner, "username"),
		AuthorName:  stringField(pinner, "fullName"),
		Title:       pinterestTitle(record),
		Description: pinterestDescription(record),
		Date:        timestampString(record["createdAt"]),
		Likes:       intField(record, "saves", "reactions", "repinCount"),
		Comments:    intField(record, "comments"),
		ToolVersion: "apify:" + strings.ReplaceAll(ActorPinterest, "~", "/"),
		ArchivedAt:  time.Now().UTC().Format(time.RFC3339),
	}
}

func pinterestVideoMetadata(record map[string]any, sourceURL string) *archivers.VideoMetadata {
	pinner := nestedObject(record, "pinner")
	return &archivers.VideoMetadata{
		SourceURL:            archivers.SanitizeURL(sourceURL, nil),
		Platform:             "pinterest",
		Extractor:            "pinterest",
		PostID:               stringField(record, "id"),
		CanonicalURL:         archivers.SanitizeURL(firstNonEmptyString(stringField(record, "url"), sourceURL), nil),
		Title:                pinterestTitle(record),
		Description:          pinterestDescription(record),
		Author:               stringField(pinner, "fullName"),
		AuthorID:             stringField(pinner, "id"),
		Uploader:             stringField(pinner, "username"),
		UploaderID:           stringField(pinner, "id"),
		PublicationTimestamp: timestampString(record["createdAt"]),
		Engagement: archivers.VideoEngagement{
			Likes:    intField(record, "saves", "reactions", "repinCount"),
			Comments: intField(record, "comments"),
		},
		Media: archivers.VideoMedia{
			Width:  intField(record, "width"),
			Height: intField(record, "height"),
		},
	}
}

func logPinterestMetadata(logWriter io.Writer, record map[string]any) {
	pinner := nestedObject(record, "pinner")
	if handle := stringField(pinner, "username"); handle != "" {
		fmt.Fprintf(logWriter, "Pinner: %s (%s)\n", handle, stringField(pinner, "fullName"))
	}
	if title := pinterestTitle(record); title != "" {
		fmt.Fprintf(logWriter, "Title: %s\n", utils.TruncateForLog(title, 300))
	}
	if date := timestampString(record["createdAt"]); date != "" {
		fmt.Fprintf(logWriter, "Created: %s\n", date)
	}
	if saves := intField(record, "saves", "reactions"); saves != nil {
		fmt.Fprintf(logWriter, "Saves: %d\n", *saves)
	}
}
