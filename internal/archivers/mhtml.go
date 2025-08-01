package archivers

import (
	"context"
	"fmt"
	"io"
	"strings"
	"gorm.io/gorm"
	"github.com/playwright-community/playwright-go"
)

// MHTMLArchiver
type MHTMLArchiver struct {
}

func (a *MHTMLArchiver) Archive(ctx context.Context, url string, logWriter io.Writer, db *gorm.DB, itemID uint) (io.Reader, string, string, *PWBundle, error) {
	fmt.Fprintf(logWriter, "Starting MHTML archive for: %s\n", url)
	
	// Check context before creating browser
	select {
	case <-ctx.Done():
		return nil, "", "", nil, ctx.Err()
	default:
	}
	
	// Create PWBundle for guaranteed cleanup
	bundle, err := NewPWBundle(logWriter)
	if err != nil {
		fmt.Fprintf(logWriter, "Failed to create PWBundle: %v\n", err)
		return nil, "", "", nil, err
	}
	
	// Create browser within bundle
	if err := bundle.CreateBrowser(); err != nil {
		bundle.Cleanup() // Cleanup on error
		return nil, "", "", nil, err
	}
	
	// Create page with default options for MHTML
	err = bundle.CreatePage()
	if err != nil {
		fmt.Fprintf(logWriter, "Failed to create browser page: %v\n", err)
		return nil, "", "", bundle, err // Return bundle for cleanup by worker
	}
	
	page, err := bundle.GetPage()
	if err != nil {
		return nil, "", "", bundle, err
	}

	// Add event listeners via bundle (ensures cleanup)
	bundle.AddEventListener(page, "console", func(msg playwright.ConsoleMessage) {
		fmt.Fprintf(logWriter, "Console [%s]: %s\n", msg.Type(), msg.Text())
	})
	bundle.AddEventListener(page, "pageerror", func(err error) {
		fmt.Fprintf(logWriter, "Page error: %v\n", err)
	})

	// Use the common complete page load sequence (with scrolling for MHTML)
	if err = PerformCompletePageLoadWithContext(ctx, page, url, logWriter, true); err != nil {
		fmt.Fprintf(logWriter, "Complete page load failed: %v\n", err)
		return nil, "", "", bundle, err // Return bundle for cleanup by worker
	}

	return a.ArchiveWithPageContext(ctx, page, url, logWriter, bundle)
}

func (a *MHTMLArchiver) ArchiveWithPageContext(ctx context.Context, page playwright.Page, url string, logWriter io.Writer, bundle *PWBundle) (io.Reader, string, string, *PWBundle, error) {
	fmt.Fprintf(logWriter, "Creating CDP session for MHTML capture...\n")
	
	// Check context before creating CDP session
	select {
	case <-ctx.Done():
		return nil, "", "", bundle, ctx.Err()
	default:
	}
	
	session, err := page.Context().NewCDPSession(page)
	if err != nil {
		fmt.Fprintf(logWriter, "Failed to create CDP session: %v\n", err)
		return nil, "", "", bundle, err
	}

	fmt.Fprintf(logWriter, "Capturing MHTML snapshot with context awareness...\n")
	
	// Check context before MHTML capture
	select {
	case <-ctx.Done():
		return nil, "", "", bundle, ctx.Err()
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
		return nil, "", "", bundle, ctx.Err()
	case cdpRes := <-resultChan:
		result = cdpRes.result
		err = cdpRes.err
	}
	
	if err != nil {
		fmt.Fprintf(logWriter, "Failed to capture MHTML snapshot: %v\n", err)
		return nil, "", "", bundle, err
	}

	dataStr := result.(map[string]interface{})["data"].(string)
	fmt.Fprintf(logWriter, "MHTML archive completed successfully, size: %d bytes\n", len(dataStr))
	return strings.NewReader(dataStr), ".mhtml", "application/x-mhtml", bundle, nil
}
