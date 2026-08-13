package brightdata

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
//	Instagram  reel/feed post   yt-dlp, gallery-dl   dataset + direct CDN
//	YouTube    video            yt-dlp               browser session (IP-locked)
//	TikTok     video            yt-dlp               dataset + browser session (IP-locked)
//	TikTok     photo post       gallery-dl           dataset + direct CDN, browser fallback
//	Reddit     post             gallery-dl           dataset + direct CDN (muxed MP4)
//	X          status           gallery-dl           dataset + direct CDN
//	Pinterest  pin              gallery-dl           dataset + direct CDN
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
		if utils.IsInstagramURL(url) {
			return true
		}
		// YouTube and TikTok both sign their media against the resolving IP,
		// so their bytes can only be fetched from inside a browser session.
		if utils.IsYouTubeURL(url) || utils.IsTikTokURL(url) {
			return c.BrowserReady()
		}
		return false
	case utils.ArchiveTypeGalleryDl:
		// Pinterest's i.pinimg.com assets are as portable as Reddit's and X's:
		// the pin costs a dataset record and the bytes cost nothing.
		if utils.IsInstagramURL(url) || utils.IsRedditPostURL(url) || utils.IsXPostURL(url) || utils.IsPinterestPinURL(url) {
			return true
		}
		// TikTok stills are ordinary CDN images: the browser session is only
		// the fallback for the ones our own IP is refused, so a client without
		// browser credentials is still worth trying.
		return utils.IsTikTokPhotoPostURL(url)
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
	}
	return archivers.Result{}, fmt.Errorf("no Bright Data fallback for %s", url)
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
		slog.Warn("Could not resolve short ID for Bright Data usage row", "item_id", itemID, "error", err)
	}
	return shortID
}

// FallbackArchiver wraps a native archiver with the Bright Data fallback. The
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

// PaidFallbackEnabled marks this archiver as able to spend money.
//
// Callers that must not bill (the production canaries) assert on this before
// running: they build their own native-only archiver map, and this method is
// how they prove at runtime that no paid wrapper leaked into it. Native
// archivers do not implement the interface, so the check fails closed.
func (f *FallbackArchiver) PaidFallbackEnabled() bool { return true }

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
		fmt.Fprintf(logWriter, "\nNative flow failed but only %s remains in the job budget; skipping Bright Data fallback this attempt\n",
			time.Until(deadline).Round(time.Second))
		return result, nativeErr
	}

	fmt.Fprintf(logWriter, "\nNative flow failed (%v); attempting Bright Data fallback...\n", nativeErr)
	slog.Info("Attempting Bright Data fallback", "url", url, "type", f.Type, "native_error", nativeErr)

	fallbackResult, fallbackErr := f.Backend.ArchiveFallback(ctx, url, f.Type, logWriter, db, itemID)
	if fallbackErr != nil {
		fmt.Fprintf(logWriter, "Bright Data fallback failed: %v\n", fallbackErr)
		return result, fmt.Errorf("native flow failed (%v); Bright Data fallback failed: %w", nativeErr, fallbackErr)
	}

	fmt.Fprintf(logWriter, "Bright Data fallback succeeded\n")
	slog.Info("Bright Data fallback succeeded", "url", url, "type", f.Type)
	return fallbackResult, nil
}
