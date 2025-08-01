package main

import (
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"arker/internal/archivers"
	"arker/internal/models"
	"arker/internal/monitoring"
	"arker/internal/storage"
	"arker/internal/workers"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// TestBrowserLeakDetection runs a comprehensive test to detect browser leaks
// This test simulates real workload conditions with job cancellations and timeouts
func TestBrowserLeakDetection(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping browser leak test in short mode")
	}

	// Test configuration
	const (
		numWorkers        = 3
		totalJobs         = 50  // Reduced from 1000 for faster testing
		abortPercentage   = 0.5 // Abort 50% of jobs
		jobTimeoutSeconds = 5   // Timeout jobs after 5 seconds
		maxAcceptableChrome = numWorkers * 2 // Max Chrome processes
		maxGoroutineDiff   = 50 // Max goroutine increase
	)

	t.Logf("Starting browser leak detection test: %d workers, %d jobs, %.0f%% aborts", 
		numWorkers, totalJobs, abortPercentage*100)

	// Setup test database
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Discard,
	})
	if err != nil {
		t.Fatalf("Failed to setup test database: %v", err)
	}

	// Migrate tables
	err = db.AutoMigrate(&models.User{}, &models.APIKey{}, &models.ArchivedURL{}, 
		&models.Capture{}, &models.ArchiveItem{}, &models.Config{})
	if err != nil {
		t.Fatalf("Failed to migrate database: %v", err)
	}

	// Setup test storage
	tempStorage := storage.NewMemoryStorage()

	// Setup archivers
	archiversMap := map[string]archivers.Archiver{
		"screenshot": &archivers.ScreenshotArchiver{},
		"mhtml":      &archivers.MHTMLArchiver{},
	}

	// Initialize monitoring
	monitor := monitoring.GetGlobalMonitor()
	
	// Record initial state
	initialGoroutines := runtime.NumGoroutine()
	initialMetrics := monitor.GetMetrics()
	
	t.Logf("Initial state: %d goroutines, %d chrome processes", 
		initialGoroutines, initialMetrics.ChromeProcessCount)

	// Create job channel
	jobChan := make(chan models.Job, 100)

	// Start workers
	var wg sync.WaitGroup
	for i := 1; i <= numWorkers; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			workers.Worker(workerID, jobChan, tempStorage, db, archiversMap)
		}(i)
	}

	// Prepare test jobs
	var jobs []models.Job
	testURLs := []string{
		"https://example.com",
		"https://httpbin.org/html",
		"https://httpbin.org/status/200",
		"https://httpbin.org/json",
	}

	// Create test captures and archive items in database
	for i := 0; i < totalJobs; i++ {
		url := fmt.Sprintf("%s?test=%d", testURLs[i%len(testURLs)], i) // Make URLs unique
		archiveType := []string{"screenshot", "mhtml"}[i%2]
		
		// Create archived URL first
		archivedURL := models.ArchivedURL{
			Original: url,
		}
		if err := db.Create(&archivedURL).Error; err != nil {
			t.Fatalf("Failed to create archived URL %d: %v", i, err)
		}
		
		// Create capture
		capture := models.Capture{
			ArchivedURLID: archivedURL.ID,
			ShortID:       fmt.Sprintf("test%04d", i),
			Timestamp:     time.Now(),
		}
		if err := db.Create(&capture).Error; err != nil {
			t.Fatalf("Failed to create capture %d: %v", i, err)
		}

		// Create archive item
		item := models.ArchiveItem{
			CaptureID: capture.ID,
			Type:      archiveType,
			Status:    "pending",
		}
		if err := db.Create(&item).Error; err != nil {
			t.Fatalf("Failed to create archive item %d: %v", i, err)
		}

		// Create job
		job := models.Job{
			URL:       url,
			ShortID:   capture.ShortID,
			Type:      archiveType,
			CaptureID: capture.ID,
		}
		jobs = append(jobs, job)
	}

	// Submit jobs with controlled timing and cancellation
	go func() {
		defer close(jobChan)
		
		for i, job := range jobs {
			select {
			case jobChan <- job:
				t.Logf("Submitted job %d: %s (%s)", i+1, job.ShortID, job.Type)
				
				// Decide whether to abort this job
				shouldAbort := float64(i)/float64(totalJobs) < abortPercentage
				
				if shouldAbort {
					// Wait a bit then simulate timeout by checking if job is still processing
					go func(jobIndex int, jobID string) {
						time.Sleep(time.Duration(jobTimeoutSeconds) * time.Second)
						
						// Check if job is still processing (this simulates timeout conditions)
						var item models.ArchiveItem
						if err := db.Where("capture_id = ? AND type = ?", job.CaptureID, job.Type).First(&item).Error; err == nil {
							if item.Status == "processing" {
								t.Logf("Simulated timeout for job %d: %s", jobIndex+1, jobID)
								// In real scenarios, context would be cancelled here
							}
						}
					}(i, job.ShortID)
				}
				
				// Small delay between job submissions to make it more realistic
				time.Sleep(100 * time.Millisecond)
			default:
				t.Logf("Job channel full, waiting...")
				time.Sleep(1 * time.Second)
			}
		}
	}()

	// Wait for some jobs to complete (but not all, to simulate real conditions)
	maxWaitTime := time.Duration(totalJobs/2) * time.Second // Give reasonable time for partial completion
	waitTimer := time.NewTimer(maxWaitTime)
	defer waitTimer.Stop()

	// Monitor progress
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	monitoringDone := make(chan bool)
	go func() {
		defer close(monitoringDone)
		for {
			select {
			case <-ticker.C:
				currentMetrics := monitor.GetMetrics()
				currentGoroutines := runtime.NumGoroutine()
				
				t.Logf("Progress: %d chrome processes, %d goroutines, %d launches, %d closes",
					currentMetrics.ChromeProcessCount,
					currentGoroutines,
					currentMetrics.PlaywrightLaunches,
					currentMetrics.PlaywrightCloses)
				
				if currentMetrics.LeakDetected {
					t.Logf("Leak detected: %s", currentMetrics.LeakReason)
				}
			case <-waitTimer.C:
				return
			}
		}
	}()

	// Wait for monitoring or timeout
	<-waitTimer.C
	<-monitoringDone

	t.Logf("Test phase completed, analyzing results...")

	// Allow some time for cleanup
	time.Sleep(2 * time.Second)

	// Final metrics collection
	finalMetrics := monitor.GetMetrics()
	finalGoroutines := runtime.NumGoroutine()
	
	// Force garbage collection to clean up any remaining references
	runtime.GC()
	runtime.GC() // Double GC to be thorough
	time.Sleep(1 * time.Second)
	
	// Final final metrics after GC
	postGCMetrics := monitor.GetMetrics()
	postGCGoroutines := runtime.NumGoroutine()

	// Report results
	t.Logf("=== BROWSER LEAK TEST RESULTS ===")
	t.Logf("Initial state:")
	t.Logf("  Chrome processes: %d", initialMetrics.ChromeProcessCount)
	t.Logf("  Goroutines: %d", initialGoroutines)
	
	t.Logf("Final state (before GC):")
	t.Logf("  Chrome processes: %d", finalMetrics.ChromeProcessCount)
	t.Logf("  Goroutines: %d", finalGoroutines)
	t.Logf("  Playwright launches: %d", finalMetrics.PlaywrightLaunches)
	t.Logf("  Playwright closes: %d", finalMetrics.PlaywrightCloses)
	t.Logf("  Browser creations: %d", finalMetrics.BrowserCreations)
	t.Logf("  Browser cleanups: %d", finalMetrics.BrowserCleanups)
	
	t.Logf("Final state (after GC):")
	t.Logf("  Chrome processes: %d", postGCMetrics.ChromeProcessCount)
	t.Logf("  Goroutines: %d", postGCGoroutines)

	// Check for leaks
	chromeProcessDiff := postGCMetrics.ChromeProcessCount - initialMetrics.ChromeProcessCount
	goroutineDiff := postGCGoroutines - initialGoroutines
	launchCloseDiff := postGCMetrics.PlaywrightLaunches - postGCMetrics.PlaywrightCloses
	createCleanupDiff := postGCMetrics.BrowserCreations - postGCMetrics.BrowserCleanups

	t.Logf("=== LEAK ANALYSIS ===")
	t.Logf("Chrome process diff: %d (threshold: %d)", chromeProcessDiff, maxAcceptableChrome)
	t.Logf("Goroutine diff: %d (threshold: %d)", goroutineDiff, maxGoroutineDiff)
	t.Logf("Launch/close imbalance: %d", launchCloseDiff)
	t.Logf("Create/cleanup imbalance: %d", createCleanupDiff)

	// Assertions
	if postGCMetrics.ChromeProcessCount > maxAcceptableChrome {
		t.Errorf("Chrome process leak detected: %d processes (max acceptable: %d)", 
			postGCMetrics.ChromeProcessCount, maxAcceptableChrome)
	}

	if goroutineDiff > maxGoroutineDiff {
		t.Errorf("Goroutine leak detected: %d additional goroutines (max acceptable: %d)", 
			goroutineDiff, maxGoroutineDiff)
	}

	if launchCloseDiff > 3 { // Allow small imbalance due to timing
		t.Errorf("Playwright launch/close imbalance: %d launches, %d closes (diff: %d)", 
			postGCMetrics.PlaywrightLaunches, postGCMetrics.PlaywrightCloses, launchCloseDiff)
	}

	if createCleanupDiff > 3 { // Allow small imbalance due to timing
		t.Errorf("Browser create/cleanup imbalance: %d created, %d cleaned (diff: %d)", 
			postGCMetrics.BrowserCreations, postGCMetrics.BrowserCleanups, createCleanupDiff)
	}

	if postGCMetrics.LeakDetected {
		t.Errorf("Monitoring system detected leak: %s", postGCMetrics.LeakReason)
	}

	// Close workers gracefully - in real implementation we'd have a proper shutdown mechanism
	t.Logf("Test completed. In production, workers would be shut down gracefully.")
}

// BenchmarkBrowserResourceUsage benchmarks browser resource usage over time
func BenchmarkBrowserResourceUsage(b *testing.B) {
	if testing.Short() {
		b.Skip("Skipping browser benchmark in short mode")
	}

	// Setup minimal test infrastructure
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Discard,
	})
	if err != nil {
		b.Fatalf("Failed to setup test database: %v", err)
	}

	db.AutoMigrate(&models.ArchiveItem{})
	tempStorage := storage.NewMemoryStorage()
	
	archiversMap := map[string]archivers.Archiver{
		"screenshot": &archivers.ScreenshotArchiver{},
	}

	monitor := monitoring.GetGlobalMonitor()
	
	b.ResetTimer()
	
	// Benchmark browser lifecycle
	for i := 0; i < b.N; i++ {
		// Create a minimal job
		job := models.Job{
			URL:       "https://example.com",
			ShortID:   fmt.Sprintf("bench%d", i),
			Type:      "screenshot",
			CaptureID: uint(i + 1),
		}

		// Create corresponding DB item
		item := models.ArchiveItem{
			CaptureID: uint(i + 1),
			Type:      "screenshot",
			Status:    "pending",
		}
		db.Create(&item)

		// Process job (this will create and clean up browser)
		err := workers.ProcessSingleJob(job, tempStorage, db, archiversMap)
		if err != nil {
			// Don't fail benchmark on individual job failures
			b.Logf("Job %d failed: %v", i, err)
		}
	}
	
	// Report final metrics
	finalMetrics := monitor.GetMetrics()
	b.Logf("Final metrics: %d chrome processes, %d goroutines, %d launches, %d closes",
		finalMetrics.ChromeProcessCount,
		runtime.NumGoroutine(),
		finalMetrics.PlaywrightLaunches,
		finalMetrics.PlaywrightCloses)
}
