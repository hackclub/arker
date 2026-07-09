package utils

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
)

func resetYtDlpCookies(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		if _, err := InitYtDlpCookies("", "", t.TempDir()); err != nil {
			t.Fatalf("failed to reset yt-dlp cookies: %v", err)
		}
	})
}

func cookieArgsPath(t *testing.T, args []string) string {
	t.Helper()
	if len(args) != 2 || args[0] != "--cookies" {
		t.Fatalf("cookie args = %v, want [--cookies <path>]", args)
	}
	return args[1]
}

func TestInitYtDlpCookiesUnconfigured(t *testing.T) {
	resetYtDlpCookies(t)

	path, err := InitYtDlpCookies("", "", t.TempDir())
	if err != nil {
		t.Fatalf("InitYtDlpCookies returned error: %v", err)
	}
	if path != "" {
		t.Fatalf("InitYtDlpCookies path = %q, want empty", path)
	}

	args, cleanup, err := YtDlpCookieArgsForRun()
	if err != nil {
		t.Fatalf("YtDlpCookieArgsForRun returned error: %v", err)
	}
	defer cleanup()
	if args != nil {
		t.Fatalf("YtDlpCookieArgsForRun args = %v, want nil", args)
	}
}

func TestYtDlpCookieArgsForRunCopiesFile(t *testing.T) {
	resetYtDlpCookies(t)

	content := "# Netscape HTTP Cookie File\n.instagram.com\tTRUE\t/\tTRUE\t0\tsessionid\tsecret\n"
	cookiesPath := filepath.Join(t.TempDir(), "cookies.txt")
	// Read-only master: yt-dlp must only ever see writable per-run copies.
	if err := os.WriteFile(cookiesPath, []byte(content), 0400); err != nil {
		t.Fatal(err)
	}

	if _, err := InitYtDlpCookies(cookiesPath, "", t.TempDir()); err != nil {
		t.Fatalf("InitYtDlpCookies returned error: %v", err)
	}

	args, cleanup, err := YtDlpCookieArgsForRun()
	if err != nil {
		t.Fatalf("YtDlpCookieArgsForRun returned error: %v", err)
	}
	runPath := cookieArgsPath(t, args)
	if runPath == cookiesPath {
		t.Fatal("YtDlpCookieArgsForRun handed out the master cookies file instead of a copy")
	}
	copied, err := os.ReadFile(runPath)
	if err != nil {
		t.Fatalf("read per-run cookies copy: %v", err)
	}
	if string(copied) != content {
		t.Fatalf("per-run cookies copy = %q, want %q", copied, content)
	}

	// Concurrent runs must not share a file.
	args2, cleanup2, err := YtDlpCookieArgsForRun()
	if err != nil {
		t.Fatalf("second YtDlpCookieArgsForRun returned error: %v", err)
	}
	if secondPath := cookieArgsPath(t, args2); secondPath == runPath {
		t.Fatalf("two runs shared the same cookies file %q", runPath)
	}
	cleanup2()

	cleanup()
	if _, err := os.Stat(runPath); !os.IsNotExist(err) {
		t.Fatalf("cleanup did not remove per-run cookies copy, stat err = %v", err)
	}
	if _, err := os.Stat(cookiesPath); err != nil {
		t.Fatalf("cleanup removed the master cookies file: %v", err)
	}
}

func TestInitYtDlpCookiesMissingFileErrors(t *testing.T) {
	resetYtDlpCookies(t)

	if _, err := InitYtDlpCookies(filepath.Join(t.TempDir(), "missing.txt"), "", t.TempDir()); err == nil {
		t.Fatal("InitYtDlpCookies did not error for missing cookies file")
	}
}

func TestInitYtDlpCookiesFromBase64(t *testing.T) {
	resetYtDlpCookies(t)

	content := "# Netscape HTTP Cookie File\n.instagram.com\tTRUE\t/\tTRUE\t0\tsessionid\tsecret\n"
	dir := t.TempDir()

	path, err := InitYtDlpCookies("", base64.StdEncoding.EncodeToString([]byte(content)), dir)
	if err != nil {
		t.Fatalf("InitYtDlpCookies returned error: %v", err)
	}
	if filepath.Dir(path) != dir {
		t.Fatalf("cookies written to %q, want directory %q", path, dir)
	}

	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(written) != content {
		t.Fatalf("cookies file content = %q, want %q", written, content)
	}

	args, cleanup, err := YtDlpCookieArgsForRun()
	if err != nil {
		t.Fatalf("YtDlpCookieArgsForRun returned error: %v", err)
	}
	defer cleanup()
	runPath := cookieArgsPath(t, args)
	copied, err := os.ReadFile(runPath)
	if err != nil {
		t.Fatalf("read per-run cookies copy: %v", err)
	}
	if string(copied) != content {
		t.Fatalf("per-run cookies copy = %q, want %q", copied, content)
	}
}

func TestInitYtDlpCookiesInvalidBase64Errors(t *testing.T) {
	resetYtDlpCookies(t)

	if _, err := InitYtDlpCookies("", "not-valid-base64!!!", t.TempDir()); err == nil {
		t.Fatal("InitYtDlpCookies did not error for invalid base64")
	}
}

func TestInitYtDlpProxy(t *testing.T) {
	t.Cleanup(func() { InitYtDlpProxy("") })

	if args := YtDlpProxyArgs(); args != nil {
		t.Fatalf("YtDlpProxyArgs with no proxy = %v, want nil", args)
	}

	InitYtDlpProxy("  http://user:pass@proxy.example.com:8080  ")
	args := YtDlpProxyArgs()
	if len(args) != 2 || args[0] != "--proxy" || args[1] != "http://user:pass@proxy.example.com:8080" {
		t.Fatalf("YtDlpProxyArgs = %v, want [--proxy http://user:pass@proxy.example.com:8080] (trimmed)", args)
	}

	InitYtDlpProxy("")
	if args := YtDlpProxyArgs(); args != nil {
		t.Fatalf("YtDlpProxyArgs after clearing = %v, want nil", args)
	}
}

func TestInitYtDlpImpersonate(t *testing.T) {
	t.Cleanup(func() { InitYtDlpImpersonate("") })

	if args := YtDlpImpersonateArgs(); args != nil {
		t.Fatalf("YtDlpImpersonateArgs with no target = %v, want nil", args)
	}

	InitYtDlpImpersonate("  chrome  ")
	args := YtDlpImpersonateArgs()
	if len(args) != 2 || args[0] != "--impersonate" || args[1] != "chrome" {
		t.Fatalf("YtDlpImpersonateArgs = %v, want [--impersonate chrome] (trimmed)", args)
	}

	if args := YtDlpImpersonateArgsForURL("https://www.instagram.com/reel/DaZWJMSzA7Q/"); len(args) != 2 || args[0] != "--impersonate" || args[1] != "chrome" {
		t.Fatalf("YtDlpImpersonateArgsForURL(instagram) = %v, want [--impersonate chrome]", args)
	}
	if args := YtDlpImpersonateArgsForURL("https://www.youtube.com/watch?v=dQw4w9WgXcQ"); args != nil {
		t.Fatalf("YtDlpImpersonateArgsForURL(youtube) = %v, want nil", args)
	}

	InitYtDlpImpersonate("")
	if args := YtDlpImpersonateArgs(); args != nil {
		t.Fatalf("YtDlpImpersonateArgs after clearing = %v, want nil", args)
	}
}

func TestInitYtDlpCookiesFileTakesPrecedenceOverBase64(t *testing.T) {
	resetYtDlpCookies(t)

	cookiesPath := filepath.Join(t.TempDir(), "cookies.txt")
	if err := os.WriteFile(cookiesPath, []byte("# from file\n"), 0600); err != nil {
		t.Fatal(err)
	}

	path, err := InitYtDlpCookies(cookiesPath, base64.StdEncoding.EncodeToString([]byte("# from b64\n")), t.TempDir())
	if err != nil {
		t.Fatalf("InitYtDlpCookies returned error: %v", err)
	}
	if path != cookiesPath {
		t.Fatalf("InitYtDlpCookies path = %q, want file path %q", path, cookiesPath)
	}
}
