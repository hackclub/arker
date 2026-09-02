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

const videoThumbnailTimeout = 90 * time.Second

// VideoFrameThumbnail extracts one displayed frame from a local path or HTTP
// URL. ffmpeg can range-read object-storage URLs, avoiding a full historical
// video download merely to make a row preview.
func VideoFrameThumbnail(ctx context.Context, input string) (*Thumbnail, error) {
	if input == "" {
		return nil, fmt.Errorf("video input is empty")
	}
	frameCtx, cancel := context.WithTimeout(ctx, videoThumbnailTimeout)
	defer cancel()
	cmd := exec.CommandContext(frameCtx, "ffmpeg", "-v", "error", "-i", input,
		"-map", "0:v:0", "-frames:v", "1", "-c:v", "mjpeg", "-q:v", "3", "-f", "image2pipe", "pipe:1")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if frameCtx.Err() != nil {
			return nil, fmt.Errorf("extract first video frame: %w", frameCtx.Err())
		}
		if detail := strings.TrimSpace(stderr.String()); detail != "" {
			return nil, fmt.Errorf("extract first video frame: %w: %s", err, detail)
		}
		return nil, fmt.Errorf("extract first video frame: %w", err)
	}
	t, err := thumbnail.OriginalFromReader(bytes.NewReader(stdout.Bytes()))
	if err != nil {
		return nil, fmt.Errorf("prepare first video frame: %w", err)
	}
	return &Thumbnail{Data: t.Data, Width: t.Width, Height: t.Height, Kind: models.ThumbnailKindSocialPreview}, nil
}
