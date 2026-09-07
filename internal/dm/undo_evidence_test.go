package dm

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"strings"
	"testing"
)

func undoFixture(t *testing.T) ([]byte, []byte) {
	t.Helper()
	decode := func(text string) []byte {
		raw, err := hex.DecodeString(text)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	page := make([]byte, 8192)
	putTestDMPageHeader(page, 1, 0, 447, 0x1d, 0)
	copy(page[36:42], []byte{0x7f, 0x1a, 0, 0, 0, 0})
	binary.LittleEndian.PutUint16(page[43:], 55)
	binary.LittleEndian.PutUint16(page[45:], 183)
	// DM8 isolated two-UPDATE capture: both records precede the current row.
	copy(page[55:], decode("3e000203060000007c1a00000000000000000000f9030000f9030000ffffffff7fffff0100000000000000010001000800424153455f4f4e450000003700"))
	copy(page[117:], decode("420002030c0000007f1a00000000000000000000f9030000f903000000bf01000037000100000000000000010001000c0046495253545f5550444154450000007500"))
	row := decode("002f0001000000945345434f4e445f4c4f4e4745525f55504441544501000000000000bf01000075007f1a00000000")
	return page, row
}

func undoTestCache(page []byte) *dataFilePageCache {
	raw := make([]byte, 448*8192)
	copy(raw[447*8192:], page)
	return newDataFilePageCache([]dataFileRef{{key: dataFileKey{groupID: 1, fileID: 0}, reader: sizedReaderAt(bytes.NewReader(raw), int64(len(raw)))}}, 8192)
}

func TestUndoEvidenceTracesRealTwoUpdateChain(t *testing.T) {
	page, row := undoFixture(t)
	record, err := decodeUndoRecordEvidence(page, 117)
	if err != nil {
		t.Fatal(err)
	}
	if record.operation != "UPDATE" || record.sequence != 12 || record.previousTransaction != 6783 || record.previous.rollOffset != 55 {
		t.Fatalf("%+v", record)
	}
	got := traceRowUndoEvidence(row, undoTestCache(page))
	want := "0:447:117 UPDATE seq=12 previous_trx=6783 -> 0:447:55 UPDATE seq=6 previous_trx=6780 -> END (visibility unknown)"
	if got != want {
		t.Fatalf("%s", got)
	}
}

func TestUndoEvidenceNeverTreatsMissingOrReusedAsCommitted(t *testing.T) {
	page, row := undoFixture(t)
	if got := traceRowUndoEvidence(row, newDataFilePageCache(nil, 8192)); got != "STOP_MISSING_ROLL_PAGE" {
		t.Fatal(got)
	}
	page[36]++
	if got := traceRowUndoEvidence(row, undoTestCache(page)); got != "STOP_ROLL_REUSED_OR_TRX_MISMATCH" {
		t.Fatal(got)
	}
	page[36]--
	binary.LittleEndian.PutUint16(page[45:], 55) // rolled back, old payload remains beyond used end
	if got := traceRowUndoEvidence(row, undoTestCache(page)); !strings.Contains(got, "outside used region") {
		t.Fatal(got)
	}
}

func TestUndoEvidenceBoundsOpcodeAndCycle(t *testing.T) {
	page, row := undoFixture(t)
	if _, err := decodeUndoRecordEvidence(page[:80], 117); err == nil {
		t.Fatal("short page accepted")
	}
	page[119] = 0xfe
	if _, err := decodeUndoRecordEvidence(page, 117); err == nil {
		t.Fatal("unknown opcode accepted")
	}
	page[119] = 2
	// The second record now points to itself.
	binary.LittleEndian.PutUint16(page[117+33:], 117)
	if got := traceRowUndoEvidence(row, undoTestCache(page)); !strings.Contains(got, "STOP_CYCLE") {
		t.Fatal(got)
	}
	page[117+64] = 0
	if _, err := decodeUndoRecordEvidence(page, 117); err == nil {
		t.Fatal("wrong self-offset accepted")
	}
}

func TestPageCheckAllowsDeletedSlotStillIncludedInRecordCount(t *testing.T) {
	page := make([]byte, 8192)
	putTestIntDataPage(page, 4, 0, 16, 1042, 1)
	binary.BigEndian.PutUint16(page[dataRowAreaStart:], 7|dataRowDeletedMask)
	if reason, ok := checkRowPageStructure(page, 8192); !ok {
		t.Fatal(reason)
	}
	if rows := locateRowsInDataPage(page, 8192, 0); len(rows) != 0 {
		t.Fatal("deleted row exported in normal mode")
	}
	binary.LittleEndian.PutUint16(page[dataPageRecordCountOff:], 2)
	if _, ok := checkRowPageStructure(page, 8192); ok {
		t.Fatal("impossible count accepted")
	}
}
