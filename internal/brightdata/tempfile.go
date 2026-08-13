package brightdata

import (
	"os"
)

// tempFileReader serves a temp file as an archiver Result and deletes it when
// the worker closes the reader, mirroring the yt-dlp archiver's temp handling.
type tempFileReader struct {
	*os.File
	path string
}

func (r *tempFileReader) Close() error {
	err1 := r.File.Close()
	err2 := os.Remove(r.path)
	if err1 != nil {
		return err1
	}
	return err2
}

func openTempFileReader(path string) (*tempFileReader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	return &tempFileReader{File: f, path: path}, nil
}

func createTempFile(pattern string) (*os.File, error) {
	return os.CreateTemp("", pattern)
}

func removeFile(path string) {
	if path != "" {
		_ = os.Remove(path)
	}
}

// renameTempFile moves a finished download into place, removing the temp file
// if the move fails so a failed download never leaks.
func renameTempFile(tmpPath, dest string) error {
	if err := os.Rename(tmpPath, dest); err != nil {
		removeFile(tmpPath)
		return err
	}
	return nil
}
