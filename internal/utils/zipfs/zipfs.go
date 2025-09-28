package zipfs

import (
	"encoding/binary"
	"fmt"
	"io"
	"strings"
)

// Archive represents a ZIP archive for reading individual files
type Archive struct {
	reader io.ReaderAt
	size   int64
	files  map[string]*Entry
}

// Entry represents a file in the ZIP archive
type Entry struct {
	Name             string
	CompressedSize   uint64
	UncompressedSize uint64
	CRC32            uint32
	Method           uint16
	HeaderOffset     uint64
	DataStart        uint64
}

// OpenArchive opens a ZIP archive from a ReaderAt
func OpenArchive(reader io.ReaderAt, size int64) (*Archive, error) {
	archive := &Archive{
		reader: reader,
		size:   size,
		files:  make(map[string]*Entry),
	}

	if err := archive.readCentralDirectory(); err != nil {
		return nil, fmt.Errorf("failed to read central directory: %w", err)
	}

	return archive, nil
}

// Lookup finds an entry by name
func (a *Archive) Lookup(name string) (*Entry, bool) {
	// Normalize path separators
	name = strings.ReplaceAll(name, "\\", "/")
	name = strings.TrimPrefix(name, "/")
	
	entry, ok := a.files[name]
	return entry, ok
}

// Files returns all file entries
func (a *Archive) Files() map[string]*Entry {
	return a.files
}

// DataRange returns the byte range for this entry's data
func (e *Entry) DataRange() (start, end int64) {
	return int64(e.DataStart), int64(e.DataStart + e.CompressedSize)
}

// Open returns a reader for this entry's data
func (e *Entry) Open(archive *Archive) (io.Reader, error) {
	if e.Method != 0 { // Only support Store method
		return nil, fmt.Errorf("unsupported compression method: %d", e.Method)
	}
	
	return io.NewSectionReader(archive.reader, int64(e.DataStart), int64(e.CompressedSize)), nil
}

// readCentralDirectory reads and parses the ZIP central directory
func (a *Archive) readCentralDirectory() error {
	// Find End of Central Directory record
	eocdOffset, err := a.findEOCD()
	if err != nil {
		return err
	}

	// Read EOCD
	eocdData := make([]byte, 22)
	if _, err := a.reader.ReadAt(eocdData, eocdOffset); err != nil {
		return fmt.Errorf("failed to read EOCD: %w", err)
	}

	// Parse EOCD
	numEntries := binary.LittleEndian.Uint16(eocdData[10:12])
	centralDirSize := binary.LittleEndian.Uint32(eocdData[12:16])
	centralDirOffset := binary.LittleEndian.Uint32(eocdData[16:20])

	// Read central directory
	centralDirData := make([]byte, centralDirSize)
	if _, err := a.reader.ReadAt(centralDirData, int64(centralDirOffset)); err != nil {
		return fmt.Errorf("failed to read central directory: %w", err)
	}

	// Parse central directory entries
	offset := 0
	for i := 0; i < int(numEntries); i++ {
		entry, nextOffset, err := a.parseCentralDirEntry(centralDirData[offset:])
		if err != nil {
			return fmt.Errorf("failed to parse central directory entry %d: %w", i, err)
		}

		// Calculate data start by reading local file header
		if err := a.calculateDataStart(entry); err != nil {
			return fmt.Errorf("failed to calculate data start for %s: %w", entry.Name, err)
		}

		a.files[entry.Name] = entry
		offset += nextOffset
		

	}

	return nil
}

// findEOCD finds the End of Central Directory record
func (a *Archive) findEOCD() (int64, error) {
	// EOCD signature is 0x06054b50
	eocdSignature := []byte{0x50, 0x4b, 0x05, 0x06}
	
	// Search backwards from end of file (up to 64KB)
	searchSize := int64(65536)
	if searchSize > a.size {
		searchSize = a.size
	}

	searchStart := a.size - searchSize
	searchData := make([]byte, searchSize)
	
	if _, err := a.reader.ReadAt(searchData, searchStart); err != nil {
		return 0, fmt.Errorf("failed to read search area: %w", err)
	}

	// Search for EOCD signature
	for i := len(searchData) - 22; i >= 0; i-- {
		if len(searchData[i:]) >= 4 && 
		   searchData[i] == eocdSignature[0] &&
		   searchData[i+1] == eocdSignature[1] &&
		   searchData[i+2] == eocdSignature[2] &&
		   searchData[i+3] == eocdSignature[3] {
			return searchStart + int64(i), nil
		}
	}

	return 0, fmt.Errorf("EOCD record not found")
}

// parseCentralDirEntry parses a single central directory entry
func (a *Archive) parseCentralDirEntry(data []byte) (*Entry, int, error) {
	if len(data) < 46 {
		return nil, 0, fmt.Errorf("central directory entry too short")
	}

	// Check signature (0x02014b50)
	signature := binary.LittleEndian.Uint32(data[0:4])
	if signature != 0x02014b50 {
		return nil, 0, fmt.Errorf("invalid central directory entry signature: %x", signature)
	}

	entry := &Entry{
		Method:           binary.LittleEndian.Uint16(data[10:12]),
		CRC32:            binary.LittleEndian.Uint32(data[16:20]),
		CompressedSize:   uint64(binary.LittleEndian.Uint32(data[20:24])),
		UncompressedSize: uint64(binary.LittleEndian.Uint32(data[24:28])),
		HeaderOffset:     uint64(binary.LittleEndian.Uint32(data[42:46])),
	}

	fileNameLen := binary.LittleEndian.Uint16(data[28:30])
	extraFieldLen := binary.LittleEndian.Uint16(data[30:32])
	commentLen := binary.LittleEndian.Uint16(data[32:34])

	totalLen := 46 + int(fileNameLen) + int(extraFieldLen) + int(commentLen)
	if len(data) < totalLen {
		return nil, 0, fmt.Errorf("central directory entry data too short")
	}

	// Extract file name
	entry.Name = string(data[46 : 46+fileNameLen])

	return entry, totalLen, nil
}

// calculateDataStart calculates where the actual file data starts
func (a *Archive) calculateDataStart(entry *Entry) error {
	// Read local file header (30 bytes minimum)
	headerData := make([]byte, 30)
	if _, err := a.reader.ReadAt(headerData, int64(entry.HeaderOffset)); err != nil {
		return fmt.Errorf("failed to read local file header: %w", err)
	}

	// Check local file header signature (0x04034b50)
	signature := binary.LittleEndian.Uint32(headerData[0:4])
	if signature != 0x04034b50 {
		return fmt.Errorf("invalid local file header signature: %x", signature)
	}

	fileNameLen := binary.LittleEndian.Uint16(headerData[26:28])
	extraFieldLen := binary.LittleEndian.Uint16(headerData[28:30])

	// Data starts after: local header (30 bytes) + file name + extra field
	entry.DataStart = entry.HeaderOffset + 30 + uint64(fileNameLen) + uint64(extraFieldLen)

	return nil
}
