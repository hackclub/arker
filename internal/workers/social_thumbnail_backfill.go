package workers

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"gorm.io/gorm"

	"arker/internal/archivers"
	"arker/internal/models"
	"arker/internal/storage"
	"arker/internal/thumbnail"
	"arker/internal/utils"
)

const (
	socialThumbnailBackfillQueue = "thumbnail_backfill"
	maxBackfillGalleryBuffer     = 200 << 20
	maxGalleryMetadataBytes      = 4 << 20
	maxCapturedMHTMLImageBytes   = 16 << 20
	backfillJobTimeout           = 8 * time.Minute
)

var errThumbnailBudgetExhausted = errors.New("social thumbnail provider budget exhausted")

// SocialThumbnailProvider is the paid/free provider fallback used only after
// stored gallery media and native yt-dlp cannot recover the published image.
// The implementation resolves a poster, never the archived video itself.
type SocialThumbnailProvider interface {
	ResolveSocialThumbnail(ctx context.Context, targetURL, itemType string, logWriter io.Writer, db *gorm.DB, itemID uint) (*archivers.Thumbnail, error)
	SocialThumbnailCostUSD(targetURL, itemType string) float64
	SupportsSocialThumbnail(targetURL, itemType string) bool
}

// SocialThumbnailBackfillJobArgs addresses one canonical URL/type group. Every
// duplicate capture in the group shares the one newly stored original object.
type SocialThumbnailBackfillJobArgs struct {
	Identity  string  `json:"identity"`
	URL       string  `json:"url"`
	ShortID   string  `json:"short_id"`
	Type      string  `json:"type"`
	StartedAt int64   `json:"started_at"`
	BudgetUSD float64 `json:"budget_usd"`
	Version   int     `json:"version,omitempty"`
}

func (SocialThumbnailBackfillJobArgs) Kind() string { return "social_thumbnail_backfill" }

// SocialThumbnailBackfillWorker replaces legacy social derivatives with the
// exact provider poster or first stored still. It runs in a one-worker queue so
// a bulk repair cannot crowd out archive jobs or race its shared cost cap.
type SocialThumbnailBackfillWorker struct {
	river.WorkerDefaults[SocialThumbnailBackfillJobArgs]
	storage  storage.Storage
	db       *gorm.DB
	native   archivers.SocialThumbnailRefresher
	provider SocialThumbnailProvider
}

func NewSocialThumbnailBackfillWorker(store storage.Storage, db *gorm.DB, native archivers.SocialThumbnailRefresher, provider SocialThumbnailProvider) *SocialThumbnailBackfillWorker {
	return &SocialThumbnailBackfillWorker{storage: store, db: db, native: native, provider: provider}
}

func (w *SocialThumbnailBackfillWorker) Work(ctx context.Context, job *river.Job[SocialThumbnailBackfillJobArgs]) error {
	jobCtx, cancel := context.WithTimeout(ctx, backfillJobTimeout)
	defer cancel()
	return w.generate(jobCtx, job.Args, job.Attempt >= job.MaxAttempts)
}

func (w *SocialThumbnailBackfillWorker) generate(ctx context.Context, args SocialThumbnailBackfillJobArgs, finalAttempt bool) error {
	args.Type = utils.NormalizeArchiveType(args.Type)
	logger := slog.With("worker", "social_thumbnail_backfill", "short_id", args.ShortID, "type", args.Type)

	items, err := w.groupItems(args.Identity, args.Type)
	if err != nil {
		return err
	}
	if len(items) == 0 {
		logger.Info("Social thumbnail group no longer has completed items")
		return nil
	}

	targets := make([]models.ArchiveItem, 0, len(items))
	for i := range items {
		item := items[i]
		if socialThumbnailSatisfiesContract(item) {
			continue
		}
		targets = append(targets, item)
	}
	if len(targets) == 0 {
		return nil
	}

	// A duplicate may already hold a compact preview. Repoint the rest without
	// another provider request or object upload.
	for i := range items {
		item := items[i]
		if socialThumbnailSatisfiesContract(item) {
			return w.shareExistingOriginal(&item, targets)
		}
	}
	// Existing provider originals are the cheapest and most faithful backfill
	// source. Downsize once, store a new append-only object, and share it across
	// every duplicate row.
	for i := range items {
		item := items[i]
		if item.ThumbnailStatus != models.ThumbnailStatusReady || item.ThumbnailKey == "" ||
			item.ThumbnailKind != models.ThumbnailKindSocialOriginal {
			continue
		}
		reader, err := w.storage.Reader(item.ThumbnailKey)
		if err != nil {
			return err
		}
		compact, compactErr := thumbnail.OriginalFromReader(reader)
		reader.Close()
		if compactErr != nil {
			return compactErr
		}
		thumb := &archivers.Thumbnail{Data: compact.Data, Width: compact.Width, Height: compact.Height, Kind: models.ThumbnailKindSocialPreview}
		key := fmt.Sprintf("%s/%s-%s-backfill-thumb%s", args.ShortID, args.Type, uploadNonce(), thumbnail.FileExtension(thumb.Data))
		return w.storeForGroup(thumb, key, targets)
	}

	var logs bytes.Buffer
	var thumb *archivers.Thumbnail
	var nativeErr error
	providerFirst := args.Type == utils.ArchiveTypeGalleryDl &&
		w.provider != nil && w.provider.SupportsSocialThumbnail(args.URL, args.Type) &&
		groupHasSource(items, models.ArchiveSourceBrightData)

	if args.Type == utils.ArchiveTypeGalleryDl {
		thumb, nativeErr = w.thumbnailFromStoredGalleries(items)
		if nativeErr != nil && !errors.Is(nativeErr, archivers.ErrSocialThumbnailUnavailable) {
			return nativeErr
		}
	}

	// A sibling MHTML snapshot can retain the exact og:image bytes long after
	// the live post and its signed CDN URL disappear. Resolve the embedded part
	// before any network/provider retry; it is historical, free, and avoids
	// mistaking avatars or recommendation cards for the post image by matching
	// the root document's asset identity to Content-Location.
	if thumb == nil {
		thumb, _ = w.thumbnailFromCapturedMHTML(ctx, args.URL, items)
	}

	// A fallback marker means the provider/native poster paths were already
	// exhausted by an earlier run. Recover directly from the archived video so
	// a zero-budget contract repair does not repeat thousands of upstream calls.
	if thumb == nil && groupHasThumbnailKind(items, models.ThumbnailKindSocialFallback) {
		thumb, nativeErr = w.thumbnailFromStoredVideo(ctx, items)
	}

	// yt-dlp can recover posters for video items and for all-video gallery
	// bundles (Reddit/X/Instagram/Facebook) without downloading their media.
	if thumb == nil && w.native != nil && !providerFirst {
		thumb, nativeErr = w.native.RefreshSocialThumbnail(ctx, args.URL, &logs)
	}
	if thumb == nil && w.provider != nil && w.provider.SupportsSocialThumbnail(args.URL, args.Type) {
		if err := w.checkProviderBudget(args); err != nil {
			logger.Warn("Skipping provider poster because backfill budget is exhausted", "error", err)
			if nativeErr == nil {
				nativeErr = err
			}
		} else {
			providerThumb, providerErr := w.provider.ResolveSocialThumbnail(ctx, args.URL, args.Type, &logs, w.db, targets[0].ID)
			switch {
			case providerErr == nil:
				thumb = providerThumb
			case errors.Is(providerErr, archivers.ErrSocialThumbnailUnavailable):
				nativeErr = providerErr
			default:
				return fmt.Errorf("social thumbnail provider refresh: %w", providerErr)
			}
		}
	}
	// A Bright Data artifact proves the native archive path already failed for
	// this URL. Ask the provider that successfully described the stored post
	// for its authored cover first; only retry native when that provider says no
	// cover exists. This keeps large repairs source-faithful without spending a
	// minute re-running a known-failing Instagram extractor for every reel.
	if thumb == nil && providerFirst && w.native != nil {
		thumb, nativeErr = w.native.RefreshSocialThumbnail(ctx, args.URL, &logs)
	}
	// Deleted/private posts can no longer yield their platform poster, but the
	// exact archived media is still present. A first frame is a real,
	// aspect-correct preview and needs no paid or upstream request.
	if thumb == nil {
		thumb, _ = w.thumbnailFromStoredVideo(ctx, items)
	}

	if thumb == nil {
		if !finalAttempt && nativeErr != nil {
			return fmt.Errorf("native social thumbnail refresh: %w", nativeErr)
		}
		reason := "no provider-authored poster is available"
		if nativeErr != nil {
			reason = nativeErr.Error()
		}
		return w.markSocialFallback(targets, logger, reason)
	}

	compact, err := thumbnail.OriginalFromReader(bytes.NewReader(thumb.Data))
	if err != nil {
		return fmt.Errorf("prepare compact social preview: %w", err)
	}
	thumb = &archivers.Thumbnail{Data: compact.Data, Width: compact.Width, Height: compact.Height, Kind: models.ThumbnailKindSocialPreview}
	key := fmt.Sprintf("%s/%s-%s-backfill-thumb%s", args.ShortID, args.Type, uploadNonce(), thumbnail.FileExtension(thumb.Data))
	if err := w.storeForGroup(thumb, key, targets); err != nil {
		return err
	}
	logger.Info("Social thumbnail backfilled",
		"key", key, "width", thumb.Width, "height", thumb.Height,
		"bytes", len(thumb.Data), "items", len(targets))
	return nil
}

func (w *SocialThumbnailBackfillWorker) thumbnailFromStoredVideo(ctx context.Context, items []models.ArchiveItem) (*archivers.Thumbnail, error) {
	var lastErr error
	for i := range items {
		item := &items[i]
		if item.StorageKey == "" {
			continue
		}
		if utils.ArchiveTypesEqual(item.Type, utils.ArchiveTypeYtDlp) {
			if direct, ok := w.storage.(storage.DirectURLStorage); ok {
				input, err := direct.DirectURL(ctx, item.StorageKey, storage.DirectURLOptions{})
				if err == nil {
					if thumb, frameErr := archivers.VideoFrameThumbnail(ctx, input); frameErr == nil {
						return thumb, nil
					} else {
						lastErr = frameErr
					}
				}
			}
			reader, err := w.storage.Reader(item.StorageKey)
			if err != nil {
				lastErr = err
				continue
			}
			tmp, err := os.CreateTemp("", "arker-video-frame-*"+item.Extension)
			if err != nil {
				reader.Close()
				return nil, err
			}
			path := tmp.Name()
			_, copyErr := io.Copy(tmp, reader)
			reader.Close()
			closeErr := tmp.Close()
			if copyErr == nil && closeErr == nil {
				if thumb, frameErr := archivers.VideoFrameThumbnail(ctx, path); frameErr == nil {
					os.Remove(path)
					return thumb, nil
				} else {
					lastErr = frameErr
				}
			} else if copyErr != nil {
				lastErr = copyErr
			} else {
				lastErr = closeErr
			}
			os.Remove(path)
			continue
		}

		zr, cleanup, err := openBackfillGalleryZip(w.storage, item.StorageKey)
		if err != nil {
			lastErr = err
			continue
		}
		for _, file := range zr.File {
			if !strings.HasPrefix(archivers.GalleryMediaContentType(file.Name, nil), "video/") {
				continue
			}
			r, openErr := file.Open()
			if openErr != nil {
				lastErr = openErr
				continue
			}
			tmp, createErr := os.CreateTemp("", "arker-gallery-frame-*"+filepath.Ext(file.Name))
			if createErr != nil {
				r.Close()
				cleanup()
				return nil, createErr
			}
			path := tmp.Name()
			_, copyErr := io.Copy(tmp, r)
			r.Close()
			closeErr := tmp.Close()
			if copyErr == nil && closeErr == nil {
				if thumb, frameErr := archivers.VideoFrameThumbnail(ctx, path); frameErr == nil {
					os.Remove(path)
					cleanup()
					return thumb, nil
				} else {
					lastErr = frameErr
				}
			}
			os.Remove(path)
		}
		cleanup()
	}
	if lastErr == nil {
		lastErr = archivers.ErrSocialThumbnailUnavailable
	}
	return nil, lastErr
}

func (w *SocialThumbnailBackfillWorker) thumbnailFromCapturedMHTML(ctx context.Context, sourceURL string, items []models.ArchiveItem) (*archivers.Thumbnail, error) {
	captureIDs := make([]uint, 0, len(items))
	seen := make(map[uint]bool, len(items))
	for i := range items {
		if !seen[items[i].CaptureID] {
			seen[items[i].CaptureID] = true
			captureIDs = append(captureIDs, items[i].CaptureID)
		}
	}
	if len(captureIDs) == 0 {
		return nil, archivers.ErrSocialThumbnailUnavailable
	}
	var pages []models.ArchiveItem
	if err := w.db.Where("capture_id IN ? AND type = ? AND status = ? AND storage_key <> ''", captureIDs, utils.ArchiveTypeMHTML, "completed").
		Order("updated_at DESC").Find(&pages).Error; err != nil {
		return nil, err
	}
	var lastErr error
	for i := range pages {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		rootReader, err := w.storage.Reader(pages[i].StorageKey)
		if err != nil {
			lastErr = err
			continue
		}
		htmlData, htmlErr := utils.ExtractMHTMLHTML(io.LimitReader(rootReader, maxCapturedMHTMLScanBytes), maxCapturedHTMLBytes)
		rootReader.Close()
		if htmlErr != nil {
			lastErr = htmlErr
			continue
		}
		imageURL, canonicalURL := archivers.CapturedHTMLSocialImage(htmlData)
		if imageURL == "" || canonicalURL == "" || utils.CanonicalizeArchiveURL(canonicalURL) != utils.CanonicalizeArchiveURL(sourceURL) {
			lastErr = archivers.ErrSocialThumbnailUnavailable
			continue
		}

		resourceReader, err := w.storage.Reader(pages[i].StorageKey)
		if err != nil {
			lastErr = err
			continue
		}
		imageData, contentType, resourceErr := utils.ExtractMHTMLResource(io.LimitReader(resourceReader, maxCapturedMHTMLScanBytes), imageURL, maxCapturedMHTMLImageBytes)
		resourceReader.Close()
		if resourceErr != nil {
			lastErr = resourceErr
			continue
		}
		if contentType != "" && !strings.HasPrefix(strings.ToLower(contentType), "image/") {
			lastErr = fmt.Errorf("captured social resource is %s, not an image", contentType)
			continue
		}
		compact, err := thumbnail.OriginalFromReader(bytes.NewReader(imageData))
		if err != nil {
			lastErr = err
			continue
		}
		return &archivers.Thumbnail{Data: compact.Data, Width: compact.Width, Height: compact.Height, Kind: models.ThumbnailKindSocialPreview}, nil
	}
	if lastErr == nil {
		lastErr = archivers.ErrSocialThumbnailUnavailable
	}
	return nil, lastErr
}

func groupHasSource(items []models.ArchiveItem, source string) bool {
	for i := range items {
		if items[i].Source == source {
			return true
		}
	}
	return false
}

func groupHasThumbnailKind(items []models.ArchiveItem, kind string) bool {
	for i := range items {
		if items[i].ThumbnailKind == kind {
			return true
		}
	}
	return false
}

func socialThumbnailSatisfiesContract(item models.ArchiveItem) bool {
	return item.ThumbnailStatus == models.ThumbnailStatusReady && item.ThumbnailKey != "" &&
		item.ThumbnailWidth > 0 && item.ThumbnailHeight > 0 &&
		item.ThumbnailWidth <= thumbnail.SocialMaxDimension && item.ThumbnailHeight <= thumbnail.SocialMaxDimension &&
		(item.ThumbnailKind == models.ThumbnailKindSocialPreview || item.ThumbnailKind == models.ThumbnailKindSocialOriginal)
}

func (w *SocialThumbnailBackfillWorker) groupItems(identity, archiveType string) ([]models.ArchiveItem, error) {
	var items []models.ArchiveItem
	err := w.db.Model(&models.ArchiveItem{}).
		Joins("JOIN captures ON captures.id = archive_items.capture_id").
		Joins("JOIN archived_urls ON archived_urls.id = captures.archived_url_id").
		Where("archive_items.type = ? AND archive_items.status = ?", archiveType, "completed").
		Where("archive_items.deleted_at IS NULL AND captures.deleted_at IS NULL AND archived_urls.deleted_at IS NULL").
		Where("COALESCE(NULLIF(archived_urls.canonical_url, ''), archived_urls.original) = ?", identity).
		Order("archive_items.id DESC").Find(&items).Error
	if err != nil {
		return nil, fmt.Errorf("load social thumbnail group: %w", err)
	}
	return items, nil
}

func (w *SocialThumbnailBackfillWorker) storeForGroup(thumb *archivers.Thumbnail, key string, targets []models.ArchiveItem) error {
	if len(targets) == 0 {
		return nil
	}
	if err := StoreThumbnail(thumb, key, w.storage, w.db, &targets[0]); err != nil {
		return fmt.Errorf("store social thumbnail object: %w", err)
	}
	ids := archiveItemIDs(targets)
	return w.db.Model(&models.ArchiveItem{}).Where("id IN ?", ids).Updates(map[string]interface{}{
		"thumbnail_key":    key,
		"thumbnail_width":  thumb.Width,
		"thumbnail_height": thumb.Height,
		"thumbnail_status": models.ThumbnailStatusReady,
		"thumbnail_kind":   thumb.Kind,
	}).Error
}

func (w *SocialThumbnailBackfillWorker) shareExistingOriginal(source *models.ArchiveItem, targets []models.ArchiveItem) error {
	return w.db.Model(&models.ArchiveItem{}).Where("id IN ?", archiveItemIDs(targets)).Updates(map[string]interface{}{
		"thumbnail_key":    source.ThumbnailKey,
		"thumbnail_width":  source.ThumbnailWidth,
		"thumbnail_height": source.ThumbnailHeight,
		"thumbnail_status": models.ThumbnailStatusReady,
		"thumbnail_kind":   source.ThumbnailKind,
	}).Error
}

func (w *SocialThumbnailBackfillWorker) markSocialFallback(targets []models.ArchiveItem, logger *slog.Logger, reason string) error {
	ids := archiveItemIDs(targets)
	logger.Info("No real social thumbnail available; retaining capture fallback", "items", len(ids), "reason", reason)
	return w.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&models.ArchiveItem{}).Where("id IN ?", ids).
			Update("thumbnail_kind", models.ThumbnailKindSocialFallback).Error; err != nil {
			return err
		}
		if err := tx.Model(&models.ArchiveItem{}).Where("id IN ? AND thumbnail_key <> ''", ids).
			Update("thumbnail_status", models.ThumbnailStatusReady).Error; err != nil {
			return err
		}
		return tx.Model(&models.ArchiveItem{}).Where("id IN ? AND (thumbnail_key IS NULL OR thumbnail_key = '')", ids).
			Update("thumbnail_status", models.ThumbnailStatusUnavailable).Error
	})
}

func archiveItemIDs(items []models.ArchiveItem) []uint {
	ids := make([]uint, len(items))
	for i := range items {
		ids[i] = items[i].ID
	}
	return ids
}

func (w *SocialThumbnailBackfillWorker) checkProviderBudget(args SocialThumbnailBackfillJobArgs) error {
	estimated := w.provider.SocialThumbnailCostUSD(args.URL, args.Type)
	if estimated <= 0 {
		return nil
	}
	if args.BudgetUSD <= 0 || args.StartedAt <= 0 {
		return errThumbnailBudgetExhausted
	}
	var spent float64
	if err := w.db.Model(&models.BrightDataUsage{}).
		Where("created_at >= ?", time.Unix(args.StartedAt, 0).UTC()).
		Select("COALESCE(SUM(cost_usd), 0)").Scan(&spent).Error; err != nil {
		return fmt.Errorf("read provider spend: %w", err)
	}
	if spent+estimated > args.BudgetUSD {
		return fmt.Errorf("%w: spent $%.4f, next lookup at most $%.4f, cap $%.2f",
			errThumbnailBudgetExhausted, spent, estimated, args.BudgetUSD)
	}
	return nil
}

// thumbnailFromStoredGalleries tries newest duplicates first. A readable ZIP
// with no still is a conclusive miss; an S3/range-read failure is retryable.
func (w *SocialThumbnailBackfillWorker) thumbnailFromStoredGalleries(items []models.ArchiveItem) (*archivers.Thumbnail, error) {
	var lastErr error
	for i := range items {
		thumb, err := w.thumbnailFromStoredGallery(&items[i])
		if err == nil {
			return thumb, nil
		}
		if errors.Is(err, archivers.ErrSocialThumbnailUnavailable) {
			lastErr = err
			continue
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = archivers.ErrSocialThumbnailUnavailable
	}
	return nil, lastErr
}

func (w *SocialThumbnailBackfillWorker) thumbnailFromStoredGallery(item *models.ArchiveItem) (*archivers.Thumbnail, error) {
	zr, cleanup, err := openBackfillGalleryZip(w.storage, item.StorageKey)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	byName := make(map[string]*zip.File, len(zr.File))
	for _, f := range zr.File {
		byName[f.Name] = f
	}

	var metadata archivers.GalleryMetadata
	if metaFile := byName["metadata.json"]; metaFile != nil {
		r, openErr := metaFile.Open()
		if openErr != nil {
			return nil, fmt.Errorf("open gallery metadata: %w", openErr)
		}
		raw, readErr := io.ReadAll(io.LimitReader(r, maxGalleryMetadataBytes+1))
		r.Close()
		if readErr == nil && len(raw) <= maxGalleryMetadataBytes && json.Unmarshal(raw, &metadata) == nil {
			for _, media := range metadata.Files {
				if media.IsVideo || media.Name == "" || !strings.HasPrefix(media.ContentType, "image/") {
					continue
				}
				if file := byName[media.Name]; file != nil {
					if thumb, err := originalFromZipFile(file); err == nil {
						return thumb, nil
					}
				}
			}
		}
	}

	// Very old bundles predate metadata.json. Their flat, zero-padded media
	// names preserve slide order, so scan image extensions in ZIP order.
	for _, file := range zr.File {
		if file.FileInfo().IsDir() || strings.HasSuffix(strings.ToLower(file.Name), ".json") || !galleryImageExtension(file.Name) {
			continue
		}
		if thumb, err := originalFromZipFile(file); err == nil {
			return thumb, nil
		}
	}
	return nil, fmt.Errorf("%w: stored gallery contains no supported still image", archivers.ErrSocialThumbnailUnavailable)
}

func originalFromZipFile(file *zip.File) (*archivers.Thumbnail, error) {
	if file.UncompressedSize64 > thumbnail.MaxOriginalBytes {
		return nil, fmt.Errorf("gallery image %s exceeds thumbnail byte limit", file.Name)
	}
	r, err := file.Open()
	if err != nil {
		return nil, err
	}
	defer r.Close()
	t, err := thumbnail.OriginalFromReader(r)
	if err != nil {
		return nil, err
	}
	return &archivers.Thumbnail{
		Data: t.Data, Width: t.Width, Height: t.Height,
		Kind: models.ThumbnailKindSocialPreview,
	}, nil
}

func galleryImageExtension(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".jpg", ".jpeg", ".png", ".gif", ".webp", ".avif", ".heic", ".heif":
		return true
	default:
		return false
	}
}

type lockedSeekerReaderAt struct {
	mu sync.Mutex
	r  storage.ReadSeekCloser
}

func (r *lockedSeekerReaderAt) ReadAt(p []byte, off int64) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.r.Seek(off, io.SeekStart); err != nil {
		return 0, err
	}
	n, err := io.ReadFull(r.r, p)
	if err == io.ErrUnexpectedEOF {
		err = io.EOF
	}
	return n, err
}

func openBackfillGalleryZip(store storage.Storage, key string) (*zip.Reader, func(), error) {
	size, err := store.Size(key)
	if err != nil {
		return nil, nil, fmt.Errorf("stat stored gallery: %w", err)
	}
	if seekable, ok := store.(storage.SeekableStorage); ok {
		r, seekErr := seekable.SeekableReader(key)
		if seekErr == nil {
			zr, zipErr := zip.NewReader(&lockedSeekerReaderAt{r: r}, size)
			if zipErr == nil {
				return zr, func() { _ = r.Close() }, nil
			}
			_ = r.Close()
		}
	}
	if size > maxBackfillGalleryBuffer {
		return nil, nil, fmt.Errorf("stored gallery is %d bytes and storage cannot seek", size)
	}
	r, err := store.Reader(key)
	if err != nil {
		return nil, nil, fmt.Errorf("open stored gallery: %w", err)
	}
	data, readErr := io.ReadAll(io.LimitReader(r, maxBackfillGalleryBuffer+1))
	r.Close()
	if readErr != nil {
		return nil, nil, fmt.Errorf("read stored gallery: %w", readErr)
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, nil, fmt.Errorf("stored gallery is not a readable ZIP: %w", err)
	}
	return zr, func() {}, nil
}

// SocialThumbnailBackfillOptions controls one resumable bulk enqueue.
type SocialThumbnailBackfillOptions struct {
	BudgetUSD       float64
	PriorityShortID string
	DryRun          bool
}

type SocialThumbnailBackfillSummary struct {
	RunID        string  `json:"run_id"`
	StartedAt    string  `json:"started_at"`
	BudgetUSD    float64 `json:"budget_usd"`
	TargetItems  int     `json:"target_items"`
	TargetGroups int     `json:"target_groups"`
	Enqueued     int     `json:"enqueued"`
	DryRun       bool    `json:"dry_run"`
}

type socialThumbnailCandidate struct {
	ItemID       uint
	ShortID      string
	Type         string
	Original     string
	CanonicalURL string
	Source       string
	Priority     bool
}

// EnqueueSocialThumbnailBackfill groups every legacy social item by canonical
// URL and archive type, newest first within a group. One job repairs every
// duplicate, making the work idempotent and avoiding repeated platform calls.
func EnqueueSocialThumbnailBackfill(ctx context.Context, db *gorm.DB, client *river.Client[pgx.Tx], opts SocialThumbnailBackfillOptions) (SocialThumbnailBackfillSummary, error) {
	started := time.Now().UTC()
	summary := SocialThumbnailBackfillSummary{
		RunID: started.Format("20060102T150405Z"), StartedAt: started.Format(time.RFC3339),
		BudgetUSD: opts.BudgetUSD, DryRun: opts.DryRun,
	}

	var rows []socialThumbnailCandidate
	err := db.Table("archive_items").Select(`
		archive_items.id AS item_id,
		captures.short_id,
		archive_items.type,
		archived_urls.original,
		archived_urls.canonical_url,
		archive_items.source`).
		Joins("JOIN captures ON captures.id = archive_items.capture_id").
		Joins("JOIN archived_urls ON archived_urls.id = captures.archived_url_id").
		Where("archive_items.type IN ?", []string{utils.ArchiveTypeGalleryDl, utils.ArchiveTypeYtDlp}).
		Where("archive_items.status = ?", "completed").
		Where("archive_items.deleted_at IS NULL AND captures.deleted_at IS NULL AND archived_urls.deleted_at IS NULL").
		Where("archive_items.thumbnail_status <> ? OR archive_items.thumbnail_status IS NULL OR archive_items.thumbnail_key = '' OR archive_items.thumbnail_key IS NULL OR archive_items.thumbnail_width <= 0 OR archive_items.thumbnail_height <= 0 OR archive_items.thumbnail_width > ? OR archive_items.thumbnail_height > ? OR COALESCE(archive_items.thumbnail_kind, '') NOT IN ?",
			models.ThumbnailStatusReady, thumbnail.SocialMaxDimension, thumbnail.SocialMaxDimension,
			[]string{models.ThumbnailKindSocialOriginal, models.ThumbnailKindSocialPreview}).
		Order("archive_items.id DESC").Scan(&rows).Error
	if err != nil {
		return summary, fmt.Errorf("query social thumbnail candidates: %w", err)
	}
	summary.TargetItems = len(rows)

	groups := make(map[string]socialThumbnailCandidate)
	for _, row := range rows {
		identity := strings.TrimSpace(row.CanonicalURL)
		if identity == "" {
			identity = row.Original
		}
		key := row.Type + "\x00" + identity
		row.Priority = row.ShortID == opts.PriorityShortID
		current, exists := groups[key]
		if !exists || (row.Priority && !current.Priority) {
			row.CanonicalURL = identity
			// Rows are newest-first, except an explicitly prioritized short ID
			// becomes the representative for its whole duplicate group.
			groups[key] = row
		}
	}
	candidates := make([]socialThumbnailCandidate, 0, len(groups))
	for _, row := range groups {
		candidates = append(candidates, row)
	}
	sort.Slice(candidates, func(i, j int) bool {
		iPriority := candidates[i].Priority
		jPriority := candidates[j].Priority
		if iPriority != jPriority {
			return iPriority
		}
		iGallery := candidates[i].Type == utils.ArchiveTypeGalleryDl
		jGallery := candidates[j].Type == utils.ArchiveTypeGalleryDl
		if iGallery != jGallery {
			return iGallery // cheap stored-ZIP repairs first
		}
		iBD := candidates[i].Source == models.ArchiveSourceBrightData
		jBD := candidates[j].Source == models.ArchiveSourceBrightData
		if iBD != jBD {
			return iBD
		}
		return candidates[i].ItemID > candidates[j].ItemID
	})
	summary.TargetGroups = len(candidates)
	if opts.DryRun {
		return summary, nil
	}
	if client == nil {
		return summary, errors.New("River client is not configured")
	}

	for i, candidate := range candidates {
		args := SocialThumbnailBackfillJobArgs{
			Identity:  candidate.CanonicalURL,
			URL:       candidate.Original,
			ShortID:   candidate.ShortID,
			Type:      candidate.Type,
			StartedAt: started.Unix(),
			BudgetUSD: opts.BudgetUSD, Version: 3,
		}
		_, err := client.Insert(ctx, args, &river.InsertOpts{
			Queue: socialThumbnailBackfillQueue, MaxAttempts: 2,
			Priority:    4,
			ScheduledAt: started.Add(time.Duration(i) * time.Second),
			Tags:        []string{"thumbnail-backfill", candidate.Type, "run-" + strings.ReplaceAll(summary.RunID, ":", "")},
			UniqueOpts:  river.UniqueOpts{ByArgs: true, ByPeriod: 24 * time.Hour},
		})
		if err != nil {
			return summary, fmt.Errorf("enqueue social thumbnail group %s/%s: %w", candidate.ShortID, candidate.Type, err)
		}
		summary.Enqueued++
	}
	return summary, nil
}
