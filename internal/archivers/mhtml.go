package archivers

import (
	"fmt"
	"io"
	"strings"
	"gorm.io/gorm"
	"github.com/playwright-community/playwright-go"
)

// MHTMLArchiver
type MHTMLArchiver struct {
	Browser playwright.Browser
}

func (a *MHTMLArchiver) Archive(url string, logWriter io.Writer, db *gorm.DB, itemID uint) (io.Reader, string, string, func(), error) {
	fmt.Fprintf(logWriter, "Starting MHTML archive for: %s\n", url)
	
	page, err := a.Browser.NewPage()
	if err != nil {
		fmt.Fprintf(logWriter, "Failed to create browser page: %v\n", err)
		return nil, "", "", nil, err
	}
	cleanup := func() { page.Close() }

	// Log console messages and errors
	page.On("console", func(msg playwright.ConsoleMessage) {
		fmt.Fprintf(logWriter, "Console [%s]: %s\n", msg.Type(), msg.Text())
	})
	page.On("pageerror", func(err error) {
		fmt.Fprintf(logWriter, "Page error: %v\n", err)
	})

	// Use the common complete page load sequence (with scrolling for MHTML)
	if err = PerformCompletePageLoad(page, url, logWriter, true); err != nil {
		fmt.Fprintf(logWriter, "Complete page load failed: %v\n", err)
		cleanup()
		return nil, "", "", nil, err
	}

	return a.ArchiveWithPage(page, url, logWriter)
}

func (a *MHTMLArchiver) ArchiveWithPage(page playwright.Page, url string, logWriter io.Writer) (io.Reader, string, string, func(), error) {
	fmt.Fprintf(logWriter, "Creating CDP session for MHTML capture...\n")
	session, err := page.Context().NewCDPSession(page)
	if err != nil {
		fmt.Fprintf(logWriter, "Failed to create CDP session: %v\n", err)
		return nil, "", "", nil, err
	}

	fmt.Fprintf(logWriter, "Capturing MHTML snapshot...\n")
	result, err := session.Send("Page.captureSnapshot", map[string]interface{}{"format": "mhtml"})
	if err != nil {
		fmt.Fprintf(logWriter, "Failed to capture MHTML snapshot: %v\n", err)
		return nil, "", "", nil, err
	}

	dataStr := result.(map[string]interface{})["data"].(string)
	fmt.Fprintf(logWriter, "MHTML archive completed successfully, size: %d bytes\n", len(dataStr))
	return strings.NewReader(dataStr), ".mhtml", "application/x-mhtml", nil, nil
}
