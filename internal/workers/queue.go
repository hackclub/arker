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

// QueueCapture creates an ArchivedURL (if needed), a capture, and queues archive jobs
func QueueCapture(ctx context.Context, db *gorm.DB, riverClient *river.Client[pgx.Tx], url string, types []string, apiKeyID *uint) (string, error) {
	if len(types) == 0 {
		types = utils.GetArchiveTypes(url)
	}

	var shortID string
	var createdItems int

	err := db.Transaction(func(tx *gorm.DB) error {
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

		// Generate short ID
		shortID = utils.GenerateShortID(tx)

		// Create capture
		capture := models.Capture{
			ArchivedURLID: u.ID,
			Timestamp:     time.Now(),
			ShortID:       shortID,
			APIKeyID:      apiKeyID,
		}
		if err := tx.Create(&capture).Error; err != nil {
			slog.Error("Failed to create capture",
				"url", url,
				"types", types,
				"error", err)
			return err
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
		return "", err
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
