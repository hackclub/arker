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

const (
	FindOrCreateFound      = "found"
	FindOrCreateInProgress = "in_progress"
	FindOrCreateCreated    = "created"
)

// FindOrCreateResult describes the canonical capture selected or created by
// FindOrCreateCapture.
type FindOrCreateResult struct {
	Action  string
	ShortID string
	Status  string
}

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

	jobsEnqueued := enqueueCaptureJobs(ctx, riverClient, shortID, url, types)

	slog.Info("Queued new capture",
		"short_id", shortID,
		"url", url,
		"types", types,
		"items_created", createdItems,
		"jobs_enqueued", jobsEnqueued)

	return shortID, nil
}

func enqueueCaptureJobs(ctx context.Context, riverClient *river.Client[pgx.Tx], shortID, url string, types []string) int {
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
	return jobsEnqueued
}

// FindOrCreateCapture returns the newest completed canonical capture that
// covers types, joins a canonical in-flight capture when possible, or creates
// and queues a new canonical capture. Unlike QueueCapture's compatibility
// aliasing behavior, this operation has no freshness window and never creates
// an alias.
func FindOrCreateCapture(ctx context.Context, db *gorm.DB, riverClient *river.Client[pgx.Tx], url string, types []string, apiKeyID *uint) (FindOrCreateResult, error) {
	if len(types) == 0 {
		types = utils.GetArchiveTypes(url)
	} else {
		types = utils.NormalizeArchiveTypes(types)
	}

	var result FindOrCreateResult
	err := db.Transaction(func(tx *gorm.DB) error {
		if tx.Dialector.Name() == "postgres" {
			if err := tx.Exec("SELECT pg_advisory_xact_lock(hashtext(?))", url).Error; err != nil {
				return err
			}
		}

		var archivedURL models.ArchivedURL
		err := tx.Where("original = ?", url).First(&archivedURL).Error
		if err != nil && err != gorm.ErrRecordNotFound {
			return err
		}
		if err == nil {
			capture, status, findErr := findFindOrCreateCandidate(tx, archivedURL.ID, types)
			if findErr != nil {
				return findErr
			}
			if capture != nil {
				result = FindOrCreateResult{Action: FindOrCreateFound, ShortID: capture.ShortID, Status: status}
				if status != "completed" {
					result.Action = FindOrCreateInProgress
				}
				return nil
			}
		} else {
			archivedURL = models.ArchivedURL{Original: url}
			if err := tx.Create(&archivedURL).Error; err != nil {
				return err
			}
		}

		capture := models.Capture{ArchivedURLID: archivedURL.ID, Timestamp: time.Now(), ShortID: utils.GenerateShortID(tx), APIKeyID: apiKeyID}
		if err := tx.Create(&capture).Error; err != nil {
			return err
		}
		for _, typ := range types {
			if err := tx.Create(&models.ArchiveItem{CaptureID: capture.ID, Type: typ, Status: "pending"}).Error; err != nil {
				return err
			}
		}
		result = FindOrCreateResult{Action: FindOrCreateCreated, ShortID: capture.ShortID, Status: "pending"}
		return nil
	})
	if err != nil {
		return FindOrCreateResult{}, err
	}
	if result.Action == FindOrCreateCreated {
		if riverClient != nil { // nil is useful for transaction-focused unit tests.
			enqueueCaptureJobs(ctx, riverClient, result.ShortID, url, types)
		}
	}
	return result, nil
}

// findFindOrCreateCandidate deliberately has no age or row limit. Completion
// time is the latest UpdatedAt among the required items; this is when the
// requested set became usable, and is distinct from capture creation time.
func findFindOrCreateCandidate(tx *gorm.DB, archivedURLID uint, types []string) (*models.Capture, string, error) {
	var candidates []models.Capture
	if err := tx.Where("archived_url_id = ? AND alias_of_id IS NULL", archivedURLID).
		Preload("ArchiveItems").Find(&candidates).Error; err != nil {
		return nil, "", err
	}

	var newestCompleted *models.Capture
	var newestCompletedAt time.Time
	var newestInProgress *models.Capture
	for i := range candidates {
		status, completedAt, ok := captureStatusForTypes(&candidates[i], types)
		if !ok {
			continue
		}
		if status == "completed" {
			if newestCompleted == nil || completedAt.After(newestCompletedAt) {
				newestCompleted, newestCompletedAt = &candidates[i], completedAt
			}
		} else if newestInProgress == nil || candidates[i].Timestamp.After(newestInProgress.Timestamp) {
			newestInProgress = &candidates[i]
		}
	}
	if newestCompleted != nil {
		return newestCompleted, "completed", nil
	}
	if newestInProgress != nil {
		status, _, _ := captureStatusForTypes(newestInProgress, types)
		return newestInProgress, status, nil
	}
	return nil, "", nil
}

func captureStatusForTypes(c *models.Capture, types []string) (string, time.Time, bool) {
	byType := make(map[string]models.ArchiveItem, len(c.ArchiveItems))
	for _, item := range c.ArchiveItems {
		byType[utils.NormalizeArchiveType(item.Type)] = item
	}
	status := "completed"
	var completedAt time.Time
	for _, typ := range types {
		item, ok := byType[utils.NormalizeArchiveType(typ)]
		if !ok {
			return "", time.Time{}, false
		}
		switch item.Status {
		case "completed":
			if item.UpdatedAt.After(completedAt) {
				completedAt = item.UpdatedAt
			}
		case "processing":
			status = "processing"
		case "pending":
			if status != "processing" {
				status = "pending"
			}
		default:
			return "", time.Time{}, false
		}
	}
	return status, completedAt, true
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
