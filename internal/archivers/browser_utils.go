package archivers

import (
	"github.com/playwright-community/playwright-go"
	"arker/internal/monitoring"
)

// CreateBrowserInstance creates a fresh browser instance with proper configuration
func CreateBrowserInstance() (*playwright.Playwright, playwright.Browser, error) {
	monitor := monitoring.GetGlobalMonitor()
	monitor.RecordPlaywrightLaunch()
	
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
	
	// SOCKS5 proxy configuration is now handled at the context level
	// Chrome's --proxy-server argument doesn't support SOCKS5 authentication
	
	browser, err := pw.Chromium.Launch(playwright.BrowserTypeLaunchOptions{
		Headless: playwright.Bool(true),
		Args:     launchArgs,
	})
	if err != nil {
		pw.Stop()
		return nil, nil, err
	}
	
	monitor.RecordBrowserCreation()
	return pw, browser, nil
}
