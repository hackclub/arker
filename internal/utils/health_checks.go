package utils

import (
	"fmt"
	"os/exec"
	"time"
	"github.com/playwright-community/playwright-go"
)

// HealthCheckConfig holds configuration for health checks
type HealthCheckConfig struct {
	CheckYtDlp     bool
	CheckPlaywright bool
	CheckAria2c    bool
	Timeout        time.Duration
}

// DefaultHealthCheckConfig returns a sensible default health check configuration
func DefaultHealthCheckConfig() HealthCheckConfig {
	return HealthCheckConfig{
		CheckYtDlp:     true,
		CheckPlaywright: true,
		CheckAria2c:    true,
		Timeout:        10 * time.Second,
	}
}

// RunHealthChecks performs startup health checks
func RunHealthChecks(config HealthCheckConfig) error {
	if config.CheckYtDlp {
		if err := CheckYtDlpAvailability(config.Timeout); err != nil {
			return fmt.Errorf("yt-dlp health check failed: %v", err)
		}
	}
	
	if config.CheckAria2c {
		if err := CheckAria2cAvailability(config.Timeout); err != nil {
			return fmt.Errorf("aria2c health check failed: %v", err)
		}
	}
	
	if config.CheckPlaywright {
		if err := CheckPlaywrightAvailability(config.Timeout); err != nil {
			return fmt.Errorf("playwright health check failed: %v", err)
		}
	}
	
	return nil
}

// CheckYtDlpAvailability checks if yt-dlp is available and working
func CheckYtDlpAvailability(timeout time.Duration) error {
	done := make(chan error, 1)
	
	go func() {
		cmd := exec.Command("yt-dlp", "--version")
		done <- cmd.Run()
	}()
	
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("yt-dlp not available or not working: %v", err)
		}
		return nil
	case <-time.After(timeout):
		return fmt.Errorf("yt-dlp health check timed out after %v", timeout)
	}
}

// CheckAria2cAvailability checks if aria2c is available and working
func CheckAria2cAvailability(timeout time.Duration) error {
	done := make(chan error, 1)
	
	go func() {
		cmd := exec.Command("aria2c", "--version")
		done <- cmd.Run()
	}()
	
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("aria2c not available or not working: %v", err)
		}
		return nil
	case <-time.After(timeout):
		return fmt.Errorf("aria2c health check timed out after %v", timeout)
	}
}

// CheckPlaywrightAvailability checks if Playwright can create a browser instance
func CheckPlaywrightAvailability(timeout time.Duration) error {
	// Create a channel to signal completion
	done := make(chan error, 1)
	
	go func() {
		pw, err := playwright.Run()
		if err != nil {
			done <- fmt.Errorf("failed to start Playwright: %v", err)
			return
		}
		defer pw.Stop()
		
		browser, err := pw.Chromium.Launch(playwright.BrowserTypeLaunchOptions{
			Headless: playwright.Bool(true),
		})
		if err != nil {
			done <- fmt.Errorf("failed to launch Chromium: %v", err)
			return
		}
		defer browser.Close()
		
		// Try to create a page to ensure everything works
		page, err := browser.NewPage()
		if err != nil {
			done <- fmt.Errorf("failed to create browser page: %v", err)
			return
		}
		defer page.Close()
		
		done <- nil
	}()
	
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		return fmt.Errorf("playwright health check timed out after %v", timeout)
	}
}
