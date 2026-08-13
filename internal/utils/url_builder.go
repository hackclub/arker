package utils

import (
	"strings"

	"github.com/gin-gonic/gin"
)

// BuildFullURL constructs a full URL from the request context and a path
func BuildFullURL(c *gin.Context, path string) string {
	scheme := "https"
	if c.Request.TLS == nil {
		// Check for forwarded protocol header (common in reverse proxies)
		if proto := c.GetHeader("X-Forwarded-Proto"); proto != "" {
			scheme = strings.TrimSpace(strings.Split(proto, ",")[0])
		} else if forwarded := strings.ToLower(c.GetHeader("Forwarded")); strings.Contains(forwarded, "proto=https") {
			scheme = "https"
		} else if visitor := strings.ToLower(c.GetHeader("CF-Visitor")); strings.Contains(visitor, `"scheme":"https"`) {
			// Cloudflare supplies CF-Visitor even when an intermediate reverse
			// proxy does not preserve X-Forwarded-Proto.
			scheme = "https"
		} else {
			scheme = "http"
		}
	}
	return scheme + "://" + c.Request.Host + "/" + path
}
