package main

import (
	"testing"
	"arker/internal/utils"
)

func TestURLValidation(t *testing.T) {
	tests := []struct {
		name        string
		url         string
		shouldError bool
		errorMsg    string
	}{
		{
			name:        "Valid HTTPS URL",
			url:         "https://example.com",
			shouldError: false,
		},
		{
			name:        "Valid HTTP URL",
			url:         "http://example.com/path",
			shouldError: false,
		},
		{
			name:        "Empty URL",
			url:         "",
			shouldError: true,
			errorMsg:    "URL cannot be empty",
		},
		{
			name:        "Localhost",
			url:         "http://localhost:8080",
			shouldError: true,
			errorMsg:    "requests to localhost are not allowed",
		},
		{
			name:        "127.0.0.1",
			url:         "http://127.0.0.1:3000",
			shouldError: true,
			errorMsg:    "requests to localhost are not allowed",
		},
		{
			name:        "Private IP 192.168.x.x",
			url:         "http://192.168.1.1",
			shouldError: true,
			errorMsg:    "requests to private/internal IP addresses are not allowed",
		},
		{
			name:        "Private IP 10.x.x.x",
			url:         "http://10.0.0.1",
			shouldError: true,
			errorMsg:    "requests to private/internal IP addresses are not allowed",
		},
		{
			name:        "File protocol",
			url:         "file:///etc/passwd",
			shouldError: true,
			errorMsg:    "only HTTP and HTTPS protocols are allowed",
		},
		{
			name:        "FTP protocol",
			url:         "ftp://example.com",
			shouldError: true,
			errorMsg:    "only HTTP and HTTPS protocols are allowed",
		},
		{
			name:        "Invalid URL format",
			url:         "not-a-url",
			shouldError: true,
			errorMsg:    "only HTTP and HTTPS protocols are allowed",
		},
		{
			name:        "URL with suspicious pattern",
			url:         "http://example.com/file://attack",
			shouldError: true,
			errorMsg:    "protocol file:// is not allowed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := utils.ValidateURL(tt.url)
			
			if tt.shouldError {
				if err == nil {
					t.Errorf("Expected error for URL %s, but got none", tt.url)
					return
				}
				if tt.errorMsg != "" && !contains(err.Error(), tt.errorMsg) {
					t.Errorf("Expected error message to contain '%s', got '%s'", tt.errorMsg, err.Error())
				}
			} else {
				if err != nil {
					t.Errorf("Expected no error for URL %s, but got: %v", tt.url, err)
				}
			}
		})
	}
}

func TestArchiveRequestValidation(t *testing.T) {
	tests := []struct {
		name        string
		request     utils.ArchiveRequest
		shouldError bool
	}{
		{
			name: "Valid request with no types",
			request: utils.ArchiveRequest{
				URL: "https://example.com",
			},
			shouldError: false,
		},
		{
			name: "Valid request with types",
			request: utils.ArchiveRequest{
				URL:   "https://example.com",
				Types: []string{"mhtml", "screenshot"},
			},
			shouldError: false,
		},
		{
			name: "Empty URL",
			request: utils.ArchiveRequest{
				URL: "",
			},
			shouldError: true,
		},
		{
			name: "Invalid archive type",
			request: utils.ArchiveRequest{
				URL:   "https://example.com",
				Types: []string{"invalid-type"},
			},
			shouldError: true,
		},
		{
			name: "SSRF attempt",
			request: utils.ArchiveRequest{
				URL: "http://localhost:8080",
			},
			shouldError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.request.Validate()
			
			if tt.shouldError {
				if err == nil {
					t.Errorf("Expected error for request %+v, but got none", tt.request)
				}
			} else {
				if err != nil {
					t.Errorf("Expected no error for request %+v, but got: %v", tt.request, err)
				}
			}
		})
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && s[:len(substr)] == substr || len(s) > len(substr) && s[len(s)-len(substr):] == substr || 
		   (len(s) > len(substr) && findSubstring(s, substr))
}

func findSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
