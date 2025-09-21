package archivers

import (
	"bytes"
	"context"
	"fmt"
	"github.com/HugoSmits86/nativewebp"
	"github.com/playwright-community/playwright-go"
	"gorm.io/gorm"
	"image"
	"image/jpeg"
	"image/png"
	"io"
)

// ScreenshotArchiver
type ScreenshotArchiver struct {
}

func (a *ScreenshotArchiver) Archive(ctx context.Context, url string, logWriter io.Writer, db *gorm.DB, itemID uint) (io.Reader, string, string, *PWBundle, error) {
	fmt.Fprintf(logWriter, "Starting screenshot archive for: %s\n", url)

	pageOpts := playwright.BrowserNewPageOptions{
		Viewport: &playwright.Size{
			Width:  1500,
			Height: 1080,
		},
		DeviceScaleFactor: playwright.Float(2.0), // Retina quality
	}

	bundle, page, err := setupBrowserForArchiving(logWriter, pageOpts)
	if err != nil {
		return nil, "", "", bundle, err
	}
	// Note: PWBundle cleanup is deferred in the main worker loop.

	if err = PerformCompletePageLoadWithContext(ctx, page, url, logWriter, true); err != nil {
		return nil, "", "", bundle, err
	}

	return a.ArchiveWithPageContext(ctx, page, url, logWriter, bundle)
}

func (a *ScreenshotArchiver) ArchiveWithPageContext(ctx context.Context, page playwright.Page, url string, logWriter io.Writer, bundle *PWBundle) (io.Reader, string, string, *PWBundle, error) {
	// Check context before screenshot operations
	select {
	case <-ctx.Done():
		return nil, "", "", bundle, ctx.Err()
	default:
	}

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

	// Check context before taking screenshot
	select {
	case <-ctx.Done():
		return nil, "", "", bundle, ctx.Err()
	default:
	}

	fmt.Fprintf(logWriter, "Taking full-page screenshot...\n")
	data, err := page.Screenshot(playwright.PageScreenshotOptions{
		FullPage: playwright.Bool(true),
		Type:     (*playwright.ScreenshotType)(playwright.String("png")),
	})
	if err != nil {
		fmt.Fprintf(logWriter, "Failed to take screenshot: %v\n", err)
		return nil, "", "", bundle, err
	}

	// Decode PNG and select optimal format
	fmt.Fprintf(logWriter, "Screenshot captured, size: %d bytes. Processing image...\n", len(data))

	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		fmt.Fprintf(logWriter, "Failed to decode PNG: %v\n", err)
		return nil, "", "", bundle, err
	}

	fmt.Fprintf(logWriter, "Image decoded, bounds: %v\n", img.Bounds())

	// Select format based on image dimensions
	extension, mimeType, format := selectImageFormat(img, logWriter)

	// Use io.Pipe for streaming encoding
	pipeReader, pipeWriter := io.Pipe()

	// Start context-aware encoding in a goroutine
	go func() {
		defer pipeWriter.Close()

		// Check context before starting encoding
		select {
		case <-ctx.Done():
			fmt.Fprintf(logWriter, "Context cancelled during screenshot encoding\n")
			pipeWriter.CloseWithError(ctx.Err())
			return
		default:
		}

		// Use a channel to signal completion
		done := make(chan error, 1)
		go func() {
			var encodeErr error
			if format == "jpeg" {
				encodeErr = jpeg.Encode(pipeWriter, img, &jpeg.Options{Quality: 85})
			} else {
				encodeErr = nativewebp.Encode(pipeWriter, img, nil)
			}
			done <- encodeErr
		}()

		// Wait for either completion or context cancellation
		select {
		case <-ctx.Done():
			fmt.Fprintf(logWriter, "Context cancelled during screenshot encoding\n")
			pipeWriter.CloseWithError(ctx.Err())
		case encodeErr := <-done:
			if encodeErr != nil {
				fmt.Fprintf(logWriter, "Failed to encode %s: %v\n", format, encodeErr)
				pipeWriter.CloseWithError(encodeErr)
			} else {
				fmt.Fprintf(logWriter, "Screenshot %s encoding completed successfully\n", format)
			}
		}
	}()

	return pipeReader, extension, mimeType, bundle, nil
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
