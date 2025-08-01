package archivers

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"arker/internal/monitoring"
	"github.com/playwright-community/playwright-go"
)

// PWBundle manages Playwright resources with guaranteed idempotent cleanup
// This solves the double cleanup problem that causes browser leaks
type PWBundle struct {
	pw           *playwright.Playwright
	browser      playwright.Browser
	page         playwright.Page
	cleanupFuncs []func() // Event listener cleanup functions
	logWriter    io.Writer
	cleaned      bool
	chromePIDs   []int // Track Chrome process PIDs for proper termination
	mu           sync.Mutex
	once         sync.Once
}

// NewPWBundle creates a new Playwright resource bundle
func NewPWBundle(logWriter io.Writer) (*PWBundle, error) {
	monitor := monitoring.GetGlobalMonitor()
	monitor.RecordPlaywrightLaunch()
	
	fmt.Fprintf(logWriter, "Creating fresh Playwright instance...\n")
	
	pw, err := playwright.Run()
	if err != nil {
		fmt.Fprintf(logWriter, "Failed to create Playwright instance: %v\n", err)
		return nil, fmt.Errorf("failed to start Playwright: %w", err)
	}
	
	bundle := &PWBundle{
		pw:           pw,
		logWriter:    logWriter,
		cleanupFuncs: make([]func(), 0),
	}
	
	return bundle, nil
}

// CreateBrowser creates a browser instance within this bundle
func (b *PWBundle) CreateBrowser() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	
	if b.cleaned {
		return fmt.Errorf("cannot create browser: bundle already cleaned up")
	}
	
	if b.browser != nil {
		return fmt.Errorf("browser already exists in this bundle")
	}
	
	monitor := monitoring.GetGlobalMonitor()
	
	fmt.Fprintf(b.logWriter, "Launching Chrome browser...\n")
	
	// Standard browser launch args for security and performance
	launchArgs := []string{
		"--no-sandbox",
		"--disable-setuid-sandbox",
		"--disable-dev-shm-usage",
		"--disable-web-security",
		"--disable-features=VizDisplayCompositor",
	}
	
	// Add SOCKS5 proxy configuration if available
	if socks5Proxy := getSocks5Proxy(); socks5Proxy != "" {
		launchArgs = append(launchArgs, "--proxy-server="+socks5Proxy)
		fmt.Fprintf(b.logWriter, "Using SOCKS5 proxy: %s\n", socks5Proxy)
	}
	
	browser, err := b.pw.Chromium.Launch(playwright.BrowserTypeLaunchOptions{
		Headless: playwright.Bool(true),
		Args:     launchArgs,
	})
	if err != nil {
		fmt.Fprintf(b.logWriter, "Failed to launch browser: %v\n", err)
		return fmt.Errorf("failed to launch browser: %w", err)
	}
	
	b.browser = browser
	monitor.RecordBrowserCreation()
	fmt.Fprintf(b.logWriter, "Browser launched successfully\n")
	
	// Capture Chrome process PIDs for proper termination tracking
	// Give Chrome a moment to fully start before capturing PIDs
	time.Sleep(100 * time.Millisecond)
	b.chromePIDs = b.findChromePIDs()
	
	return nil
}

// CreatePage creates a page instance within this bundle
func (b *PWBundle) CreatePage(options ...playwright.BrowserNewPageOptions) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	
	if b.cleaned {
		return fmt.Errorf("cannot create page: bundle already cleaned up")
	}
	
	if b.browser == nil {
		return fmt.Errorf("cannot create page: browser not created")
	}
	
	if b.page != nil {
		return fmt.Errorf("page already exists in this bundle")
	}
	
	fmt.Fprintf(b.logWriter, "Creating new page...\n")
	
	var page playwright.Page
	var err error
	
	if len(options) > 0 {
		page, err = b.browser.NewPage(options[0])
	} else {
		page, err = b.browser.NewPage()
	}
	
	if err != nil {
		fmt.Fprintf(b.logWriter, "Failed to create page: %v\n", err)
		return fmt.Errorf("failed to create page: %w", err)
	}
	
	b.page = page
	fmt.Fprintf(b.logWriter, "Page created successfully\n")
	
	return nil
}

// GetPage returns the page instance (creates it if it doesn't exist)
func (b *PWBundle) GetPage() (playwright.Page, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	
	if b.cleaned {
		return nil, fmt.Errorf("cannot get page: bundle already cleaned up")
	}
	
	if b.page == nil {
		return nil, fmt.Errorf("page not created - call CreatePage first")
	}
	
	return b.page, nil
}

// AddEventListener registers an event listener and ensures it gets cleaned up
func (b *PWBundle) AddEventListener(page playwright.Page, event string, handler interface{}) {
	b.mu.Lock()
	defer b.mu.Unlock()
	
	if b.cleaned {
		slog.Warn("Attempted to add event listener to cleaned bundle", "event", event)
		return
	}
	
	// Register the listener - page.On doesn't return a cleanup function in this version
	page.On(event, handler)
	
	// Store a placeholder cleanup function 
	// Event listeners will be cleaned up when the page is closed
	b.cleanupFuncs = append(b.cleanupFuncs, func() {
		// Event listeners are automatically cleaned up when page is closed
		// This is just a placeholder for future versions that might support removal
	})
	
	fmt.Fprintf(b.logWriter, "Registered event listener for: %s\n", event)
}

// Cleanup performs idempotent cleanup of all Playwright resources
// This method can be called multiple times safely - only the first call does actual cleanup
func (b *PWBundle) Cleanup() {
	b.once.Do(func() {
		b.performCleanup()
	})
}

// performCleanup does the actual cleanup work (called only once via sync.Once)
func (b *PWBundle) performCleanup() {
	b.mu.Lock()
	defer b.mu.Unlock()
	
	if b.cleaned {
		// Double-protection: should never happen due to sync.Once, but just in case
		fmt.Fprintf(b.logWriter, "Bundle already cleaned up (double cleanup prevented)\n")
		return
	}
	
	monitor := monitoring.GetGlobalMonitor()
	fmt.Fprintf(b.logWriter, "Starting bundle cleanup...\n")
	
	// Step 1: Remove all event listeners first
	if len(b.cleanupFuncs) > 0 {
		fmt.Fprintf(b.logWriter, "Removing %d event listeners...\n", len(b.cleanupFuncs))
		for i, cleanup := range b.cleanupFuncs {
			if cleanup != nil {
				func() {
					defer func() {
						if r := recover(); r != nil {
							fmt.Fprintf(b.logWriter, "Warning: Event listener cleanup %d panicked: %v\n", i, r)
						}
					}()
					cleanup()
				}()
			}
		}
		b.cleanupFuncs = nil
	}
	
	// Step 2: Close page
	if b.page != nil {
		fmt.Fprintf(b.logWriter, "Closing page...\n")
		func() {
			defer func() {
				if r := recover(); r != nil {
					fmt.Fprintf(b.logWriter, "Warning: Page close panicked: %v\n", r)
				}
			}()
			if err := b.page.Close(); err != nil {
				fmt.Fprintf(b.logWriter, "Warning: Page close error: %v\n", err)
			}
		}()
		b.page = nil
	}
	
	// Step 3: Close browser
	if b.browser != nil {
		fmt.Fprintf(b.logWriter, "Closing browser...\n")
		func() {
			defer func() {
				if r := recover(); r != nil {
					fmt.Fprintf(b.logWriter, "Warning: Browser close panicked: %v\n", r)
				}
			}()
			if err := b.browser.Close(); err != nil {
				fmt.Fprintf(b.logWriter, "Warning: Browser close error: %v\n", err)
			}
		}()
		b.browser = nil
		monitor.RecordBrowserCleanup()
	}
	
	// Step 4: Stop Playwright
	if b.pw != nil {
		fmt.Fprintf(b.logWriter, "Stopping Playwright...\n")
		func() {
			defer func() {
				if r := recover(); r != nil {
					fmt.Fprintf(b.logWriter, "Warning: Playwright stop panicked: %v\n", r)
				}
			}()
			if err := b.pw.Stop(); err != nil {
				fmt.Fprintf(b.logWriter, "Warning: Playwright stop error: %v\n", err)
			}
		}()
		b.pw = nil
		monitor.RecordPlaywrightClose()
	}
	
	// CRITICAL: Wait for Chrome processes to actually terminate
	// Playwright's browser.Close() and pw.Stop() only send shutdown signals via IPC
	// but don't wait for the OS processes to fully exit, causing zombie accumulation
	fmt.Fprintf(b.logWriter, "Verifying Chrome process termination...\n")
	
	// IMPORTANT: Refresh PID list before cleanup to catch any processes spawned during browser operation
	// Chrome may spawn additional processes (renderers, utilities) after initial browser creation
	fmt.Fprintf(b.logWriter, "Refreshing Chrome PID list before cleanup...\n")
	refreshedPIDs := b.findChromePIDs()
	if len(refreshedPIDs) > len(b.chromePIDs) {
		fmt.Fprintf(b.logWriter, "Found %d additional Chrome processes since creation (was %d, now %d)\n", 
			len(refreshedPIDs)-len(b.chromePIDs), len(b.chromePIDs), len(refreshedPIDs))
		b.chromePIDs = refreshedPIDs
	} else {
		fmt.Fprintf(b.logWriter, "No additional Chrome processes found (still %d processes)\n", len(b.chromePIDs))
	}
	
	// Use proper process management: poll every 250ms, force kill after 10s
	b.waitForProcessTermination()
	
	b.cleaned = true
	fmt.Fprintf(b.logWriter, "Bundle cleanup completed successfully\n")
}

// IsCleanedUp returns whether this bundle has been cleaned up
func (b *PWBundle) IsCleanedUp() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.cleaned
}

// GetLogWriter returns the log writer for this bundle
func (b *PWBundle) GetLogWriter() io.Writer {
	return b.logWriter
}

// findChromePIDs discovers ALL Chrome process PIDs associated with this browser instance
// This includes main processes and ALL child processes (zygote, gpu-process, utility, renderer, etc.)
func (b *PWBundle) findChromePIDs() []int {
	// Step 1: Find main Chrome processes with --remote-debugging-pipe
	// These are the "root" processes that spawn all the child processes
	cmd := exec.Command("pgrep", "-f", "chrome.*--remote-debugging-pipe")
	output, err := cmd.Output()
	if err != nil {
		fmt.Fprintf(b.logWriter, "Warning: Could not find main Chrome PIDs: %v\n", err)
		return nil
	}
	
	var mainPIDs []int
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	for _, line := range lines {
		if line == "" {
			continue
		}
		if pid, err := strconv.Atoi(line); err == nil {
			mainPIDs = append(mainPIDs, pid)
		}
	}
	
	if len(mainPIDs) == 0 {
		fmt.Fprintf(b.logWriter, "No main Chrome processes found\n")
		return nil
	}
	
	// Step 2: For each main process, find ALL child Chrome processes recursively
	allPIDs := make(map[int]bool)
	
	// Add main processes
	for _, pid := range mainPIDs {
		allPIDs[pid] = true
	}
	
	// Find all Chrome child processes recursively
	for _, mainPID := range mainPIDs {
		childPIDs := b.findChromeChildProcesses(mainPID)
		for _, childPID := range childPIDs {
			allPIDs[childPID] = true
		}
	}
	
	// Convert map back to slice
	var result []int
	for pid := range allPIDs {
		result = append(result, pid)
	}
	
	fmt.Fprintf(b.logWriter, "Found %d total Chrome processes (including children): %v\n", len(result), result)
	return result
}

// findChromeChildProcesses recursively finds all Chrome child processes for a given parent PID
func (b *PWBundle) findChromeChildProcesses(parentPID int) []int {
	var result []int
	
	// Find direct children that are Chrome processes
	// Use pgrep to find processes that have chrome in the command line and are children of parentPID
	cmd := exec.Command("pgrep", "-P", strconv.Itoa(parentPID), "-f", "chrome")
	output, err := cmd.Output()
	if err != nil {
		// Not finding children is not an error - some processes may not have Chrome children
		return result
	}
	
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	for _, line := range lines {
		if line == "" {
			continue
		}
		if childPID, err := strconv.Atoi(line); err == nil {
			result = append(result, childPID)
			// Recursively find children of this child
			grandChildren := b.findChromeChildProcesses(childPID)
			result = append(result, grandChildren...)
		}
	}
	
	return result
}

// processExists checks if a process with the given PID still exists
func processExists(pid int) bool {
	if pid <= 0 {
		return false
	}
	
	// On Unix systems, sending signal 0 tests if process exists without affecting it
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	
	err = process.Signal(os.Signal(nil))
	return err == nil
}

// forceKillProcess attempts to force kill a process
func (b *PWBundle) forceKillProcess(pid int) {
	fmt.Fprintf(b.logWriter, "Force killing Chrome process %d...\n", pid)
	
	process, err := os.FindProcess(pid)
	if err != nil {
		fmt.Fprintf(b.logWriter, "Could not find process %d: %v\n", pid, err)
		return
	}
	
	if err := process.Kill(); err != nil {
		fmt.Fprintf(b.logWriter, "Failed to kill process %d: %v\n", pid, err)
	} else {
		fmt.Fprintf(b.logWriter, "Sent SIGKILL to process %d\n", pid)
	}
}

// waitForProcessTermination polls for Chrome processes to exit, with force kill fallback
func (b *PWBundle) waitForProcessTermination() {
	if len(b.chromePIDs) == 0 {
		fmt.Fprintf(b.logWriter, "No Chrome PIDs to wait for\n")
		return
	}
	
	fmt.Fprintf(b.logWriter, "Waiting for %d Chrome processes to terminate: %v\n", len(b.chromePIDs), b.chromePIDs)
	
	const (
		pollInterval = 250 * time.Millisecond
		forceKillTimeout = 10 * time.Second
	)
	
	startTime := time.Now()
	remaining := make([]int, len(b.chromePIDs))
	copy(remaining, b.chromePIDs)
	
	for len(remaining) > 0 {
		// Check which processes still exist
		var stillRunning []int
		for _, pid := range remaining {
			if processExists(pid) {
				stillRunning = append(stillRunning, pid)
			} else {
				fmt.Fprintf(b.logWriter, "Chrome process %d terminated\n", pid)
			}
		}
		remaining = stillRunning
		
		// If all processes are gone, we're done
		if len(remaining) == 0 {
			fmt.Fprintf(b.logWriter, "All Chrome processes terminated successfully\n")
			return
		}
		
		// Check if we should force kill
		elapsed := time.Since(startTime)
		if elapsed >= forceKillTimeout {
			fmt.Fprintf(b.logWriter, "Timeout reached (%v), force killing %d remaining processes\n", elapsed, len(remaining))
			for _, pid := range remaining {
				b.forceKillProcess(pid)
			}
			// Reset timer for final wait after force kill
			startTime = time.Now()
		}
		
		// Wait before next poll
		time.Sleep(pollInterval)
		
		// Absolute timeout - give up after 20 seconds total
		if time.Since(startTime) > 20*time.Second {
			fmt.Fprintf(b.logWriter, "Absolute timeout reached, giving up on %d processes: %v\n", len(remaining), remaining)
			break
		}
	}
}

// Helper function to get SOCKS5 proxy setting (moved from browser_utils.go)
func getSocks5Proxy() string {
	return os.Getenv("SOCKS5_PROXY")
}
