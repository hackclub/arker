package monitoring

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// BrowserMetrics tracks browser and system metrics for leak detection
type BrowserMetrics struct {
	// Process counts
	ChromeProcessCount int `json:"chrome_process_count"`
	TotalGoroutines    int `json:"total_goroutines"`
	
	// Playwright operation counters
	PlaywrightLaunches int64 `json:"playwright_launches"`
	PlaywrightCloses   int64 `json:"playwright_closes"`
	PlaywrightKills    int64 `json:"playwright_kills"`
	BrowserCreations   int64 `json:"browser_creations"`
	BrowserCleanups    int64 `json:"browser_cleanups"`
	
	// Timing information
	LastUpdated        time.Time `json:"last_updated"`
	UptimeSeconds      int64     `json:"uptime_seconds"`
	
	// Health indicators
	LeakDetected       bool   `json:"leak_detected"`
	LeakReason         string `json:"leak_reason,omitempty"`
}

// BrowserMonitor provides real-time monitoring of browser processes and resource usage
type BrowserMonitor struct {
	startTime  time.Time
	metrics    BrowserMetrics
	mu         sync.RWMutex
	
	// Atomic counters for thread-safe updates
	launchCount  int64
	closeCount   int64
	killCount    int64
	createCount  int64
	cleanupCount int64
}

// Global monitor instance
var globalMonitor *BrowserMonitor
var monitorOnce sync.Once

// GetGlobalMonitor returns the singleton browser monitor instance
func GetGlobalMonitor() *BrowserMonitor {
	monitorOnce.Do(func() {
		globalMonitor = NewBrowserMonitor()
		// Start background metrics collection
		go globalMonitor.startMetricsCollection()
	})
	return globalMonitor
}

// NewBrowserMonitor creates a new browser monitor instance
func NewBrowserMonitor() *BrowserMonitor {
	return &BrowserMonitor{
		startTime: time.Now(),
		metrics: BrowserMetrics{
			LastUpdated: time.Now(),
		},
	}
}

// RecordPlaywrightLaunch increments the Playwright launch counter
func (bm *BrowserMonitor) RecordPlaywrightLaunch() {
	atomic.AddInt64(&bm.launchCount, 1)
	slog.Debug("Playwright launch recorded", "total_launches", atomic.LoadInt64(&bm.launchCount))
}

// RecordPlaywrightClose increments the Playwright close counter
func (bm *BrowserMonitor) RecordPlaywrightClose() {
	atomic.AddInt64(&bm.closeCount, 1)
	slog.Debug("Playwright close recorded", "total_closes", atomic.LoadInt64(&bm.closeCount))
}

// RecordPlaywrightKill increments the Playwright kill counter
func (bm *BrowserMonitor) RecordPlaywrightKill() {
	atomic.AddInt64(&bm.killCount, 1)
	slog.Debug("Playwright kill recorded", "total_kills", atomic.LoadInt64(&bm.killCount))
}

// RecordBrowserCreation increments the browser creation counter
func (bm *BrowserMonitor) RecordBrowserCreation() {
	atomic.AddInt64(&bm.createCount, 1)
	slog.Debug("Browser creation recorded", "total_creations", atomic.LoadInt64(&bm.createCount))
}

// RecordBrowserCleanup increments the browser cleanup counter
func (bm *BrowserMonitor) RecordBrowserCleanup() {
	atomic.AddInt64(&bm.cleanupCount, 1)
	slog.Debug("Browser cleanup recorded", "total_cleanups", atomic.LoadInt64(&bm.cleanupCount))
}

// GetChromeProcessCount returns the current number of Chrome processes
func (bm *BrowserMonitor) GetChromeProcessCount() (int, error) {
	// Use pgrep to count Chrome processes
	cmd := exec.Command("pgrep", "-af", "chrome")
	output, err := cmd.Output()
	if err != nil {
		// pgrep returns exit code 1 if no processes found, which is normal
		if exitError, ok := err.(*exec.ExitError); ok && exitError.ExitCode() == 1 {
			return 0, nil
		}
		return 0, fmt.Errorf("failed to count chrome processes: %w", err)
	}
	
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return 0, nil
	}
	
	return len(lines), nil
}

// GetMetrics returns the current browser metrics
func (bm *BrowserMonitor) GetMetrics() BrowserMetrics {
	bm.mu.RLock()
	defer bm.mu.RUnlock()
	
	// Create a copy to avoid race conditions
	metrics := bm.metrics
	
	// Update atomic counters
	metrics.PlaywrightLaunches = atomic.LoadInt64(&bm.launchCount)
	metrics.PlaywrightCloses = atomic.LoadInt64(&bm.closeCount)
	metrics.PlaywrightKills = atomic.LoadInt64(&bm.killCount)
	metrics.BrowserCreations = atomic.LoadInt64(&bm.createCount)
	metrics.BrowserCleanups = atomic.LoadInt64(&bm.cleanupCount)
	
	return metrics
}

// updateMetrics refreshes the current metrics
func (bm *BrowserMonitor) updateMetrics() {
	bm.mu.Lock()
	defer bm.mu.Unlock()
	
	// Update Chrome process count
	chromeCount, err := bm.GetChromeProcessCount()
	if err != nil {
		slog.Error("Failed to get Chrome process count", "error", err)
		chromeCount = -1 // Indicate error
	}
	
	// Update metrics
	bm.metrics = BrowserMetrics{
		ChromeProcessCount: chromeCount,
		TotalGoroutines:    runtime.NumGoroutine(),
		PlaywrightLaunches: atomic.LoadInt64(&bm.launchCount),
		PlaywrightCloses:   atomic.LoadInt64(&bm.closeCount),
		PlaywrightKills:    atomic.LoadInt64(&bm.killCount),
		BrowserCreations:   atomic.LoadInt64(&bm.createCount),
		BrowserCleanups:    atomic.LoadInt64(&bm.cleanupCount),
		LastUpdated:        time.Now(),
		UptimeSeconds:      int64(time.Since(bm.startTime).Seconds()),
		LeakDetected:       false,
		LeakReason:         "",
	}
	
	// Detect potential leaks
	bm.detectLeaks()
}

// detectLeaks analyzes metrics to identify potential browser leaks
func (bm *BrowserMonitor) detectLeaks() {
	// Leak detection heuristics
	
	// 1. More than 10 Chrome processes
	if bm.metrics.ChromeProcessCount > 10 {
		bm.metrics.LeakDetected = true
		bm.metrics.LeakReason = fmt.Sprintf("High Chrome process count: %d", bm.metrics.ChromeProcessCount)
		return
	}
	
	// 2. Significant imbalance between launches and closes (>5 difference)
	launchCloseImbalance := bm.metrics.PlaywrightLaunches - bm.metrics.PlaywrightCloses
	if launchCloseImbalance > 5 {
		bm.metrics.LeakDetected = true
		bm.metrics.LeakReason = fmt.Sprintf("Launch/close imbalance: %d launches, %d closes", 
			bm.metrics.PlaywrightLaunches, bm.metrics.PlaywrightCloses)
		return
	}
	
	// 3. Significant imbalance between browser creations and cleanups
	createCleanupImbalance := bm.metrics.BrowserCreations - bm.metrics.BrowserCleanups
	if createCleanupImbalance > 5 {
		bm.metrics.LeakDetected = true
		bm.metrics.LeakReason = fmt.Sprintf("Browser create/cleanup imbalance: %d created, %d cleaned", 
			bm.metrics.BrowserCreations, bm.metrics.BrowserCleanups)
		return
	}
	
	// 4. Rapidly growing goroutine count (>1000)
	if bm.metrics.TotalGoroutines > 1000 {
		bm.metrics.LeakDetected = true
		bm.metrics.LeakReason = fmt.Sprintf("High goroutine count: %d", bm.metrics.TotalGoroutines)
		return
	}
}

// startMetricsCollection starts a background goroutine to periodically update metrics
func (bm *BrowserMonitor) startMetricsCollection() {
	ticker := time.NewTicker(10 * time.Second) // Update every 10 seconds
	defer ticker.Stop()
	
	slog.Info("Started browser metrics collection", "update_interval", "10s")
	
	for {
		select {
		case <-ticker.C:
			bm.updateMetrics()
			
			metrics := bm.GetMetrics()
			if metrics.LeakDetected {
				slog.Warn("Browser leak detected", 
					"reason", metrics.LeakReason,
					"chrome_processes", metrics.ChromeProcessCount,
					"goroutines", metrics.TotalGoroutines,
					"launch_close_diff", metrics.PlaywrightLaunches - metrics.PlaywrightCloses)
			} else {
				slog.Debug("Browser metrics updated",
					"chrome_processes", metrics.ChromeProcessCount,
					"goroutines", metrics.TotalGoroutines,
					"playwright_launches", metrics.PlaywrightLaunches,
					"playwright_closes", metrics.PlaywrightCloses)
			}
		}
	}
}

// GetMetricsJSON returns metrics as JSON string
func (bm *BrowserMonitor) GetMetricsJSON() (string, error) {
	metrics := bm.GetMetrics()
	jsonBytes, err := json.MarshalIndent(metrics, "", "  ")
	if err != nil {
		return "", fmt.Errorf("failed to marshal metrics to JSON: %w", err)
	}
	return string(jsonBytes), nil
}

// LogCurrentStatus logs the current browser monitoring status
func (bm *BrowserMonitor) LogCurrentStatus() {
	metrics := bm.GetMetrics()
	
	slog.Info("Browser Monitor Status",
		"chrome_processes", metrics.ChromeProcessCount,
		"goroutines", metrics.TotalGoroutines,
		"playwright_launches", metrics.PlaywrightLaunches,
		"playwright_closes", metrics.PlaywrightCloses,
		"playwright_kills", metrics.PlaywrightKills,
		"browser_creations", metrics.BrowserCreations,
		"browser_cleanups", metrics.BrowserCleanups,
		"uptime_seconds", metrics.UptimeSeconds,
		"leak_detected", metrics.LeakDetected,
		"leak_reason", metrics.LeakReason)
}
