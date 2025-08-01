package archivers

import (
	"os"
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
	
	monitor.RecordBrowserCreation()
	return pw, browser, nil
}
