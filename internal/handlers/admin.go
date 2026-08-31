package handlers

import (
	"arker/internal/models"
	"arker/internal/utils"
	"arker/internal/workers"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"gorm.io/gorm"
)

func AdminGet(c *gin.Context, db *gorm.DB) {
	if !RequireLogin(c) {
		return
	}

	// Get pagination parameters
	page := 1
	if p := c.Query("page"); p != "" {
		if parsed, err := strconv.Atoi(p); err == nil && parsed > 0 {
			page = parsed
		}
	}

	// Get search parameter
	search := c.Query("search")

	const limit = 1000
	offset := (page - 1) * limit

	// Build base query with search filter
	baseQuery := db.Model(&models.ArchivedURL{}).
		Joins("LEFT JOIN captures ON archived_urls.id = captures.archived_url_id").
		Joins("LEFT JOIN archive_items ON captures.id = archive_items.capture_id").
		Group("archived_urls.id")

	if search != "" {
		baseQuery = baseQuery.Where("archived_urls.original ILIKE ?", "%"+search+"%")
	}

	// Get total count for pagination info
	var total int64
	baseQuery.Count(&total)

	// Build query for fetching URLs with same search filter
	urlQuery := db.Preload("Captures.ArchiveItems").Preload("Captures.APIKey").
		Preload("Captures.AliasOf").Preload("Captures.AliasOf.ArchiveItems").
		Preload("Captures", func(db *gorm.DB) *gorm.DB {
			return db.Order("created_at DESC")
		}).Joins("LEFT JOIN captures ON archived_urls.id = captures.archived_url_id").
		Joins("LEFT JOIN archive_items ON captures.id = archive_items.capture_id").
		Group("archived_urls.id").
		Order("MAX(archive_items.created_at) DESC").
		Offset(offset).Limit(limit)

	if search != "" {
		urlQuery = urlQuery.Where("archived_urls.original ILIKE ?", "%"+search+"%")
	}

	var urls []models.ArchivedURL
	urlQuery.Find(&urls)

	// Add queue status summary for dashboard
	var queueSummary struct {
		Pending         int64
		Processing      int64
		Failed          int64
		QueueSize       int
		RecentCompleted int64
	}
	db.Model(&models.ArchiveItem{}).Where("status = 'pending'").Count(&queueSummary.Pending)
	db.Model(&models.ArchiveItem{}).Where("status = 'processing'").Count(&queueSummary.Processing)
	db.Model(&models.ArchiveItem{}).Where("status = 'failed'").Count(&queueSummary.Failed)
	// River queue size is managed internally, we can get stats if needed
	queueSummary.QueueSize = 0 // River handles queue internally

	// Count jobs completed in the past 5 minutes
	fiveMinutesAgo := time.Now().Add(-5 * time.Minute)
	db.Model(&models.ArchiveItem{}).Where("status = 'completed' AND updated_at > ?", fiveMinutesAgo).Count(&queueSummary.RecentCompleted)

	// Calculate pagination info
	totalPages := int(math.Ceil(float64(total) / float64(limit)))

	c.HTML(http.StatusOK, "admin.html", gin.H{
		"urls":         urls,
		"queueSummary": queueSummary,
		"search":       search,
		"pagination": gin.H{
			"currentPage": page,
			"totalPages":  totalPages,
			"totalItems":  total,
			"hasNext":     page < totalPages,
			"hasPrev":     page > 1,
			"nextPage":    page + 1,
			"prevPage":    page - 1,
		},
	})
}

func RequestCapture(c *gin.Context, db *gorm.DB, riverClient *river.Client[pgx.Tx]) {
	id := c.Param("id")
	var u models.ArchivedURL
	if db.First(&u, id).Error != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid URL ID"})
		return
	}

	types := utils.GetArchiveTypes(u.Original)
	// Admin re-archive always forces a real capture, never an alias.
	shortID, err := workers.QueueCapture(c.Request.Context(), db, riverClient, u.Original, types, nil, true)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to queue capture"})
		return
	}

	// Construct full URL from request host
	fullURL := utils.BuildFullURL(c, shortID)

	c.JSON(http.StatusOK, gin.H{"url": fullURL})
}

func GetItemLog(c *gin.Context, db *gorm.DB) {
	id := c.Param("id")
	var item models.ArchiveItem
	if db.First(&item, id).Error != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Not found"})
		return
	}
	logs, err := utils.ArchiveItemLogString(db, item.ID, item.Logs)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to get logs"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"logs": logs})
}

// RetryAllFailedJobs directly retries all failed archive items.
// Pass ?type=yt-dlp (or any archive type) to retry only one archive type.
func RetryAllFailedJobs(c *gin.Context, db *gorm.DB, riverClient *river.Client[pgx.Tx]) {
	query := db.Where("status = 'failed'")
	if typ := c.Query("type"); typ != "" {
		// Normalize so a runbook that still says ?type=youtube keeps matching
		// rows rather than silently retrying nothing.
		query = query.Where("type = ?", utils.NormalizeArchiveType(typ))
	}

	// Get all failed items
	var items []models.ArchiveItem
	if err := query.Find(&items).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to get failed items"})
		return
	}

	if len(items) == 0 {
		c.JSON(http.StatusOK, gin.H{"message": "No failed jobs to retry"})
		return
	}

	// Reset status to pending and enqueue new jobs
	retriedCount := 0
	for _, item := range items {
		// Get the capture info for this item
		var capture models.Capture
		if err := db.Preload("ArchivedURL").First(&capture, item.CaptureID).Error; err != nil {
			continue // Skip if we can't find the capture
		}

		// Update status to pending
		if err := db.Model(&item).Update("status", "pending").Error; err != nil {
			continue // Skip this item if update fails
		}

		// Queue new archive job
		args := workers.ArchiveJobArgs{
			CaptureID: 0, // Will be looked up by short_id and type
			ShortID:   capture.ShortID,
			Type:      item.Type,
			URL:       capture.ArchivedURL.Original,
		}

		opts := &river.InsertOpts{
			MaxAttempts: 3,
			Tags:        []string{"archive", item.Type, "retry"},
			UniqueOpts: river.UniqueOpts{
				ByArgs:   true,
				ByPeriod: 1 * time.Minute,
			},
		}

		if _, err := riverClient.Insert(c.Request.Context(), args, opts); err == nil {
			retriedCount++
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"message": fmt.Sprintf("Retried %d of %d failed items", retriedCount, len(items)),
	})
}

// BackfillSocialThumbnails queues one low-priority job per canonical social
// URL/type group. cost_limit_usd is a hard shared ceiling for provider lookups;
// zero permits only stored/native work. priority_short_id lets an operator put
// a known bad capture first without creating a second, independently funded
// run.
func BackfillSocialThumbnails(c *gin.Context, db *gorm.DB, riverClient *river.Client[pgx.Tx]) {
	budget := 0.0
	if raw := c.Query("cost_limit_usd"); raw != "" {
		parsed, err := strconv.ParseFloat(raw, 64)
		if err != nil || parsed < 0 || parsed > 100 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "cost_limit_usd must be between 0 and 100"})
			return
		}
		budget = parsed
	}
	summary, err := workers.EnqueueSocialThumbnailBackfill(c.Request.Context(), db, riverClient, workers.SocialThumbnailBackfillOptions{
		BudgetUSD:       budget,
		PriorityShortID: c.Query("priority_short_id"),
		DryRun:          c.Query("dry_run") == "true",
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error(), "summary": summary})
		return
	}
	c.JSON(http.StatusAccepted, summary)
}

// SocialThumbnailBackfillStatus reports durable progress from archive_items
// plus River's live job states. Pass ?since=<RFC3339> to include provider spend
// for a particular run.
func SocialThumbnailBackfillStatus(c *gin.Context, db *gorm.DB) {
	base := db.Model(&models.ArchiveItem{}).
		Where("type IN ? AND status = ?", []string{utils.ArchiveTypeGalleryDl, utils.ArchiveTypeYtDlp}, "completed")
	var total, originals, fallbacks int64
	base.Count(&total)
	base.Where("thumbnail_kind = ?", models.ThumbnailKindSocialOriginal).Count(&originals)
	base.Where("thumbnail_kind = ?", models.ThumbnailKindSocialFallback).Count(&fallbacks)

	queue := map[string]int64{}
	var queueRows []struct {
		State string
		Count int64
	}
	if err := db.Table("river_job").Select("state, COUNT(*) AS count").
		Where("kind = ?", (workers.SocialThumbnailBackfillJobArgs{}).Kind()).
		Group("state").Scan(&queueRows).Error; err == nil {
		for _, row := range queueRows {
			queue[row.State] = row.Count
		}
	}

	response := gin.H{
		"total_social_items": total,
		"original":           originals,
		"fallback":           fallbacks,
		"remaining":          total - originals - fallbacks,
		"queue":              queue,
	}
	if raw := c.Query("since"); raw != "" {
		if since, err := time.Parse(time.RFC3339, raw); err == nil {
			var spend float64
			if db.Model(&models.BrightDataUsage{}).Where("created_at >= ?", since).
				Select("COALESCE(SUM(cost_usd), 0)").Scan(&spend).Error == nil {
				response["provider_cost_usd_since"] = spend
			}
		} else {
			c.JSON(http.StatusBadRequest, gin.H{"error": "since must be RFC3339"})
			return
		}
	}
	c.JSON(http.StatusOK, response)
}

// mediaBackfillURLPattern pre-filters candidate URLs in SQL for each media
// archive type. Loading every capture and filtering in Go blows Postgres's
// 65535-parameter limit at production scale (~100k captures); the Go predicate
// named alongside each pattern stays the exact filter.
var mediaBackfillURLPattern = map[string]struct {
	sqlLike string
	matches func(string) bool
}{
	utils.ArchiveTypeYtDlp: {
		sqlLike: "(LOWER(archived_urls.original) LIKE '%youtube.com%' OR LOWER(archived_urls.original) LIKE '%youtu.be%' OR LOWER(archived_urls.original) LIKE '%vimeo.com%' OR LOWER(archived_urls.original) LIKE '%instagram.com%' OR LOWER(archived_urls.original) LIKE '%tiktok.com%' OR LOWER(archived_urls.original) LIKE '%facebook.com%' OR LOWER(archived_urls.original) LIKE '%fb.watch%')",
		matches: utils.IsVideoURL,
	},
	utils.ArchiveTypeGalleryDl: {
		sqlLike: "(LOWER(archived_urls.original) LIKE '%instagram.com%' OR LOWER(archived_urls.original) LIKE '%twitter.com%' OR LOWER(archived_urls.original) LIKE '%x.com%' OR LOWER(archived_urls.original) LIKE '%reddit.com%' OR LOWER(archived_urls.original) LIKE '%redd.it%' OR LOWER(archived_urls.original) LIKE '%tumblr.com%' OR LOWER(archived_urls.original) LIKE '%bsky.app%' OR LOWER(archived_urls.original) LIKE '%flickr.com%' OR LOWER(archived_urls.original) LIKE '%imgur.com%' OR LOWER(archived_urls.original) LIKE '%deviantart.com%' OR LOWER(archived_urls.original) LIKE '%artstation.com%' OR LOWER(archived_urls.original) LIKE '%pixiv.net%' OR LOWER(archived_urls.original) LIKE '%pinterest.com%' OR LOWER(archived_urls.original) LIKE '%newgrounds.com%' OR LOWER(archived_urls.original) LIKE '%vsco.co%' OR LOWER(archived_urls.original) LIKE '%facebook.com%')",
		// Same gate as live capture: without cookies, backfilling a login-only
		// site would queue thousands of guaranteed failures.
		//
		// With the Bright Data fallback configured, a login-only site it covers
		// (Instagram, X, Pinterest, Facebook posts) passes this gate instead —
		// so those rows are no longer guaranteed failures, they are guaranteed
		// *spend*: one dataset record each, after the native attempt fails. Use
		// ?limit and ?dry_run before a bulk run against those platforms.
		//
		// facebook.com is in both patterns because the host splits by shape:
		// video permalinks take the yt-dlp row, photo posts take this one. The
		// Go predicate is what actually decides.
		matches: utils.ShouldCreateGalleryDLItem,
	},
}

// BackfillMissingMediaItems creates and enqueues missing media archive items
// for captures whose URL should have one.
//
// Two things produce these gaps: a URL family that gained support after the
// capture was taken (TikTok short links, or every Instagram photo post taken
// before gallery-dl existed), and detection rules that changed underneath
// existing rows. Failed items are re-run via RetryAllFailedJobs instead — this
// only creates items that are absent entirely.
//
// Pass ?type=gallery-dl or ?type=yt-dlp to backfill one type (default: both),
// ?dry_run=true to preview without queueing, and ?limit=N to bound a run.
// Bounding matters for Instagram, which has soft-blocked this account for hours
// in response to bulk traffic.
func BackfillMissingMediaItems(c *gin.Context, db *gorm.DB, riverClient *river.Client[pgx.Tx]) {
	dryRun := c.Query("dry_run") == "true"

	requestedTypes := []string{utils.ArchiveTypeGalleryDl, utils.ArchiveTypeYtDlp}
	if requested := c.Query("type"); requested != "" {
		canonical := utils.NormalizeArchiveType(requested)
		if _, ok := mediaBackfillURLPattern[canonical]; !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("unsupported backfill type: %s", requested)})
			return
		}
		requestedTypes = []string{canonical}
	}

	limit := 0
	if rawLimit := c.Query("limit"); rawLimit != "" {
		parsed, err := strconv.Atoi(rawLimit)
		if err != nil || parsed < 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "limit must be a non-negative integer"})
			return
		}
		limit = parsed
	}

	backfilled := map[string][]string{}
	total := 0

	for _, archiveType := range requestedTypes {
		filter := mediaBackfillURLPattern[archiveType]
		// The limit is per type, not a shared budget: with both types selected
		// a shared budget would be spent entirely on whichever runs first and
		// silently do none of the other.
		typeCount := 0

		var candidates []struct {
			ID       uint
			ShortID  string
			Original string
		}
		if err := db.Table("captures").
			Select("captures.id, captures.short_id, archived_urls.original").
			Joins("JOIN archived_urls ON archived_urls.id = captures.archived_url_id").
			Where("captures.deleted_at IS NULL AND archived_urls.deleted_at IS NULL").
			// Alias captures own no items by design; the canonical capture is
			// the one that gets backfilled.
			Where("captures.alias_of_id IS NULL").
			Where("NOT EXISTS (SELECT 1 FROM archive_items WHERE archive_items.capture_id = captures.id AND archive_items.type = ? AND archive_items.deleted_at IS NULL)", archiveType).
			Where(filter.sqlLike).
			// Deterministic order so a bounded dry run previews the same rows
			// the real run will take.
			Order("captures.id").
			Scan(&candidates).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to query captures"})
			return
		}

		for _, capture := range candidates {
			if limit > 0 && typeCount >= limit {
				break
			}
			if !filter.matches(capture.Original) {
				continue
			}
			if dryRun {
				backfilled[archiveType] = append(backfilled[archiveType], capture.ShortID)
				typeCount++
				total++
				continue
			}

			item := models.ArchiveItem{CaptureID: capture.ID, Type: archiveType, Status: "pending"}
			if err := db.Create(&item).Error; err != nil {
				continue
			}

			args := workers.ArchiveJobArgs{
				ShortID: capture.ShortID,
				Type:    archiveType,
				URL:     capture.Original,
			}
			opts := &river.InsertOpts{
				MaxAttempts: 3,
				Tags:        []string{"archive", archiveType, "backfill"},
				UniqueOpts: river.UniqueOpts{
					ByArgs:   true,
					ByPeriod: 1 * time.Minute,
				},
			}
			if _, err := riverClient.Insert(c.Request.Context(), args, opts); err != nil {
				continue
			}
			backfilled[archiveType] = append(backfilled[archiveType], capture.ShortID)
			typeCount++
			total++
		}
	}

	message := fmt.Sprintf("Backfilled %d captures with media archive jobs", total)
	if dryRun {
		message = fmt.Sprintf("Dry run: %d captures would be backfilled with media archive jobs", total)
	}
	c.JSON(http.StatusOK, gin.H{"message": message, "count": total, "short_ids": backfilled})
}

func AdminArchive(c *gin.Context, db *gorm.DB, riverClient *river.Client[pgx.Tx]) {
	var req utils.ArchiveRequest
	if err := c.BindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request format"})
		return
	}

	// Validate the request including SSRF protection
	if err := req.Validate(); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Admin archive always forces a real capture, never an alias.
	shortID, err := workers.QueueCapture(c.Request.Context(), db, riverClient, req.URL, req.Types, nil, true)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to queue capture"})
		return
	}

	// Construct full URL from request host
	fullURL := utils.BuildFullURL(c, shortID)

	c.JSON(http.StatusOK, gin.H{"url": fullURL})
}
