package utils

import (
	"bytes"
	"strings"
	"testing"
)

func TestRedactingWriterReplacesSecret(t *testing.T) {
	secret := "http://user:pass@proxy.example.com:8080"
	var buf bytes.Buffer
	w := NewRedactingWriter(&buf, []string{secret})

	line := "[debug] Proxy: " + secret + " connecting...\n"
	if _, err := w.Write([]byte(line)); err != nil {
		t.Fatalf("write: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, secret) {
		t.Fatalf("secret leaked into output: %q", out)
	}
	if !strings.Contains(out, RedactPlaceholder) {
		t.Fatalf("expected placeholder in output: %q", out)
	}
}

func TestRedactingWriterPassthroughWhenNoSecret(t *testing.T) {
	var buf bytes.Buffer
	w := NewRedactingWriter(&buf, nil)
	if w != &buf {
		t.Fatal("expected NewRedactingWriter to return the underlying writer when no secrets")
	}
	msg := "no secrets here\n"
	w.Write([]byte(msg))
	if buf.String() != msg {
		t.Fatalf("passthrough altered output: %q", buf.String())
	}
}
