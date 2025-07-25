package utils

import (
	"fmt"
	"io"
	"time"
)

// WithErrorHandling wraps a function with error handling, retries, and logging
func WithErrorHandling(fn func() error, logWriter io.Writer, retryCount *int, maxRetries int) error {
	for *retryCount < maxRetries {
		err := fn()
		if err != nil {
			fmt.Fprintf(logWriter, "Error (retry %d/%d): %v\n", *retryCount+1, maxRetries, err)
			*retryCount++
			if *retryCount < maxRetries {
				backoffDuration := time.Second * time.Duration(*retryCount) // Linear backoff
				fmt.Fprintf(logWriter, "Retrying in %v...\n", backoffDuration)
				time.Sleep(backoffDuration)
			}
			continue
		}
		return nil
	}
	return fmt.Errorf("max retries (%d) exceeded", maxRetries)
}

// RetryConfig holds configuration for retry behavior
type RetryConfig struct {
	MaxRetries      int
	BackoffType     BackoffType
	InitialDelay    time.Duration
	MaxDelay        time.Duration
	BackoffMultiplier float64
}

// BackoffType defines the type of backoff strategy
type BackoffType int

const (
	LinearBackoff BackoffType = iota
	ExponentialBackoff
	FixedBackoff
)

// DefaultRetryConfig returns a sensible default retry configuration
func DefaultRetryConfig() RetryConfig {
	return RetryConfig{
		MaxRetries:        3,
		BackoffType:       ExponentialBackoff,
		InitialDelay:      time.Second,
		MaxDelay:          30 * time.Second,
		BackoffMultiplier: 2.0,
	}
}

// WithRetryConfig wraps a function with configurable retry behavior
func WithRetryConfig(fn func() error, logWriter io.Writer, retryCount *int, config RetryConfig) error {
	for *retryCount < config.MaxRetries {
		err := fn()
		if err != nil {
			fmt.Fprintf(logWriter, "Error (attempt %d/%d): %v\n", *retryCount+1, config.MaxRetries, err)
			*retryCount++
			if *retryCount < config.MaxRetries {
				delay := calculateBackoff(*retryCount, config)
				fmt.Fprintf(logWriter, "Retrying in %v...\n", delay)
				time.Sleep(delay)
			}
			continue
		}
		return nil
	}
	return fmt.Errorf("max retries (%d) exceeded", config.MaxRetries)
}

func calculateBackoff(attempt int, config RetryConfig) time.Duration {
	switch config.BackoffType {
	case ExponentialBackoff:
		delay := time.Duration(float64(config.InitialDelay) * pow(config.BackoffMultiplier, float64(attempt-1)))
		if delay > config.MaxDelay {
			return config.MaxDelay
		}
		return delay
	case LinearBackoff:
		delay := config.InitialDelay * time.Duration(attempt)
		if delay > config.MaxDelay {
			return config.MaxDelay
		}
		return delay
	case FixedBackoff:
		return config.InitialDelay
	default:
		return config.InitialDelay
	}
}

// Simple power function for exponential backoff
func pow(base, exp float64) float64 {
	if exp == 0 {
		return 1
	}
	result := base
	for i := 1; i < int(exp); i++ {
		result *= base
	}
	return result
}
