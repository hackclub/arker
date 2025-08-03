package archivers

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"time"

	"arker/internal/monitoring"
	proxyutil "arker/internal/proxy"
	"github.com/playwright-community/playwright-go"
)

// PWBundle manages Playwright resources with guaranteed idempotent cleanup
// Uses BrowserContext for proper isolation and relies on Playwright's built-in cleanup
type PWBundle struct {
	pw           *playwright.Playwright
	browser      playwright.Browser
	context      playwright.BrowserContext
	page         playwright.Page
	cleanupFuncs []func() // Event listener cleanup functions
	logWriter    io.Writer
	cleaned      bool
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
		// WORKING Intel GPU hardware acceleration (tested with intel_gpu_top)
		"--enable-gpu",                   // Enable GPU processes
		"--disable-gpu-sandbox",          // Required in containers
		"--use-gl=angle",                 // Use ANGLE for GL
		"--use-angle=gl-egl",            // Use EGL backend through ANGLE (WORKING!)
		"--enable-accelerated-2d-canvas", // Enable hardware-accelerated 2D canvas
		"--enable-gpu-rasterization",     // Enable GPU-accelerated rasterization
		// Critical args for preventing zombie processes in Docker containers (DO NOT REMOVE)
		"--no-zygote",        // Disable zygote process forking (prevents orphaned child processes)
		"--single-process",   // Run renderer in the same process as browser (reduces process count)
	}
	
	// SOCKS5 proxy configuration is now handled at the context level
	// Chrome's --proxy-server argument doesn't support SOCKS5 authentication
	
	// Set EGL_PLATFORM for Intel GPU hardware acceleration
	os.Setenv("EGL_PLATFORM", "surfaceless")
	
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
	
	// Create a new browser context for isolation
	// Each context is like an incognito window with its own storage
	contextOptions := playwright.BrowserNewContextOptions{}
	
	// Configure SOCKS5 proxy if available
	if proxyURL := proxyutil.GetProxyURL(); proxyURL != "" {
		// Use local proxy server that handles authentication
		contextOptions.Proxy = &playwright.Proxy{
			Server: proxyURL,
		}
		fmt.Fprintf(b.logWriter, "Using SOCKS5 proxy: %s\n", proxyURL)
	}
	
	context, err := browser.NewContext(contextOptions)
	if err != nil {
		fmt.Fprintf(b.logWriter, "Failed to create browser context: %v\n", err)
		return fmt.Errorf("failed to create browser context: %w", err)
	}
	
	b.context = context
	fmt.Fprintf(b.logWriter, "Browser context created successfully\n")
	
	return nil
}

// CreatePage creates a page instance within this bundle
func (b *PWBundle) CreatePage(options ...playwright.BrowserNewPageOptions) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	
	if b.cleaned {
		return fmt.Errorf("cannot create page: bundle already cleaned up")
	}
	
	if b.context == nil {
		return fmt.Errorf("cannot create page: browser context not created")
	}
	
	if b.page != nil {
		return fmt.Errorf("page already exists in this bundle")
	}
	
	fmt.Fprintf(b.logWriter, "Creating new page...\n")
	
	// Create page within the browser context for proper isolation
	page, err := b.context.NewPage()
	
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
	
	// Step 2: Close browser context (this closes all pages within the context)
	if b.context != nil {
		fmt.Fprintf(b.logWriter, "Closing browser context...\n")
		func() {
			defer func() {
				if r := recover(); r != nil {
					fmt.Fprintf(b.logWriter, "Warning: Context close panicked: %v\n", r)
				}
			}()
			if err := b.context.Close(); err != nil {
				fmt.Fprintf(b.logWriter, "Warning: Context close error: %v\n", err)
			}
		}()
		b.context = nil
		b.page = nil // Page is automatically closed with context
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
	
	// Step 5: Give Playwright time to clean up processes properly
	// This allows the browser.Close() and pw.Stop() operations to complete
	fmt.Fprintf(b.logWriter, "Allowing Playwright cleanup to complete...\n")
	time.Sleep(500 * time.Millisecond)
	
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



// Helper function to get SOCKS5 proxy setting (deprecated - use proxyutil.GetProxyURL)
func getSocks5Proxy() string {
	return proxyutil.GetProxyURL()
}
