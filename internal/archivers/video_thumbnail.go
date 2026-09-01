package archivers

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"arker/internal/models"
	"arker/internal/thumbnail"
)

const videoThumbnailTimeout = 45 * time.Second

// VideoFrameThumbnailFile decodes the first displayed frame of an archived
// video at its intrinsic dimensions. Unlike a platform's small CDN preview,
// these pixels come from the exact media bytes Arker retained.
//
// JPEG is used as the output container because a decoded video frame has no
// original image encoding to preserve. No scale or crop filter is applied.
func VideoFrameThumbnailFile(ctx context.Context, path string) (*Thumbnail, error) {
	if path == "" {
		return nil, fmt.Errorf("video path is empty")
	}
	frameCtx, cancel := context.WithTimeout(ctx, videoThumbnailTimeout)
	defer cancel()

	cmd := exec.CommandContext(frameCtx, "ffmpeg",
		"-v", "error",
		"-i", path,
		"-map", "0:v:0",
		"-frames:v", "1",
		"-c:v", "mjpeg",
		"-q:v", "2",
		"-f", "image2pipe",
		"pipe:1",
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if frameCtx.Err() != nil {
			return nil, fmt.Errorf("extract first video frame: %w", frameCtx.Err())
		}
		if detail != "" {
			return nil, fmt.Errorf("extract first video frame: %w: %s", err, detail)
		}
		return nil, fmt.Errorf("extract first video frame: %w", err)
	}
	if stdout.Len() == 0 {
		return nil, fmt.Errorf("extract first video frame: ffmpeg returned no image")
	}

	t, err := thumbnail.OriginalFromReader(bytes.NewReader(stdout.Bytes()))
	if err != nil {
		return nil, fmt.Errorf("decode first video frame: %w", err)
	}
	return &Thumbnail{
		Data: t.Data, Width: t.Width, Height: t.Height,
		Kind: models.ThumbnailKindSocialOriginal,
	}, nil
}
