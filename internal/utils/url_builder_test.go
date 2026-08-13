package utils

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestBuildFullURLProxySchemes(t *testing.T) {
	for _, tc := range []struct{ name, header, value string }{
		{"forwarded proto", "X-Forwarded-Proto", "https"},
		{"standard forwarded", "Forwarded", "for=192.0.2.1;proto=https;host=archive.example"},
		{"cloudflare visitor", "CF-Visitor", `{"scheme":"https"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest("GET", "http://archive.example/result", nil)
			c.Request.Header.Set(tc.header, tc.value)
			if got := BuildFullURL(c, "abc12"); got != "https://archive.example/abc12" {
				t.Fatalf("got %q", got)
			}
		})
	}
}

func TestBuildFullURLDefaultsPublicDNSHostsToHTTPS(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", "http://archive.hackclub.com/result", nil)
	c.Request.Header.Set("X-Forwarded-Proto", "http")
	if got := BuildFullURL(c, "abc12"); got != "https://archive.hackclub.com/abc12" {
		t.Fatalf("got %q", got)
	}
	c.Request = httptest.NewRequest("GET", "http://localhost:8080/result", nil)
	if got := BuildFullURL(c, "abc12"); got != "http://localhost:8080/abc12" {
		t.Fatalf("local got %q", got)
	}
}
