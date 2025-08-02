package utils

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"

	"golang.org/x/net/proxy"
)

// SOCKSHealthStatus represents the current status of the SOCKS proxy
type SOCKSHealthStatus struct {
	IsHealthy    bool      `json:"is_healthy"`
	LastChecked  time.Time `json:"last_checked"`
	ErrorMessage string    `json:"error_message,omitempty"`
	Enabled      bool      `json:"enabled"` // Whether SOCKS proxy is configured
}

// SOCKSHealthChecker manages SOCKS proxy health monitoring
type SOCKSHealthChecker struct {
	mu           sync.RWMutex
	status       SOCKSHealthStatus
	proxyURL     string
	enabled      bool
	stopChan     chan struct{}
	checkInterval time.Duration
}

var (
	globalSOCKSChecker *SOCKSHealthChecker
	checkerOnce        sync.Once
)

// GetSOCKSHealthChecker returns the global SOCKS health checker instance
func GetSOCKSHealthChecker() *SOCKSHealthChecker {
	checkerOnce.Do(func() {
		globalSOCKSChecker = NewSOCKSHealthChecker()
	})
	return globalSOCKSChecker
}

// NewSOCKSHealthChecker creates a new SOCKS health checker
func NewSOCKSHealthChecker() *SOCKSHealthChecker {
	proxyURL := os.Getenv("SOCKS5_PROXY")
	enabled := proxyURL != ""
	
	checker := &SOCKSHealthChecker{
		proxyURL:      proxyURL,
		enabled:       enabled,
		stopChan:      make(chan struct{}),
		checkInterval: 30 * time.Second, // Check every 30 seconds
		status: SOCKSHealthStatus{
			Enabled:     enabled,
			IsHealthy:   true, // Assume healthy until proven otherwise
			LastChecked: time.Now(),
		},
	}
	
	if enabled {
		slog.Info("SOCKS proxy health monitoring enabled", "proxy", proxyURL)
		// Perform initial check
		checker.checkHealth()
		// Start background monitoring
		go checker.startMonitoring()
	} else {
		slog.Info("SOCKS proxy not configured, health monitoring disabled")
	}
	
	return checker
}

// GetStatus returns the current SOCKS proxy health status
func (c *SOCKSHealthChecker) GetStatus() SOCKSHealthStatus {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.status
}

// checkHealth performs a single health check of the SOCKS proxy
func (c *SOCKSHealthChecker) checkHealth() {
	if !c.enabled {
		return
	}
	
	// Parse the SOCKS proxy URL
	proxyURL, err := url.Parse(c.proxyURL)
	if err != nil {
		c.updateStatus(false, fmt.Sprintf("Invalid proxy URL: %v", err))
		return
	}
	
	// Create SOCKS5 dialer
	dialer, err := proxy.FromURL(proxyURL, proxy.Direct)
	if err != nil {
		c.updateStatus(false, fmt.Sprintf("Failed to create SOCKS dialer: %v", err))
		return
	}
	
	// Create HTTP client with SOCKS proxy
	httpClient := &http.Client{
		Transport: &http.Transport{
			Dial: dialer.Dial,
		},
		Timeout: 10 * time.Second,
	}
	
	// Test connectivity through the proxy
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	
	req, err := http.NewRequestWithContext(ctx, "GET", "http://httpbin.org/ip", nil)
	if err != nil {
		c.updateStatus(false, fmt.Sprintf("Failed to create request: %v", err))
		return
	}
	
	resp, err := httpClient.Do(req)
	if err != nil {
		// Check if it's a network error
		if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
			c.updateStatus(false, "SOCKS proxy connection timeout")
		} else {
			c.updateStatus(false, fmt.Sprintf("SOCKS proxy connection failed: %v", err))
		}
		return
	}
	defer resp.Body.Close()
	
	if resp.StatusCode == 200 {
		c.updateStatus(true, "")
		slog.Debug("SOCKS proxy health check passed")
	} else {
		c.updateStatus(false, fmt.Sprintf("HTTP request failed with status: %d", resp.StatusCode))
	}
}

// updateStatus updates the current health status
func (c *SOCKSHealthChecker) updateStatus(healthy bool, errorMsg string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	
	previouslyHealthy := c.status.IsHealthy
	c.status.IsHealthy = healthy
	c.status.LastChecked = time.Now()
	c.status.ErrorMessage = errorMsg
	
	// Log status changes
	if previouslyHealthy != healthy {
		if healthy {
			slog.Info("SOCKS proxy is now healthy")
		} else {
			slog.Warn("SOCKS proxy is now unhealthy", "error", errorMsg)
		}
	}
}

// startMonitoring starts the background health monitoring
func (c *SOCKSHealthChecker) startMonitoring() {
	ticker := time.NewTicker(c.checkInterval)
	defer ticker.Stop()
	
	for {
		select {
		case <-ticker.C:
			c.checkHealth()
		case <-c.stopChan:
			slog.Info("SOCKS health monitoring stopped")
			return
		}
	}
}

// Stop stops the health monitoring
func (c *SOCKSHealthChecker) Stop() {
	if c.enabled {
		close(c.stopChan)
	}
}

// ForceCheck performs an immediate health check (useful for testing)
func (c *SOCKSHealthChecker) ForceCheck() {
	if c.enabled {
		c.checkHealth()
	}
}
