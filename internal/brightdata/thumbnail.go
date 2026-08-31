package brightdata

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

// SupportsSocialThumbnail reports whether ResolveSocialThumbnail has a real
// poster-only path for this URL. Keeping it separate from the cost estimate
// distinguishes free YouTube poster URLs from unsupported platforms.
func (c *Client) SupportsSocialThumbnail(targetURL, itemType string) bool {
	if c == nil {
		return false
	}
	if utils.IsYouTubeURL(targetURL) {
		return true
	}
	if !c.Enabled() {
		return false
	}
	return utils.IsInstagramURL(targetURL) || utils.IsTikTokURL(targetURL) ||
		utils.IsTikTokPhotoPostURL(targetURL) || utils.IsFacebookURL(targetURL) ||
		utils.IsFacebookPostURL(targetURL)
}

// SocialThumbnailCostUSD returns a conservative upper bound for resolving one
// provider poster. YouTube posters are public and constructed from the video
// ID, so they cost nothing. Instagram reels may need both marketplace datasets;
// the remaining supported platforms use one record.
func (c *Client) SocialThumbnailCostUSD(targetURL, itemType string) float64 {
	if c == nil || utils.IsYouTubeURL(targetURL) {
		return 0
	}
	if !c.Enabled() {
		return 0
	}
	if utils.IsInstagramURL(targetURL) {
		if strings.Contains(strings.ToLower(targetURL), "/reel/") {
			return 2 * c.cfg.ScraperCostPerRecord
		}
		return c.cfg.ScraperCostPerRecord
	}
	if utils.IsTikTokURL(targetURL) || utils.IsTikTokPhotoPostURL(targetURL) ||
		utils.IsFacebookURL(targetURL) || utils.IsFacebookPostURL(targetURL) {
		return c.cfg.ScraperCostPerRecord
	}
	return 0
}

// ResolveSocialThumbnail resolves and downloads only the published poster for
// a historical social item. It never downloads the archived media. Dataset
// calls are recorded through the same BrightDataUsage path as an archive so
// the backfill's paid work stays visible and enforceable.
func (c *Client) ResolveSocialThumbnail(ctx context.Context, targetURL, itemType string, logWriter io.Writer, db *gorm.DB, itemID uint) (*archivers.Thumbnail, error) {
	if c == nil {
		return nil, fmt.Errorf("Bright Data thumbnail resolver is not configured")
	}
	if logWriter == nil {
		logWriter = io.Discard
	}

	// YouTube's poster CDN is public. Try the authored maximum-resolution image
	// first, then the universally available high-quality variant. This fallback
	// does not use the Bright Data API and therefore has zero provider cost.
	if utils.IsYouTubeURL(targetURL) {
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
			// A missing maxres image is normal; hqdefault is the fallback.
			if !strings.Contains(err.Error(), "media download returned 404") {
				return nil, err
			}
		}
		return nil, fmt.Errorf("%w: YouTube poster not found: %v", archivers.ErrSocialThumbnailUnavailable, lastErr)
	}

	if !c.Enabled() {
		return nil, fmt.Errorf("Bright Data thumbnail resolver is disabled")
	}

	datasets, posterURL := socialThumbnailDatasets(targetURL)
	if len(datasets) == 0 {
		return nil, fmt.Errorf("%w: no provider poster resolver for this platform", archivers.ErrSocialThumbnailUnavailable)
	}

	shortID := shortIDForItem(db, itemID)
	var lastErr error
	for _, datasetID := range datasets {
		usage := &models.BrightDataUsage{ArchiveItemID: itemID, ShortID: shortID}
		record, err := c.resolveRecord(ctx, db, usage, datasetID, targetURL, logWriter)
		if err != nil {
			lastErr = err
			continue
		}
		imageURL := posterURL(record)
		if imageURL == "" {
			lastErr = fmt.Errorf("provider record has no published poster")
			usage.Detail = truncate("thumbnail backfill: "+lastErr.Error(), 500)
			c.recordUsage(db, usage)
			continue
		}
		thumb, err := c.thumbnailFromURLStrict(ctx, imageURL, logWriter)
		if err != nil {
			lastErr = err
			usage.Detail = truncate("thumbnail backfill poster download failed: "+err.Error(), 500)
			c.recordUsage(db, usage)
			continue
		}
		usage.Success = true
		usage.Detail = fmt.Sprintf("thumbnail backfill: poster %dx%d, %d bytes", thumb.Width, thumb.Height, len(thumb.Data))
		c.recordUsage(db, usage)
		return thumb, nil
	}

	if lastErr == nil {
		lastErr = errors.New("no provider record returned a poster")
	}
	// A completed dataset lookup with no poster/404 is conclusive. Transport,
	// auth, timeout and snapshot errors remain retryable.
	if strings.Contains(lastErr.Error(), "no published poster") ||
		strings.Contains(lastErr.Error(), "media download returned 404") ||
		strings.Contains(lastErr.Error(), "snapshot contained no records") ||
		strings.Contains(lastErr.Error(), "error record") {
		return nil, fmt.Errorf("%w: %v", archivers.ErrSocialThumbnailUnavailable, lastErr)
	}
	return nil, lastErr
}

func socialThumbnailDatasets(targetURL string) ([]string, func(map[string]any) string) {
	switch {
	case utils.IsInstagramURL(targetURL):
		if strings.Contains(strings.ToLower(targetURL), "/reel/") {
			return []string{DatasetInstagramReels, DatasetInstagramPosts}, func(record map[string]any) string {
				return stringField(record, "thumbnail")
			}
		}
		return []string{DatasetInstagramPosts}, func(record map[string]any) string {
			return stringField(record, "thumbnail")
		}
	case utils.IsTikTokURL(targetURL) || utils.IsTikTokPhotoPostURL(targetURL):
		return []string{DatasetTikTokPosts}, func(record map[string]any) string {
			return stringField(record, "preview_image")
		}
	case utils.IsFacebookURL(targetURL) || utils.IsFacebookPostURL(targetURL):
		return []string{DatasetFacebookPosts}, facebookPosterURL
	default:
		return nil, nil
	}
}
