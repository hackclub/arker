package utils

import (
	"context"
	"time"
)

// TimeoutConfig holds timeout configuration for different operations
type TimeoutConfig struct {
	ArchiveTimeout time.Duration // Max time for a complete archive operation
	GitCloneTimeout time.Duration // Max time for git clone operations
	YtDlpTimeout   time.Duration // Max time for yt-dlp operations
	PageLoadTimeout time.Duration // Max time for page loading
}

// DefaultTimeoutConfig returns sensible default timeouts
func DefaultTimeoutConfig() TimeoutConfig {
	return TimeoutConfig{
		ArchiveTimeout:  2 * time.Minute,  // Much shorter timeout to prevent browser accumulation
		GitCloneTimeout: 5 * time.Minute,  // Git operations can be slow but not too slow
		YtDlpTimeout:    10 * time.Minute, // Video downloads can take time but not 45 minutes
		PageLoadTimeout: 30 * time.Second, // Page loading should be quick
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
		cancel() // Clean up
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
