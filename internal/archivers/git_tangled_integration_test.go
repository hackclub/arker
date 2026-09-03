package archivers

import (
	"archive/tar"
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// TestTangledGitArchiveLive exercises Tangled's real smart-HTTP redirects and
// a browser URL below the repository root. It is opt-in because it needs the
// public network; run it before shipping changes to Tangled support.
func TestTangledGitArchiveLive(t *testing.T) {
	if os.Getenv("ARKER_TANGLED_LIVE") == "" {
		t.Skip("set ARKER_TANGLED_LIVE=1 to exercise Tangled")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	result, err := (&GitArchiver{}).Archive(ctx,
		"https://tangled.org/@dunkirk.sh/akami/commit/45f6aaecfe822236cca2982fae4b0e4e8c501380",
		io.Discard, nil, 0)
	if err != nil {
		t.Fatalf("archive Tangled repository: %v", err)
	}

	if result.Extension != ".tar" || result.ContentType != "application/x-tar" {
		t.Fatalf("artifact = (%q, %q), want (.tar, application/x-tar)", result.Extension, result.ContentType)
	}

	tr := tar.NewReader(result.Data)
	foundHead := false
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read archive: %v", err)
		}
		if strings.TrimPrefix(header.Name, "./") == "HEAD" {
			foundHead = true
		}
	}
	if !foundHead {
		t.Fatal("Tangled archive did not contain the bare repository HEAD")
	}
}
