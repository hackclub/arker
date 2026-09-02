package apify

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"time"

	"gorm.io/gorm"

	"arker/internal/archivers"
	"arker/internal/models"
	"arker/internal/utils"
)

// minFallbackBudget is the least remaining job time worth starting a fallback
// in: a dataset collection alone routinely takes two minutes.
const minFallbackBudget = 3 * time.Minute

// Backend is the slice of Client the fallback archiver uses, split out so
// tests can substitute a fake without network access.
type Backend interface {
	Enabled() bool
	SupportsFallback(url, itemType string) bool
	ArchiveFallback(ctx context.Context, url, itemType string, logWriter io.Writer, db *gorm.DB, itemID uint) (archivers.Result, error)
}

// SupportsFallback reports whether this client can plausibly rescue a failed
// native run for the URL and archive type.
//
// The covered platforms are the ones whose native failures are positional —
// login walls, throttles, geo-blocks, datacenter-range bans, IP-locked media —
// rather than content that is genuinely gone. Nothing else belongs here: a
// fallback that cannot beat the native failure only spends money to fail
// twice.
//
//	Instagram  reel/feed post   yt-dlp, gallery-dl   actor record + direct CDN
//	YouTube    video            yt-dlp               actor stores the MP4 (IP-locked CDN)
//	TikTok     video            yt-dlp               actor stores the MP4 (IP-locked CDN)
//	TikTok     photo post       gallery-dl           actor record + direct CDN, KVS copy as fallback
//	Reddit     post             gallery-dl           actor record + direct CDN
//	X          status           gallery-dl           actor record + direct CDN
//	Pinterest  pin              gallery-dl           actor record + direct CDN
//	Facebook   video permalink  yt-dlp               actor record + direct CDN
//	Facebook   photo/post       gallery-dl           actor record + direct CDN
//
// This is also the answer routing asks before creating a gallery item for a
// login-only site (utils.ShouldCreateGalleryDLItem), so a platform added here
// starts getting items the moment the client is configured.
func (c *Client) SupportsFallback(url, itemType string) bool {
	if !c.Enabled() {
		return false
	}
	switch itemType {
	case utils.ArchiveTypeYtDlp:
		return utils.IsInstagramURL(url) || utils.IsFacebookURL(url) ||
			utils.IsYouTubeURL(url) || utils.IsTikTokURL(url)
	case utils.ArchiveTypeGalleryDl:
		return utils.IsInstagramURL(url) || utils.IsRedditPostURL(url) || utils.IsXPostURL(url) ||
			utils.IsPinterestPinURL(url) || utils.IsFacebookPostURL(url) || utils.IsTikTokPhotoPostURL(url)
	}
	return false
}

// ArchiveFallback dispatches to the platform flow.
func (c *Client) ArchiveFallback(ctx context.Context, url, itemType string, logWriter io.Writer, db *gorm.DB, itemID uint) (archivers.Result, error) {
	shortID := shortIDForItem(db, itemID)
	switch {
	case utils.IsInstagramURL(url):
		return c.archiveInstagram(ctx, url, itemType, logWriter, db, itemID, shortID)
	case utils.IsYouTubeURL(url):
		return c.archiveYouTube(ctx, url, logWriter, db, itemID, shortID)
	case utils.IsTikTokURL(url) || utils.IsTikTokPhotoPostURL(url):
		return c.archiveTikTok(ctx, url, itemType, logWriter, db, itemID, shortID)
	case utils.IsRedditPostURL(url):
		return c.archiveReddit(ctx, url, itemType, logWriter, db, itemID, shortID)
	case utils.IsXPostURL(url):
		return c.archiveX(ctx, url, itemType, logWriter, db, itemID, shortID)
	case utils.IsPinterestPinURL(url):
		return c.archivePinterest(ctx, url, itemType, logWriter, db, itemID, shortID)
	case utils.IsFacebookURL(url) || utils.IsFacebookPostURL(url):
		return c.archiveFacebook(ctx, url, itemType, logWriter, db, itemID, shortID)
	}
	return archivers.Result{}, fmt.Errorf("no Apify fallback for %s", url)
}

// shortIDForItem resolves the capture short ID for usage rows. Best-effort:
// usage tracking must not fail an archive over a lookup.
func shortIDForItem(db *gorm.DB, itemID uint) string {
	if db == nil {
		return ""
	}
	var shortID string
	if err := db.Model(&models.ArchiveItem{}).
		Joins("JOIN captures ON captures.id = archive_items.capture_id").
		Where("archive_items.id = ?", itemID).
		Pluck("captures.short_id", &shortID).Error; err != nil {
		slog.Warn("Could not resolve short ID for Apify usage row", "item_id", itemID, "error", err)
	}
	return shortID
}

// FallbackArchiver wraps a native archiver with the Apify fallback. The
// native flow always runs first and its success is always preferred: it is
// free and full-fidelity. The fallback only spends money after the native
// flow has actually failed on a URL the backend can plausibly rescue.
type FallbackArchiver struct {
	Primary archivers.Archiver
	Type    string
	Backend Backend
}

// WithFallback wraps an archiver, returning it unchanged when the backend is
// not usable so the archiver map stays free of dead indirection.
func WithFallback(primary archivers.Archiver, itemType string, backend Backend) archivers.Archiver {
	if backend == nil || !backend.Enabled() {
		return primary
	}
	return &FallbackArchiver{Primary: primary, Type: itemType, Backend: backend}
}

// RefreshVideoMetadata delegates to the native archiver. The fallback exists
// to fetch media the native flow cannot reach; a metadata-only refresh reuses
// media that is already stored, so there is nothing for Apify to rescue
// and no reason to spend money. A refresh failure is handled by the worker
// (it falls back to the stored sidecars or a full run), not by the backend.
func (f *FallbackArchiver) RefreshVideoMetadata(ctx context.Context, url string, logWriter io.Writer, media archivers.VideoMedia) (archivers.Result, error) {
	refresher, ok := f.Primary.(archivers.VideoMetadataRefresher)
	if !ok {
		return archivers.Result{}, fmt.Errorf("archiver %T cannot refresh video metadata", f.Primary)
	}
	return refresher.RefreshVideoMetadata(ctx, url, logWriter, media)
}

func (f *FallbackArchiver) Archive(ctx context.Context, url string, logWriter io.Writer, db *gorm.DB, itemID uint) (archivers.Result, error) {
	result, nativeErr := f.Primary.Archive(ctx, url, logWriter, db, itemID)
	if nativeErr == nil {
		return result, nil
	}

	// A cancelled or expired context means the job is out of budget, not that
	// the native flow was refused; starting a paid retry now would be spending
	// money on a job River is about to kill anyway.
	if ctx.Err() != nil {
		return result, nativeErr
	}
	if !f.Backend.SupportsFallback(url, f.Type) {
		return result, nativeErr
	}
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < minFallbackBudget {
		fmt.Fprintf(logWriter, "\nNative flow failed but only %s remains in the job budget; skipping Apify fallback this attempt\n",
			time.Until(deadline).Round(time.Second))
		return result, nativeErr
	}

	fmt.Fprintf(logWriter, "\nNative flow failed (%v); attempting Apify fallback...\n", nativeErr)
	slog.Info("Attempting Apify fallback", "url", url, "type", f.Type, "native_error", nativeErr)

	fallbackResult, fallbackErr := f.Backend.ArchiveFallback(ctx, url, f.Type, logWriter, db, itemID)
	if fallbackErr != nil {
		fmt.Fprintf(logWriter, "Apify fallback failed: %v\n", fallbackErr)
		return result, fmt.Errorf("native flow failed (%v); Apify fallback failed: %w", nativeErr, fallbackErr)
	}

	fmt.Fprintf(logWriter, "Apify fallback succeeded\n")
	slog.Info("Apify fallback succeeded", "url", url, "type", f.Type)
	return fallbackResult, nil
}
