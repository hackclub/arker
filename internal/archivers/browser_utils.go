package archivers

import (
	"fmt"
	"io"
	"os"
	"github.com/playwright-community/playwright-go"
)

// CreateBrowserInstance creates a fresh browser instance with proper configuration
func CreateBrowserInstance() (*playwright.Playwright, playwright.Browser, error) {
	pw, err := playwright.Run()
	if err != nil {
		return nil, nil, err
	}
	
	// Standard browser launch args for security and performance
	launchArgs := []string{
		"--no-sandbox",
		"--disable-setuid-sandbox",
		"--disable-dev-shm-usage",
		"--disable-web-security",
		"--disable-features=VizDisplayCompositor",
	}
	
	// Add SOCKS5 proxy configuration if SOCKS5_PROXY is set
	if socks5Proxy := os.Getenv("SOCKS5_PROXY"); socks5Proxy != "" {
		launchArgs = append(launchArgs, "--proxy-server="+socks5Proxy)
	}
	
	browser, err := pw.Chromium.Launch(playwright.BrowserTypeLaunchOptions{
		Headless: playwright.Bool(true),
		Args:     launchArgs,
	})
	if err != nil {
		pw.Stop()
		return nil, nil, err
	}
	
	return pw, browser, nil
}

// CreateSafeCleanupFunc creates a panic-safe cleanup function for browser resources
func CreateSafeCleanupFunc(page playwright.Page, browser playwright.Browser, pw *playwright.Playwright, logWriter io.Writer) func() {
	return func() {
		defer func() {
			if r := recover(); r != nil {
				fmt.Fprintf(logWriter, "Warning: panic during browser cleanup: %v\n", r)
			}
		}()
		
		// Try to close page first
		if page != nil {
			func() {
				defer func() {
					if r := recover(); r != nil {
						fmt.Fprintf(logWriter, "Warning: panic during page close: %v\n", r)
					}
				}()
				page.Close()
			}()
		}
		
		// Try to close browser
		if browser != nil {
			func() {
				defer func() {
					if r := recover(); r != nil {
						fmt.Fprintf(logWriter, "Warning: panic during browser close: %v\n", r)
					}
				}()
				browser.Close()
			}()
		}
		
		// Try to stop playwright
		if pw != nil {
			func() {
				defer func() {
					if r := recover(); r != nil {
						fmt.Fprintf(logWriter, "Warning: panic during playwright stop: %v\n", r)
					}
				}()
				pw.Stop()
			}()
		}
		
		fmt.Fprintf(logWriter, "Browser instance cleaned up safely\n")
	}
}
