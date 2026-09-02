package apify

import (
	"context"
	"fmt"
	"io"
	"strings"

	"gorm.io/gorm"

	"arker/internal/archivers"
	"arker/internal/models"
	"arker/internal/utils"
)

// SupportsStoredVideoMetadata reports whether Apify can resolve a post
// record without buying or replacing media bytes. Every supported platform
// has an actor input that returns the record alone; YouTube uses the
// metadata scraper and never the downloader.
func (c *Client) SupportsStoredVideoMetadata(targetURL string) bool {
	if c == nil {
		return false
	}
	// Vimeo's official oEmbed endpoint is public and metadata-only, so it is
	// available even when the paid Apify fallback is disabled.
	if isVimeoMetadataURL(targetURL) {
		return true
	}
	if !c.Enabled() {
		return false
	}
	switch {
	case utils.IsInstagramURL(targetURL), utils.IsTikTokURL(targetURL), utils.IsFacebookURL(targetURL):
		return true
	case utils.IsYouTubeURL(targetURL):
		return ExtractYouTubeVideoID(targetURL) != ""
	default:
		return false
	}
}

// RefreshStoredVideoMetadata resolves only structured metadata for historical
// video bytes Arker already holds. It is intentionally separate from
// ArchiveFallback: re-downloading the media merely to recover sidecars would
// be slower, costlier, and capable of silently changing the archived
// artifact. The worker probes the stored object after this returns, so
// byte-derived duration and dimensions remain authoritative.
func (c *Client) RefreshStoredVideoMetadata(ctx context.Context, targetURL string, logWriter io.Writer, db *gorm.DB, itemID uint, media archivers.VideoMedia) (archivers.Result, error) {
	if !c.SupportsStoredVideoMetadata(targetURL) {
		return archivers.Result{}, fmt.Errorf("no metadata-only Apify path for %s", targetURL)
	}
	if logWriter == nil {
		logWriter = io.Discard
	}
	shortID := shortIDForItem(db, itemID)
	usage := &models.FallbackUsage{ArchiveItemID: itemID, ShortID: shortID, URL: targetURL}

	var (
		meta    *archivers.VideoMetadata
		record  map[string]any
		product string
		err     error
	)
	switch {
	case isVimeoMetadataURL(targetURL):
		return c.refreshStoredVimeoMetadata(ctx, targetURL, logWriter, media)
	case utils.IsInstagramURL(targetURL):
		product = ActorInstagram
		record, _, err = c.resolveRecord(ctx, db, usage, ActorInstagram, instagramInput{PostURLs: []string{instagramCanonicalURL(targetURL)}}, logWriter, "code")
		if err == nil {
			logInstagramMetadata(logWriter, record)
			meta = instagramVideoMetadata(record, targetURL)
		}
	case utils.IsTikTokURL(targetURL):
		product = ActorTikTok
		record, _, err = c.resolveRecord(ctx, db, usage, ActorTikTok, tiktokMetadataInput(targetURL), logWriter, "id")
		if err == nil {
			logTikTokMetadata(logWriter, record)
			meta = tiktokVideoMetadata(record, targetURL)
		}
	case utils.IsFacebookURL(targetURL):
		product = ActorFacebook
		var post facebookPost
		post, record, err = c.facebookResolve(ctx, usage, targetURL, db, logWriter)
		if err == nil {
			logFacebookMetadata(logWriter, post)
			meta = facebookVideoMetadata(post, targetURL, "")
		}
	case utils.IsYouTubeURL(targetURL):
		product = ActorYouTubeMetadata
		meta, record, err = c.youtubeStoredMetadata(ctx, usage, targetURL, db, logWriter)
	}
	if err != nil {
		return archivers.Result{}, err
	}

	metadata, raw, err := metadataOnlyVideoArtifacts(meta, record, media, strings.ReplaceAll(product, "~", "/"))
	if err != nil {
		usage.Detail = truncate("metadata build failed: "+err.Error(), 500)
		c.recordUsage(db, usage)
		return archivers.Result{}, err
	}
	usage.Success = true
	usage.Detail = "metadata-only historical repair"
	c.recordUsage(db, usage)
	return metadataOnlyResult(metadata, raw), nil
}

// youtubeStoredMetadata runs only the metadata scraper, with oEmbed as the
// free last resort, and never the downloader.
func (c *Client) youtubeStoredMetadata(ctx context.Context, usage *models.FallbackUsage, targetURL string, db *gorm.DB, logWriter io.Writer) (*archivers.VideoMetadata, map[string]any, error) {
	videoID := ExtractYouTubeVideoID(targetURL)
	watchURL := youtubeWatchURL(videoID)
	facts, _, err := c.resolveRecord(ctx, db, usage, ActorYouTubeMetadata, youtubeScrapeInput{
		StartURLs:        []youtubeStartURL{{URL: watchURL}},
		MaxResults:       1,
		MaxResultsShorts: 1,
	}, logWriter, "id")
	if err != nil {
		fmt.Fprintf(logWriter, "YouTube metadata scrape failed (%v); falling back to oEmbed for title and channel\n", err)
		facts = c.youtubeOEmbed(ctx, watchURL, logWriter)
		if facts == nil {
			return nil, nil, err
		}
		// The scraper's usage row is already recorded as a failure; the
		// oEmbed answer is free, so the refresh is tracked on a fresh row.
		*usage = models.FallbackUsage{ArchiveItemID: usage.ArchiveItemID, ShortID: usage.ShortID, URL: usage.URL}
	}
	logYouTubeMetadata(logWriter, facts)
	meta := youtubeVideoMetadata(facts, nil, targetURL, videoID)
	meta.Provider = "apify:" + strings.ReplaceAll(ActorYouTubeMetadata, "~", "/")
	meta.Media = archivers.VideoMedia{}
	return meta, youtubeRawRecord(nil, facts), nil
}

func metadataOnlyResult(metadata, raw *archivers.Sidecar) archivers.Result {
	return archivers.Result{
		Extension:    ".mp4",
		ContentType:  "video/mp4",
		Source:       models.ArchiveSourceApify,
		Metadata:     metadata,
		RawMetadata:  raw,
		Completeness: archivers.CompletenessComplete,
	}
}
