package utils

import (
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/go-playground/validator/v10"
)

var validate *validator.Validate

func init() {
	validate = validator.New()
}

// ValidateURL validates and sanitizes a URL to prevent SSRF attacks
func ValidateURL(rawURL string) error {
	if strings.TrimSpace(rawURL) == "" {
		return fmt.Errorf("URL cannot be empty")
	}

	// Parse the URL
	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL format: %v", err)
	}

	// Check for valid scheme
	if parsedURL.Scheme != "http" && parsedURL.Scheme != "https" {
		return fmt.Errorf("only HTTP and HTTPS protocols are allowed")
	}

	// Check for hostname
	if parsedURL.Hostname() == "" {
		return fmt.Errorf("URL must have a valid hostname")
	}

	// SSRF protection: Check for private/internal IP addresses
	if err := checkSSRFProtection(parsedURL.Hostname()); err != nil {
		return err
	}

	// Additional checks for malicious patterns
	if err := checkMaliciousPatterns(rawURL); err != nil {
		return err
	}

	return nil
}

// checkSSRFProtection prevents requests to private/internal networks
func checkSSRFProtection(hostname string) error {
	// Check for localhost variations
	if isLocalhost(hostname) {
		return fmt.Errorf("requests to localhost are not allowed")
	}

	// Resolve hostname to IP addresses
	ips, err := net.LookupIP(hostname)
	if err != nil {
		// If we can't resolve, we might want to allow it and let the request fail naturally
		// But for security, we'll be strict and reject unresolvable hostnames
		return fmt.Errorf("unable to resolve hostname: %v", err)
	}

	// Check each resolved IP
	for _, ip := range ips {
		if isPrivateIP(ip) {
			return fmt.Errorf("requests to private/internal IP addresses are not allowed")
		}
	}

	return nil
}

// isLocalhost checks for localhost variations
func isLocalhost(hostname string) bool {
	localhost := []string{
		"localhost",
		"127.0.0.1",
		"::1",
		"0.0.0.0",
	}
	
	for _, local := range localhost {
		if strings.EqualFold(hostname, local) {
			return true
		}
	}
	
	return false
}

// isPrivateIP checks if an IP address is in a private/internal range
func isPrivateIP(ip net.IP) bool {
	// IPv4 private ranges
	privateRanges := []string{
		"10.0.0.0/8",     // RFC1918
		"172.16.0.0/12",  // RFC1918
		"192.168.0.0/16", // RFC1918
		"127.0.0.0/8",    // Loopback
		"169.254.0.0/16", // Link-local
		"224.0.0.0/4",    // Multicast
		"240.0.0.0/4",    // Reserved
	}

	// IPv6 private ranges
	ipv6PrivateRanges := []string{
		"::1/128",        // Loopback
		"fe80::/10",      // Link-local
		"fc00::/7",       // Unique local
		"ff00::/8",       // Multicast
	}

	// Check IPv4 ranges
	if ip.To4() != nil {
		for _, rangeStr := range privateRanges {
			_, privateNet, _ := net.ParseCIDR(rangeStr)
			if privateNet.Contains(ip) {
				return true
			}
		}
	} else {
		// Check IPv6 ranges
		for _, rangeStr := range ipv6PrivateRanges {
			_, privateNet, _ := net.ParseCIDR(rangeStr)
			if privateNet.Contains(ip) {
				return true
			}
		}
	}

	return false
}

// checkMaliciousPatterns checks for suspicious URL patterns
func checkMaliciousPatterns(rawURL string) error {
	// Convert to lowercase for pattern matching
	lower := strings.ToLower(rawURL)
	
	// Check for suspicious patterns
	suspiciousPatterns := []string{
		"file://",
		"ftp://",
		"gopher://",
		"dict://",
		"ldap://",
		"ldaps://",
		"telnet://",
		"ssh://",
		"sftp://",
		"tftp://",
	}
	
	for _, pattern := range suspiciousPatterns {
		if strings.Contains(lower, pattern) {
			return fmt.Errorf("protocol %s is not allowed", pattern)
		}
	}
	
	// Check for URL encoding attempts to bypass filters
	if strings.Contains(lower, "%") {
		decoded, err := url.QueryUnescape(lower)
		if err == nil && decoded != lower {
			// Recursively check the decoded URL
			return checkMaliciousPatterns(decoded)
		}
	}
	
	return nil
}

// ValidateArchiveRequest validates the request structure for archiving
type ArchiveRequest struct {
	URL   string   `json:"url" validate:"required"`
	Types []string `json:"types,omitempty"`
}

func (r *ArchiveRequest) Validate() error {
	// Validate struct tags
	if err := validate.Struct(r); err != nil {
		return fmt.Errorf("validation failed: %v", err)
	}
	
	// Validate URL for SSRF
	if err := ValidateURL(r.URL); err != nil {
		return fmt.Errorf("URL validation failed: %v", err)
	}
	
	// Validate archive types if provided
	if len(r.Types) > 0 {
		validTypes := map[string]bool{
			"mhtml":      true,
			"screenshot": true,
			"git":        true,
			"youtube":    true,
		}
		
		for _, archiveType := range r.Types {
			if !validTypes[archiveType] {
				return fmt.Errorf("invalid archive type: %s", archiveType)
			}
		}
	}
	
	return nil
}
