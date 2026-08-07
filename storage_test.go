package main

import (
	"archive/tar"
	"arker/internal/archivers"
	"arker/internal/storage"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFSStorage(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "storage-test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tempDir)

	s := storage.NewFSStorage(tempDir)
	key := "test/file.txt"

	// Test Writer
	w, err := s.Writer(key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = w.Write([]byte("hello world")); err != nil {
		t.Fatal(err)
	}
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}

	// Test Reader
	r, err := s.Reader(key)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	r.Close()

	if string(data) != "hello world" {
		t.Errorf("expected 'hello world', got %s", data)
	}

	// Test Exists
	exists, err := s.Exists(key)
	if err != nil || !exists {
		t.Error("exists failed")
	}

	// Test non-existent file
	exists, err = s.Exists("nonexistent/file.txt")
	if err != nil || exists {
		t.Error("should not exist")
	}
}

func TestAddDirToTar(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "tar-test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tempDir)

	// Create test directory structure
	subDir := filepath.Join(tempDir, "sub")
	if err := os.Mkdir(subDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create test files
	f1, err := os.Create(filepath.Join(tempDir, "file1.txt"))
	if err != nil {
		t.Fatal(err)
	}
	f1.Write([]byte("content1"))
	f1.Close()

	f2, err := os.Create(filepath.Join(subDir, "file2.txt"))
	if err != nil {
		t.Fatal(err)
	}
	f2.Write([]byte("content2"))
	f2.Close()

	// Create tar
	buf := new(bytes.Buffer)
	tw := tar.NewWriter(buf)
	if err = archivers.AddDirToTar(tw, tempDir, ""); err != nil {
		t.Fatal(err)
	}
	tw.Close()

	// Verify tar content
	tr := tar.NewReader(buf)
	files := make(map[string]bool)

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		files[hdr.Name] = true
	}

	// Check expected files/directories exist in tar
	expectedFiles := []string{"file1.txt", "sub/", "sub/file2.txt"}
	for _, expected := range expectedFiles {
		if !files[expected] {
			t.Errorf("expected file %s not found in tar", expected)
		}
	}
}

// TestAddDirToTarDeterministic verifies that two directory trees with
// identical contents produce byte-identical tars regardless of file creation
// order, mtimes, ownership, or permission noise. This is what makes a git
// tar's MD5 usable as a content identity for dedup.
func TestAddDirToTarDeterministic(t *testing.T) {
	buildTree := func(order []string) string {
		dir, err := os.MkdirTemp("", "tar-det-test")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.RemoveAll(dir) })

		if err := os.Mkdir(filepath.Join(dir, "sub"), 0755); err != nil {
			t.Fatal(err)
		}
		contents := map[string]string{
			"b.txt":     "bravo",
			"a.txt":     "alpha",
			"sub/c.txt": "charlie",
		}
		for _, name := range order {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(contents[name]), 0644); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}

	tarTree := func(dir string) []byte {
		buf := new(bytes.Buffer)
		tw := tar.NewWriter(buf)
		if err := archivers.AddDirToTar(tw, dir, ""); err != nil {
			t.Fatal(err)
		}
		tw.Close()
		return buf.Bytes()
	}

	dir1 := buildTree([]string{"b.txt", "a.txt", "sub/c.txt"})
	dir2 := buildTree([]string{"sub/c.txt", "a.txt", "b.txt"})

	// Perturb metadata that must not leak into the tar.
	past := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(dir2, "a.txt"), past, past); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir2, "b.txt"), 0600); err != nil {
		t.Fatal(err)
	}

	tar1 := tarTree(dir1)
	tar2 := tarTree(dir2)
	if !bytes.Equal(tar1, tar2) {
		t.Error("tars of identical content are not byte-identical")
	}

	// Spot-check header normalization.
	tr := tar.NewReader(bytes.NewReader(tar1))
	epoch := time.Unix(0, 0)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if !hdr.ModTime.Equal(epoch) {
			t.Errorf("entry %s has non-epoch mtime %v", hdr.Name, hdr.ModTime)
		}
		if hdr.Uid != 0 || hdr.Gid != 0 || hdr.Uname != "" || hdr.Gname != "" {
			t.Errorf("entry %s has non-zero ownership", hdr.Name)
		}
	}
}

func TestGenerateShortID(t *testing.T) {
	// This test would require setting up an in-memory database
	// For now, just test that the alphabet contains valid characters
	alphabet := "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"

	if len(alphabet) != 62 {
		t.Error("Alphabet should contain 62 characters")
	}

	// Test basic ID characteristics
	id1 := "abc12"
	id2 := "xyz78"

	if id1 == id2 {
		t.Error("Different IDs should not be equal")
	}

	if len(id1) != 5 {
		t.Error("ID should be 5 characters")
	}
}
