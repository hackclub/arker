package apify

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"gorm.io/gorm"

	"arker/internal/archivers"
	"arker/internal/models"
	"arker/internal/utils"
)

// Conservative per-record upper bounds for a poster-only actor run, in USD.
// They feed the thumbnail backfill's spend cap; the usage row records the
// actual charge afterwards.
const (
	instagramRecordCostUSD = 0.005
	tiktokRecordCostUSD    = 0.01
	facebookRecordCostUSD  = 0.01
)

// SupportsSocialThumbnail reports whether ResolveSocialThumbnail has a real
// poster-only path for this URL. YouTube posters are public and need no
// actor, so they are supported even with no token configured.
func (c *Client) SupportsSocialThumbnail(targetURL, itemType string) bool {
	if c == nil {
		return false
	}
	if utils.IsYouTubeURL(targetURL) {
		return ExtractYouTubeVideoID(targetURL) != ""
	}
	if !c.Enabled() {
		return false
	}
	return utils.IsInstagramURL(targetURL) || utils.IsTikTokURL(targetURL) ||
		utils.IsTikTokPhotoPostURL(targetURL) || utils.IsFacebookURL(targetURL) ||
		utils.IsFacebookPostURL(targetURL)
}

// SocialThumbnailCostUSD returns an upper bound for resolving one poster.
func (c *Client) SocialThumbnailCostUSD(targetURL, itemType string) float64 {
	if c == nil || !c.Enabled() || utils.IsYouTubeURL(targetURL) {
		return 0
	}
	switch {
	case utils.IsInstagramURL(targetURL):
		return instagramRecordCostUSD
	case utils.IsTikTokURL(targetURL), utils.IsTikTokPhotoPostURL(targetURL):
		return tiktokRecordCostUSD
	case utils.IsFacebookURL(targetURL), utils.IsFacebookPostURL(targetURL):
		return facebookRecordCostUSD
	}
	return 0
}

// ResolveSocialThumbnail resolves and downloads only the published poster
// for a historical social item. It never downloads the archived media. Actor
// runs are recorded through the same usage path as an archive so the
// backfill's paid work stays visible.
//
// Errors wrapped in ErrSocialThumbnailUnavailable are conclusive (the post
// is gone or has no poster); everything else is retryable.
func (c *Client) ResolveSocialThumbnail(ctx context.Context, targetURL, itemType string, logWriter io.Writer, db *gorm.DB, itemID uint) (*archivers.Thumbnail, error) {
	if c == nil {
		return nil, fmt.Errorf("Apify thumbnail resolver is not configured")
	}
	if logWriter == nil {
		logWriter = io.Discard
	}

	if utils.IsYouTubeURL(targetURL) {
		return c.youtubePosterThumbnail(ctx, targetURL, logWriter)
	}
	if !c.Enabled() {
		return nil, fmt.Errorf("Apify thumbnail resolver is disabled")
	}

	actorID, input, idKey, posterURL := socialThumbnailPlan(targetURL)
	if actorID == "" {
		return nil, fmt.Errorf("%w: no provider poster resolver for this platform", archivers.ErrSocialThumbnailUnavailable)
	}

	usage := &models.FallbackUsage{ArchiveItemID: itemID, ShortID: shortIDForItem(db, itemID), URL: targetURL}
	record, _, err := c.resolveRecord(ctx, db, usage, actorID, input, logWriter, idKey)
	if err != nil {
		if errors.Is(err, errNotFound) {
			return nil, fmt.Errorf("%w: %v", archivers.ErrSocialThumbnailUnavailable, err)
		}
		return nil, err
	}
	imageURL := posterURL(record)
	if imageURL == "" {
		err := errors.New("provider record has no published poster")
		usage.Detail = truncate("thumbnail backfill: "+err.Error(), 500)
		c.recordUsage(db, usage)
		return nil, fmt.Errorf("%w: %v", archivers.ErrSocialThumbnailUnavailable, err)
	}
	thumb, err := c.thumbnailFromURLStrict(ctx, imageURL, logWriter)
	if err != nil {
		usage.Detail = truncate("thumbnail backfill poster download failed: "+err.Error(), 500)
		c.recordUsage(db, usage)
		if strings.Contains(err.Error(), "media download returned 404") {
			return nil, fmt.Errorf("%w: %v", archivers.ErrSocialThumbnailUnavailable, err)
		}
		return nil, err
	}
	usage.Success = true
	usage.Detail = fmt.Sprintf("thumbnail backfill: poster %dx%d, %d bytes", thumb.Width, thumb.Height, len(thumb.Data))
	c.recordUsage(db, usage)
	return thumb, nil
}

// youtubePosterThumbnail fetches the public poster: the authored
// maximum-resolution image first, then the always-present hqdefault.
func (c *Client) youtubePosterThumbnail(ctx context.Context, targetURL string, logWriter io.Writer) (*archivers.Thumbnail, error) {
	videoID := ExtractYouTubeVideoID(targetURL)
	if videoID == "" {
		return nil, fmt.Errorf("%w: could not extract YouTube video ID", archivers.ErrSocialThumbnailUnavailable)
	}
	var lastErr error
	for _, name := range []string{"maxresdefault.jpg", "hqdefault.jpg"} {
		thumb, err := c.thumbnailFromURLStrict(ctx, "https://i.ytimg.com/vi/"+videoID+"/"+name, logWriter)
		if err == nil {
			return thumb, nil
		}
		lastErr = err
		if !strings.Contains(err.Error(), "media download returned 404") {
			return nil, err
		}
	}
	return nil, fmt.Errorf("%w: YouTube poster not found: %v", archivers.ErrSocialThumbnailUnavailable, lastErr)
}

// socialThumbnailPlan picks the metadata-only actor run for a platform and
// the poster field to read from its record.
func socialThumbnailPlan(targetURL string) (actorID string, input any, idKey string, posterURL func(map[string]any) string) {
	switch {
	case utils.IsInstagramURL(targetURL):
		return ActorInstagram, instagramInput{PostURLs: []string{instagramCanonicalURL(targetURL)}}, "code", instagramPosterURL
	case utils.IsTikTokURL(targetURL), utils.IsTikTokPhotoPostURL(targetURL):
		return ActorTikTok, tiktokMetadataInput(targetURL), "id", tiktokPosterURL
	case utils.IsFacebookURL(targetURL), utils.IsFacebookPostURL(targetURL):
		return ActorFacebook, facebookRunInput(targetURL), "postId", func(record map[string]any) string {
			return parseFacebookRecord(record, io.Discard).Poster
		}
	}
	return "", nil, "", nil
}

// instagramPosterURL is the post's cover: the video's poster frame, or the
// first still.
func instagramPosterURL(record map[string]any) string {
	if u := instagramImageURL(record); u != "" {
		return u
	}
	for _, entry := range instagramMediaEntries(record) {
		if !entry.isVideo() {
			return entry.URL
		}
	}
	return ""
}

// tiktokMetadataInput runs the TikTok actor without asking it to copy any
// media into its store: the record's own fields are all a metadata-only
// caller needs, and the copies are what make the full run cost more.
func tiktokMetadataInput(targetURL string) tiktokInput {
	return tiktokInput{PostURLs: []string{targetURL}, ResultsPerPage: 1}
}
