package utils

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

var (
	ytDlpCookiesMu       sync.RWMutex
	ytDlpCookiesFilePath string

	ytDlpProxyMu  sync.RWMutex
	ytDlpProxyURL string

	ytDlpImpersonateMu     sync.RWMutex
	ytDlpImpersonateTarget string
)

// InitYtDlpProxy configures an optional proxy passed to every yt-dlp invocation.
// Instagram (and other sites) rate-limit datacenter IP ranges aggressively; a
// residential/mobile proxy lets a server-hosted archiver fetch media that would
// otherwise be throttled. proxyURL is a full proxy URL (http://, https://,
// socks5://, optionally with credentials); empty disables it.
func InitYtDlpProxy(proxyURL string) string {
	url := strings.TrimSpace(proxyURL)
	ytDlpProxyMu.Lock()
	ytDlpProxyURL = url
	ytDlpProxyMu.Unlock()
	return url
}

// YtDlpProxyArgs returns the --proxy arguments for yt-dlp invocations, or nil
// when no proxy is configured.
func YtDlpProxyArgs() []string {
	ytDlpProxyMu.RLock()
	defer ytDlpProxyMu.RUnlock()
	if ytDlpProxyURL == "" {
		return nil
	}
	return []string{"--proxy", ytDlpProxyURL}
}

// InitYtDlpImpersonate configures yt-dlp's browser impersonation target.
// The production Docker image sets this to "chrome" because it includes
// curl-cffi. Empty disables impersonation, which is useful for manual installs
// of yt-dlp without the curl-cffi extra.
func InitYtDlpImpersonate(target string) string {
	target = strings.TrimSpace(target)
	ytDlpImpersonateMu.Lock()
	ytDlpImpersonateTarget = target
	ytDlpImpersonateMu.Unlock()
	return target
}

// YtDlpImpersonateArgs returns --impersonate arguments for yt-dlp invocations,
// or nil when impersonation is not configured.
func YtDlpImpersonateArgs() []string {
	ytDlpImpersonateMu.RLock()
	defer ytDlpImpersonateMu.RUnlock()
	if ytDlpImpersonateTarget == "" {
		return nil
	}
	return []string{"--impersonate", ytDlpImpersonateTarget}
}

// YtDlpImpersonateArgsForURL applies browser impersonation only to video sites
// that commonly need browser-like TLS/headers. This avoids changing YouTube and
// Vimeo behavior when the Docker image defaults YTDLP_IMPERSONATE=chrome.
func YtDlpImpersonateArgsForURL(rawURL string) []string {
	if !(IsInstagramURL(rawURL) || IsTikTokURL(rawURL) || IsFacebookURL(rawURL)) {
		return nil
	}
	return YtDlpImpersonateArgs()
}

// InitYtDlpCookies configures the cookies file passed to every yt-dlp invocation.
// Sites like Instagram refuse media requests from logged-out clients, so a
// Netscape-format cookies.txt from a logged-in browser session is required.
//
// cookiesFile is a path to an existing cookies.txt (e.g. a mounted secret;
// it may be read-only — yt-dlp only ever sees per-run copies of it).
// cookiesB64 is the base64-encoded content of a cookies.txt, written to a
// file under dir; it is used only when cookiesFile is empty. Returns the
// resolved path, or "" when no cookies are configured.
func InitYtDlpCookies(cookiesFile, cookiesB64, dir string) (string, error) {
	path := strings.TrimSpace(cookiesFile)
	if path != "" {
		if _, err := os.Stat(path); err != nil {
			return "", fmt.Errorf("YTDLP_COOKIES_FILE %q is not readable: %w", path, err)
		}
	} else if b64 := strings.TrimSpace(cookiesB64); b64 != "" {
		content, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return "", fmt.Errorf("YTDLP_COOKIES_B64 is not valid base64: %w", err)
		}
		path = filepath.Join(dir, "yt-dlp-cookies.txt")
		if err := os.WriteFile(path, content, 0600); err != nil {
			return "", fmt.Errorf("failed to write yt-dlp cookies file: %w", err)
		}
	}

	ytDlpCookiesMu.Lock()
	ytDlpCookiesFilePath = path
	ytDlpCookiesMu.Unlock()
	return path, nil
}

// YtDlpCookieArgsForRun returns --cookies arguments pointing at a fresh
// private copy of the configured cookies, plus a cleanup function to call
// after yt-dlp exits. yt-dlp rewrites its --cookies file on exit, so handing
// concurrent invocations one shared jar risks corrupting it, and a read-only
// YTDLP_COOKIES_FILE would make every run crash at exit. When no cookies are
// configured it returns nil args and a no-op cleanup.
func YtDlpCookieArgsForRun() ([]string, func(), error) {
	ytDlpCookiesMu.RLock()
	master := ytDlpCookiesFilePath
	ytDlpCookiesMu.RUnlock()
	if master == "" {
		return nil, func() {}, nil
	}

	content, err := os.ReadFile(master)
	if err != nil {
		return nil, func() {}, fmt.Errorf("failed to read yt-dlp cookies file: %w", err)
	}

	tmp, err := os.CreateTemp("", "yt-dlp-cookies-run-*.txt")
	if err != nil {
		return nil, func() {}, fmt.Errorf("failed to create per-run yt-dlp cookies file: %w", err)
	}
	path := tmp.Name()
	cleanup := func() { _ = os.Remove(path) }

	_, writeErr := tmp.Write(content)
	closeErr := tmp.Close()
	if writeErr == nil {
		writeErr = closeErr
	}
	if writeErr != nil {
		cleanup()
		return nil, func() {}, fmt.Errorf("failed to write per-run yt-dlp cookies file: %w", writeErr)
	}

	return []string{"--cookies", path}, cleanup, nil
}
