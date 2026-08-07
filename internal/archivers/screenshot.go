package archivers

import (
	"bytes"
	"context"
	"fmt"
	"github.com/HugoSmits86/nativewebp"
	"github.com/mxschmitt/playwright-go"
	"gorm.io/gorm"
	"image"
	"image/jpeg"
	"image/png"
	"io"

	"arker/internal/thumbnail"
)

// ScreenshotArchiver
type ScreenshotArchiver struct {
}

func (a *ScreenshotArchiver) Archive(ctx context.Context, url string, logWriter io.Writer, db *gorm.DB, itemID uint) (Result, error) {
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
		return Result{Bundle: bundle}, err
	}
	// Note: PWBundle cleanup is deferred in the main worker loop.

	if err = PerformCompletePageLoadWithContext(ctx, page, url, logWriter, true); err != nil {
		return Result{Bundle: bundle}, err
	}

	return a.ArchiveWithPageContext(ctx, page, url, logWriter, bundle)
}

func (a *ScreenshotArchiver) ArchiveWithPageContext(ctx context.Context, page playwright.Page, url string, logWriter io.Writer, bundle *PWBundle) (Result, error) {
	// Check context before screenshot operations
	select {
	case <-ctx.Done():
		return Result{Bundle: bundle}, ctx.Err()
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
		return Result{Bundle: bundle}, ctx.Err()
	default:
	}

	fmt.Fprintf(logWriter, "Taking full-page screenshot...\n")
	data, err := page.Screenshot(playwright.PageScreenshotOptions{
		FullPage: playwright.Bool(true),
		Type:     (*playwright.ScreenshotType)(playwright.String("png")),
	})
	if err != nil {
		fmt.Fprintf(logWriter, "Failed to take screenshot: %v\n", err)
		return Result{Bundle: bundle}, err
	}

	// Decode PNG and select optimal format
	fmt.Fprintf(logWriter, "Screenshot captured, size: %d bytes. Processing image...\n", len(data))

	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		fmt.Fprintf(logWriter, "Failed to decode PNG: %v\n", err)
		return Result{Bundle: bundle}, err
	}

	fmt.Fprintf(logWriter, "Image decoded, bounds: %v\n", img.Bounds())

	// Derive the thumbnail here, from the image we have already decoded.
	//
	// This is why the screenshot archiver is the thumbnail source: the decode
	// above is work we do regardless, so the thumbnail costs one downscale and
	// no second browser round-trip. Deriving it later from the stored artifact
	// would mean re-decoding a full-page screenshot that can reach 60
	// megapixels.
	//
	// A thumbnail failure is never an archive failure: the screenshot itself is
	// fine, and a missing preview is a cosmetic loss.
	thumb := deriveThumbnail(img, logWriter)

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

	return Result{
		Data:        pipeReader,
		Extension:   extension,
		ContentType: mimeType,
		Bundle:      bundle,
		Thumbnail:   thumb,
	}, nil
}

// deriveThumbnail builds the preview image for a screenshot, returning nil (and
// logging) rather than an error on any failure.
func deriveThumbnail(img image.Image, logWriter io.Writer) *Thumbnail {
	t, err := thumbnail.FromImage(img)
	if err != nil {
		fmt.Fprintf(logWriter, "Thumbnail generation skipped: %v\n", err)
		return nil
	}
	fmt.Fprintf(logWriter, "Thumbnail generated: %dx%d, %d bytes\n", t.Width, t.Height, len(t.Data))
	return &Thumbnail{Data: t.Data, Width: t.Width, Height: t.Height}
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
