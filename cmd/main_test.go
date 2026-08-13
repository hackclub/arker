package main

import (
	"reflect"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestNormalizeGinModeDefaultsToRelease(t *testing.T) {
	tests := map[string]string{
		"":        gin.ReleaseMode,
		"release": gin.ReleaseMode,
		"debug":   gin.DebugMode,
		"test":    gin.TestMode,
		"bad":     gin.ReleaseMode,
	}

	for input, want := range tests {
		if got := normalizeGinMode(input); got != want {
			t.Fatalf("normalizeGinMode(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestParseTrustedProxies(t *testing.T) {
	got := parseTrustedProxies("127.0.0.1, ::1, 172.16.0.0/12,")
	want := []string{"127.0.0.1", "::1", "172.16.0.0/12"}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseTrustedProxies returned %#v, want %#v", got, want)
	}
}
