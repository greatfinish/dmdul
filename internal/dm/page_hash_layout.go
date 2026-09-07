package dm

import (
	"bytes"
	"hash"
)

type dmPageHashLayout struct {
	name       string
	digestSize int
	sectorHash bool
}

func (layout dmPageHashLayout) trailerLen(pageSize int) int {
	if layout.sectorHash {
		return pageSize/systemSectorSize*layout.digestSize + dmPageCheckTailSize
	}
	return layout.digestSize + dmPageCheckTailSize
}

func verifyDMWholePageHash(page []byte, h hash.Hash) bool {
	offset := len(page) - h.Size() - dmPageCheckTailSize
	if offset <= dmPageChecksumOffset+dmPageChecksumSize {
		return false
	}
	h.Reset()
	h.Write(page[:offset])
	return bytes.Equal(page[offset:offset+h.Size()], h.Sum(nil))
}

// DM9 HASH pages can protect each 4 KiB sector independently. Intermediate
// digests replace row bytes; their originals precede the final sector digest.
// allowRestored is for row/slot readers only, never raw checksum validation.
func verifyDMSectorHashes(page []byte, h hash.Hash, allowRestored bool) bool {
	if len(page) < 2*systemSectorSize || len(page)%systemSectorSize != 0 {
		return false
	}
	sectors, size := len(page)/systemSectorSize, h.Size()
	backupStart := len(page) - dmPageCheckTailSize - sectors*size
	for sector := sectors - 1; sector >= 0; sector-- {
		start, end := sector*systemSectorSize, (sector+1)*systemSectorSize-size
		if sector == sectors-1 {
			end -= dmPageCheckTailSize
		}
		h.Reset()
		h.Write(page[start:end])
		if bytes.Equal(page[end:end+size], h.Sum(nil)) {
			continue
		}
		backup := backupStart + sector*size
		if allowRestored && sector < sectors-1 && bytes.Equal(page[end:end+size], page[backup:backup+size]) {
			continue
		}
		return false
	}
	return true
}

func detectDMPageHashLayout(page []byte, allowRestored bool) (dmPageHashLayout, bool) {
	if len(page) < dmPageChecksumOffset+dmPageChecksumSize+dmPageCheckTailSize {
		return dmPageHashLayout{}, false
	}
	for _, b := range page[dmPageChecksumOffset : dmPageChecksumOffset+dmPageChecksumSize] {
		if b != 0 {
			return dmPageHashLayout{}, false
		}
	}
	for _, name := range []string{"SHA256", "SM3", "SHA1", "MD5", "SHA224", "SHA384", "SHA512"} {
		h, canonical, err := newDMPageHash(name)
		if err != nil {
			continue
		}
		if verifyDMWholePageHash(page, h) {
			return dmPageHashLayout{name: canonical, digestSize: h.Size()}, true
		}
		if verifyDMSectorHashes(page, h, allowRestored) {
			return dmPageHashLayout{name: canonical, digestSize: h.Size(), sectorHash: true}, true
		}
	}
	return dmPageHashLayout{}, false
}

func restoreDMHashSectorBytes(page []byte, layout dmPageHashLayout) {
	if !layout.sectorHash {
		return
	}
	backupStart := len(page) - layout.trailerLen(len(page))
	for sector := 0; sector < len(page)/systemSectorSize-1; sector++ {
		src := backupStart + sector*layout.digestSize
		dst := (sector+1)*systemSectorSize - layout.digestSize
		copy(page[dst:dst+layout.digestSize], page[src:src+layout.digestSize])
	}
}
