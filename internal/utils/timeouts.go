package utils

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// TimeoutConfig holds timeout configuration for different operations
type TimeoutConfig struct {
	ArchiveTimeout        time.Duration // Max time for a complete archive operation
	GitCloneTimeout       time.Duration // Max time for git clone operations
	YtDlpTimeout          time.Duration // Fallback max time for yt-dlp operations when duration is unknown
	YtDlpMinTimeout       time.Duration // Minimum max time for short yt-dlp operations
	YtDlpMaxTimeout       time.Duration // Maximum dynamic max time for yt-dlp operations
	YtDlpProbeTimeout     time.Duration // Max time for probing video metadata
	YtDlpDurationOverhead time.Duration // Extra time for extraction, muxing, and upload
	PageLoadTimeout       time.Duration // Max time for page loading
}

// DefaultTimeoutConfig returns sensible default timeouts
func DefaultTimeoutConfig() TimeoutConfig {
	return TimeoutConfig{
		ArchiveTimeout:        2 * time.Minute,  // Much shorter timeout to prevent browser accumulation
		GitCloneTimeout:       5 * time.Minute,  // Git operations can be slow but not too slow
		YtDlpTimeout:          60 * time.Minute, // Safe fallback when duration probing fails
		YtDlpMinTimeout:       15 * time.Minute,
		YtDlpMaxTimeout:       3 * time.Hour,
		YtDlpProbeTimeout:     30 * time.Second,
		YtDlpDurationOverhead: 10 * time.Minute,
		PageLoadTimeout:       30 * time.Second, // Page loading should be quick
	}
}

// WithTimeout wraps a function with a timeout context
func WithTimeout(timeout time.Duration, fn func(ctx context.Context) error) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- fn(ctx)
	}()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ContextAwareFunction represents a function that can be cancelled via context
type ContextAwareFunction func(ctx context.Context) error

// WithTimeoutAndCancel wraps a context-aware function with timeout and returns a cancel function
func WithTimeoutAndCancel(timeout time.Duration, fn ContextAwareFunction) (error, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)

	done := make(chan error, 1)
	go func() {
		done <- fn(ctx)
	}()

	select {
	case err := <-done:
		cancel()              // Clean up
		return err, func() {} // Return no-op cancel since already completed
	case <-ctx.Done():
		return ctx.Err(), cancel
	}
}

// TimeoutForJobType returns the appropriate timeout for a given job type
func TimeoutForJobType(jobType string) time.Duration {
	config := DefaultTimeoutConfig()
	switch jobType {
	case "git":
		return config.GitCloneTimeout
	case "youtube":
		return config.YtDlpTimeout
	default:
		return config.ArchiveTimeout
	}
}

// EstimateYtDlpTimeout scales the video archive timeout from media duration.
func EstimateYtDlpTimeout(duration time.Duration) time.Duration {
	config := DefaultTimeoutConfig()
	if duration <= 0 {
		return config.YtDlpTimeout
	}

	estimated := duration + duration/2 + config.YtDlpDurationOverhead
	if estimated < config.YtDlpMinTimeout {
		return config.YtDlpMinTimeout
	}
	if estimated > config.YtDlpMaxTimeout {
		return config.YtDlpMaxTimeout
	}
	return estimated
}

// TimeoutForArchiveJob returns the timeout for a specific archive job.
func TimeoutForArchiveJob(ctx context.Context, jobType, url string, logWriter io.Writer) time.Duration {
	return timeoutForArchiveJob(ctx, jobType, url, logWriter, ProbeYtDlpDuration)
}

func timeoutForArchiveJob(ctx context.Context, jobType, url string, logWriter io.Writer, probeDuration func(context.Context, string) (time.Duration, error)) time.Duration {
	if jobType != "youtube" {
		return TimeoutForJobType(jobType)
	}

	duration, err := probeDuration(ctx, url)
	if err != nil {
		timeout := EstimateYtDlpTimeout(0)
		if logWriter != nil {
			fmt.Fprintf(logWriter, "Could not determine video duration, using fallback timeout %s: %v\n", timeout, err)
		}
		return timeout
	}

	timeout := EstimateYtDlpTimeout(duration)
	if logWriter != nil {
		fmt.Fprintf(logWriter, "Detected video duration %s, using dynamic timeout %s\n", duration.Round(time.Second), timeout.Round(time.Second))
	}
	return timeout
}

// ProbeYtDlpDuration asks yt-dlp for the video duration without downloading media.
func ProbeYtDlpDuration(ctx context.Context, url string) (time.Duration, error) {
	config := DefaultTimeoutConfig()
	probeCtx, cancel := context.WithTimeout(ctx, config.YtDlpProbeTimeout)
	defer cancel()

	cookieArgs, cleanupCookies, err := YtDlpCookieArgsForRun()
	if err != nil {
		return 0, fmt.Errorf("yt-dlp cookies unavailable for duration probe: %w", err)
	}
	defer cleanupCookies()

	args := []string{"--print", "duration", "--no-playlist", "--skip-download"}
	args = append(args, YtDlpImpersonateArgsForURL(url)...)
	args = append(args, cookieArgs...)
	args = append(args, YtDlpProxyArgs()...)
	args = append(args, url)
	cmd := exec.CommandContext(probeCtx, "yt-dlp", args...)
	output, err := cmd.CombinedOutput()
	if probeCtx.Err() != nil {
		return 0, fmt.Errorf("yt-dlp duration probe timed out after %s", config.YtDlpProbeTimeout)
	}
	if err != nil {
		outputText := RedactSecrets(strings.TrimSpace(string(output)), YtDlpProxyRedactionSecrets())
		return 0, fmt.Errorf("yt-dlp duration probe failed: %w: %s", err, outputText)
	}

	duration, err := parseYtDlpDuration(output)
	if err != nil {
		return 0, err
	}
	return duration, nil
}

func parseYtDlpDuration(output []byte) (time.Duration, error) {
	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		seconds, err := strconv.ParseFloat(line, 64)
		if err == nil && seconds > 0 {
			return time.Duration(seconds * float64(time.Second)), nil
		}
	}
	return 0, fmt.Errorf("yt-dlp duration probe did not return a positive duration: %s", strings.TrimSpace(string(output)))
}
