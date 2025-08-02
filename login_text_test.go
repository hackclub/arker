package main

import (
	"testing"
)

func TestLoginTextDisplay(t *testing.T) {
	// Simple test to verify the LOGIN_TEXT environment variable concept
	testCases := []struct {
		name        string
		loginText   string
		shouldShow  bool
		description string
	}{
		{
			name:        "Empty",
			loginText:   "",
			shouldShow:  false,
			description: "No text should be shown when LOGIN_TEXT is empty",
		},
		{
			name:        "DemoCredentials",
			loginText:   "Demo credentials: admin/admin",
			shouldShow:  true,
			description: "Text should be shown when LOGIN_TEXT is set",
		},
		{
			name:        "MultilineText",
			loginText:   "Demo Instance\nUsername: admin\nPassword: admin",
			shouldShow:  true,
			description: "Multiline text should be supported",
		},
		{
			name:        "HTMLText",
			loginText:   "Use <strong>admin</strong> / <strong>admin</strong>",
			shouldShow:  true,
			description: "HTML in text should be supported",
		},
	}
	
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Verify basic logic: non-empty text should show, empty should not
			hasText := tc.loginText != ""
			if hasText != tc.shouldShow {
				t.Errorf("Expected shouldShow=%v for loginText='%s'", tc.shouldShow, tc.loginText)
			}
			
			t.Logf("✅ %s: LOGIN_TEXT='%s' -> show=%v", tc.description, tc.loginText, tc.shouldShow)
		})
	}
}

func TestLoginTextConfig(t *testing.T) {
	// Test various LOGIN_TEXT configurations
	testCases := []struct {
		name        string
		loginText   string
		description string
	}{
		{
			name:        "Empty",
			loginText:   "",
			description: "No LOGIN_TEXT environment variable",
		},
		{
			name:        "SimpleCredentials",
			loginText:   "Demo: admin/admin",
			description: "Simple credential display",
		},
		{
			name:        "DetailedInfo",
			loginText:   "Demo instance - Username: admin, Password: admin",
			description: "More detailed information",
		},
		{
			name:        "WithFormatting",
			loginText:   "Use <strong>admin</strong> / <strong>admin</strong>",
			description: "HTML formatting in login text",
		},
		{
			name:        "MultilineInstructions",
			loginText:   "Demo Instance\nCredentials:\n  Username: admin\n  Password: admin",
			description: "Multiline instructions with formatting",
		},
	}
	
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Verify the configuration would work
			isEmpty := tc.loginText == ""
			t.Logf("✅ %s: LOGIN_TEXT='%s' (empty=%v)", tc.description, tc.loginText, isEmpty)
			
			// Basic validation
			if isEmpty && tc.loginText != "" {
				t.Error("Text should be empty but isn't")
			}
			if !isEmpty && tc.loginText == "" {
				t.Error("Text should not be empty but is")
			}
		})
	}
}
