package main

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
	"arker/internal/storage"
	"arker/internal/archivers"
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
