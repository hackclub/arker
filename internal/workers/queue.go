package workers

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"gorm.io/gorm"

	"arker/internal/models"
	"arker/internal/utils"
)

// QueueCapture creates an ArchivedURL (if needed), a capture, and queues
// archive jobs.
//
// When force is false and a canonical capture of the same URL exists within
// the freshness window (configs key "capture_freshness_window") that covers
// every requested type with none of them failed, the new capture is created as
// an alias of it: it gets its own short ID, timestamp, and API key for
// provenance, but owns no archive items and enqueues no jobs. Serving resolves
// aliases to the canonical capture with a visible redirect.
func QueueCapture(ctx context.Context, db *gorm.DB, riverClient *river.Client[pgx.Tx], url string, types []string, apiKeyID *uint, force bool) (string, error) {
	if len(types) == 0 {
		types = utils.GetArchiveTypes(url)
	} else {
		// Callers may still send retired type names (e.g. "youtube"); store
		// and queue the canonical name so there is one type per archiver.
		types = utils.NormalizeArchiveTypes(types)
	}

	shortID, aliasOf, createdItems, err := createCapture(db, url, types, apiKeyID, force)
	if err != nil {
		return "", err
	}

	if aliasOf != nil {
		slog.Info("Queued alias capture",
			"short_id", shortID,
			"url", url,
			"types", types,
			"alias_of_short_id", aliasOf.ShortID,
			"alias_of_id", aliasOf.ID)
		return shortID, nil
	}

	// Enqueue jobs in River (after successful DB transaction)
	jobsEnqueued := 0

	for _, t := range types {
		args := ArchiveJobArgs{
			CaptureID: 0, // Will be looked up by short_id and type
			ShortID:   shortID,
			Type:      t,
			URL:       url,
		}

		opts := &river.InsertOpts{
			MaxAttempts: 3,
			Tags:        []string{"archive", t},
			UniqueOpts: river.UniqueOpts{
				ByArgs:   true,
				ByPeriod: 1 * time.Minute,
			},
		}

		if _, err := riverClient.Insert(ctx, args, opts); err != nil {
			slog.Error("Failed to enqueue archive job",
				"short_id", shortID,
				"type", t,
				"error", err)
			// Continue with other jobs even if one fails
		} else {
			jobsEnqueued++
		}
	}

	slog.Info("Queued new capture",
		"short_id", shortID,
		"url", url,
		"types", types,
		"items_created", createdItems,
		"jobs_enqueued", jobsEnqueued)

	return shortID, nil
}

// createCapture runs the capture-creation transaction: find-or-create the
// ArchivedURL, decide between a full capture and an alias, and create the
// capture row (plus archive items for full captures). It returns the new
// short ID, the canonical capture when the new capture is an alias (nil for
// full captures), and the number of archive items created.
func createCapture(db *gorm.DB, url string, types []string, apiKeyID *uint, force bool) (string, *models.Capture, int, error) {
	var shortID string
	var createdItems int
	var aliasOf *models.Capture

	err := db.Transaction(func(tx *gorm.DB) error {
		// Serialize concurrent submissions of the same URL until this
		// transaction commits. Without this, two same-second POSTs (18.4% of
		// same-day repeats in prod) both pass the freshness check below and
		// both trigger a full re-archive. hashtext collisions merely cause
		// harmless extra serialization. Advisory locks are Postgres-only;
		// other dialects (SQLite in tests) skip this.
		if tx.Dialector.Name() == "postgres" {
			if err := tx.Exec("SELECT pg_advisory_xact_lock(hashtext(?))", url).Error; err != nil {
				return err
			}
		}

		// Find or create ArchivedURL
		var u models.ArchivedURL
		err := tx.Where("original = ?", url).First(&u).Error
		if err == gorm.ErrRecordNotFound {
			u = models.ArchivedURL{Original: url}
			if err = tx.Create(&u).Error; err != nil {
				return err
			}
		} else if err != nil {
			return err
		}

		if !force {
			aliasOf = findReusableCapture(tx, u.ID, types)
		}

		// Generate short ID
		shortID = utils.GenerateShortID(tx)

		// Create capture
		capture := models.Capture{
			ArchivedURLID: u.ID,
			Timestamp:     time.Now(),
			ShortID:       shortID,
			APIKeyID:      apiKeyID,
		}
		if aliasOf != nil {
			capture.AliasOfID = &aliasOf.ID
		}
		if err := tx.Create(&capture).Error; err != nil {
			slog.Error("Failed to create capture",
				"url", url,
				"types", types,
				"error", err)
			return err
		}

		// Alias captures own no archive items; the canonical capture's items
		// serve for both.
		if aliasOf != nil {
			return nil
		}

		// Create archive items
		for _, t := range types {
			item := models.ArchiveItem{
				CaptureID: capture.ID,
				Type:      t,
				Status:    "pending",
			}
			if err := tx.Create(&item).Error; err != nil {
				slog.Error("Failed to create archive item",
					"short_id", shortID,
					"type", t,
					"error", err)
				return err
			}
			createdItems++
		}

		return nil
	})
	if err != nil {
		return "", nil, 0, err
	}
	return shortID, aliasOf, createdItems, nil
}

// findReusableCapture returns the newest canonical (non-alias) capture of
// archivedURLID within the freshness window whose archive items cover every
// requested type with none of those items failed, or nil when a full capture
// is required. Pending/processing items are acceptable: their jobs are already
// in flight on the canonical capture.
func findReusableCapture(tx *gorm.DB, archivedURLID uint, types []string) *models.Capture {
	window := utils.CaptureFreshnessWindow(tx)
	if window <= 0 {
		return nil // aliasing disabled via config
	}

	var candidates []models.Capture
	if err := tx.Where("archived_url_id = ? AND alias_of_id IS NULL AND timestamp > ?",
		archivedURLID, time.Now().Add(-window)).
		Order("timestamp DESC").
		Limit(10).
		Preload("ArchiveItems").
		Find(&candidates).Error; err != nil {
		slog.Error("Failed to look up reusable captures", "archived_url_id", archivedURLID, "error", err)
		return nil
	}

	for i := range candidates {
		if captureCoversTypes(&candidates[i], types) {
			return &candidates[i]
		}
	}
	return nil
}

// captureCoversTypes reports whether the capture has an archive item for every
// requested type and none of those items is failed. Types are compared in
// canonical form so rows still carrying a retired name (the startup rename
// migration is best-effort) keep matching.
func captureCoversTypes(c *models.Capture, types []string) bool {
	byType := make(map[string]string, len(c.ArchiveItems))
	for _, item := range c.ArchiveItems {
		byType[utils.NormalizeArchiveType(item.Type)] = item.Status
	}
	for _, t := range types {
		status, ok := byType[utils.NormalizeArchiveType(t)]
		if !ok || status == "failed" {
			return false
		}
	}
	return true
}
