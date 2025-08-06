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
    
    bundle, page, err := setupBrowserForArchiving(logWriter)
    if err != nil {
        // If bundle is not nil, it means the browser was created and must be cleaned up by the worker.
        return nil, "", "", bundle, err
    }
    // Note: PWBundle cleanup is deferred in the main worker loop.

    if err = PerformCompletePageLoadWithContext(ctx, page, url, logWriter, true); err != nil {
        return nil, "", "", bundle, err
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
