package utils

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// YtDlpVersion returns the installed yt-dlp version for archive logs. Logging
// this per video job makes stale Docker/cache deployments obvious when an
// extractor breaks on Instagram.
func YtDlpVersion(ctx context.Context) (string, error) {
	versionCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	output, err := exec.CommandContext(versionCtx, "yt-dlp", "--version").Output()
	if versionCtx.Err() != nil {
		return "", versionCtx.Err()
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}
