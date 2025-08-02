package main

import (
	"os"
	"testing"
	"time"

	"arker/internal/utils"
)

func TestSOCKSHealthChecker(t *testing.T) {
	// Test 1: No SOCKS proxy configured
	os.Unsetenv("SOCKS5_PROXY")
	checker := utils.NewSOCKSHealthChecker()
	status := checker.GetStatus()
	
	if status.Enabled {
		t.Error("Expected SOCKS checker to be disabled when no proxy is configured")
	}
	
	checker.Stop()
	
	// Test 2: Invalid SOCKS proxy configured
	os.Setenv("SOCKS5_PROXY", "invalid-proxy-url")
	checker2 := utils.NewSOCKSHealthChecker()
	status2 := checker2.GetStatus()
	
	if !status2.Enabled {
		t.Error("Expected SOCKS checker to be enabled when proxy is configured")
	}
	
	// Give it a moment to check (it starts with healthy=true)
	time.Sleep(100 * time.Millisecond)
	checker2.ForceCheck()
	time.Sleep(100 * time.Millisecond)
	
	status2Updated := checker2.GetStatus()
	if status2Updated.IsHealthy {
		t.Error("Expected SOCKS checker to be unhealthy with invalid proxy")
	}
	
	if status2Updated.ErrorMessage == "" {
		t.Error("Expected error message when SOCKS proxy is unhealthy")
	}
	
	checker2.Stop()
	
	// Test 3: Valid but unreachable SOCKS proxy
	os.Setenv("SOCKS5_PROXY", "socks5://localhost:9999")
	checker3 := utils.NewSOCKSHealthChecker()
	
	// Force a check
	checker3.ForceCheck()
	time.Sleep(100 * time.Millisecond)
	
	status3 := checker3.GetStatus()
	if status3.IsHealthy {
		t.Error("Expected SOCKS checker to be unhealthy with unreachable proxy")
	}
	
	checker3.Stop()
	
	// Clean up
	os.Unsetenv("SOCKS5_PROXY")
}

func TestSOCKSStatusFields(t *testing.T) {
	os.Setenv("SOCKS5_PROXY", "socks5://localhost:9999")
	defer os.Unsetenv("SOCKS5_PROXY")
	
	checker := utils.NewSOCKSHealthChecker()
	defer checker.Stop()
	
	status := checker.GetStatus()
	
	// Check that all required fields are present
	if !status.Enabled {
		t.Error("Expected status.Enabled to be true")
	}
	
	if status.LastChecked.IsZero() {
		t.Error("Expected LastChecked to be set")
	}
	
	// Force a health check to get error message
	checker.ForceCheck()
	time.Sleep(100 * time.Millisecond)
	
	updatedStatus := checker.GetStatus()
	if updatedStatus.ErrorMessage == "" {
		t.Error("Expected error message when proxy is unreachable")
	}
}
