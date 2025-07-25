package utils

import (
	"github.com/gin-gonic/gin"
)

// BuildFullURL constructs a full URL from the request context and a path
func BuildFullURL(c *gin.Context, path string) string {
	scheme := "https"
	if c.Request.TLS == nil {
		// Check for forwarded protocol header (common in reverse proxies)
		if proto := c.GetHeader("X-Forwarded-Proto"); proto != "" {
			scheme = proto
		} else {
			scheme = "http"
		}
	}
	return scheme + "://" + c.Request.Host + "/" + path
}
