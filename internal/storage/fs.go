package storage

import (
	"io"
	"os"
	"path/filepath"
)

// Storage interface (modular for future S3)
type Storage interface {
	Writer(key string) (io.WriteCloser, error)
	Reader(key string) (io.ReadCloser, error)
	Exists(key string) (bool, error)
	Size(key string) (int64, error)
}

// SeekableStorage extends Storage with seekable readers
type SeekableStorage interface {
	Storage
	SeekableReader(key string) (interface { io.ReadCloser; io.Seeker }, error)
}

// FSStorage impl
type FSStorage struct {
	baseDir string
}

func NewFSStorage(baseDir string) *FSStorage {
	return &FSStorage{baseDir: baseDir}
}

func (s *FSStorage) Writer(key string) (io.WriteCloser, error) {
	path := filepath.Join(s.baseDir, key)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, err
	}
	return os.Create(path)
}

func (s *FSStorage) Reader(key string) (io.ReadCloser, error) {
	path := filepath.Join(s.baseDir, key)
	return os.Open(path)
}

func (s *FSStorage) Exists(key string) (bool, error) {
	path := filepath.Join(s.baseDir, key)
	_, err := os.Stat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	return err == nil, err
}

func (s *FSStorage) Size(key string) (int64, error) {
	path := filepath.Join(s.baseDir, key)
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

func (s *FSStorage) SeekableReader(key string) (interface { io.ReadCloser; io.Seeker }, error) {
	path := filepath.Join(s.baseDir, key)
	return os.Open(path)
}
