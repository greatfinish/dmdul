package dm

import (
	"bytes"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/binary"
	"fmt"
	"hash"
	"hash/crc32"
	"strings"
)

const (
	dmPageChecksumOffset = 0x18
	dmPageChecksumSize   = 4
	dmPageCheckTailSize  = 8
)

func verifyDMPageCheck(page []byte, mode uint32, hashName string) (bool, error) {
	if len(page) < dmPageChecksumOffset+dmPageChecksumSize+dmPageCheckTailSize {
		return false, fmt.Errorf("page too short for PAGE_CHECK: %d", len(page))
	}
	switch mode {
	case 0:
		return true, nil
	case 1:
		return verifyDMPageCRC(page, crc32.IEEETable), nil
	case 2:
		return verifyDMPageHash(page, hashName)
	case 3:
		return verifyDMPageCRC(page, crc32.MakeTable(crc32.Castagnoli)), nil
	default:
		return false, fmt.Errorf("unsupported PAGE_CHECK mode %d", mode)
	}
}

func verifyDMPageCRC(page []byte, table *crc32.Table) bool {
	stored := binary.LittleEndian.Uint32(page[dmPageChecksumOffset:])
	h := crc32.New(table)
	_, _ = h.Write(page[:dmPageChecksumOffset])
	_, _ = h.Write([]byte{0, 0, 0, 0})
	_, _ = h.Write(page[dmPageChecksumOffset+dmPageChecksumSize : len(page)-dmPageCheckTailSize])
	return stored == h.Sum32()
}

func verifyDMPageHash(page []byte, hashName string) (bool, error) {
	h, canonicalName, err := newDMPageHash(hashName)
	if err != nil {
		return false, err
	}
	hashOffset := len(page) - h.Size() - dmPageCheckTailSize
	if hashOffset <= dmPageChecksumOffset+dmPageChecksumSize {
		return false, fmt.Errorf("page too short for PAGE_CHECK hash %s", canonicalName)
	}
	_, _ = h.Write(page[:hashOffset])
	return bytes.Equal(page[hashOffset:hashOffset+h.Size()], h.Sum(nil)), nil
}

func detectDMPageHash(page []byte) (string, int, bool) {
	if len(page) < dmPageChecksumOffset+dmPageChecksumSize+dmPageCheckTailSize ||
		binary.LittleEndian.Uint32(page[dmPageChecksumOffset:]) != 0 {
		return "", 0, false
	}
	// SHA256 is the most common configured page hash and is checked first.
	for _, name := range []string{"SHA256", "SHA1", "MD5", "SHA224", "SHA384", "SHA512"} {
		h, canonical, err := newDMPageHash(name)
		if err != nil {
			continue
		}
		offset := len(page) - h.Size() - dmPageCheckTailSize
		if offset <= dmPageChecksumOffset+dmPageChecksumSize {
			continue
		}
		_, _ = h.Write(page[:offset])
		if bytes.Equal(page[offset:offset+h.Size()], h.Sum(nil)) {
			return canonical, h.Size(), true
		}
	}
	return "", 0, false
}

func pageSlotTrailerLenForPage(page []byte) int {
	if _, digestSize, ok := detectDMPageHash(page); ok {
		return pageSlotTrailerLen + digestSize
	}
	if trailerLen, ok := inferDMPageProtectionTrailerLen(page); ok {
		return trailerLen
	}
	if digestSize, ok := inferDMPageHashSizeFromSlots(page); ok {
		return pageSlotTrailerLen + digestSize
	}
	return pageSlotTrailerLen
}

type dmPageSlotLayoutEvidence struct {
	score   int
	rows    int
	invalid int
	clean   bool
}

// inferDMPageProtectionTrailerLen distinguishes the sector-protection backup
// area from ordinary row bytes. On protected 32 KiB pages, for example, the
// slot directory ends 40 bytes before the page end: seven four-byte backups,
// four protection bytes, and the fixed eight-byte trailer. Treating only the
// final eight bytes as reserved shifts every slot by 32 bytes.
func inferDMPageProtectionTrailerLen(page []byte) (int, bool) {
	if len(page) < dataPageRecordCountOff+2 || len(page)%systemSectorSize != 0 {
		return 0, false
	}
	kind := dataPageKind(page)
	if kind != dmPageKindRowData && kind != dmPageKindRowOverflow && kind != dmPageKindBTreeRoot {
		return 0, false
	}
	nSlot := binary.LittleEndian.Uint16(page[dataPageSlotCountOff:])
	nRec := binary.LittleEndian.Uint16(page[dataPageRecordCountOff:])
	freeEnd := binary.LittleEndian.Uint16(page[dataPageFreeEndOff:])
	if nSlot == 0 || nSlot >= 2048 || nRec == 0 || nRec > nSlot || freeEnd < dataRowAreaStart || int(freeEnd) > len(page) {
		return 0, false
	}
	protectedLen := pageTailReservedLen(uint32(len(page)))
	if protectedLen <= pageSlotTrailerLen || protectedLen+int(nSlot)*2 > len(page)-dmMinSlotOffset {
		return 0, false
	}
	base := evaluateDMPageSlotLayout(page, pageSlotTrailerLen, nSlot, nRec, freeEnd)
	protected := evaluateDMPageSlotLayout(page, protectedLen, nSlot, nRec, freeEnd)
	if !protected.clean || protected.rows == 0 {
		return 0, false
	}
	baseDelta := absInt(base.rows - int(nRec))
	protectedDelta := absInt(protected.rows - int(nRec))
	if protected.rows == int(nRec) && (!base.clean || base.rows != int(nRec)) {
		return protectedLen, true
	}
	if protectedDelta <= baseDelta && protected.score >= base.score+32 {
		return protectedLen, true
	}
	return 0, false
}

func evaluateDMPageSlotLayout(page []byte, trailerLen int, nSlot, nRec, freeEnd uint16) dmPageSlotLayoutEvidence {
	start := len(page) - trailerLen - int(nSlot)*2
	if start < dmMinSlotOffset || start+int(nSlot)*2 > len(page) {
		return dmPageSlotLayoutEvidence{score: -1 << 20, invalid: int(nSlot)}
	}
	evidence := dmPageSlotLayoutEvidence{}
	seen := make(map[uint16]bool, nSlot)
	for slot := uint16(0); slot < nSlot; slot++ {
		off := binary.LittleEndian.Uint16(page[start+int(slot)*2:])
		switch {
		case off == 0x52 || off == 0x5A:
			evidence.score += 12
		case off == 0 || off == 0xFFFF:
			evidence.score += 3
		case off < dmMinSlotOffset || off >= freeEnd:
			evidence.invalid++
			evidence.score -= 24
		case seen[off]:
			evidence.invalid++
			evidence.score -= 16
		default:
			seen[off] = true
			if _, ok := decodeDataRowHeader(page, off, uint32(len(page)), freeEnd); ok {
				evidence.rows++
				evidence.score += 16
			} else {
				evidence.invalid++
				evidence.score -= 12
			}
		}
	}
	delta := absInt(evidence.rows - int(nRec))
	evidence.score -= delta * 8
	if delta == 0 {
		evidence.score += 32
	}
	evidence.clean = evidence.invalid == 0
	return evidence
}

func absInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

func inferDMPageHashSizeFromSlots(page []byte) (int, bool) {
	if len(page) < dataPageFreeEndOff+2 || binary.LittleEndian.Uint32(page[dmPageChecksumOffset:]) != 0 {
		return 0, false
	}
	nSlot := binary.LittleEndian.Uint16(page[dataPageSlotCountOff:])
	freeEnd := binary.LittleEndian.Uint16(page[dataPageFreeEndOff:])
	if nSlot == 0 || nSlot >= 2048 || int(freeEnd) < 0x40 || int(freeEnd) > len(page) {
		return 0, false
	}
	baseStart := len(page) - pageSlotTrailerLen - int(nSlot)*2
	baseScore := scoreDMPageSlotLayout(page, baseStart, nSlot, freeEnd)
	// A structurally sound fixed-tail slot array is authoritative. This guard
	// prevents PAGE_CHECK=0 pages from being mistaken for corrupt hash pages
	// merely because bytes before the real slot array happen to resemble offsets.
	if validDMPageSlotLayout(page, baseStart, nSlot, freeEnd) {
		return 0, false
	}
	bestSize, bestScore := 0, baseScore
	for _, digestSize := range []int{16, 20, 28, 32, 48, 64} {
		start := len(page) - pageSlotTrailerLen - digestSize - int(nSlot)*2
		if score := scoreDMPageSlotLayout(page, start, nSlot, freeEnd); score > bestScore {
			bestSize, bestScore = digestSize, score
		}
	}
	if bestSize == 0 || bestScore < baseScore+4 || bestScore < int(nSlot)*2 {
		return 0, false
	}
	return bestSize, true
}

func validDMPageSlotLayout(page []byte, start int, nSlot uint16, freeEnd uint16) bool {
	if start < 0x40 || start+int(nSlot)*2 > len(page) {
		return false
	}
	for slot := uint16(0); slot < nSlot; slot++ {
		off := binary.LittleEndian.Uint16(page[start+int(slot)*2:])
		if off != 0 && off != 0xFFFF && off != 0x52 && off != 0x5A && (off < 0x40 || off >= freeEnd) {
			return false
		}
	}
	return true
}

func scoreDMPageSlotLayout(page []byte, start int, nSlot uint16, freeEnd uint16) int {
	if start < 0x40 || start+int(nSlot)*2 > len(page) {
		return -1 << 20
	}
	score := 0
	for slot := uint16(0); slot < nSlot; slot++ {
		off := binary.LittleEndian.Uint16(page[start+int(slot)*2:])
		switch {
		case off == 0x52 || off == 0x5A:
			score += 4
		case off == 0 || off == 0xFFFF:
			score++
		case off >= 0x40 && off < freeEnd:
			score += 3
		default:
			score -= 2
		}
	}
	return score
}

func newDMPageHash(name string) (hash.Hash, string, error) {
	canonical := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(name), "-", ""))
	switch canonical {
	case "MD5":
		return md5.New(), "MD5", nil
	case "SHA1":
		return sha1.New(), "SHA1", nil
	case "SHA224":
		return sha256.New224(), "SHA224", nil
	case "SHA256":
		return sha256.New(), "SHA256", nil
	case "SHA384":
		return sha512.New384(), "SHA384", nil
	case "SHA512":
		return sha512.New(), "SHA512", nil
	default:
		return nil, "", fmt.Errorf("unsupported PAGE_CHECK hash %q", name)
	}
}
