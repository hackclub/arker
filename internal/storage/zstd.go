package storage

import (
	"fmt"
	"io"

	seekable "github.com/SaveTheRbtz/zstd-seekable-format-go/pkg"
	"github.com/klauspost/compress/zstd"
)

// ZSTDStorage wraps another storage with seekable zstd compression
type ZSTDStorage struct {
	storage SeekableStorage
}

func NewZSTDStorage(storage SeekableStorage) *ZSTDStorage {
	return &ZSTDStorage{storage: storage}
}

func (z *ZSTDStorage) Writer(key string) (io.WriteCloser, error) {
	w, err := z.storage.Writer(key)
	if err != nil {
		return nil, err
	}

	// Create zstd encoder
	encoder, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		w.Close()
		return nil, err
	}

	// Create seekable writer
	seekableWriter, err := seekable.NewWriter(w, encoder)
	if err != nil {
		w.Close()
		encoder.Close()
		return nil, err
	}

	return &zstdWriteCloser{
		seekableWriter: seekableWriter,
		underlying:     w,
		encoder:        encoder,
	}, nil
}

func (z *ZSTDStorage) Reader(key string) (io.ReadCloser, error) {
	r, err := z.storage.SeekableReader(key)
	if err != nil {
		return nil, err
	}

	// Create zstd decoder
	decoder, err := zstd.NewReader(nil)
	if err != nil {
		r.Close()
		return nil, err
	}

	// Create seekable reader - need to cast to io.ReadSeeker
	readSeeker, ok := r.(io.ReadSeeker)
	if !ok {
		r.Close()
		return nil, err
	}
	
	seekableReader, err := seekable.NewReader(readSeeker, decoder)
	if err != nil {
		r.Close()
		return nil, err
	}

	return &zstdReadCloser{
		seekableReader: seekableReader,
		underlying:     r,
		decoder:        decoder,
	}, nil
}

func (z *ZSTDStorage) Exists(key string) (bool, error) {
	return z.storage.Exists(key)
}

func (z *ZSTDStorage) Size(key string) (int64, error) {
	return z.storage.Size(key)
}

// UncompressedSize returns the uncompressed size of a zstd file
func (z *ZSTDStorage) UncompressedSize(key string) (int64, error) {
	r, err := z.Reader(key)
	if err != nil {
		return 0, err
	}
	defer r.Close()

	// Cast to our zstdReadCloser which has the Size method
	if zstdReader, ok := r.(*zstdReadCloser); ok {
		return zstdReader.Size()
	}

	return 0, fmt.Errorf("reader does not support size operation")
}

// extractSeekTable extracts the seek table from a seekable zstd file
func (z *ZSTDStorage) extractSeekTable(r io.ReadSeeker) ([]byte, error) {
	// The seek table is stored as a ZSTD skippable frame at the end of the file
	// First, seek to the end to find the skippable frame
	fileSize, err := r.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, fmt.Errorf("failed to seek to end: %w", err)
	}

	// Read the last 8 bytes to get the skippable frame header
	// ZSTD skippable frame format: 4-byte magic + 4-byte size + data
	if fileSize < 8 {
		return nil, fmt.Errorf("file too small (%d bytes)", fileSize)
	}

	_, err = r.Seek(-8, io.SeekEnd)
	if err != nil {
		return nil, fmt.Errorf("failed to seek to header: %w", err)
	}

	var header [8]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, fmt.Errorf("failed to read header: %w", err)
	}

	// Check if this is a skippable frame (magic starts with 0x184D2A5?)
	// ZSTD skippable frame magic numbers range from 0x184D2A50 to 0x184D2A5F
	magic := uint32(header[0]) | uint32(header[1])<<8 | uint32(header[2])<<16 | uint32(header[3])<<24
	if (magic & 0xFFFFFFF0) != 0x184D2A50 {
		return nil, fmt.Errorf("invalid skippable frame magic: 0x%x", magic)
	}

	// Extract the size of the skippable frame data
	seekTableSize := uint32(header[4]) | uint32(header[5])<<8 | uint32(header[6])<<16 | uint32(header[7])<<24

	if seekTableSize == 0 || seekTableSize > 1024*1024 { // Sanity check: max 1MB seek table
		return nil, fmt.Errorf("invalid seek table size: %d", seekTableSize)
	}

	// Seek back to read the seek table data (skip the 8-byte header)
	_, err = r.Seek(-int64(seekTableSize+8), io.SeekEnd)
	if err != nil {
		return nil, fmt.Errorf("failed to seek to seek table: %w", err)
	}

	// Read the seek table
	seekTable := make([]byte, seekTableSize)
	if _, err := io.ReadFull(r, seekTable); err != nil {
		return nil, fmt.Errorf("failed to read seek table: %w", err)
	}

	return seekTable, nil
}

// SeekableReader extends the base Reader interface with seeking capabilities
type SeekableReader interface {
	io.ReadCloser
	io.Seeker
	io.ReaderAt
}

// SeekableReaderStorage interface for storages that support seekable readers
type SeekableReaderStorage interface {
	Storage
	SeekableReader(key string) (SeekableReader, error)
}

func (z *ZSTDStorage) SeekableReader(key string) (SeekableReader, error) {
	r, err := z.storage.SeekableReader(key)
	if err != nil {
		return nil, err
	}

	// Create zstd decoder
	decoder, err := zstd.NewReader(nil)
	if err != nil {
		r.Close()
		return nil, err
	}

	// Create seekable reader - need to cast to io.ReadSeeker
	readSeeker, ok := r.(io.ReadSeeker)
	if !ok {
		r.Close()
		return nil, err
	}
	
	seekableReader, err := seekable.NewReader(readSeeker, decoder)
	if err != nil {
		r.Close()
		return nil, err
	}

	return &zstdSeekableReader{
		seekableReader: seekableReader,
		underlying:     r,
		decoder:        decoder,
	}, nil
}

// zstdWriteCloser implements io.WriteCloser with seekable zstd compression
type zstdWriteCloser struct {
	seekableWriter seekable.Writer
	underlying     io.WriteCloser
	encoder        *zstd.Encoder
}

func (w *zstdWriteCloser) Write(p []byte) (n int, err error) {
	return w.seekableWriter.Write(p)
}

func (w *zstdWriteCloser) Close() error {
	// Close seekable writer first to flush and write seek table
	if err := w.seekableWriter.Close(); err != nil {
		w.underlying.Close()
		return err
	}

	// Close encoder
	w.encoder.Close()

	// Close underlying writer
	return w.underlying.Close()
}

// zstdReadCloser implements io.ReadCloser with seekable zstd decompression
type zstdReadCloser struct {
	seekableReader seekable.Reader
	underlying     io.ReadCloser
	decoder        *zstd.Decoder
}

func (r *zstdReadCloser) Read(p []byte) (n int, err error) {
	return r.seekableReader.Read(p)
}

func (r *zstdReadCloser) Close() error {
	// Close seekable reader
	if err := r.seekableReader.Close(); err != nil {
		r.underlying.Close()
		return err
	}

	// Close decoder
	r.decoder.Close()

	// Close underlying reader
	return r.underlying.Close()
}

// Size returns the uncompressed size by seeking to the end
func (r *zstdReadCloser) Size() (int64, error) {
	// Save current position
	currentPos, err := r.seekableReader.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, err
	}
	
	// Seek to end to get size
	size, err := r.seekableReader.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, err
	}
	
	// Restore original position
	_, err = r.seekableReader.Seek(currentPos, io.SeekStart)
	if err != nil {
		return 0, err
	}
	
	return size, nil
}

// regularZstdReadCloser implements io.ReadCloser for regular zstd files
type regularZstdReadCloser struct {
	reader     *zstd.Decoder
	underlying io.ReadCloser
}

func (r *regularZstdReadCloser) Read(p []byte) (n int, err error) {
	return r.reader.Read(p)
}

func (r *regularZstdReadCloser) Close() error {
	// Close decoder
	r.reader.Close()
	
	// Close underlying reader
	return r.underlying.Close()
}

// zstdSeekableReader implements SeekableReader with full seeking capabilities
type zstdSeekableReader struct {
	seekableReader seekable.Reader
	underlying     io.ReadCloser
	decoder        *zstd.Decoder
}

func (r *zstdSeekableReader) Read(p []byte) (n int, err error) {
	return r.seekableReader.Read(p)
}

func (r *zstdSeekableReader) Seek(offset int64, whence int) (int64, error) {
	return r.seekableReader.Seek(offset, whence)
}

func (r *zstdSeekableReader) ReadAt(p []byte, off int64) (n int, err error) {
	return r.seekableReader.ReadAt(p, off)
}

func (r *zstdSeekableReader) Close() error {
	// Close seekable reader
	if err := r.seekableReader.Close(); err != nil {
		r.underlying.Close()
		return err
	}

	// Close decoder
	r.decoder.Close()

	// Close underlying reader
	return r.underlying.Close()
}
