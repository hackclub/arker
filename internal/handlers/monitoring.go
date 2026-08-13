package handlers

import (
	"net/http"
	"syscall"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"arker/internal/canary"
	"arker/internal/monitoring"
)

// CanaryHealthProvider supplies the canary signal embedded in /health. Nil
// leaves the payload exactly as it was before canaries existed.
type CanaryHealthProvider func() canary.Summary

// HealthCheckHandler checks the health of the application.
//
// The canary fields are additive and deliberately do not change the HTTP
// status: /health is the container's liveness probe, and a platform-side
// breakage (YouTube changing its player, say) must not get the archiver
// restarted in a loop. Degradation is reported in the body — "status" stays
// "healthy" for existing consumers, while "degraded" and "canaries" carry the
// new signal for anything that looks.
func HealthCheckHandler(db *gorm.DB, canaries CanaryHealthProvider) gin.HandlerFunc {
	return func(c *gin.Context) {
		sqlDB, err := db.DB()
		if err != nil || sqlDB.Ping() != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "unhealthy", "error": "database connection failed"})
			return
		}
		payload := gin.H{"status": "healthy"}
		if canaries != nil {
			summary := canaries()
			payload["canaries"] = summary
			if summary.Status == canary.HealthFailing {
				payload["degraded"] = true
			}
		}
		c.JSON(http.StatusOK, payload)
	}
}

// BrowserMetricsHandler serves browser monitoring metrics.
func BrowserMetricsHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		monitor := monitoring.GetGlobalMonitor()
		c.JSON(http.StatusOK, monitor.GetMetrics())
	}
}

// BrowserStatusHandler provides a detailed status including leak detection.
func BrowserStatusHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		monitor := monitoring.GetGlobalMonitor()
		metrics := monitor.GetMetrics()

		status := "healthy"
		if metrics.LeakDetected {
			status = "leak_detected"
		}

		response := gin.H{
			"status":                 status,
			"chrome_process_count":   metrics.ChromeProcessCount,
			"total_goroutines":       metrics.TotalGoroutines,
			"launch_close_balance":   metrics.PlaywrightLaunches - metrics.PlaywrightCloses,
			"create_cleanup_balance": metrics.BrowserCreations - metrics.BrowserCleanups,
			"leak_detected":          metrics.LeakDetected,
			"leak_reason":            metrics.LeakReason,
			"last_updated":           metrics.LastUpdated,
		}

		if metrics.LeakDetected {
			c.JSON(http.StatusServiceUnavailable, response)
		} else {
			c.JSON(http.StatusOK, response)
		}
	}
}

// DBStorageStatusHandler reports database relation sizes that are useful for
// detecting archive log TOAST growth before it threatens the host disk.
func DBStorageStatusHandler(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		var stat syscall.Statfs_t
		root := gin.H{}
		if err := syscall.Statfs("/", &stat); err == nil {
			total := stat.Blocks * uint64(stat.Bsize)
			available := stat.Bavail * uint64(stat.Bsize)
			used := total - available
			root = gin.H{
				"total_bytes":     total,
				"available_bytes": available,
				"used_bytes":      used,
			}
		}

		if db.Dialector.Name() != "postgres" {
			c.JSON(http.StatusOK, gin.H{"root_disk": root})
			return
		}

		var sizes struct {
			DBSizeBytes               int64 `gorm:"column:db_size_bytes"`
			ArchiveItemsBytes         int64 `gorm:"column:archive_items_bytes"`
			ArchiveItemLogsBytes      int64 `gorm:"column:archive_item_logs_bytes"`
			ArchiveItemsTOASTBytes    int64 `gorm:"column:archive_items_toast_bytes"`
			ArchiveItemLogsTOASTBytes int64 `gorm:"column:archive_item_logs_toast_bytes"`
		}
		err := db.Raw(`
			select
				pg_database_size(current_database()) as db_size_bytes,
				pg_total_relation_size('archive_items') as archive_items_bytes,
				pg_total_relation_size('archive_item_logs') as archive_item_logs_bytes,
				COALESCE(pg_total_relation_size((select NULLIF(reltoastrelid, 0) from pg_class where oid='archive_items'::regclass)), 0) as archive_items_toast_bytes,
				COALESCE(pg_total_relation_size((select NULLIF(reltoastrelid, 0) from pg_class where oid='archive_item_logs'::regclass)), 0) as archive_item_logs_toast_bytes
		`).Scan(&sizes).Error
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "database storage query failed"})
			return
		}

		c.JSON(http.StatusOK, gin.H{
			"db_size_bytes":                 sizes.DBSizeBytes,
			"archive_items_bytes":           sizes.ArchiveItemsBytes,
			"archive_item_logs_bytes":       sizes.ArchiveItemLogsBytes,
			"archive_items_toast_bytes":     sizes.ArchiveItemsTOASTBytes,
			"archive_item_logs_toast_bytes": sizes.ArchiveItemLogsTOASTBytes,
			"root_disk":                     root,
		})
	}
}
