package workers

import (
	"context"
	"log/slog"
	"sync"
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

// captureIdentityLocks serializes capture creation for one canonical identity
// within this process. It is the in-process half of the pair described on
// withCaptureIdentityLock.
var captureIdentityLocks = &identityLockSet{locks: map[string]*identityLock{}}

type identityLockSet struct {
	mu    sync.Mutex
	locks map[string]*identityLock
}

type identityLock struct {
	mu   sync.Mutex
	refs int
}

// acquire blocks until identity is free and returns its release func. Waiters
// are counted before blocking so an entry cannot be evicted out from under one.
func (s *identityLockSet) acquire(identity string) func() {
	s.mu.Lock()
	lock := s.locks[identity]
	if lock == nil {
		lock = &identityLock{}
		s.locks[identity] = lock
	}
	lock.refs++
	s.mu.Unlock()

	lock.mu.Lock()
	return func() {
		lock.mu.Unlock()
		s.mu.Lock()
		lock.refs--
		if lock.refs == 0 {
			delete(s.locks, identity)
		}
		s.mu.Unlock()
	}
}

// withCaptureIdentityLock runs fn inside a transaction serialized on a canonical
// identity, so concurrent submissions of one post cannot each conclude that no
// capture exists and each start a full archive. Two same-second POSTs are 18.4%
// of same-day repeats in prod; keying on the canonical identity rather than the
// raw string extends that protection to two different *spellings* of one post,
// which is the whole point of this change.
//
// Both halves are needed and neither is redundant:
//
//   - pg_advisory_xact_lock is the real lock. It is held until the transaction
//     commits and it works across app instances, which is the deployed shape.
//     hashtext collisions merely cause harmless extra serialization.
//   - The in-process mutex covers dialects that have no advisory locks (SQLite,
//     which is what the tests run on). It is not cross-process safe and does not
//     pretend to be — on Postgres it is a cheap fast-path that the advisory lock
//     would enforce anyway — but it makes the concurrency contract executable
//     rather than assertable only against a live Postgres.
//
// Lock order is fixed (in-process, then advisory) and fn takes no further
// identity locks, so the pair cannot deadlock.
func withCaptureIdentityLock(db *gorm.DB, identity string, fn func(tx *gorm.DB) error) error {
	unlock := captureIdentityLocks.acquire(identity)
	defer unlock()

	return db.Transaction(func(tx *gorm.DB) error {
		if tx.Dialector.Name() == "postgres" {
			if err := tx.Exec("SELECT pg_advisory_xact_lock(hashtext(?))", identity).Error; err != nil {
				return err
			}
		}
		return fn(tx)
	})
}

// loadIdentityRows returns every ArchivedURL row sharing url's canonical
// identity, plus the row (if any) holding url verbatim.
//
// The original column is matched as well as the canonical one so the lookup
// degrades to exactly its pre-canonicalization behavior when canonical_url is
// still empty — a row created before the column existed, or one the startup
// backfill has not reached yet. Matching the canonical string against original
// catches the same case from the other side: a legacy row that happens to have
// been stored in canonical form already.
func loadIdentityRows(tx *gorm.DB, url, canonical string) ([]models.ArchivedURL, *models.ArchivedURL, error) {
	var rows []models.ArchivedURL
	if err := tx.Where("canonical_url = ? OR original = ? OR original = ?", canonical, url, canonical).
		Order("id").Find(&rows).Error; err != nil {
		return nil, nil, err
	}
	for i := range rows {
		if rows[i].Original == url {
			return rows, &rows[i], nil
		}
	}
	return rows, nil, nil
}

func archivedURLIDs(rows []models.ArchivedURL) []uint {
	ids := make([]uint, len(rows))
	for i := range rows {
		ids[i] = rows[i].ID
	}
	return ids
}

// ensureArchivedURL returns the row a new capture of url should hang off,
// creating it when this exact spelling has never been archived.
//
// Every distinct spelling keeps its own row on purpose: the contract is that
// the submitted URL is stored, displayed, and archived untouched, and rewriting
// Original to a canonical form would break that (and would rewrite history for
// rows that already exist). The canonical column is what ties the spellings
// together.
func ensureArchivedURL(tx *gorm.DB, url, canonical string, exact *models.ArchivedURL) (models.ArchivedURL, error) {
	if exact != nil {
		// Self-heal a row the startup backfill missed or predates, so the next
		// lookup can match it on the indexed canonical column.
		if exact.CanonicalURL != canonical {
			if err := tx.Model(&models.ArchivedURL{}).Where("id = ?", exact.ID).
				UpdateColumn("canonical_url", canonical).Error; err != nil {
				return models.ArchivedURL{}, err
			}
			exact.CanonicalURL = canonical
		}
		return *exact, nil
	}
	created := models.ArchivedURL{Original: url, CanonicalURL: canonical}
	if err := tx.Create(&created).Error; err != nil {
		return models.ArchivedURL{}, err
	}
	return created, nil
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

	canonical := utils.CanonicalizeArchiveURL(url)

	var result FindOrCreateResult
	err := withCaptureIdentityLock(db, canonical, func(tx *gorm.DB) error {
		rows, exact, err := loadIdentityRows(tx, url, canonical)
		if err != nil {
			return err
		}
		if len(rows) > 0 {
			// Candidates are gathered across every row sharing the identity:
			// after the backfill several spellings of one post each have their
			// own row, and the newest completed capture of the post can hang off
			// any of them.
			capture, status, findErr := findFindOrCreateCandidate(tx, archivedURLIDs(rows), types)
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
		}

		archivedURL, err := ensureArchivedURL(tx, url, canonical, exact)
		if err != nil {
			return err
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
//
// archivedURLIDs is every row sharing one canonical identity, so a capture made
// under one spelling of a post answers a request made under another.
func findFindOrCreateCandidate(tx *gorm.DB, archivedURLIDs []uint, types []string) (*models.Capture, string, error) {
	if len(archivedURLIDs) == 0 {
		return nil, "", nil
	}
	var candidates []models.Capture
	if err := tx.Where("archived_url_id IN ? AND alias_of_id IS NULL", archivedURLIDs).
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
	canonical := utils.CanonicalizeArchiveURL(url)

	var shortID string
	var createdItems int
	var aliasOf *models.Capture

	err := withCaptureIdentityLock(db, canonical, func(tx *gorm.DB) error {
		rows, exact, err := loadIdentityRows(tx, url, canonical)
		if err != nil {
			return err
		}

		if !force {
			aliasOf = findReusableCapture(tx, archivedURLIDs(rows), types)
		}

		// Find or create the ArchivedURL for this exact spelling.
		u, err := ensureArchivedURL(tx, url, canonical, exact)
		if err != nil {
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

// findReusableCapture returns the newest canonical (non-alias) capture sharing
// the requested canonical identity, within the freshness window, whose archive
// items cover every requested type with none of those items failed — or nil
// when a full capture is required. Pending/processing items are acceptable:
// their jobs are already in flight on the canonical capture.
func findReusableCapture(tx *gorm.DB, archivedURLIDs []uint, types []string) *models.Capture {
	if len(archivedURLIDs) == 0 {
		return nil
	}
	window := utils.CaptureFreshnessWindow(tx)
	if window <= 0 {
		return nil // aliasing disabled via config
	}

	var candidates []models.Capture
	if err := tx.Where("archived_url_id IN ? AND alias_of_id IS NULL AND timestamp > ?",
		archivedURLIDs, time.Now().Add(-window)).
		Order("timestamp DESC").
		Limit(10).
		Preload("ArchiveItems").
		Find(&candidates).Error; err != nil {
		slog.Error("Failed to look up reusable captures", "archived_url_ids", archivedURLIDs, "error", err)
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
