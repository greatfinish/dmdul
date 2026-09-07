package dm

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"testing"
)

func sectorHashTestPage(size int, name string) ([]byte, []byte) {
	page := make([]byte, size)
	binary.LittleEndian.PutUint16(page, 4)
	binary.LittleEndian.PutUint32(page[4:], 1)
	binary.LittleEndian.PutUint32(page[dmPageKindOff:], dmPageKindRowData)
	binary.LittleEndian.PutUint32(page[dataPageAssistIndexOff:], 123)
	h, _, _ := newDMPageHash(name)
	trailer := size/systemSectorSize*h.Size() + dmPageCheckTailSize
	const rowLen = 560
	count := (size - trailer - dataRowAreaStart - 4) / (rowLen + 2)
	pos := dataRowAreaStart
	start := size - trailer - (count+2)*2
	binary.LittleEndian.PutUint16(page[start:], 0x5A)
	for i := 0; i < count; i++ {
		putTestRow(page, pos, rowLen, byte('A'+i%26))
		binary.LittleEndian.PutUint16(page[start+2+2*i:], uint16(pos))
		pos += rowLen
	}
	binary.LittleEndian.PutUint16(page[start+(count+1)*2:], 0x52)
	binary.LittleEndian.PutUint16(page[dataPageSlotCountOff:], uint16(count+2))
	binary.LittleEndian.PutUint16(page[dataPageRecordCountOff:], uint16(count))
	binary.LittleEndian.PutUint16(page[dataPageFreeEndOff:], uint16(pos))
	for sector := 0; sector < size/systemSectorSize-1; sector++ {
		dst := size - trailer + sector*h.Size()
		src := (sector+1)*systemSectorSize - h.Size()
		copy(page[dst:dst+h.Size()], page[src:src+h.Size()])
	}
	want := bytes.Clone(page)
	for sector := 0; sector < size/systemSectorSize; sector++ {
		end := (sector+1)*systemSectorSize - h.Size()
		if sector == size/systemSectorSize-1 {
			end -= dmPageCheckTailSize
		}
		h.Reset()
		h.Write(page[sector*systemSectorSize : end])
		copy(page[end:end+h.Size()], h.Sum(nil))
	}
	return page, want
}

func TestSectorHashProtectionAndRawScan(t *testing.T) {
	for _, name := range []string{"SM3", "SHA256"} {
		for _, size := range []int{8192, 16384, 32768} {
			t.Run(fmt.Sprintf("%s-%d", name, size), func(t *testing.T) {
				raw, want := sectorHashTestPage(size, name)
				if ok, err := verifyDMPageCheck(raw, 2, name); !ok || err != nil {
					t.Fatalf("raw verification=%t %v", ok, err)
				}
				for _, restore := range []func([]byte, uint32){restorePageProtectionBytes, restoreUserDataPageProtectionBytes} {
					page := bytes.Clone(raw)
					restore(page, uint32(size))
					if !bytes.Equal(page[:size-40], want[:size-40]) {
						t.Fatal("restoration changed non-protection bytes or lost boundary bytes")
					}
					if detail, ok := checkRowPageStructure(page, uint32(size)); !ok {
						t.Fatal(detail)
					}
					before := bytes.Clone(page)
					restore(page, uint32(size))
					if !bytes.Equal(before, page) {
						t.Fatal("restoration is not idempotent")
					}
					if ok, _ := verifyDMPageCheck(page, 2, name); ok {
						t.Fatal("restored bytes counted as raw checksum proof")
					}
				}
				for _, off := range []int{100, 4095, size - 41, size - 9} {
					bad := bytes.Clone(raw)
					bad[off] ^= 1
					if ok, _ := verifyDMPageCheck(bad, 2, name); ok {
						t.Fatalf("missed corruption at %d", off)
					}
				}
				file := append(make([]byte, size), raw...)
				result, err := CheckPhysicalPageSource(OfflineDataSource{GroupID: 4, Reader: bytes.NewReader(file)}, PageCheckOptions{PageSize: uint32(size), PageCheckMode: 2, PageHashName: name})
				if err != nil || result.BadPagesTotal != 0 {
					t.Fatalf("physical scan restored bytes too early: %+v %v", result, err)
				}
			})
		}
	}
}

func TestDM9SM3SystemBoundaryFixture(t *testing.T) {
	page, err := os.ReadFile("testdata/dm9_sm3_system_page304.bin")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := verifyDMPageCheck(page, 2, "SM3"); !ok || err != nil {
		t.Fatalf("fixture checksum=%t %v", ok, err)
	}
	restorePageProtectionBytes(page, 8192)
	if got := page[0xFE6:0xFED]; !bytes.Equal(got, []byte{0, 0x36, 0, 0x3C, 0x0C, 4, 0}) {
		t.Fatalf("boundary bytes=%X", got)
	}
	if detail, ok := checkRowPageStructure(page, 8192); !ok {
		t.Fatal(detail)
	}
}
