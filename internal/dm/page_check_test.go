package dm

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"testing"
)

func TestVerifyDMPageCheckModes(t *testing.T) {
	base := make([]byte, 8192)
	for i := range base {
		base[i] = byte((i*31 + 7) % 251)
	}
	clear(base[dmPageChecksumOffset : dmPageChecksumOffset+dmPageChecksumSize])

	tests := []struct {
		name       string
		mode       uint32
		hashName   string
		storedCRC  uint32
		storedHash string
	}{
		{name: "disabled", mode: 0},
		{name: "crc32", mode: 1, storedCRC: 0x62E5B802},
		{name: "sha256", mode: 2, hashName: "SHA256", storedHash: "e2a5223c07bf224c9fb378ed1b8574755e860cd4157ff7287e3bb815d8311ed7"},
		{name: "crc32c", mode: 3, storedCRC: 0xA35AEA48},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			page := append([]byte(nil), base...)
			if tt.storedCRC != 0 {
				binary.LittleEndian.PutUint32(page[dmPageChecksumOffset:], tt.storedCRC)
			}
			if tt.storedHash != "" {
				digest, err := hex.DecodeString(tt.storedHash)
				if err != nil {
					t.Fatal(err)
				}
				offset := len(page) - len(digest) - dmPageCheckTailSize
				copy(page[offset:], digest)
			}
			ok, err := verifyDMPageCheck(page, tt.mode, tt.hashName)
			if err != nil || !ok {
				t.Fatalf("verify mode %d: ok=%t err=%v", tt.mode, ok, err)
			}
			if tt.mode != 0 {
				page[0x100] ^= 0x01
				ok, err = verifyDMPageCheck(page, tt.mode, tt.hashName)
				if err != nil || ok {
					t.Fatalf("corruption was not detected: ok=%t err=%v", ok, err)
				}
			}
		})
	}
}

func TestVerifyDMPageCheckRejectsUnknownModeAndHash(t *testing.T) {
	page := make([]byte, 8192)
	if _, err := verifyDMPageCheck(page, 9, ""); err == nil {
		t.Fatal("unknown PAGE_CHECK mode was accepted")
	}
	if _, err := verifyDMPageCheck(page, 2, "SM3"); err == nil {
		t.Fatal("unsupported hash was accepted")
	}
}

func TestHashPageMovesSlotDirectoryBeforeDigest(t *testing.T) {
	const pageSize = 8192
	page := make([]byte, pageSize)
	binary.LittleEndian.PutUint32(page[dmPageKindOff:], dmPageKindRowData)
	putTestRow(page, dataRowAreaStart, 7, 0x00)
	binary.LittleEndian.PutUint16(page[dataPageSlotCountOff:], 3)
	binary.LittleEndian.PutUint16(page[dataPageFreeEndOff:], dataRowAreaStart+7)
	binary.LittleEndian.PutUint16(page[dataPageRecordCountOff:], 1)

	hashOffset := pageSize - sha256.Size - dmPageCheckTailSize
	slotStart := hashOffset - 3*2
	binary.LittleEndian.PutUint16(page[slotStart:], 0x5A)
	binary.LittleEndian.PutUint16(page[slotStart+2:], dataRowAreaStart)
	binary.LittleEndian.PutUint16(page[slotStart+4:], 0x52)
	digest := sha256.Sum256(page[:hashOffset])
	copy(page[hashOffset:], digest[:])

	name, size, ok := detectDMPageHash(page)
	if !ok || name != "SHA256" || size != sha256.Size {
		t.Fatalf("hash page was not detected: name=%q size=%d ok=%t", name, size, ok)
	}
	rows := locateRowsInDataPage(page, pageSize, 1)
	if len(rows) != 1 || rows[0].offset != dataRowAreaStart {
		t.Fatalf("hash-adjusted slot directory was not used: %+v", rows)
	}

	page[hashOffset] ^= 0x01
	if _, _, ok := detectDMPageHash(page); ok {
		t.Fatal("corrupted digest unexpectedly verified")
	}
	if got := pageSlotTrailerLenForPage(page); got != sha256.Size+dmPageCheckTailSize {
		t.Fatalf("corrupt hash trailer length=%d, want %d", got, sha256.Size+dmPageCheckTailSize)
	}
	rows = locateRowsInDataPage(page, pageSize, 1)
	if len(rows) != 1 || rows[0].offset != dataRowAreaStart {
		t.Fatalf("corrupt hash page lost its inferable slot directory: %+v", rows)
	}
}

func TestNoCheckPageKeepsStructurallyValidFixedTrailer(t *testing.T) {
	page := make([]byte, 8192)
	binary.LittleEndian.PutUint16(page[dataPageSlotCountOff:], 5)
	binary.LittleEndian.PutUint16(page[dataPageFreeEndOff:], 0x500)
	start := len(page) - pageSlotTrailerLen - 5*2
	for i, off := range []uint16{0x5A, 0x400, 0x300, 0x200, 0x52} {
		binary.LittleEndian.PutUint16(page[start+i*2:], off)
	}
	// Repeated 0x005A values before the real directory reproduced the false
	// positive seen on a PAGE_CHECK=0 SYSOBJECTS root page.
	for pos := start - 64; pos < start; pos += 2 {
		binary.LittleEndian.PutUint16(page[pos:], 0x5A)
	}
	if got := pageSlotTrailerLenForPage(page); got != pageSlotTrailerLen {
		t.Fatalf("PAGE_CHECK=0 fixed trailer was misdetected as hash trailer: %d", got)
	}
}

func TestProtected32KDataPageUsesReservedTailAndRestoresSectorBytes(t *testing.T) {
	const (
		pageSize = 32768
		rowCount = 57
		rowLen   = 560
		nSlot    = rowCount + 2
	)
	page := make([]byte, pageSize)
	binary.LittleEndian.PutUint32(page[dmPageKindOff:], dmPageKindRowData)
	binary.LittleEndian.PutUint16(page[dataPageSlotCountOff:], nSlot)
	binary.LittleEndian.PutUint16(page[dataPageRecordCountOff:], rowCount)
	rowOffsets := make([]int, 0, rowCount)
	pos := dataRowAreaStart
	for row := 0; row < rowCount; row++ {
		rowOffsets = append(rowOffsets, pos)
		putTestRow(page, pos, rowLen, byte('A'+row%26))
		pos += rowLen
	}
	binary.LittleEndian.PutUint16(page[dataPageFreeEndOff:], uint16(pos))
	tailLen := pageTailReservedLen(pageSize)
	slotStart := pageSize - tailLen - nSlot*2
	binary.LittleEndian.PutUint16(page[slotStart:], 0x5A)
	for row, offset := range rowOffsets {
		binary.LittleEndian.PutUint16(page[slotStart+2+row*2:], uint16(offset))
	}
	binary.LittleEndian.PutUint16(page[slotStart+(nSlot-1)*2:], 0x52)
	wantRows := append([]byte(nil), page[:pos]...)
	tailStart := pageSize - tailLen
	for sector := 1; sector < pageSize/systemSectorSize; sector++ {
		target := sector*systemSectorSize - 4
		copy(page[tailStart+(sector-1)*4:], page[target:target+4])
		binary.LittleEndian.PutUint32(page[target:], uint32(0xA5000000+sector))
	}

	if got := pageSlotTrailerLenForPage(page); got != tailLen {
		t.Fatalf("protected page trailer length=%d, want %d", got, tailLen)
	}
	if rows := locateRowsInDataPage(page, pageSize, rowCount); len(rows) != rowCount {
		t.Fatalf("protected page rows=%d, want %d", len(rows), rowCount)
	}
	restoreUserDataPageProtectionBytes(page, pageSize)
	if !bytes.Equal(page[:pos], wantRows) {
		t.Fatal("sector-boundary row bytes were not restored from the protected tail")
	}
}

func TestInferHashTrailerWhenShiftedSlotsStillScoreWell(t *testing.T) {
	const (
		pageSize = 32768
		nSlot    = 100
		freeEnd  = 0x6000
	)
	page := make([]byte, pageSize)
	binary.LittleEndian.PutUint16(page[dataPageSlotCountOff:], nSlot)
	binary.LittleEndian.PutUint16(page[dataPageFreeEndOff:], freeEnd)
	trueStart := pageSize - dmPageCheckTailSize - sha256.Size - nSlot*2
	for slot := 0; slot < nSlot; slot++ {
		binary.LittleEndian.PutUint16(page[trueStart+slot*2:], uint16(0x100+slot*8))
	}
	// The fixed-tail candidate keeps 84 valid offsets and consumes 16 digest
	// words. Its old score still exceeded 2*nSlot, which caused the premature
	// PAGE_CHECK=0 decision seen on a 32 KiB SYSCOLUMNS page.
	for pos := pageSize - dmPageCheckTailSize - sha256.Size; pos < pageSize-dmPageCheckTailSize; pos += 2 {
		binary.LittleEndian.PutUint16(page[pos:], 0xF000)
	}
	if got := pageSlotTrailerLenForPage(page); got != sha256.Size+dmPageCheckTailSize {
		t.Fatalf("inferred trailer length=%d, want %d", got, sha256.Size+dmPageCheckTailSize)
	}
}
