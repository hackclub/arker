package storage

import (
	"bytes"
	"fmt"
	"io"
	"sync"
)

// MemoryStorage implements Storage interface using in-memory storage
// This is useful for testing and development
type MemoryStorage struct {
	data map[string][]byte
	mu   sync.RWMutex
}

// NewMemoryStorage creates a new in-memory storage instance
func NewMemoryStorage() *MemoryStorage {
	return &MemoryStorage{
		data: make(map[string][]byte),
	}
}

// Writer returns a writer for the given key
func (ms *MemoryStorage) Writer(key string) (io.WriteCloser, error) {
	return &memoryWriter{
		storage: ms,
		key:     key,
		buffer:  &bytes.Buffer{},
	}, nil
}

// Reader returns a reader for the given key
func (ms *MemoryStorage) Reader(key string) (io.ReadCloser, error) {
	ms.mu.RLock()
	defer ms.mu.RUnlock()

	data, exists := ms.data[key]
	if !exists {
		return nil, fmt.Errorf("key not found: %s", key)
	}

	return io.NopCloser(bytes.NewReader(data)), nil
}

// Size returns the size of the data for the given key
func (ms *MemoryStorage) Size(key string) (int64, error) {
	ms.mu.RLock()
	defer ms.mu.RUnlock()

	data, exists := ms.data[key]
	if !exists {
		return 0, fmt.Errorf("key not found: %s", key)
	}

	return int64(len(data)), nil
}

// Delete removes the data for the given key
func (ms *MemoryStorage) Delete(key string) error {
	ms.mu.Lock()
	defer ms.mu.Unlock()

	delete(ms.data, key)
	return nil
}

// Exists checks if a key exists in storage
func (ms *MemoryStorage) Exists(key string) (bool, error) {
	ms.mu.RLock()
	defer ms.mu.RUnlock()

	_, exists := ms.data[key]
	return exists, nil
}

// memoryWriter implements io.WriteCloser for in-memory storage
type memoryWriter struct {
	storage *MemoryStorage
	key     string
	buffer  *bytes.Buffer
	closed  bool
}

// Write writes data to the buffer
func (mw *memoryWriter) Write(p []byte) (n int, err error) {
	if mw.closed {
		return 0, fmt.Errorf("writer is closed")
	}
	return mw.buffer.Write(p)
}

// Close finalizes the write operation and stores the data
func (mw *memoryWriter) Close() error {
	if mw.closed {
		return nil
	}

	mw.storage.mu.Lock()
	defer mw.storage.mu.Unlock()

	mw.storage.data[mw.key] = mw.buffer.Bytes()
	mw.closed = true

	return nil
}
