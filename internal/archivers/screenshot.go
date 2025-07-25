package archivers

import (
	"bytes"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"gorm.io/gorm"
	"github.com/HugoSmits86/nativewebp"
	"github.com/playwright-community/playwright-go"
)

// ScreenshotArchiver
type ScreenshotArchiver struct {
	Browser playwright.Browser
}

func (a *ScreenshotArchiver) Archive(url string, logWriter io.Writer, db *gorm.DB, itemID uint) (io.Reader, string, string, func(), error) {
	fmt.Fprintf(logWriter, "Starting screenshot archive for: %s\n", url)
	
	page, err := a.Browser.NewPage(playwright.BrowserNewPageOptions{
		Viewport: &playwright.Size{
			Width:  1500,
			Height: 1080,
		},
		DeviceScaleFactor: playwright.Float(2.0), // Retina quality
	})
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

	// Use the common complete page load sequence (with scrolling for full-page screenshots)
	if err = PerformCompletePageLoad(page, url, logWriter, true); err != nil {
		fmt.Fprintf(logWriter, "Complete page load failed: %v\n", err)
		cleanup()
		return nil, "", "", nil, err
	}

	return a.ArchiveWithPage(page, url, logWriter, cleanup)
}

func (a *ScreenshotArchiver) ArchiveWithPage(page playwright.Page, url string, logWriter io.Writer, cleanup func()) (io.Reader, string, string, func(), error) {
	// Ensure we're at the top of the page before taking screenshot
	fmt.Fprintf(logWriter, "Ensuring page is scrolled to top before screenshot...\n")
	_, err := page.Evaluate(`
		async () => {
			window.scrollTo(0, 0);
			// Wait for scroll to complete and any layout shifts
			await new Promise(resolve => setTimeout(resolve, 300));
			// Double-check we're at the top
			window.scrollTo(0, 0);
		}
	`)
	if err != nil {
		fmt.Fprintf(logWriter, "Warning: Could not scroll to top before screenshot: %v\n", err)
	}

	fmt.Fprintf(logWriter, "Taking full-page screenshot...\n")
	data, err := page.Screenshot(playwright.PageScreenshotOptions{
		FullPage: playwright.Bool(true),
		Type:     (*playwright.ScreenshotType)(playwright.String("png")),
	})
	if err != nil {
		fmt.Fprintf(logWriter, "Failed to take screenshot: %v\n", err)
		return nil, "", "", nil, err
	}

	// Decode PNG and select optimal format
	fmt.Fprintf(logWriter, "Screenshot captured, size: %d bytes. Processing image...\n", len(data))
	
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		fmt.Fprintf(logWriter, "Failed to decode PNG: %v\n", err)
		return nil, "", "", nil, err
	}
	
	fmt.Fprintf(logWriter, "Image decoded, bounds: %v\n", img.Bounds())

	// Select format based on image dimensions
	extension, mimeType, format := selectImageFormat(img, logWriter)

	// Use io.Pipe for streaming encoding
	pipeReader, pipeWriter := io.Pipe()
	
	// Start encoding in a goroutine
	go func() {
		defer pipeWriter.Close()
		
		var encodeErr error
		if format == "jpeg" {
			encodeErr = jpeg.Encode(pipeWriter, img, &jpeg.Options{Quality: 85})
		} else {
			encodeErr = nativewebp.Encode(pipeWriter, img, nil)
		}
		
		if encodeErr != nil {
			fmt.Fprintf(logWriter, "Failed to encode %s: %v\n", format, encodeErr)
			pipeWriter.CloseWithError(encodeErr)
		} else {
			fmt.Fprintf(logWriter, "Screenshot %s encoding completed successfully\n", format)
		}
	}()

	return pipeReader, extension, mimeType, cleanup, nil
}

// selectImageFormat determines the best format based on image dimensions
// Uses JPEG for tall images to avoid WebP size limitations, WebP for others
func selectImageFormat(img image.Image, logWriter io.Writer) (string, string, string) {
	bounds := img.Bounds()
	height := bounds.Dy()
	width := bounds.Dx()
	
	// Use JPEG for very tall images to avoid WebP limitations and reduce file size
	const heightThreshold = 16383 // WebP maximum dimension limit
	
	if height > heightThreshold {
		fmt.Fprintf(logWriter, "Image is tall (%dx%d), using JPEG format\n", width, height)
		return ".jpg", "image/jpeg", "jpeg"
	} else {
		fmt.Fprintf(logWriter, "Image dimensions (%dx%d), using WebP format\n", width, height)
		return ".webp", "image/webp", "webp"
	}
}
