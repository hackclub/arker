package archivers

import (
	"context"
	"fmt"
	"github.com/playwright-community/playwright-go"
	"io"
	"strings"
	"sync"
	"time"
)

// waitForRobustPageLoad implements a robust page loading strategy for dynamic sites
// like Next.js and YouTube that handles progressive/lazy images and async content
func WaitForRobustPageLoad(page playwright.Page, logWriter io.Writer, idleDurationMs int, totalTimeoutMs int, pollIntervalMs int) error {
	fmt.Fprintf(logWriter, "Starting robust page load wait (idle: %dms, timeout: %dms)...\n", idleDurationMs, totalTimeoutMs)

	// Step 1: Disable animations for instant loads
	_, err := page.Evaluate(`
		() => {
			const style = document.createElement('style');
			style.innerHTML = '* { transition: none !important; animation: none !important; }';
			document.head.appendChild(style);
		}
	`)
	if err != nil {
		fmt.Fprintf(logWriter, "Warning: Failed to disable animations: %v\n", err)
	}

	// Step 2: Force lazy images to eager load
	_, err = page.Evaluate(`
		() => {
			document.querySelectorAll('img[loading="lazy"]').forEach(img => {
				img.setAttribute('loading', 'eager');
			});
			// Also trigger any intersection observers by scrolling briefly
			const images = document.querySelectorAll('img[data-src], img[data-lazy-src]');
			images.forEach(img => {
				if (img.dataset.src) {
					img.src = img.dataset.src;
				}
				if (img.dataset.lazySrc) {
					img.src = img.dataset.lazySrc;
				}
			});
		}
	`)
	if err != nil {
		fmt.Fprintf(logWriter, "Warning: Failed to force lazy images: %v\n", err)
	}

	// Step 3: Custom network idle wait
	fmt.Fprintf(logWriter, "Waiting for network idle...\n")
	err = waitForCustomNetworkIdle(page, logWriter, idleDurationMs, totalTimeoutMs, pollIntervalMs)
	if err != nil {
		fmt.Fprintf(logWriter, "Network idle wait failed: %v\n", err)
		return err
	}

	// Step 4: Final check for images/videos loaded
	fmt.Fprintf(logWriter, "Checking all media resources are loaded...\n")
	_, err = page.WaitForFunction(`
		() => {
			const images = Array.from(document.querySelectorAll('img'));
			const videos = Array.from(document.querySelectorAll('video'));
			
			const imagesLoaded = images.every(img => {
				// Skip images that haven't started loading or are decorative
				if (!img.src || img.src === '' || img.naturalWidth === 0) {
					return img.complete; // Consider complete if no src or still loading
				}
				return img.complete && img.naturalWidth > 0;
			});
			
			const videosLoaded = videos.every(video => video.readyState >= 2); // HAVE_CURRENT_DATA
			
			return imagesLoaded && videosLoaded;
		}
	`, playwright.PageWaitForFunctionOptions{
		Timeout: playwright.Float(10000), // 10s timeout for this check
	})
	if err != nil {
		fmt.Fprintf(logWriter, "Warning: Not all media resources loaded: %v\n", err)
		// Don't fail, just warn - some images may be broken or slow
	}

	fmt.Fprintf(logWriter, "Robust page load completed successfully\n")
	return nil
}

// PerformCompletePageLoad handles the full page loading sequence for archiving
// Includes navigation, robust loading, scrolling, and final stabilization
func PerformCompletePageLoad(page playwright.Page, url string, logWriter io.Writer, includeScrolling bool) error {
	return PerformCompletePageLoadWithContext(context.Background(), page, url, logWriter, includeScrolling)
}

// PerformCompletePageLoadWithContext handles the full page loading sequence for archiving with context cancellation
func PerformCompletePageLoadWithContext(ctx context.Context, page playwright.Page, url string, logWriter io.Writer, includeScrolling bool) error {
	fmt.Fprintf(logWriter, "Starting complete page load sequence for: %s\n", url)

	// Check context before starting
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	// Step 1: Navigate with initial wait for 'load'
	fmt.Fprintf(logWriter, "Navigating to URL...\n")
	if _, err := page.Goto(url, playwright.PageGotoOptions{
		Timeout:   playwright.Float(30000),
		WaitUntil: playwright.WaitUntilStateLoad,
	}); err != nil {
		fmt.Fprintf(logWriter, "Failed to navigate to URL: %v\n", err)
		return err
	}

	// Check context after navigation
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	// Step 2: Use robust page loading wait for dynamic content
	fmt.Fprintf(logWriter, "Waiting for robust page load (handling progressive images and async content)...\n")
	if err := WaitForRobustPageLoadWithContext(ctx, page, logWriter, 2000, 20000, 100); err != nil {
		fmt.Fprintf(logWriter, "Robust page load failed: %v\n", err)
		return err
	}

	// Check context before scrolling
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	// Step 3: Optionally scroll through the page to trigger lazy loading
	if includeScrolling {
		fmt.Fprintf(logWriter, "Scrolling through page to trigger lazy-loaded content...\n")
		if err := scrollToBottomAndWaitWithContext(ctx, page, logWriter); err != nil {
			fmt.Fprintf(logWriter, "Warning: Scrolling failed, continuing: %v\n", err)
			// Don't fail the entire process, just continue
		}

		// Check context before final wait
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		// Step 4: Wait for any additional content that may have loaded during scrolling
		fmt.Fprintf(logWriter, "Waiting for post-scroll content to stabilize...\n")
		if err := waitForCustomNetworkIdleWithContext(ctx, page, logWriter, 1000, 10000, 100); err != nil {
			fmt.Fprintf(logWriter, "Warning: Post-scroll network idle wait failed: %v\n", err)
			// Don't fail, just continue
		}
	}

	fmt.Fprintf(logWriter, "Complete page load sequence finished successfully\n")
	return nil
}

// scrollToBottomAndWait gradually scrolls through the page to trigger lazy loading
func scrollToBottomAndWait(page playwright.Page, logWriter io.Writer) error {
	fmt.Fprintf(logWriter, "Starting gradual scroll to trigger lazy loading...\n")

	// Get initial page height
	initialHeight, err := page.Evaluate(`() => document.body.scrollHeight`)
	if err != nil {
		fmt.Fprintf(logWriter, "Warning: Could not get initial page height: %v\n", err)
		return nil // Don't fail, just skip scrolling
	}

	fmt.Fprintf(logWriter, "Initial page height: %v\n", initialHeight)

	// Scroll in chunks with pauses to allow content to load
	_, err = page.Evaluate(`
		async (initialHeight) => {
			const scrollStep = window.innerHeight * 0.8; // Scroll 80% of viewport height at a time
			const scrollDelay = 500; // Wait 500ms between scrolls
			
			let currentPos = 0;
			let lastHeight = document.body.scrollHeight;
			let stableCount = 0;
			const maxStableChecks = 5; // Stop if height is stable for 5 checks
			
			while (stableCount < maxStableChecks) {
				// Scroll down
				currentPos += scrollStep;
				window.scrollTo(0, currentPos);
				
				// Wait for content to potentially load
				await new Promise(resolve => setTimeout(resolve, scrollDelay));
				
				const newHeight = document.body.scrollHeight;
				
				// If we've reached the bottom or height hasn't changed
				if (currentPos >= newHeight) {
					if (newHeight === lastHeight) {
						stableCount++;
					} else {
						stableCount = 0; // Reset if height changed
					}
					lastHeight = newHeight;
					currentPos = newHeight; // Ensure we're at the bottom
				} else {
					stableCount = 0; // Reset stable count if we're still scrolling
				}
			}
			
			// Final scroll to absolute bottom
			window.scrollTo(0, document.body.scrollHeight);
			
			// Wait a bit more for any final loading
			await new Promise(resolve => setTimeout(resolve, 1000));
			
			return {
				finalHeight: document.body.scrollHeight,
				initialHeight: initialHeight
			};
		}
	`, initialHeight)

	if err != nil {
		fmt.Fprintf(logWriter, "Warning: Scrolling failed: %v\n", err)
		return nil // Don't fail the entire process
	}

	// Get final height for logging
	finalHeight, err := page.Evaluate(`() => document.body.scrollHeight`)
	if err == nil {
		fmt.Fprintf(logWriter, "Scrolling completed. Final height: %v (initial: %v)\n", finalHeight, initialHeight)
	}

	// Scroll back to top for consistent archiving
	fmt.Fprintf(logWriter, "Scrolling back to top for consistent archiving...\n")
	_, err = page.Evaluate(`
		async () => {
			window.scrollTo(0, 0);
			// Wait for scroll to complete and any layout shifts
			await new Promise(resolve => setTimeout(resolve, 500));
			// Ensure we're really at the top
			window.scrollTo(0, 0);
		}
	`)
	if err != nil {
		fmt.Fprintf(logWriter, "Warning: Could not scroll back to top: %v\n", err)
	} else {
		fmt.Fprintf(logWriter, "Successfully scrolled back to top\n")
	}

	fmt.Fprintf(logWriter, "Scroll-based lazy loading trigger completed\n")
	return nil
}

// waitForCustomNetworkIdle waits for network to be idle for specified duration
// Uses fallback strategies to avoid getting stuck on persistent trackers
func waitForCustomNetworkIdle(page playwright.Page, logWriter io.Writer, idleDurationMs int, totalTimeoutMs int, pollIntervalMs int) error {
	pendingRequests := sync.Map{} // Thread-safe map for pending requests
	var mu sync.Mutex
	startTime := time.Now()

	// Resilience parameters
	const maxAcceptablePersistentRequests = 3 // Allow up to 3 persistent requests
	const fallbackTimeoutMs = 15000           // Give up on perfect idle after 15s
	const minWaitBeforeFallback = 5000        // Wait at least 5s before considering fallback

	// Track requests
	requestListener := func(req playwright.Request) {
		// Filter out requests we don't care about for idle detection
		url := req.URL()

		// Comprehensive list of ad networks, trackers, and analytics domains to ignore
		ignoredPatterns := []string{
			// Analytics
			"analytics", "google-analytics", "googletagmanager", "gtag", "gtm",
			"segment.com", "mixpanel", "amplitude", "hotjar", "fullstory",

			// Ad networks and tracking
			"doubleclick", "googlesyndication", "googleadservices", "adsystem",
			"facebook.com/tr", "connect.facebook.net", "fbcdn.net",
			"amazon-adsystem", "adsafeprotected", "moatads", "scorecardresearch",
			"quantserve", "outbrain", "taboola", "criteo", "adsystem",

			// Media/ad companies that often have slow/persistent requests
			"krushmedia", "adsafeprotected", "doubleverify", "ias.net",
			"moatads", "amazon-adsystem", "adsystem.com", "googleadservices",

			// Common tracking pixels and beacons
			"beacon", "pixel", "track", "ping", "event", "impression",
			"?puid=", "?redir=", "gdpr=", "ccpa=", "gpp=", // URL params often in tracking

			// Social media tracking
			"twitter.com/i/adsct", "t.co/i/adsct", "linkedin.com/px",
			"snapchat.com/tr", "tiktok.com/tr", "reddit.com/api/v2/pixel",

			// Other persistent/slow services
			"pusher", "websocket", "socket.io", "sse", "eventsource",
			"livechat", "intercom", "zendesk", "drift",
		}

		shouldIgnore := false
		for _, pattern := range ignoredPatterns {
			if strings.Contains(strings.ToLower(url), pattern) {
				shouldIgnore = true
				break
			}
		}

		if shouldIgnore {
			fmt.Fprintf(logWriter, "Ignoring tracking/ad request: %s\n", url)
			return
		}

		mu.Lock()
		pendingRequests.Store(url, true)
		mu.Unlock()
		fmt.Fprintf(logWriter, "Request started: %s\n", url)
	}

	requestFinishedListener := func(req playwright.Request) {
		url := req.URL()
		mu.Lock()
		if _, exists := pendingRequests.Load(url); exists {
			pendingRequests.Delete(url)
			fmt.Fprintf(logWriter, "Request finished: %s\n", url)
		}
		mu.Unlock()
	}

	requestFailedListener := func(req playwright.Request) {
		url := req.URL()
		mu.Lock()
		if _, exists := pendingRequests.Load(url); exists {
			pendingRequests.Delete(url)
			fmt.Fprintf(logWriter, "Request failed: %s\n", url)
		}
		mu.Unlock()
	}

	page.On("request", requestListener)
	page.On("requestfinished", requestFinishedListener)
	page.On("requestfailed", requestFailedListener)

	var idleStart *time.Time
	fallbackTriggered := false

	for {
		elapsed := time.Since(startTime).Milliseconds()

		// Hard timeout - always fail after total timeout
		if elapsed > int64(totalTimeoutMs) {
			var urls []string
			pendingRequests.Range(func(key, value interface{}) bool {
				urls = append(urls, key.(string))
				return true
			})
			return fmt.Errorf("network idle timed out after %dms. Pending requests: %v", totalTimeoutMs, urls)
		}

		count := 0
		var pendingUrls []string
		pendingRequests.Range(func(key, value interface{}) bool {
			count++
			pendingUrls = append(pendingUrls, key.(string))
			return true
		})

		// Perfect idle case - no pending requests
		if count == 0 {
			now := time.Now()
			if idleStart == nil {
				idleStart = &now
				fmt.Fprintf(logWriter, "Network idle period started (0 pending requests)\n")
			}
			if time.Since(*idleStart).Milliseconds() >= int64(idleDurationMs) {
				fmt.Fprintf(logWriter, "Perfect network idle achieved for %dms\n", idleDurationMs)
				return nil
			}
		} else {
			// Check for fallback conditions after minimum wait time
			if elapsed > minWaitBeforeFallback && !fallbackTriggered {
				if count <= maxAcceptablePersistentRequests {
					fmt.Fprintf(logWriter, "Fallback: Accepting %d persistent requests after %dms\n", count, elapsed)
					fmt.Fprintf(logWriter, "Persistent requests: %v\n", pendingUrls)
					return nil // Accept this as "good enough"
				}

				// Give up on perfect idle after fallback timeout
				if elapsed > fallbackTimeoutMs {
					fmt.Fprintf(logWriter, "Fallback: Too many persistent requests (%d), but continuing after %dms\n", count, elapsed)
					fmt.Fprintf(logWriter, "Giving up on perfect idle. Persistent requests: %v\n", pendingUrls)
					return nil // Continue anyway - better than failing completely
				}

				if !fallbackTriggered {
					fmt.Fprintf(logWriter, "Fallback mode: %d persistent requests detected, will continue after %dms if not resolved\n", count, fallbackTimeoutMs)
					fallbackTriggered = true
				}
			}

			// Reset idle timer if we're still trying for perfect idle
			if idleStart != nil && !fallbackTriggered {
				fmt.Fprintf(logWriter, "Network activity detected (%d requests), resetting idle timer\n", count)
			}
			idleStart = nil
		}

		time.Sleep(time.Duration(pollIntervalMs) * time.Millisecond)
	}
}

// Context-aware versions for backward compatibility
func WaitForRobustPageLoadWithContext(ctx context.Context, page playwright.Page, logWriter io.Writer, idleDurationMs int, totalTimeoutMs int, pollIntervalMs int) error {
	fmt.Fprintf(logWriter, "Starting robust page load wait with context (idle: %dms, timeout: %dms)...\n", idleDurationMs, totalTimeoutMs)

	// Check context before starting
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	// Step 1: Disable animations for instant loads
	_, err := page.Evaluate(`
		() => {
			const style = document.createElement('style');
			style.innerHTML = '* { transition: none !important; animation: none !important; }';
			document.head.appendChild(style);
		}
	`)
	if err != nil {
		fmt.Fprintf(logWriter, "Warning: Failed to disable animations: %v\n", err)
	}

	// Check context after step 1
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	// Step 2: Force lazy images to eager load
	_, err = page.Evaluate(`
		() => {
			document.querySelectorAll('img[loading="lazy"]').forEach(img => {
				img.setAttribute('loading', 'eager');
			});
			// Also trigger any intersection observers by scrolling briefly
			const images = document.querySelectorAll('img[data-src], img[data-lazy-src]');
			images.forEach(img => {
				if (img.dataset.src) {
					img.src = img.dataset.src;
				}
				if (img.dataset.lazySrc) {
					img.src = img.dataset.lazySrc;
				}
			});
		}
	`)
	if err != nil {
		fmt.Fprintf(logWriter, "Warning: Failed to force lazy images: %v\n", err)
	}

	// Check context after step 2
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	// Step 3: Custom network idle wait with context
	fmt.Fprintf(logWriter, "Waiting for network idle with context...\n")
	err = waitForCustomNetworkIdleWithContext(ctx, page, logWriter, idleDurationMs, totalTimeoutMs, pollIntervalMs)
	if err != nil {
		fmt.Fprintf(logWriter, "Network idle wait failed: %v\n", err)
		return err
	}

	// Check context after network idle
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	// Step 4: Final check for images/videos loaded with context timeout
	fmt.Fprintf(logWriter, "Checking all media resources are loaded...\n")

	// Create a done channel for the WaitForFunction operation
	done := make(chan error, 1)
	go func() {
		_, err := page.WaitForFunction(`
			() => {
				const images = Array.from(document.querySelectorAll('img'));
				const videos = Array.from(document.querySelectorAll('video'));
				
				const imagesLoaded = images.every(img => {
					// Skip images that haven't started loading or are decorative
					if (!img.src || img.src === '' || img.naturalWidth === 0) {
						return img.complete; // Consider complete if no src or still loading
					}
					return img.complete && img.naturalWidth > 0;
				});
				
				const videosLoaded = videos.every(video => video.readyState >= 2); // HAVE_CURRENT_DATA
				
				return imagesLoaded && videosLoaded;
			}
		`, playwright.PageWaitForFunctionOptions{
			Timeout: playwright.Float(10000), // 10s timeout for this check
		})
		done <- err
	}()

	// Wait for either completion or context cancellation
	select {
	case <-ctx.Done():
		fmt.Fprintf(logWriter, "Context cancelled during media resource check\n")
		return ctx.Err()
	case err := <-done:
		if err != nil {
			fmt.Fprintf(logWriter, "Warning: Not all media resources loaded: %v\n", err)
			// Don't fail, just warn - some images may be broken or slow
		}
	}

	fmt.Fprintf(logWriter, "Robust page load completed successfully with context\n")
	return nil
}

func scrollToBottomAndWaitWithContext(ctx context.Context, page playwright.Page, logWriter io.Writer) error {
	fmt.Fprintf(logWriter, "Starting context-aware scroll to bottom\n")

	done := make(chan error, 1)
	go func() {
		done <- scrollToBottomAndWait(page, logWriter)
	}()

	select {
	case <-ctx.Done():
		fmt.Fprintf(logWriter, "Context cancelled during scroll operation\n")
		return ctx.Err()
	case err := <-done:
		return err
	}
}

func waitForCustomNetworkIdleWithContext(ctx context.Context, page playwright.Page, logWriter io.Writer, idleDurationMs int, totalTimeoutMs int, pollIntervalMs int) error {
	fmt.Fprintf(logWriter, "Starting context-aware network idle wait (idle: %dms, timeout: %dms)\n", idleDurationMs, totalTimeoutMs)

	done := make(chan error, 1)
	go func() {
		done <- waitForCustomNetworkIdle(page, logWriter, idleDurationMs, totalTimeoutMs, pollIntervalMs)
	}()

	select {
	case <-ctx.Done():
		fmt.Fprintf(logWriter, "Context cancelled during network idle wait\n")
		return ctx.Err()
	case err := <-done:
		return err
	}
}

// setupBrowserForArchiving is a helper to reduce boilerplate in Playwright-based archivers.
func setupBrowserForArchiving(logWriter io.Writer, pageOpts ...playwright.BrowserNewPageOptions) (*PWBundle, playwright.Page, error) {
	bundle, err := NewPWBundle(logWriter)
	if err != nil {
		fmt.Fprintf(logWriter, "Failed to create PWBundle: %v\n", err)
		return nil, nil, err
	}

	if err := bundle.CreateBrowser(); err != nil {
		bundle.Cleanup() // Cleanup on error
		return nil, nil, err
	}

	if err := bundle.CreatePage(pageOpts...); err != nil {
		// Return bundle for cleanup by the worker, as browser exists.
		return bundle, nil, err
	}

	page, err := bundle.GetPage()
	if err != nil {
		return bundle, nil, err
	}

	// Add default event listeners.
	bundle.AddEventListener(page, "console", func(msg playwright.ConsoleMessage) {
		fmt.Fprintf(logWriter, "Console [%s]: %s\n", msg.Type(), msg.Text())
	})
	bundle.AddEventListener(page, "pageerror", func(err error) {
		fmt.Fprintf(logWriter, "Page error: %v\n", err)
	})

	return bundle, page, nil
}
