// Package thumbnail prepares preview images from archived content. Page
// screenshots become compact derived previews; social posters and post images
// keep their original bytes and intrinsic geometry.
//
// Thumbnails are a derived artifact, not an archive type: they are generated as
// a side effect of the job that produced their source, stored as their own
// object, and pointed at by columns on the owning archive_item. Nothing in the
// archive pipeline depends on a thumbnail existing, and no thumbnail failure is
// ever allowed to fail an archive.
package thumbnail

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"io"

	// Decoders for every format an archiver can write a still image in.
	// x/image/webp is decode-only, which is all we need: nativewebp writes the
	// stored screenshots, this reads them back.
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"

	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp"

	"arker/internal/utils"
)

const (
	// Width and Height are the target dimensions. 16:9 at 2x, so it renders
	// crisply at ~240px in a dashboard row and is still large enough to be the
	// image in an API consumer's card.
	Width  = 480
	Height = 270

	// Quality is the JPEG quality for the encoded thumbnail.
	//
	// JPEG, not WebP, deliberately. The only WebP encoder in the tree
	// (nativewebp) is lossless-only, which produced a ~490KB "thumbnail" in
	// testing versus ~30KB for JPEG at this size, and it panics on some
	// low-colour-count inputs -- exactly what a mostly-blank page decodes to.
	Quality = 78

	// Extension and ContentType describe the encoded output.
	Extension   = ".jpg"
	ContentType = "image/jpeg"

	// MaxSourcePixels caps how large a source image we are willing to fully
	// decode. A full-page screenshot of a long page reaches 3000x20000, and
	// decoding that costs ~240MB of RGBA. Reject beyond this rather than let a
	// worker OOM; image.DecodeConfig tells us the size before we commit.
	MaxSourcePixels = 40_000_000

	// headerPeek is how far we look ahead to read image dimensions without
	// consuming the stream. Every format's header fits comfortably inside this.
	headerPeek = 256 << 10

	// MaxOriginalBytes bounds a platform poster or social post image retained
	// byte-for-byte. These are normally hundreds of kilobytes; the generous
	// ceiling prevents a bogus provider response from turning a cosmetic
	// artifact into an unbounded allocation.
	MaxOriginalBytes = 32 << 20
)

// ErrSourceTooLarge is returned when a source image exceeds MaxSourcePixels.
// It is a permanent condition for a given source: callers should record the
// thumbnail as unavailable rather than retry.
var ErrSourceTooLarge = errors.New("thumbnail: source image exceeds maximum decodable size")

// Crop selects which part of a source taller than 16:9 survives.
//
// The right answer depends entirely on what the image is, so callers must say.
// A page screenshot puts its identity at the top -- the header, the headline --
// and the archiver scrolls to (0,0) before capturing, so CropTop is correct and
// centring would slice out a random band of body text. A video or photo
// thumbnail is the opposite: a 9:16 reel cover frames its subject in the middle,
// and CropTop would return the ceiling above their head.
//
// Sources wider than 16:9 are always centred horizontally; there is no
// equivalent reason to prefer one side.
type Crop int

const (
	// CropTop keeps the top band. For page screenshots.
	CropTop Crop = iota
	// CropCenter keeps the middle band. For video and photo thumbnails.
	CropCenter
)

// Thumb is an encoded thumbnail ready to be written to storage.
type Thumb struct {
	Data   []byte
	Width  int
	Height int
}

// OriginalFromReader validates a social post's own preview image and retains
// it byte-for-byte, including its intrinsic aspect ratio, dimensions and
// encoding. Social thumbnails are already deliberately framed by the poster
// or platform; cropping them to 16:9 and re-encoding them would replace that
// authored image with an Arker derivative.
//
// Screenshot previews still go through FromReader/FromImage below: a full-page
// screenshot is not a usable card image until it has been cropped and scaled.
func OriginalFromReader(r io.Reader) (*Thumb, error) {
	if r == nil {
		return nil, errors.New("thumbnail: nil source reader")
	}

	data, err := io.ReadAll(io.LimitReader(r, MaxOriginalBytes+1))
	if err != nil {
		return nil, fmt.Errorf("thumbnail: reading original image: %w", err)
	}
	if len(data) == 0 {
		return nil, errors.New("thumbnail: source image is empty")
	}
	if len(data) > MaxOriginalBytes {
		return nil, fmt.Errorf("thumbnail: original image exceeds %d-byte limit", MaxOriginalBytes)
	}

	width, height, _, _, ok := encodedImageInfo(data)
	if !ok {
		return nil, errors.New("thumbnail: unsupported or unreadable source image format")
	}
	if width <= 0 || height <= 0 {
		return nil, fmt.Errorf("thumbnail: source image has invalid dimensions %dx%d", width, height)
	}
	if px := int64(width) * int64(height); px > MaxSourcePixels {
		return nil, fmt.Errorf("%w: %dx%d (%d pixels, limit %d)",
			ErrSourceTooLarge, width, height, px, MaxSourcePixels)
	}

	return &Thumb{Data: data, Width: width, Height: height}, nil
}

// FileExtension returns the extension matching an encoded thumbnail's real
// format. It falls back to the historical JPEG extension for old callers and
// deliberately synthetic test thumbnails that predate format preservation.
func FileExtension(data []byte) string {
	if _, _, extension, _, ok := encodedImageInfo(data); ok {
		return extension
	}
	return Extension
}

// ContentTypeForData returns the media type matching the bytes served by the
// thumbnail endpoint. Existing thumbnails are JPEG; new social thumbnails may
// retain JPEG, PNG, GIF or WebP exactly as the provider published them.
func ContentTypeForData(data []byte) string {
	if _, _, _, contentType, ok := encodedImageInfo(data); ok {
		return contentType
	}
	return ContentType
}

// encodedImageInfo recognizes every still format gallery-dl may archive.
// Go's image packages decode JPEG/PNG/GIF/WebP headers. AVIF and HEIC/HEIF are
// ISO-BMFF containers; retaining their original bytes needs only the ftyp brand
// and the standardized ispe (image spatial extents) property, not a pixel
// decoder or re-encoder.
func encodedImageInfo(data []byte) (width, height int, extension, contentType string, ok bool) {
	if cfg, format, err := image.DecodeConfig(bytes.NewReader(data)); err == nil {
		if extension, contentType, ok := imageFormat(format); ok {
			return cfg.Width, cfg.Height, extension, contentType, true
		}
	}

	extension, contentType, ok = bmffImageFormat(data)
	if !ok {
		return 0, 0, "", "", false
	}
	for i := 4; i+16 <= len(data); i++ {
		if !bytes.Equal(data[i:i+4], []byte("ispe")) {
			continue
		}
		boxStart := i - 4
		boxSize := int(binary.BigEndian.Uint32(data[boxStart:i]))
		if boxSize < 20 || boxStart+boxSize > len(data) {
			continue
		}
		w := binary.BigEndian.Uint32(data[i+8 : i+12])
		h := binary.BigEndian.Uint32(data[i+12 : i+16])
		if w == 0 || h == 0 || uint64(w) > uint64(^uint(0)>>1) || uint64(h) > uint64(^uint(0)>>1) {
			continue
		}
		return int(w), int(h), extension, contentType, true
	}
	return 0, 0, "", "", false
}

func bmffImageFormat(data []byte) (extension, contentType string, ok bool) {
	if len(data) < 16 || !bytes.Equal(data[4:8], []byte("ftyp")) {
		return "", "", false
	}
	boxSize := int(binary.BigEndian.Uint32(data[:4]))
	if boxSize < 16 || boxSize > len(data) {
		return "", "", false
	}
	brands := data[8:boxSize]
	hasBrand := func(wants ...string) bool {
		for i := 0; i+4 <= len(brands); i += 4 {
			brand := string(brands[i : i+4])
			for _, want := range wants {
				if brand == want {
					return true
				}
			}
		}
		return false
	}
	if hasBrand("avif", "avis") {
		return ".avif", "image/avif", true
	}
	if hasBrand("heic", "heix", "hevc", "hevx", "heim", "heis") {
		return ".heic", "image/heic", true
	}
	if hasBrand("heif", "mif1", "msf1") {
		return ".heif", "image/heif", true
	}
	return "", "", false
}

func imageFormat(format string) (extension, contentType string, ok bool) {
	switch format {
	case "jpeg":
		return ".jpg", "image/jpeg", true
	case "png":
		return ".png", "image/png", true
	case "gif":
		return ".gif", "image/gif", true
	case "webp":
		return ".webp", "image/webp", true
	default:
		return "", "", false
	}
}

// CanDeriveFromArchive reports whether the stored artifact for an archive type
// is itself a still image that FromReader can thumbnail directly.
//
// Only the screenshot artifact qualifies today. Video, zip and MHTML artifacts
// need their own extraction path; until one exists, their items are recorded as
// unavailable so the lazy generator does not retry them forever.
func CanDeriveFromArchive(archiveType string) bool {
	return utils.NormalizeArchiveType(archiveType) == utils.ArchiveTypeScreenshot
}

// FromReader decodes an encoded image and returns a thumbnail of it.
//
// The reader is only partially consumed when the source is rejected for size,
// so callers must not assume it has been drained.
func FromReader(r io.Reader, crop Crop) (thumb *Thumb, err error) {
	// Decoding attacker-influenced image data. A malformed file that trips a
	// bounds check inside a decoder must not take down the worker process.
	defer func() {
		if rec := recover(); rec != nil {
			thumb = nil
			err = fmt.Errorf("thumbnail: panic while decoding source image: %v", rec)
		}
	}()

	br := bufio.NewReaderSize(r, headerPeek)

	// Peek rather than read: DecodeConfig would consume the header bytes that
	// Decode then needs. A short file yields io.EOF here with the bytes it did
	// have, which is fine -- headers are tiny.
	head, peekErr := br.Peek(headerPeek)
	if len(head) == 0 {
		if peekErr != nil {
			return nil, fmt.Errorf("thumbnail: reading source image: %w", peekErr)
		}
		return nil, errors.New("thumbnail: source image is empty")
	}

	cfg, format, err := image.DecodeConfig(bytes.NewReader(head))
	if err != nil {
		return nil, fmt.Errorf("thumbnail: reading source image header: %w", err)
	}
	if px := int64(cfg.Width) * int64(cfg.Height); px > MaxSourcePixels {
		return nil, fmt.Errorf("%w: %dx%d (%d pixels, limit %d)",
			ErrSourceTooLarge, cfg.Width, cfg.Height, px, MaxSourcePixels)
	}

	src, _, err := image.Decode(br)
	if err != nil {
		return nil, fmt.Errorf("thumbnail: decoding %s source image: %w", format, err)
	}
	return FromImage(src, crop)
}

// FromImage crops an already-decoded image to the thumbnail aspect ratio and
// scales it down.
//
// Callers that already hold a decoded image should use this: the screenshot
// archiver decodes its own PNG anyway, so deriving the thumbnail there costs no
// extra decode and no extra browser round-trip.
func FromImage(src image.Image, crop Crop) (thumb *Thumb, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			thumb = nil
			err = fmt.Errorf("thumbnail: panic while encoding thumbnail: %v", rec)
		}
	}()

	if src == nil {
		return nil, errors.New("thumbnail: nil source image")
	}
	bounds := src.Bounds()
	if bounds.Dx() <= 0 || bounds.Dy() <= 0 {
		return nil, fmt.Errorf("thumbnail: source image has empty bounds %v", bounds)
	}

	region := cropRect(bounds, crop)
	w, h := targetSize(region)

	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	// Flatten onto white first. Screenshots of pages that never paint a body
	// background decode with a transparent backdrop, and JPEG has no alpha
	// channel -- without this they encode as solid black.
	draw.Draw(dst, dst.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), src, region, xdraw.Over, nil)

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: Quality}); err != nil {
		return nil, fmt.Errorf("thumbnail: encoding jpeg: %w", err)
	}
	return &Thumb{Data: buf.Bytes(), Width: w, Height: h}, nil
}

// cropRect picks the region of the source that becomes the thumbnail. See Crop
// for why the vertical anchor is the caller's decision.
func cropRect(b image.Rectangle, crop Crop) image.Rectangle {
	w, h := b.Dx(), b.Dy()

	if w*Height > h*Width {
		// Source is wider than the target aspect: trim the sides, centred.
		cropW := h * Width / Height
		if cropW < 1 {
			cropW = 1
		}
		off := (w - cropW) / 2
		return image.Rect(b.Min.X+off, b.Min.Y, b.Min.X+off+cropW, b.Max.Y)
	}

	// Source is taller than the target aspect: trim vertically.
	cropH := w * Height / Width
	if cropH < 1 {
		cropH = 1
	}
	offY := 0
	if crop == CropCenter {
		offY = (h - cropH) / 2
	}
	return image.Rect(b.Min.X, b.Min.Y+offY, b.Max.X, b.Min.Y+offY+cropH)
}

// targetSize returns the output dimensions for a crop, never upscaling. A
// favicon-sized source stays small rather than being blown up into a blurry
// 480px image; the real dimensions are recorded so templates can set width and
// height attributes and avoid layout shift.
func targetSize(crop image.Rectangle) (int, int) {
	if crop.Dx() >= Width {
		return Width, Height
	}
	w := crop.Dx()
	h := w * Height / Width
	if h < 1 {
		h = 1
	}
	return w, h
}
