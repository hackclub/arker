package storage

import (
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
