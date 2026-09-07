package dm

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
)

// ProbePageSize is shared by DBF discovery, dictionary loading and raw rescue.
// A valid header is checked for conflicting physical evidence. Damaged headers
// require at least three matching pages with a common group/file identity.
func ProbePageSize(reader io.ReaderAt, size int64) (uint32, string, error) {
	if reader == nil {
		return 0, "unknown", fmt.Errorf("nil DBF reader")
	}
	header, err := readSystemHeaderFromReader(reader, size)
	if err != nil {
		return 0, "unknown", err
	}
	return detectPageSizeFromReader(reader, size, header)
}

func probeFilePageSize(path string) (uint32, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, "unknown", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return 0, "unknown", err
	}
	return ProbePageSize(f, st.Size())
}

func detectPageSizeFromReader(reader io.ReaderAt, size int64, header []byte) (uint32, string, error) {
	headerSize, headerSource := detectSystemPageSize(header, size)
	if reader == nil || size < 3*4096 {
		if headerSize != 0 {
			return headerSize, headerSource, nil
		}
		return 0, "unknown", fmt.Errorf("cannot determine page size: insufficient physical evidence")
	}
	type evidence struct{ pages, structured int }
	var winners []uint32
	var descriptions []string
	for _, pageSize := range []uint32{4096, 8192, 16384, 32768} {
		count := size / int64(pageSize)
		refs := make(map[int64]bool)
		for n := int64(1); n < count && n <= 128; n++ {
			refs[n] = true
		}
		for n := int64(1); n <= 32; n++ {
			p := n * (count - 1) / 32
			if p > 0 && p < count {
				refs[p] = true
			}
		}
		byFile := make(map[dataFileKey]evidence)
		page := make([]byte, pageSize)
		for n := range refs {
			if n > int64(^uint32(0)) {
				continue
			}
			got, err := reader.ReadAt(page, n*int64(pageSize))
			if got != len(page) || (err != nil && err != io.EOF) {
				continue
			}
			if binary.LittleEndian.Uint32(page[4:8]) != uint32(n) {
				continue
			}
			kind := dataPageKind(page)
			// Only observed allocated page classes contribute positive evidence.
			if kind == 0 || kind > 0x40 {
				continue
			}
			key := dataFileKey{groupID: uint32(binary.LittleEndian.Uint16(page)), fileID: int16(binary.LittleEndian.Uint16(page[2:]))}
			if key.groupID == 0xffff || key.fileID < 0 {
				continue
			}
			ev := byFile[key]
			ev.pages++
			structured := false
			if binary.LittleEndian.Uint32(page[dmPageChecksumOffset:]) != 0 {
				crc1, _ := verifyDMPageCheck(page, 1, "")
				crc3, _ := verifyDMPageCheck(page, 3, "")
				structured = crc1 || crc3
			} else if _, _, ok := detectDMPageHash(page); ok {
				structured = true
			}
			if !structured && (kind == dmPageKindRowData || kind == dmPageKindRowOverflow) {
				copyPage := append([]byte(nil), page...)
				restoreUserDataPageProtectionBytes(copyPage, pageSize)
				_, structured = checkRowPageStructure(copyPage, pageSize)
			}
			if structured {
				ev.structured++
			}
			byFile[key] = ev
		}
		for key, ev := range byFile {
			if ev.pages >= 3 && ev.structured > 0 {
				winners = append(winners, pageSize)
				descriptions = append(descriptions, fmt.Sprintf("%d bytes group=%d file=%d matching_pages=%d structured_pages=%d", pageSize, key.groupID, key.fileID, ev.pages, ev.structured))
				break
			}
		}
	}
	if headerSize != 0 {
		if len(winners) == 0 || (len(winners) == 1 && winners[0] == headerSize) {
			return headerSize, headerSource, nil
		}
		return 0, "unknown", fmt.Errorf("page size header=%d conflicts with physical candidates=%v; refusing contradictory geometry", headerSize, winners)
	}
	if len(winners) != 1 {
		return 0, "unknown", fmt.Errorf("cannot uniquely determine page size: candidates=%v; multi-page identity/structure evidence required", winners)
	}
	return winners[0], "multi-page probe: " + descriptions[0], nil
}
