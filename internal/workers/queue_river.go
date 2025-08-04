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

// RiverQueueManager handles River-based job queueing
type RiverQueueManager struct {
	RiverClient *river.Client[pgx.Tx]
	db          *gorm.DB
}

// NewRiverQueueManager creates a new River queue manager
func NewRiverQueueManager(riverClient *river.Client[pgx.Tx], db *gorm.DB) *RiverQueueManager {
	return &RiverQueueManager{
		RiverClient: riverClient,
		db:          db,
	}
}

// QueueCapture creates a capture and queues jobs using River's transactional enqueueing
func (rqm *RiverQueueManager) QueueCapture(urlID uint, originalURL string, types []string) (string, error) {
	return rqm.QueueCaptureWithAPIKey(urlID, originalURL, types, nil)
}

// QueueCaptureWithAPIKey creates a capture with optional API key tracking using River
func (rqm *RiverQueueManager) QueueCaptureWithAPIKey(urlID uint, originalURL string, types []string, apiKeyID *uint) (string, error) {
	var shortID string
	var createdItems int

	err := rqm.db.Transaction(func(tx *gorm.DB) error {
		// Generate short ID
		shortID = utils.GenerateShortID(rqm.db)
		
		// Create capture
		capture := models.Capture{
			ArchivedURLID: urlID,
			Timestamp:     time.Now(),
			ShortID:       shortID,
			APIKeyID:      apiKeyID,
		}
		if err := tx.Create(&capture).Error; err != nil {
			slog.Error("Failed to create capture",
				"url", originalURL,
				"types", types,
				"error", err)
			return err
		}

		ctx := context.Background()

		// Create archive items and enqueue jobs
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

			// Enqueue job in River (no transaction support yet, so we'll enqueue after commit)
			// For now, we'll enqueue outside the transaction
			args := ArchiveJobArgs{
				CaptureID: capture.ID,
				ShortID:   capture.ShortID,
				Type:      t,
				URL:       originalURL,
			}

			opts := &river.InsertOpts{
				MaxAttempts: 3,
				Tags:        []string{"archive", t},
				UniqueOpts: river.UniqueOpts{
					ByArgs:   true,
					ByPeriod: 1 * time.Minute,
				},
			}

			if _, err := rqm.RiverClient.Insert(ctx, args, opts); err != nil {
				slog.Error("Failed to enqueue archive job",
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

	slog.Info("Queued new capture with River",
		"short_id", shortID,
		"url", originalURL,
		"types", types,
		"items_created", createdItems)

	return shortID, nil
}

// QueueCaptureForURL creates or finds an ArchivedURL and queues a capture using River
func (rqm *RiverQueueManager) QueueCaptureForURL(url string, types []string) (string, error) {
	return rqm.QueueCaptureForURLWithAPIKey(url, types, nil)
}

// QueueCaptureForURLWithAPIKey creates or finds an ArchivedURL and queues a capture with API key tracking using River
func (rqm *RiverQueueManager) QueueCaptureForURLWithAPIKey(url string, types []string, apiKeyID *uint) (string, error) {
	if len(types) == 0 {
		types = utils.GetArchiveTypes(url)
	}

	var u models.ArchivedURL
	err := rqm.db.Where("original = ?", url).First(&u).Error
	if err == gorm.ErrRecordNotFound {
		u = models.ArchivedURL{Original: url}
		if err = rqm.db.Create(&u).Error; err != nil {
			return "", err
		}
	} else if err != nil {
		return "", err
	}

	return rqm.QueueCaptureWithAPIKey(u.ID, url, types, apiKeyID)
}

// Legacy function wrappers for backward compatibility
// These will use River under the hood but maintain the same API

func QueueCaptureWithRiver(riverQueueManager *RiverQueueManager, urlID uint, originalURL string, types []string, apiKeyID *uint) (string, error) {
	return riverQueueManager.QueueCaptureWithAPIKey(urlID, originalURL, types, apiKeyID)
}

func QueueCaptureForURLWithRiver(riverQueueManager *RiverQueueManager, url string, types []string, apiKeyID *uint) (string, error) {
	return riverQueueManager.QueueCaptureForURLWithAPIKey(url, types, apiKeyID)
}
