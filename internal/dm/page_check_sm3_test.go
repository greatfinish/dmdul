package dm

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"testing"
)

func TestSM3KnownAnswers(t *testing.T) {
	for _, tc := range []struct{ input, digest string }{
		{"abc", "66c7f0f462eeedd9d1f2d46bdc10e4e24167c4875cf2f7a2297da02b8f4ba8e0"},
		{string(bytes.Repeat([]byte("abcd"), 16)), "debe9ff92275b8a138604889c18e5a4d6fdb70e5387e5765293dcba39c0c5732"},
	} {
		for _, alias := range []string{"SM3", "sm3", " OPENSSL_SM3 "} {
			h, canonical, err := newDMPageHash(alias)
			if err != nil || canonical != "SM3" || h.Size() != 32 {
				t.Fatalf("alias=%s canonical=%s err=%v", alias, canonical, err)
			}
			h.Write([]byte(tc.input))
			if got := hex.EncodeToString(h.Sum(nil)); got != tc.digest {
				t.Fatalf("SM3(%q)=%s want %s", tc.input, got, tc.digest)
			}
		}
	}
}

func TestSM3PageCheckAndSlotDirectory(t *testing.T) {
	for _, size := range []int{4096, 8192, 16384, 32768} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			page := make([]byte, size)
			binary.LittleEndian.PutUint32(page[dmPageKindOff:], dmPageKindRowData)
			putTestRow(page, dataRowAreaStart, 7, 0)
			binary.LittleEndian.PutUint16(page[dataPageSlotCountOff:], 3)
			binary.LittleEndian.PutUint16(page[dataPageRecordCountOff:], 1)
			binary.LittleEndian.PutUint16(page[dataPageFreeEndOff:], dataRowAreaStart+7)
			offset := size - 32 - dmPageCheckTailSize
			for i, slot := range []uint16{0x5A, dataRowAreaStart, 0x52} {
				binary.LittleEndian.PutUint16(page[offset-6+2*i:], slot)
			}
			h, _, _ := newDMPageHash("SM3")
			h.Write(page[:offset])
			copy(page[offset:], h.Sum(nil))
			original := bytes.Clone(page)
			if ok, err := verifyDMPageCheck(page, 2, "OPENSSL_SM3"); !ok || err != nil {
				t.Fatalf("verify=%t err=%v", ok, err)
			}
			if name, n, ok := detectDMPageHash(page); !ok || name != "SM3" || n != 32 {
				t.Fatalf("detect=%s/%d/%t", name, n, ok)
			}
			if got := locateRowsInDataPage(page, uint32(size), 1); len(got) != 1 || got[0].offset != dataRowAreaStart {
				t.Fatalf("SM3 slot rows=%+v", got)
			}
			if !bytes.Equal(page, original) {
				t.Fatal("verification changed raw page")
			}
			for _, off := range []int{0, 100, offset, offset + 31} {
				bad := bytes.Clone(page)
				bad[off] ^= 1
				if ok, err := verifyDMPageCheck(bad, 2, "SM3"); ok || err != nil {
					t.Fatalf("corruption offset=%d ok=%t err=%v", off, ok, err)
				}
			}
		})
	}
}
