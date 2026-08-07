package workers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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

// ThumbnailJobArgs is the payload for backfilling a thumbnail onto an archive
// item that was captured before thumbnails existed.
//
// Items are addressed by (short_id, type) rather than by row ID, matching
// ArchiveJobArgs, so the args stay meaningful and greppable in the River UI.
type ThumbnailJobArgs struct {
	ShortID string `json:"short_id"`
	Type    string `json:"type"`
}

// Kind returns the job kind for River.
func (ThumbnailJobArgs) Kind() string { return "thumbnail" }

// ThumbnailWorker generates a thumbnail from an already-stored archive artifact.
//
// New captures never reach this worker: their thumbnail is produced inline by
// the archiver that already holds the decoded image. This exists for archives
// captured before the feature, and it runs on demand -- the /thumb handler
// enqueues one the first time somebody actually looks at an archive that has no
// preview yet, so the backlog is paid off in the order it is needed rather than
// in one bulk sweep.
type ThumbnailWorker struct {
	river.WorkerDefaults[ThumbnailJobArgs]
	storage storage.Storage
	db      *gorm.DB
}

// NewThumbnailWorker creates a new thumbnail worker.
func NewThumbnailWorker(store storage.Storage, db *gorm.DB) *ThumbnailWorker {
	return &ThumbnailWorker{storage: store, db: db}
}

// Work generates and stores one thumbnail.
//
// It returns nil for every permanent condition (wrong type, source too large,
// undecodable bytes) after recording the item as unavailable. Returning an
// error there would buy three River attempts at an outcome that cannot change,
// and would leave the item unmarked so the next page view enqueues it again.
func (w *ThumbnailWorker) Work(ctx context.Context, job *river.Job[ThumbnailJobArgs]) error {
	return w.generate(ctx, job.Args)
}

// generate is Work without the River envelope, so it can be exercised directly.
func (w *ThumbnailWorker) generate(ctx context.Context, args ThumbnailJobArgs) error {
	args.Type = utils.NormalizeArchiveType(args.Type)

	logger := slog.With("worker", "thumbnail", "short_id", args.ShortID, "type", args.Type)

	var item models.ArchiveItem
	if err := w.db.Joins("JOIN captures ON archive_items.capture_id = captures.id").
		Where("captures.short_id = ? AND archive_items.type = ?", args.ShortID, args.Type).
		First(&item).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			logger.Warn("Archive item no longer exists; dropping thumbnail job")
			return nil
		}
		return fmt.Errorf("thumbnail: finding archive item for %s/%s: %w", args.ShortID, args.Type, err)
	}

	// Another worker may have finished this while the job sat in the queue.
	if item.ThumbnailStatus == models.ThumbnailStatusReady && item.ThumbnailKey != "" {
		logger.Debug("Thumbnail already present; nothing to do")
		return nil
	}

	if item.Status != "completed" || item.StorageKey == "" {
		// Not an error: the capture may still be running. Leave the status
		// unset so a later view can try again once the archive lands.
		logger.Debug("Archive item not completed; skipping thumbnail")
		return nil
	}

	if !thumbnail.CanDeriveFromArchive(item.Type) {
		return w.markUnavailable(&item, logger, "archive type cannot produce a thumbnail")
	}

	reader, err := w.storage.Reader(item.StorageKey)
	if err != nil {
		// Transient (network/S3) as far as we can tell from here; let River retry.
		return fmt.Errorf("thumbnail: opening %s: %w", item.StorageKey, err)
	}
	defer reader.Close()

	// CropTop: this path only ever runs on screenshots (CanDeriveFromArchive
	// gates it above), whose identity is at the top of the page.
	thumb, err := thumbnail.FromReader(reader, thumbnail.CropTop)
	if err != nil {
		// Every failure below the decoder is a property of these bytes and will
		// not change on retry.
		return w.markUnavailable(&item, logger, err.Error())
	}

	key := fmt.Sprintf("%s/%s-%s-thumb%s", args.ShortID, args.Type, uploadNonce(), thumbnail.Extension)
	if err := StoreThumbnail(&archivers.Thumbnail{Data: thumb.Data, Width: thumb.Width, Height: thumb.Height}, key, w.storage, w.db, &item); err != nil {
		return fmt.Errorf("thumbnail: storing %s: %w", key, err)
	}

	logger.Info("Thumbnail generated", "key", key, "width", thumb.Width, "height", thumb.Height, "bytes", len(thumb.Data))
	return nil
}

// markUnavailable records that this item will never have a thumbnail.
func (w *ThumbnailWorker) markUnavailable(item *models.ArchiveItem, logger *slog.Logger, reason string) error {
	logger.Info("Marking thumbnail unavailable", "reason", reason)
	if err := w.db.Model(item).Update("thumbnail_status", models.ThumbnailStatusUnavailable).Error; err != nil {
		return fmt.Errorf("thumbnail: marking unavailable: %w", err)
	}
	return nil
}

// EnqueueThumbnail requests generation of a thumbnail for one archive item.
//
// Safe to call on every cache miss. River's ByArgs uniqueness collapses the
// burst a single dashboard render produces -- one page can show hundreds of
// archives, and without this each would insert its own job.
func EnqueueThumbnail(ctx context.Context, riverClient *river.Client[pgx.Tx], shortID, archiveType string) error {
	if riverClient == nil {
		return nil
	}
	args := ThumbnailJobArgs{ShortID: shortID, Type: utils.NormalizeArchiveType(archiveType)}
	_, err := riverClient.Insert(ctx, args, &river.InsertOpts{
		// Deliberately not the default queue: thumbnail work is best-effort and
		// must never queue behind a three-hour video download. high_priority is
		// bounded at max(2, MaxWorkers/2), which also caps how many large
		// decodes can run at once.
		Queue:       "high_priority",
		MaxAttempts: 2,
		Tags:        []string{"thumbnail", args.Type},
		UniqueOpts: river.UniqueOpts{
			ByArgs:   true,
			ByPeriod: 5 * time.Minute,
		},
	})
	return err
}
