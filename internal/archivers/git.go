package archivers

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"gorm.io/gorm"
	"github.com/go-git/go-git/v5"
)

// GitArchiver
type GitArchiver struct{}

func (a *GitArchiver) Archive(ctx context.Context, url string, logWriter io.Writer, db *gorm.DB, itemID uint) (io.Reader, string, string, func(), error) {
	fmt.Fprintf(logWriter, "Starting git archive for: %s\n", url)
	
	// Check context before starting
	select {
	case <-ctx.Done():
		return nil, "", "", nil, ctx.Err()
	default:
	}
	
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

	go func() {
		defer pw.Close()
		tw := tar.NewWriter(pw)
		defer tw.Close()
		
		fmt.Fprintf(logWriter, "Creating tar archive...\n")
		if err := AddDirToTar(tw, tempDir, ""); err != nil {
			fmt.Fprintf(logWriter, "Failed to create tar archive: %v\n", err)
			pw.CloseWithError(err)
			return
		}
		fmt.Fprintf(logWriter, "Git archive completed successfully\n")
	}()

	return pr, ".tar", "application/x-tar", cleanup, nil
}

// extractGitRepoURL extracts the repository URL from GitHub URLs with extra paths
func extractGitRepoURL(url string) string {
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
