package archivers

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestYtDlpDownloadArgsWriteRemuxedMP4File(t *testing.T) {
	outputTemplate := "/tmp/arker-video.%(ext)s"
	args := ytDlpDownloadArgs(outputTemplate)

	if hasArgPair(args, "-o", "-") {
		t.Fatal("yt-dlp download args write to stdout")
	}
	if !hasArgPair(args, "-o", outputTemplate) {
		t.Fatalf("yt-dlp download args do not include output template %q: %v", outputTemplate, args)
	}
	if !hasArgPair(args, "--merge-output-format", "mp4") {
		t.Fatalf("yt-dlp download args do not force MP4 merge output: %v", args)
	}
	if !hasArgPair(args, "--remux-video", "mp4") {
		t.Fatalf("yt-dlp download args do not remux final video to MP4: %v", args)
	}
}

func TestFindDownloadedMP4PrefersFinalOutput(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "video")

	intermediatePath := base + ".fdash-123.mp4"
	finalPath := base + ".mp4"
	if err := os.WriteFile(intermediatePath, []byte("intermediate"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(finalPath, []byte("final"), 0644); err != nil {
		t.Fatal(err)
	}

	got, err := findDownloadedMP4(base)
	if err != nil {
		t.Fatalf("findDownloadedMP4 returned error: %v", err)
	}
	if got != finalPath {
		t.Fatalf("findDownloadedMP4 = %q, want %q", got, finalPath)
	}
}

func TestTempVideoReaderRemovesFileOnClose(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "video.mp4")
	if err := os.WriteFile(path, []byte("video"), 0644); err != nil {
		t.Fatal(err)
	}

	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}

	reader := &tempVideoReader{File: file, path: path}
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "video" {
		t.Fatalf("tempVideoReader read %q, want video", data)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("tempVideoReader.Close returned error: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("tempVideoReader.Close did not remove file, stat err = %v", err)
	}
}

func TestCleanupTempVideoFilesExceptKeepsFinalOutput(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "video")
	keepPath := base + ".mp4"
	removePath := base + ".fdash-123.mp4"

	if err := os.WriteFile(keepPath, []byte("keep"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(removePath, []byte("remove"), 0644); err != nil {
		t.Fatal(err)
	}

	cleanupTempVideoFilesExcept(base, keepPath)

	if _, err := os.Stat(keepPath); err != nil {
		t.Fatalf("cleanupTempVideoFilesExcept removed kept file: %v", err)
	}
	if _, err := os.Stat(removePath); !os.IsNotExist(err) {
		t.Fatalf("cleanupTempVideoFilesExcept did not remove intermediate file, stat err = %v", err)
	}
}

func hasArgPair(args []string, key, value string) bool {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == key && args[i+1] == value {
			return true
		}
	}
	return false
}
