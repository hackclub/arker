package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"arker/internal/monitoring"
)

// HealthCheckHandler checks the health of the application.
func HealthCheckHandler(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		sqlDB, err := db.DB()
		if err != nil || sqlDB.Ping() != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "unhealthy", "error": "database connection failed"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "healthy"})
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
