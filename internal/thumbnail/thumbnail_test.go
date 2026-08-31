package thumbnail

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"

	"github.com/HugoSmits86/nativewebp"
)

// solid builds an image whose top band is one colour and bottom band another,
// so tests can tell which part of the source survived the crop.
func banded(w, h int, top, bottom color.RGBA) *image.RGBA {
	m := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		c := top
		if y >= h/2 {
			c = bottom
		}
		for x := 0; x < w; x++ {
			m.Set(x, y, c)
		}
	}
	return m
}

func solid(w, h int, c color.RGBA) *image.RGBA {
	return banded(w, h, c, c)
}

// avg returns the mean RGB of a decoded thumbnail.
func avg(t *testing.T, data []byte) (r, g, b int) {
	t.Helper()
	img, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode thumbnail: %v", err)
	}
	bounds := img.Bounds()
	var sr, sg, sb, n int
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			cr, cg, cb, _ := img.At(x, y).RGBA()
			sr += int(cr >> 8)
			sg += int(cg >> 8)
			sb += int(cb >> 8)
			n++
		}
	}
	return sr / n, sg / n, sb / n
}

func TestFromImageProducesTargetSize(t *testing.T) {
	// A full-page screenshot shape: narrow and very tall.
	got, err := FromImage(solid(3000, 18000, color.RGBA{10, 120, 200, 255}), CropTop)
	if err != nil {
		t.Fatalf("FromImage: %v", err)
	}
	if got.Width != Width || got.Height != Height {
		t.Errorf("size = %dx%d, want %dx%d", got.Width, got.Height, Width, Height)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(got.Data))
	if err != nil {
		t.Fatalf("decode config: %v", err)
	}
	if cfg.Width != Width || cfg.Height != Height {
		t.Errorf("encoded size = %dx%d, want %dx%d", cfg.Width, cfg.Height, Width, Height)
	}
}

// The whole point of anchoring to the top: a long page's thumbnail must show
// its header, not a slice of whatever is halfway down.
func TestFromImageCropsFromTopForTallSources(t *testing.T) {
	red := color.RGBA{255, 0, 0, 255}
	blue := color.RGBA{0, 0, 255, 255}
	got, err := FromImage(banded(1000, 10000, red, blue), CropTop)
	if err != nil {
		t.Fatalf("FromImage: %v", err)
	}
	r, _, b := avg(t, got.Data)
	if r < 200 || b > 60 {
		t.Errorf("thumbnail is not the top band: avg r=%d b=%d, want red-dominant", r, b)
	}
}

// thirds builds an image in three horizontal bands, so a test can tell exactly
// which vertical slice a crop selected.
func thirds(w, h int, top, mid, bottom color.RGBA) *image.RGBA {
	m := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		c := top
		switch {
		case y >= 2*h/3:
			c = bottom
		case y >= h/3:
			c = mid
		}
		for x := 0; x < w; x++ {
			m.Set(x, y, c)
		}
	}
	return m
}

// A 9:16 reel cover frames its subject in the middle. Top-cropping one returns
// the empty space above their head, which is why the anchor is a parameter.
func TestFromImageCropsFromCenterForTallSources(t *testing.T) {
	red := color.RGBA{255, 0, 0, 255}
	green := color.RGBA{0, 255, 0, 255}
	blue := color.RGBA{0, 0, 255, 255}

	// Portrait video thumbnail proportions.
	src := thirds(1080, 1920, red, green, blue)

	centered, err := FromImage(src, CropCenter)
	if err != nil {
		t.Fatalf("FromImage(CropCenter): %v", err)
	}
	r, g, b := avg(t, centered.Data)
	if g < 200 || r > 60 || b > 60 {
		t.Errorf("CropCenter picked the wrong band: avg r=%d g=%d b=%d, want green-dominant", r, g, b)
	}

	// The same source under CropTop must select a different band, or the
	// parameter is not doing anything.
	top, err := FromImage(src, CropTop)
	if err != nil {
		t.Fatalf("FromImage(CropTop): %v", err)
	}
	tr, tg, _ := avg(t, top.Data)
	if tr < 200 || tg > 60 {
		t.Errorf("CropTop picked the wrong band: avg r=%d g=%d, want red-dominant", tr, tg)
	}
}

// Wide sources are centred horizontally regardless of the vertical anchor, so
// both modes must agree there.
func TestCropModeIrrelevantForWideSources(t *testing.T) {
	src := thirds(4000, 500, color.RGBA{200, 0, 0, 255}, color.RGBA{0, 200, 0, 255}, color.RGBA{0, 0, 200, 255})
	a, err := FromImage(src, CropTop)
	if err != nil {
		t.Fatalf("CropTop: %v", err)
	}
	b, err := FromImage(src, CropCenter)
	if err != nil {
		t.Fatalf("CropCenter: %v", err)
	}
	if !bytes.Equal(a.Data, b.Data) {
		t.Error("crop mode changed the result for a source wider than 16:9, where it should not apply")
	}
}

func TestFromImageCentersWideSources(t *testing.T) {
	// Wider than 16:9, so the crop trims the sides and keeps full height.
	got, err := FromImage(solid(4000, 500, color.RGBA{0, 200, 0, 255}), CropTop)
	if err != nil {
		t.Fatalf("FromImage: %v", err)
	}
	if got.Width != Width || got.Height != Height {
		t.Errorf("size = %dx%d, want %dx%d", got.Width, got.Height, Width, Height)
	}
	_, g, _ := avg(t, got.Data)
	if g < 150 {
		t.Errorf("expected green-dominant thumbnail, got g=%d", g)
	}
}

func TestFromImageNeverUpscales(t *testing.T) {
	got, err := FromImage(solid(160, 4000, color.RGBA{40, 40, 40, 255}), CropTop)
	if err != nil {
		t.Fatalf("FromImage: %v", err)
	}
	if got.Width != 160 {
		t.Errorf("width = %d, want 160 (source width, not upscaled to %d)", got.Width, Width)
	}
	if want := 160 * Height / Width; got.Height != want {
		t.Errorf("height = %d, want %d (aspect preserved)", got.Height, want)
	}
}

// JPEG has no alpha channel. A page that never paints a body background decodes
// transparent, and without flattening it would encode as solid black.
func TestFromImageFlattensTransparencyToWhite(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 1200, 1200)) // fully transparent
	got, err := FromImage(src, CropTop)
	if err != nil {
		t.Fatalf("FromImage: %v", err)
	}
	r, g, b := avg(t, got.Data)
	if r < 240 || g < 240 || b < 240 {
		t.Errorf("transparent source encoded as rgb(%d,%d,%d), want near-white", r, g, b)
	}
}

func TestFromImageRejectsEmptyBounds(t *testing.T) {
	if _, err := FromImage(image.NewRGBA(image.Rect(0, 0, 0, 0)), CropTop); err == nil {
		t.Fatal("expected an error for a zero-sized source")
	}
	if _, err := FromImage(nil, CropTop); err == nil {
		t.Fatal("expected an error for a nil source")
	}
}

// The stored screenshot is WebP unless the page was too tall, in which case it
// is JPEG. Both must be readable, or the lazy backfill path is useless.
func TestFromReaderDecodesEveryStoredScreenshotFormat(t *testing.T) {
	src := banded(1200, 2400, color.RGBA{200, 30, 30, 255}, color.RGBA{30, 30, 200, 255})

	var pngBuf, jpgBuf, webpBuf bytes.Buffer
	if err := png.Encode(&pngBuf, src); err != nil {
		t.Fatalf("png encode: %v", err)
	}
	if err := jpeg.Encode(&jpgBuf, src, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("jpeg encode: %v", err)
	}
	// nativewebp is what actually writes the stored screenshots, so this
	// asserts the real round-trip rather than a synthetic WebP.
	if err := nativewebp.Encode(&webpBuf, src, nil); err != nil {
		t.Fatalf("webp encode: %v", err)
	}

	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"png", pngBuf.Bytes()},
		{"jpeg", jpgBuf.Bytes()},
		{"webp", webpBuf.Bytes()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := FromReader(bytes.NewReader(tc.data), CropTop)
			if err != nil {
				t.Fatalf("FromReader: %v", err)
			}
			if got.Width != Width || got.Height != Height {
				t.Errorf("size = %dx%d, want %dx%d", got.Width, got.Height, Width, Height)
			}
			if len(got.Data) == 0 {
				t.Error("empty thumbnail data")
			}
			r, _, b := avg(t, got.Data)
			if r < 150 || b > 100 {
				t.Errorf("expected top (red) band, got avg r=%d b=%d", r, b)
			}
		})
	}
}

func TestOriginalFromReaderPreservesSocialImageBytesAndDimensions(t *testing.T) {
	src := banded(321, 517, color.RGBA{200, 30, 30, 255}, color.RGBA{30, 30, 200, 255})

	var pngBuf, jpgBuf, webpBuf bytes.Buffer
	if err := png.Encode(&pngBuf, src); err != nil {
		t.Fatalf("png encode: %v", err)
	}
	if err := jpeg.Encode(&jpgBuf, src, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("jpeg encode: %v", err)
	}
	if err := nativewebp.Encode(&webpBuf, src, nil); err != nil {
		t.Fatalf("webp encode: %v", err)
	}

	for _, tc := range []struct {
		name        string
		data        []byte
		extension   string
		contentType string
	}{
		{"png", pngBuf.Bytes(), ".png", "image/png"},
		{"jpeg", jpgBuf.Bytes(), ".jpg", "image/jpeg"},
		{"webp", webpBuf.Bytes(), ".webp", "image/webp"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := OriginalFromReader(bytes.NewReader(tc.data))
			if err != nil {
				t.Fatalf("OriginalFromReader: %v", err)
			}
			if !bytes.Equal(got.Data, tc.data) {
				t.Error("social image was cropped, scaled, or re-encoded")
			}
			if got.Width != 321 || got.Height != 517 {
				t.Errorf("dimensions = %dx%d, want the source's 321x517", got.Width, got.Height)
			}
			if extension := FileExtension(got.Data); extension != tc.extension {
				t.Errorf("extension = %q, want %q", extension, tc.extension)
			}
			if contentType := ContentTypeForData(got.Data); contentType != tc.contentType {
				t.Errorf("content type = %q, want %q", contentType, tc.contentType)
			}
		})
	}
}

func TestOriginalFromReaderRejectsInvalidImage(t *testing.T) {
	if _, err := OriginalFromReader(bytes.NewReader([]byte("not an image"))); err == nil {
		t.Fatal("expected invalid provider image to be rejected")
	}
}

// hugePNGHeader builds just enough valid PNG for DecodeConfig to report a huge
// size. Encoding a real 50-megapixel image would cost hundreds of megabytes,
// which is precisely what the guard exists to avoid.
func hugePNGHeader(w, h uint32) []byte {
	var buf bytes.Buffer
	buf.Write([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'})

	ihdr := make([]byte, 0, 13)
	ihdr = binary.BigEndian.AppendUint32(ihdr, w)
	ihdr = binary.BigEndian.AppendUint32(ihdr, h)
	ihdr = append(ihdr, 8, 2, 0, 0, 0) // bit depth 8, truecolour, no interlace

	chunk := append([]byte("IHDR"), ihdr...)
	_ = binary.Write(&buf, binary.BigEndian, uint32(len(ihdr)))
	buf.Write(chunk)
	_ = binary.Write(&buf, binary.BigEndian, crc32.ChecksumIEEE(chunk))
	return buf.Bytes()
}

func TestFromReaderRejectsOversizedSourceBeforeDecoding(t *testing.T) {
	// 8000x8000 = 64 megapixels, comfortably over the limit.
	_, err := FromReader(bytes.NewReader(hugePNGHeader(8000, 8000)), CropTop)
	if err == nil {
		t.Fatal("expected an error for an oversized source")
	}
	if !errors.Is(err, ErrSourceTooLarge) {
		t.Fatalf("error = %v, want ErrSourceTooLarge so callers mark it permanently unavailable", err)
	}
}

func TestFromReaderAcceptsSourceJustUnderTheLimit(t *testing.T) {
	// Header says 6000x6000 = 36MP (under the limit) but the pixel data is
	// truncated, so this proves the size gate passed and decoding was attempted.
	_, err := FromReader(bytes.NewReader(hugePNGHeader(6000, 6000)), CropTop)
	if errors.Is(err, ErrSourceTooLarge) {
		t.Fatalf("36MP source was rejected as too large: %v", err)
	}
	if err == nil {
		t.Fatal("expected a decode error for truncated pixel data")
	}
}

func TestFromReaderHandlesGarbageWithoutPanicking(t *testing.T) {
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"empty", nil},
		{"short", []byte{0x01, 0x02}},
		{"not an image", []byte("this is definitely not an image file at all")},
		{"truncated png", func() []byte {
			var b bytes.Buffer
			_ = png.Encode(&b, solid(64, 64, color.RGBA{1, 2, 3, 255}))
			return b.Bytes()[:20]
		}()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := FromReader(bytes.NewReader(tc.data), CropTop); err == nil {
				t.Error("expected an error, got nil")
			}
		})
	}
}

func TestCanDeriveFromArchive(t *testing.T) {
	if !CanDeriveFromArchive("screenshot") {
		t.Error("screenshot should be thumbnailable")
	}
	for _, typ := range []string{"mhtml", "git", "yt-dlp", "gallery-dl", "itch", "youtube", ""} {
		if CanDeriveFromArchive(typ) {
			t.Errorf("%q should not be thumbnailable from its stored artifact", typ)
		}
	}
}
