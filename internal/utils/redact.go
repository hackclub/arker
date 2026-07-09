package utils

import (
	"io"
	"strings"
)

// RedactPlaceholder replaces any configured secret substring in log output.
const RedactPlaceholder = "[REDACTED]"

// YtDlpProxyRedactionSecrets returns the substrings that must never appear in
// persisted logs (currently the configured proxy URL, which may embed
// credentials). Returns nil when no proxy is configured.
func YtDlpProxyRedactionSecrets() []string {
	ytDlpProxyMu.RLock()
	defer ytDlpProxyMu.RUnlock()
	if ytDlpProxyURL == "" {
		return nil
	}
	return []string{ytDlpProxyURL}
}

// redactingWriter replaces occurrences of any secret with RedactPlaceholder in
// each write before forwarding to the underlying writer. It operates per-write;
// see the note in NewRedactingWriter about the (low-risk) split-across-writes case.
type redactingWriter struct {
	w       io.Writer
	secrets []string
}

// NewRedactingWriter wraps w so that any of secrets is replaced with
// RedactPlaceholder before being written. When secrets is empty it returns w
// unchanged. yt-dlp prints the proxy URL as a single contiguous token on one
// line, so per-write replacement removes it in practice; this is defense at the
// source so the credential is never persisted.
func NewRedactingWriter(w io.Writer, secrets []string) io.Writer {
	nonEmpty := make([]string, 0, len(secrets))
	for _, s := range secrets {
		if s != "" {
			nonEmpty = append(nonEmpty, s)
		}
	}
	if len(nonEmpty) == 0 {
		return w
	}
	return &redactingWriter{w: w, secrets: nonEmpty}
}

// RedactSecrets replaces any non-empty configured secret substring in s.
func RedactSecrets(s string, secrets []string) string {
	for _, secret := range secrets {
		if secret != "" {
			s = strings.ReplaceAll(s, secret, RedactPlaceholder)
		}
	}
	return s
}

func (r *redactingWriter) Write(p []byte) (int, error) {
	s := RedactSecrets(string(p), r.secrets)
	if _, err := r.w.Write([]byte(s)); err != nil {
		// Report full consumption of the caller's bytes to avoid short-write retries
		// that would double-log; the redacted content stands in for the original.
		return len(p), err
	}
	return len(p), nil
}
