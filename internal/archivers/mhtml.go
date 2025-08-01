package archivers

import (
	"context"
	"fmt"
	"io"
	"strings"
	"gorm.io/gorm"
	"github.com/playwright-community/playwright-go"
	"arker/internal/monitoring"
)

// MHTMLArchiver
type MHTMLArchiver struct {
}

func (a *MHTMLArchiver) Archive(ctx context.Context, url string, logWriter io.Writer, db *gorm.DB, itemID uint) (io.Reader, string, string, func(), error) {
	fmt.Fprintf(logWriter, "Starting MHTML archive for: %s\n", url)
	
	// Check context before creating browser
	select {
	case <-ctx.Done():
		return nil, "", "", nil, ctx.Err()
	default:
	}
	
	// Create a fresh browser instance for this job
	fmt.Fprintf(logWriter, "Creating fresh browser instance for MHTML job...\n")
	pw, browser, err := CreateBrowserInstance()
	if err != nil {
		fmt.Fprintf(logWriter, "Failed to create browser instance: %v\n", err)
		return nil, "", "", nil, err
	}
	
	page, err := browser.NewPage()
	if err != nil {
		browser.Close()
		pw.Stop()
		fmt.Fprintf(logWriter, "Failed to create browser page: %v\n", err)
		return nil, "", "", nil, err
	}
	
	cleanup := func() { 
		monitor := monitoring.GetGlobalMonitor()
		page.Close()
		browser.Close()
		pw.Stop()
		monitor.RecordPlaywrightClose()
		monitor.RecordBrowserCleanup()
		fmt.Fprintf(logWriter, "Browser instance cleaned up\n")
	}

	// Log console messages and errors
	page.On("console", func(msg playwright.ConsoleMessage) {
		fmt.Fprintf(logWriter, "Console [%s]: %s\n", msg.Type(), msg.Text())
	})
	page.On("pageerror", func(err error) {
		fmt.Fprintf(logWriter, "Page error: %v\n", err)
	})

	// Use the common complete page load sequence (with scrolling for MHTML)
	if err = PerformCompletePageLoadWithContext(ctx, page, url, logWriter, true); err != nil {
		fmt.Fprintf(logWriter, "Complete page load failed: %v\n", err)
		cleanup()
		return nil, "", "", nil, err
	}

	return a.ArchiveWithPageContext(ctx, page, url, logWriter, cleanup)
}

func (a *MHTMLArchiver) ArchiveWithPage(page playwright.Page, url string, logWriter io.Writer) (io.Reader, string, string, func(), error) {
	// For backward compatibility, create a background context
	return a.ArchiveWithPageContext(context.Background(), page, url, logWriter, nil)
}

func (a *MHTMLArchiver) ArchiveWithPageContext(ctx context.Context, page playwright.Page, url string, logWriter io.Writer, cleanup func()) (io.Reader, string, string, func(), error) {
	fmt.Fprintf(logWriter, "Creating CDP session for MHTML capture...\n")
	
	// Check context before creating CDP session
	select {
	case <-ctx.Done():
		return nil, "", "", nil, ctx.Err()
	default:
	}
	
	session, err := page.Context().NewCDPSession(page)
	if err != nil {
		fmt.Fprintf(logWriter, "Failed to create CDP session: %v\n", err)
		return nil, "", "", nil, err
	}

	fmt.Fprintf(logWriter, "Capturing MHTML snapshot with context awareness...\n")
	
	// Check context before MHTML capture
	select {
	case <-ctx.Done():
		return nil, "", "", nil, ctx.Err()
	default:
	}
	
	// Make the CDP call context-aware
	type cdpResult struct {
		result interface{}
		err    error
	}
	
	resultChan := make(chan cdpResult, 1)
	go func() {
		result, err := session.Send("Page.captureSnapshot", map[string]interface{}{"format": "mhtml"})
		resultChan <- cdpResult{result: result, err: err}
	}()
	
	// Wait for either completion or context cancellation
	var result interface{}
	select {
	case <-ctx.Done():
		fmt.Fprintf(logWriter, "Context cancelled during MHTML capture\n")
		return nil, "", "", nil, ctx.Err()
	case cdpRes := <-resultChan:
		result = cdpRes.result
		err = cdpRes.err
	}
	
	if err != nil {
		fmt.Fprintf(logWriter, "Failed to capture MHTML snapshot: %v\n", err)
		return nil, "", "", nil, err
	}

	dataStr := result.(map[string]interface{})["data"].(string)
	fmt.Fprintf(logWriter, "MHTML archive completed successfully, size: %d bytes\n", len(dataStr))
	return strings.NewReader(dataStr), ".mhtml", "application/x-mhtml", cleanup, nil
}
