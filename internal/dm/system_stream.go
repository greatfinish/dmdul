package dm

import (
	"errors"
	"fmt"
	"io"
	"os"
)

var errStopPageScan = errors.New("stop page scan")

const (
	systemStreamChunkTarget = 1024 * 1024
	rawRoutineWindowLimit   = 512 * 1024
)

// systemPageStream keeps SYSTEM.DBF scans bounded to a page (or a small raw
// source window) instead of retaining the complete data file in memory.
type systemPageStream struct {
	file       io.ReaderAt
	closeFile  func() error
	path       string
	fileSize   int64
	header     []byte
	pageSize   uint32
	pageCount  uint32
	extentSize uint32
	extentSrc  string
}

func openSystemPageStream(path string) (*systemPageStream, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open SYSTEM.DBF: %w", err)
	}
	stat, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("stat SYSTEM.DBF: %w", err)
	}
	stream, err := openSystemPageStreamReader(path, file, stat.Size())
	if err != nil {
		file.Close()
		return nil, err
	}
	stream.closeFile = file.Close
	return stream, nil
}

func openSystemPageStreamReader(path string, reader io.ReaderAt, fileSize int64) (*systemPageStream, error) {
	header, err := readSystemHeaderFromReader(reader, fileSize)
	if err != nil {
		return nil, fmt.Errorf("read SYSTEM.DBF header: %w", err)
	}
	pageSize, _, err := detectPageSizeFromReader(reader, fileSize, header)
	if err != nil {
		return nil, fmt.Errorf("cannot detect SYSTEM.DBF page size: %w", err)
	}
	pageCount, _ := detectSystemPageCount(header, fileSize, pageSize)
	extentSize, extentSrc := detectSystemExtentSize(header)
	return &systemPageStream{
		file:       reader,
		path:       path,
		fileSize:   fileSize,
		header:     header,
		pageSize:   pageSize,
		pageCount:  pageCount,
		extentSize: extentSize,
		extentSrc:  extentSrc,
	}, nil
}

func (s *systemPageStream) close() {
	if s != nil && s.closeFile != nil {
		_ = s.closeFile()
		s.closeFile = nil
	}
}

func (s *systemPageStream) forEachPage(visit func(page []byte, pageNo uint32)) error {
	if s == nil || s.file == nil || s.pageSize == 0 {
		return fmt.Errorf("SYSTEM.DBF stream is not open")
	}
	page := make([]byte, int(s.pageSize))
	for pageNo := uint32(0); pageNo < s.pageCount; pageNo++ {
		n, err := s.file.ReadAt(page, int64(pageNo)*int64(s.pageSize))
		if err != nil && err != io.EOF {
			return fmt.Errorf("read SYSTEM.DBF page %d: %w", pageNo, err)
		}
		if n != len(page) {
			return fmt.Errorf("read SYSTEM.DBF page %d: short read %d/%d", pageNo, n, len(page))
		}
		restorePageProtectionBytes(page, s.pageSize)
		visit(page, pageNo)
	}
	return nil
}

func (s *systemPageStream) readPage(pageNo uint32) ([]byte, error) {
	if s == nil || s.file == nil || s.pageSize == 0 || pageNo >= s.pageCount {
		return nil, fmt.Errorf("SYSTEM.DBF page %d is out of range", pageNo)
	}
	page := make([]byte, int(s.pageSize))
	n, err := s.file.ReadAt(page, int64(pageNo)*int64(s.pageSize))
	if err != nil && err != io.EOF {
		return nil, fmt.Errorf("read SYSTEM.DBF page %d: %w", pageNo, err)
	}
	if n != len(page) {
		return nil, fmt.Errorf("read SYSTEM.DBF page %d: short read %d/%d", pageNo, n, len(page))
	}
	restorePageProtectionBytes(page, s.pageSize)
	return page, nil
}

func (s *systemPageStream) forEachDictionaryRow(visit func(page []byte, pageNo uint32, slotNo uint16, slotOff uint16)) error {
	return s.forEachPage(func(page []byte, pageNo uint32) {
		iterDictionaryRowsInPage(page, s.pageSize, pageNo, visit)
	})
}

func (s *systemPageStream) forEachDictionarySlotRange(visit func(page []byte, pageNo uint32, slotNo uint16, slotOff uint16, nextOff uint16)) error {
	return s.forEachPage(func(page []byte, pageNo uint32) {
		iterDictionarySlotRangesInPage(page, s.pageSize, pageNo, visit)
	})
}

func (s *systemPageStream) charset() (systemCharset, bool) {
	return detectSystemCharsetFromReader(s.file, s.pageSize)
}

func (s *systemPageStream) caseSensitive() (bool, bool) {
	return detectSystemCaseSensitiveFromReader(s.file, s.pageSize)
}

func (s *systemPageStream) rawRoutineTexts(decoder textDecoder) (map[string]string, error) {
	return s.scanRawWindows(decoder, scanRawRoutineTexts)
}

func (s *systemPageStream) rawTriggerTexts(decoder textDecoder) (map[string]string, error) {
	return s.scanRawWindows(decoder, scanRawTriggerTexts)
}

func (s *systemPageStream) scanRawWindows(decoder textDecoder, scan func([]byte, textDecoder) map[string]string) (map[string]string, error) {
	result := make(map[string]string)
	pagesPerChunk := systemStreamChunkTarget / int(s.pageSize)
	if pagesPerChunk < 1 {
		pagesPerChunk = 1
	}
	lookaheadPages := (rawRoutineWindowLimit + int(s.pageSize) - 1) / int(s.pageSize)
	for firstPage := uint32(0); firstPage < s.pageCount; firstPage += uint32(pagesPerChunk) {
		primaryPages := pagesPerChunk
		if remaining := int(s.pageCount - firstPage); primaryPages > remaining {
			primaryPages = remaining
		}
		readPages := primaryPages + lookaheadPages
		if remaining := int(s.pageCount - firstPage); readPages > remaining {
			readPages = remaining
		}
		window := make([]byte, readPages*int(s.pageSize))
		n, err := s.file.ReadAt(window, int64(firstPage)*int64(s.pageSize))
		if err != nil && err != io.EOF {
			return nil, fmt.Errorf("read SYSTEM.DBF raw source window at page %d: %w", firstPage, err)
		}
		if n != len(window) {
			return nil, fmt.Errorf("read SYSTEM.DBF raw source window at page %d: short read %d/%d", firstPage, n, len(window))
		}
		for offset := 0; offset < len(window); offset += int(s.pageSize) {
			restorePageProtectionBytes(window[offset:offset+int(s.pageSize)], s.pageSize)
		}
		for key, sql := range scan(window, decoder) {
			if len(sql) > len(result[key]) {
				result[key] = sql
			}
		}
	}
	return result, nil
}

func forEachDataFilePage(path string, pageSize uint32, visit func(page []byte, pageNo uint32) error) (int, error) {
	if pageSize == 0 {
		return 0, fmt.Errorf("invalid page size 0")
	}
	file, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return 0, err
	}
	return forEachReaderPage(file, info.Size(), pageSize, visit)
}

func forEachSizedReaderPage(reader SizedReaderAt, pageSize uint32, visit func(page []byte, pageNo uint32) error) (int, error) {
	if reader == nil {
		return 0, fmt.Errorf("data file reader is nil")
	}
	return forEachReaderPage(reader, reader.Size(), pageSize, visit)
}

func forEachReaderPage(reader io.ReaderAt, size int64, pageSize uint32, visit func(page []byte, pageNo uint32) error) (int, error) {
	return forEachRawReaderPage(reader, size, pageSize, func(page []byte, pageNo uint32) error {
		restoreUserDataPageProtectionBytes(page, pageSize)
		return visit(page, pageNo)
	})
}

// Checksum consumers must see the bytes on disk, before protection recovery.
func forEachRawReaderPage(reader io.ReaderAt, size int64, pageSize uint32, visit func(page []byte, pageNo uint32) error) (int, error) {
	if pageSize == 0 {
		return 0, fmt.Errorf("invalid page size 0")
	}
	pageCount := int(size / int64(pageSize))
	page := make([]byte, int(pageSize))
	for pageNo := 0; pageNo < pageCount; pageNo++ {
		n, readErr := reader.ReadAt(page, int64(pageNo)*int64(pageSize))
		if readErr != nil && readErr != io.EOF {
			return pageNo, readErr
		}
		if n != len(page) {
			return pageNo, fmt.Errorf("short read %d/%d", n, len(page))
		}
		if err := visit(page, uint32(pageNo)); err != nil {
			return pageNo + 1, err
		}
	}
	return pageCount, nil
}
