package main

import (
	"testing"
	"time"

	"arker/internal/monitoring"
)

// TestBrowserMonitoringBasics tests the basic monitoring functionality
func TestBrowserMonitoringBasics(t *testing.T) {
	monitor := monitoring.GetGlobalMonitor()
	
	// Record some operations
	monitor.RecordPlaywrightLaunch()
	monitor.RecordBrowserCreation()
	
	// Wait a moment for background updates
	time.Sleep(500 * time.Millisecond)
	
	// Get metrics
	metrics := monitor.GetMetrics()
	
	// Check that our operations were recorded
	if metrics.PlaywrightLaunches == 0 {
		t.Error("Expected at least 1 Playwright launch")
	}
	
	if metrics.BrowserCreations == 0 {
		t.Error("Expected at least 1 browser creation")
	}
	
	// Check that metrics have reasonable values
	if metrics.TotalGoroutines < 1 {
		t.Logf("Warning: Got %d goroutines, which seems low but may be valid in test environment", metrics.TotalGoroutines)
	}
	
	if metrics.ChromeProcessCount < 0 {
		t.Error("Chrome process count should not be negative")
	}
	
	t.Logf("Current metrics: Chrome=%d, Goroutines=%d, Launches=%d, Creates=%d", 
		metrics.ChromeProcessCount, metrics.TotalGoroutines, 
		metrics.PlaywrightLaunches, metrics.BrowserCreations)
	
	// Test cleanup recording
	monitor.RecordBrowserCleanup()
	monitor.RecordPlaywrightClose()
	
	updatedMetrics := monitor.GetMetrics()
	if updatedMetrics.BrowserCleanups == 0 {
		t.Error("Expected at least 1 browser cleanup")
	}
	
	if updatedMetrics.PlaywrightCloses == 0 {
		t.Error("Expected at least 1 Playwright close")
	}
	
	t.Logf("After cleanup: Launches=%d, Closes=%d, Creates=%d, Cleanups=%d",
		updatedMetrics.PlaywrightLaunches, updatedMetrics.PlaywrightCloses,
		updatedMetrics.BrowserCreations, updatedMetrics.BrowserCleanups)
}

// TestBrowserMonitorJSON tests JSON serialization
func TestBrowserMonitorJSON(t *testing.T) {
	monitor := monitoring.GetGlobalMonitor()
	
	jsonStr, err := monitor.GetMetricsJSON()
	if err != nil {
		t.Fatalf("Failed to get metrics as JSON: %v", err)
	}
	
	if len(jsonStr) == 0 {
		t.Error("JSON string should not be empty")
	}
	
	t.Logf("Metrics JSON: %s", jsonStr)
}
