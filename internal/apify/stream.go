package apify

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// Streaming sources. Reddit and Pinterest publish their videos as HLS
// manifests whose renditions carry separate audio; the file Arker stores is
// a muxed MP4, so these go through ffmpeg rather than a plain download.

// remuxStream fetches a manifest (HLS or DASH) into a muxed MP4 via ffmpeg,
// stream-copying both tracks. Pass a variant playlist, not a master: ffmpeg
// takes the first program of a master, which is usually the smallest.
func remuxStream(ctx context.Context, manifestURL, dest string) (int64, error) {
	return runFFmpeg(ctx, dest, "-i", manifestURL, "-map", "0:v:0", "-map", "0:a:0?", "-c", "copy", "-bsf:a", "aac_adtstoasc")
}

// muxFiles joins a video-only file and an audio-only file into one MP4.
func muxFiles(ctx context.Context, videoPath, audioPath, dest string) (int64, error) {
	return runFFmpeg(ctx, dest, "-i", videoPath, "-i", audioPath, "-map", "0:v:0", "-map", "1:a:0", "-c", "copy")
}

func runFFmpeg(ctx context.Context, dest string, inputArgs ...string) (int64, error) {
	tmp := dest + ".part.mp4"
	removeFile(tmp)
	args := append([]string{"-v", "error", "-nostdin", "-y"}, inputArgs...)
	args = append(args, "-movflags", "+faststart", tmp)
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		removeFile(tmp)
		if ctx.Err() != nil {
			return 0, fmt.Errorf("ffmpeg: %w", ctx.Err())
		}
		detail := urlInErrorText.ReplaceAllString(strings.TrimSpace(stderr.String()), "<url>")
		return 0, fmt.Errorf("ffmpeg: %w: %s", err, truncate(detail, 300))
	}
	if err := renameTempFile(tmp, dest); err != nil {
		removeFile(tmp)
		return 0, err
	}
	info, err := os.Stat(dest)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

// isStreamManifest reports whether a URL points at an HLS or DASH playlist
// rather than a file.
func isStreamManifest(rawURL string) bool {
	p := strings.ToLower(urlPath(rawURL))
	return strings.HasSuffix(p, ".m3u8") || strings.HasSuffix(p, ".mpd")
}

var hlsBandwidthPattern = regexp.MustCompile(`(?:^|,)BANDWIDTH=(\d+)`)

// bestHLSVariant fetches an HLS master playlist and returns the URL of its
// highest-bandwidth rendition. A media playlist (no #EXT-X-STREAM-INF) is
// returned as-is.
func (c *Client) bestHLSVariant(ctx context.Context, masterURL string) (string, error) {
	req, err := c.newMediaRequest(ctx, masterURL)
	if err != nil {
		return "", err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", sanitizeTransportError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("playlist fetch returned %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	base, err := url.Parse(masterURL)
	if err != nil {
		return "", err
	}
	var best string
	var bestBandwidth int64 = -1
	var pending int64 = -1
	scanner := bufio.NewScanner(bytes.NewReader(body))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		switch {
		case strings.HasPrefix(line, "#EXT-X-STREAM-INF:"):
			pending = 0
			if m := hlsBandwidthPattern.FindStringSubmatch(line); m != nil {
				pending, _ = strconv.ParseInt(m[1], 10, 64)
			}
		case line == "" || strings.HasPrefix(line, "#"):
			continue
		default:
			if pending < 0 {
				continue
			}
			if pending > bestBandwidth {
				ref, err := url.Parse(line)
				if err == nil {
					best, bestBandwidth = base.ResolveReference(ref).String(), pending
				}
			}
			pending = -1
		}
	}
	if best == "" {
		return masterURL, nil
	}
	return best, nil
}
