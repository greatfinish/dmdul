package dm

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
)

type dmLOBValue struct {
	reader  *dmLOBReader
	locator dmLOBLocator
	kind    uint32
	text    bool
	decoder textDecoder
}

type dmLOBReader struct {
	cache *dataFilePageCache
}

type dmLOBLocator struct {
	raw       []byte
	lobID     uint32
	byteLen   uint32
	groupID   uint32
	firstPage uint32
}

type dmLOBChainReader struct {
	owner       *dmLOBReader
	locator     dmLOBLocator
	kind        uint32
	current     dataPageRef
	hasCurrent  bool
	remaining   uint64
	payload     []byte
	payloadPos  int
	seen        map[dataPageRef]bool
	steps       int
	maxSteps    int
	terminalErr error
}

func readInlineTextLOB(row []byte, pos int, decoder textDecoder, lobReader *dmLOBReader) (any, int, error) {
	raw, next, marker, err := readShortDataBytesWithMarker(row, pos)
	if err != nil {
		return nil, pos, fmt.Errorf("%s", strings.Replace(err.Error(), "raw value", "text LOB", 1))
	}
	if payload, ok := unwrapInlineLOBPayload(raw); ok {
		raw = payload
	} else if locator, ok := parseDMLOBLocatorRaw(raw); ok {
		if lobReader == nil {
			return nil, pos, fmt.Errorf("out-of-line text LOB locator cannot be resolved without data files")
		}
		if value, lazyErr := lobReader.lazyLOBValue(locator, dmPageKindLOBData, true, decoder); lazyErr == nil {
			return value, next, nil
		}
		raw, err = lobReader.readLongRowPayload(locator)
		if err != nil {
			return nil, pos, err
		}
	}
	value, ok := decoder.decode(raw)
	if !ok {
		return nil, pos, fmt.Errorf("cannot decode text LOB marker=0x%02X pos=%d raw=%s", marker, pos, strings.ToUpper(hex.EncodeToString(raw)))
	}
	if strings.ContainsRune(value, '\uFFFD') || containsBadControl(value) {
		return nil, pos, fmt.Errorf("decoded text LOB contains invalid characters marker=0x%02X pos=%d raw=%s", marker, pos, strings.ToUpper(hex.EncodeToString(raw)))
	}
	return value, next, nil
}

func decodeLOBTextValue(column string, raw []byte, decoder textDecoder) (string, error) {
	value, ok := decoder.decode(raw)
	if !ok || strings.ContainsRune(value, '\uFFFD') || containsBadControl(value) {
		return "", fmt.Errorf("%s: cannot decode out-of-line text LOB", column)
	}
	return value, nil
}

func unwrapInlineLOBPayload(raw []byte) ([]byte, bool) {
	if len(raw) < 13 {
		return nil, false
	}
	// DM9 uses subtype 0x04 for Unicode databases and 0x03 for
	// GB18030/EUC-KR inline text LOB envelopes. The remaining header and
	// payload-length fields are identical.
	if raw[0] != 0x01 || (raw[2] != 0x03 && raw[2] != 0x04) {
		return nil, false
	}
	payloadLen := int(binary.LittleEndian.Uint32(raw[9:13]))
	if payloadLen < 0 || payloadLen != len(raw)-13 {
		return nil, false
	}
	return append([]byte(nil), raw[13:]...), true
}

func readDMLOBLocator(row []byte, pos int) (dmLOBLocator, int, error) {
	if pos < 0 || pos+dmLOBLocatorSize > len(row) {
		return dmLOBLocator{}, pos, fmt.Errorf("LOB locator out of range")
	}
	raw := append([]byte(nil), row[pos:pos+dmLOBLocatorSize]...)
	locator, ok := parseDMLOBLocatorRaw(raw)
	if !ok {
		return dmLOBLocator{}, pos, fmt.Errorf("invalid LOB locator %s", strings.ToUpper(hex.EncodeToString(raw)))
	}
	return locator, pos + dmLOBLocatorSize, nil
}

func parseDMLOBLocatorRaw(raw []byte) (dmLOBLocator, bool) {
	if len(raw) != dmLOBLocatorSize || raw[0] != 0x02 {
		return dmLOBLocator{}, false
	}
	locator := dmLOBLocator{
		raw:       append([]byte(nil), raw...),
		lobID:     binary.LittleEndian.Uint32(raw[1:5]),
		byteLen:   binary.LittleEndian.Uint32(raw[9:13]),
		groupID:   binary.LittleEndian.Uint32(raw[13:17]),
		firstPage: binary.LittleEndian.Uint32(raw[17:21]),
	}
	if locator.lobID == 0 || locator.groupID == 0 || locator.firstPage == 0 {
		return dmLOBLocator{}, false
	}
	return locator, true
}

func (r *dmLOBReader) lazyLOBValue(locator dmLOBLocator, kind uint32, text bool, decoder textDecoder) (dmLOBValue, error) {
	if r == nil || r.cache == nil {
		return dmLOBValue{}, fmt.Errorf("LOB reader is not available")
	}
	if _, ok := r.findFirstLOBPage(locator, kind); !ok {
		return dmLOBValue{}, fmt.Errorf("LOB page not found: lob_id=%d group=%d page=%d kind=0x%X", locator.lobID, locator.groupID, locator.firstPage, kind)
	}
	return dmLOBValue{reader: r, locator: locator, kind: kind, text: text, decoder: decoder}, nil
}

func (v dmLOBValue) open() (io.Reader, error) {
	if v.reader == nil {
		return nil, fmt.Errorf("LOB reader is not available")
	}
	return v.reader.openLOBPayload(v.locator, v.kind)
}

func (v dmLOBValue) readAll() ([]byte, error) {
	reader, err := v.open()
	if err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	return raw, nil
}

func (r *dmLOBReader) openLOBPayload(locator dmLOBLocator, kind uint32) (io.Reader, error) {
	if r == nil || r.cache == nil {
		return nil, fmt.Errorf("LOB reader is not available")
	}
	start, ok := r.findFirstLOBPage(locator, kind)
	if !ok {
		return nil, fmt.Errorf("LOB page not found: lob_id=%d group=%d page=%d kind=0x%X", locator.lobID, locator.groupID, locator.firstPage, kind)
	}
	maxSteps := r.cache.totalPageCount() * maxLeafChainWalkMultiplier
	if maxSteps <= 0 {
		maxSteps = 1
	}
	return &dmLOBChainReader{
		owner: r, locator: locator, kind: kind, current: start, hasCurrent: true,
		remaining: uint64(locator.byteLen), seen: make(map[dataPageRef]bool), maxSteps: maxSteps,
	}, nil
}

func (r *dmLOBChainReader) Read(dst []byte) (int, error) {
	if len(dst) == 0 {
		return 0, nil
	}
	if r.terminalErr != nil {
		return 0, r.terminalErr
	}
	if r.remaining == 0 {
		return 0, io.EOF
	}
	written := 0
	for written < len(dst) && r.remaining > 0 {
		if r.payloadPos >= len(r.payload) {
			if err := r.loadNextPayload(); err != nil {
				if written > 0 {
					r.terminalErr = err
					return written, nil
				}
				return 0, err
			}
		}
		available := len(r.payload) - r.payloadPos
		if available <= 0 {
			continue
		}
		length := len(dst) - written
		if length > available {
			length = available
		}
		if uint64(length) > r.remaining {
			length = int(r.remaining)
		}
		copy(dst[written:written+length], r.payload[r.payloadPos:r.payloadPos+length])
		r.payloadPos += length
		r.remaining -= uint64(length)
		written += length
	}
	if written > 0 {
		return written, nil
	}
	return 0, io.EOF
}

func (r *dmLOBChainReader) loadNextPayload() error {
	if !r.hasCurrent || r.steps >= r.maxSteps {
		return fmt.Errorf("LOB payload incomplete: remaining=%d want=%d", r.remaining, r.locator.byteLen)
	}
	if r.seen[r.current] {
		return fmt.Errorf("LOB page chain cycle at group=%d file=%d page=%d", r.current.key.groupID, r.current.key.fileID, r.current.pageNo)
	}
	r.seen[r.current] = true
	r.steps++
	page, ok := r.owner.cache.readPage(r.current)
	if !ok || !pageHeaderMatchesRef(page, r.current) || dataPageKind(page) != r.kind || lobPageID(page) != r.locator.lobID {
		return fmt.Errorf("invalid LOB page at group=%d file=%d page=%d", r.current.key.groupID, r.current.key.fileID, r.current.pageNo)
	}
	payloadLen := int(lobPagePayloadLen(page))
	if payloadLen < 0 || dmLOBPagePayloadOff+payloadLen > len(page) {
		return fmt.Errorf("bad LOB payload length %d at page %d", payloadLen, r.current.pageNo)
	}
	if uint64(payloadLen) > r.remaining {
		payloadLen = int(r.remaining)
	}
	r.payload = page[dmLOBPagePayloadOff : dmLOBPagePayloadOff+payloadLen]
	r.payloadPos = 0
	nextFileID, nextPageNo, hasNext := readDMPageRef(page, dmPageNextRefOff)
	if hasNext {
		r.current = dataPageRef{
			key: dataFileKey{groupID: r.locator.groupID, fileID: nextFileID}, pageNo: nextPageNo,
		}
	} else {
		r.hasCurrent = false
	}
	if payloadLen == 0 && !r.hasCurrent && r.remaining > 0 {
		return fmt.Errorf("LOB payload incomplete: remaining=%d want=%d", r.remaining, r.locator.byteLen)
	}
	return nil
}

func (r *dmLOBReader) readLOBPayload(locator dmLOBLocator, kind uint32) ([]byte, error) {
	reader, err := r.openLOBPayload(locator, kind)
	if err != nil {
		return nil, err
	}
	out, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	if uint64(len(out)) != uint64(locator.byteLen) {
		return nil, fmt.Errorf("LOB payload incomplete: got=%d want=%d", len(out), locator.byteLen)
	}
	return out, nil
}

func (r *dmLOBReader) readTextLOBOrLongRowPayload(locator dmLOBLocator) ([]byte, error) {
	payload, err := r.readLOBPayload(locator, dmPageKindLOBData)
	if err == nil {
		return payload, nil
	}
	longPayload, longErr := r.readLongRowPayload(locator)
	if longErr == nil {
		return longPayload, nil
	}
	return nil, err
}

func (r *dmLOBReader) readLongRowPayload(locator dmLOBLocator) ([]byte, error) {
	if r == nil || r.cache == nil {
		return nil, fmt.Errorf("LOB reader is not available")
	}
	start, ok := r.findFirstLOBPage(locator, dmPageKindLongRowData)
	if !ok {
		return nil, fmt.Errorf("long-row page not found: lob_id=%d group=%d page=%d", locator.lobID, locator.groupID, locator.firstPage)
	}
	current := start
	seen := make(map[dataPageRef]bool)
	maxSteps := r.cache.totalPageCount() * maxLeafChainWalkMultiplier
	if maxSteps <= 0 {
		maxSteps = 1
	}
	for steps := 0; steps < maxSteps; steps++ {
		if seen[current] {
			break
		}
		seen[current] = true
		page, ok := r.cache.readPage(current)
		if !ok || !pageHeaderMatchesRef(page, current) || dataPageKind(page) != dmPageKindLongRowData {
			break
		}
		if payload, ok := longRowPayloadFromPage(page, locator); ok {
			return payload, nil
		}
		nextFileID, nextPageNo, ok := readDMPageRef(page, dmPageNextRefOff)
		if !ok {
			break
		}
		current = dataPageRef{
			key: dataFileKey{
				groupID: locator.groupID,
				fileID:  nextFileID,
			},
			pageNo: nextPageNo,
		}
	}
	return nil, fmt.Errorf("long-row payload not found: lob_id=%d", locator.lobID)
}

func (r *dmLOBReader) findFirstLOBPage(locator dmLOBLocator, kind uint32) (dataPageRef, bool) {
	if r == nil || r.cache == nil {
		return dataPageRef{}, false
	}
	for key := range r.cache.refs {
		if key.groupID != locator.groupID {
			continue
		}
		ref := dataPageRef{key: key, pageNo: locator.firstPage}
		page, ok := r.cache.readPage(ref)
		if !ok || !pageHeaderMatchesRef(page, ref) || dataPageKind(page) != kind {
			continue
		}
		if kind == dmPageKindLOBData && lobPageID(page) != locator.lobID {
			continue
		}
		if kind == dmPageKindLongRowData {
			if _, ok := longRowPayloadFromPage(page, locator); !ok {
				continue
			}
		}
		return ref, true
	}
	return dataPageRef{}, false
}

func lobPageID(page []byte) uint32 {
	if len(page) < dmLOBPageIDOff+4 {
		return 0
	}
	return binary.LittleEndian.Uint32(page[dmLOBPageIDOff:])
}

func lobPagePayloadLen(page []byte) uint16 {
	if len(page) < dmLOBPagePayloadLenOff+2 {
		return 0
	}
	return binary.LittleEndian.Uint16(page[dmLOBPagePayloadLenOff:])
}

func longRowPayloadFromPage(page []byte, locator dmLOBLocator) ([]byte, bool) {
	for off := dmLOBPagePayloadOff; off+0x0E <= len(page); off++ {
		recordLen := int(binary.BigEndian.Uint16(page[off:]))
		if recordLen < 0x0E || off+recordLen > len(page) {
			continue
		}
		if binary.LittleEndian.Uint32(page[off+0x02:off+0x06]) != locator.lobID {
			continue
		}
		payloadLen1 := int(binary.LittleEndian.Uint16(page[off+0x0A:]))
		payloadLen2 := int(binary.LittleEndian.Uint16(page[off+0x0C:]))
		payloadLen := payloadLen1
		if payloadLen2 > 0 && payloadLen2 < payloadLen {
			payloadLen = payloadLen2
		}
		if payloadLen <= 0 || payloadLen > int(locator.byteLen) || off+0x0E+payloadLen > off+recordLen {
			continue
		}
		return append([]byte(nil), page[off+0x0E:off+0x0E+payloadLen]...), true
	}
	return nil, false
}
