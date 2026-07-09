package archivers

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"net/http"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/transport/client"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"gorm.io/gorm"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"
)

// GitArchiver
type GitArchiver struct{}

var (
	gitHTTPClientOnce sync.Once
	gitHTTPClient     *http.Client
)

// getGitHTTPClient returns a pooled HTTP client configured for git operations
func getGitHTTPClient() *http.Client {
	gitHTTPClientOnce.Do(func() {
		transport := &http.Transport{
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 10,
			IdleConnTimeout:     90 * time.Second,
			DisableKeepAlives:   false,
		}

		gitHTTPClient = &http.Client{
			Transport: transport,
			Timeout:   5 * time.Minute, // Overall request timeout
		}
	})
	return gitHTTPClient
}

var installGitProtocolsOnce sync.Once

// installGitProtocols registers the pooled HTTP client for git http(s)
// operations exactly once. go-git's client.InstallProtocol writes a global,
// unsynchronized map, so calling it per-job races concurrent git jobs into a
// "concurrent map writes" crash. Registering once is equivalent (the client is
// process-global) and race-free.
func installGitProtocols() {
	installGitProtocolsOnce.Do(func() {
		httpClient := getGitHTTPClient()
		client.InstallProtocol("https", githttp.NewClient(httpClient))
		client.InstallProtocol("http", githttp.NewClient(httpClient))
	})
}

func (a *GitArchiver) Archive(ctx context.Context, url string, logWriter io.Writer, db *gorm.DB, itemID uint) (io.Reader, string, string, *PWBundle, error) {
	fmt.Fprintf(logWriter, "Starting git archive for: %s\n", url)

	// Check context before starting
	select {
	case <-ctx.Done():
		return nil, "", "", nil, ctx.Err()
	default:
	}

	// Register the pooled HTTP client for git operations exactly once.
	installGitProtocols()

	// Extract repository URL for GitHub URLs with extra paths
	repoURL := extractGitRepoURL(url)
	if repoURL != url {
		fmt.Fprintf(logWriter, "Extracted repository URL: %s\n", repoURL)
	}

	tempDir, err := os.MkdirTemp("", "git-archive-")
	if err != nil {
		fmt.Fprintf(logWriter, "Failed to create temp directory: %v\n", err)
		return nil, "", "", nil, err
	}
	cleanup := func() { os.RemoveAll(tempDir) }

	fmt.Fprintf(logWriter, "Cloning repository to: %s\n", tempDir)
	_, err = git.PlainCloneContext(ctx, tempDir, true, &git.CloneOptions{
		URL:      repoURL,
		Progress: logWriter,
	})
	if err != nil {
		fmt.Fprintf(logWriter, "Failed to clone repository: %v\n", err)
		cleanup()
		return nil, "", "", nil, err
	}
	fmt.Fprintf(logWriter, "Repository cloned successfully\n")

	pr, pw := io.Pipe()

	// Start context-aware tar creation in a goroutine
	go func() {
		defer pw.Close()
		defer cleanup() // remove the clone temp dir once tar bytes are fully written

		// Check context before starting tar creation
		select {
		case <-ctx.Done():
			fmt.Fprintf(logWriter, "Context cancelled during git tar creation\n")
			pw.CloseWithError(ctx.Err())
			return
		default:
		}

		tw := tar.NewWriter(pw)
		defer tw.Close()

		fmt.Fprintf(logWriter, "Creating tar archive...\n")

		// Use a channel to signal completion
		done := make(chan error, 1)
		go func() {
			done <- AddDirToTar(tw, tempDir, "")
		}()

		// Wait for either completion or context cancellation
		select {
		case <-ctx.Done():
			fmt.Fprintf(logWriter, "Context cancelled during git tar creation\n")
			pw.CloseWithError(ctx.Err())
		case err := <-done:
			if err != nil {
				fmt.Fprintf(logWriter, "Failed to create tar archive: %v\n", err)
				pw.CloseWithError(err)
			} else {
				fmt.Fprintf(logWriter, "Git archive completed successfully\n")
			}
		}
	}()

	return pr, ".tar", "application/x-tar", nil, nil
}

// extractGitRepoURL extracts the repository URL from GitHub URLs with extra paths and fragments
func extractGitRepoURL(url string) string {
	// First, strip any fragment (part after #)
	if fragmentIndex := regexp.MustCompile(`#.*$`).FindStringIndex(url); fragmentIndex != nil {
		url = url[:fragmentIndex[0]]
	}

	// GitHub repository URL pattern: https://github.com/owner/repo
	githubPattern := regexp.MustCompile(`^(https?://github\.com/[^/]+/[^/]+)(/.*)?$`)

	if matches := githubPattern.FindStringSubmatch(url); len(matches) > 1 {
		return matches[1] // Return just the repo part
	}

	// GitLab repository URL pattern: https://gitlab.com/owner/repo
	gitlabPattern := regexp.MustCompile(`^(https?://gitlab\.com/[^/]+/[^/]+)(/.*)?$`)

	if matches := gitlabPattern.FindStringSubmatch(url); len(matches) > 1 {
		return matches[1] // Return just the repo part
	}

	// For other URLs, return as-is
	return url
}

// Helper to tar dir streaming
func AddDirToTar(tw *tar.Writer, dir string, prefix string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()

	fis, err := f.Readdir(-1)
	if err != nil {
		return err
	}

	for _, fi := range fis {
		curPath := filepath.Join(dir, fi.Name())
		if fi.IsDir() {
			if err = tw.WriteHeader(&tar.Header{
				Name:     prefix + fi.Name() + "/",
				Size:     0,
				Mode:     int64(fi.Mode()),
				ModTime:  fi.ModTime(),
				Typeflag: tar.TypeDir,
			}); err != nil {
				return err
			}
			if err = AddDirToTar(tw, curPath, prefix+fi.Name()+"/"); err != nil {
				return err
			}
		} else {
			if err = tw.WriteHeader(&tar.Header{
				Name:     prefix + fi.Name(),
				Size:     fi.Size(),
				Mode:     int64(fi.Mode()),
				ModTime:  fi.ModTime(),
				Typeflag: tar.TypeReg,
			}); err != nil {
				return err
			}
			ff, err := os.Open(curPath)
			if err != nil {
				return err
			}
			if _, err = io.Copy(tw, ff); err != nil {
				ff.Close()
				return err
			}
			ff.Close()
		}
	}
	return nil
}
