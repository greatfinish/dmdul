package dm

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// buildHealthyRowPage writes a minimal but structurally valid leaf/data page:
// one boundary-key slot at 0x5A plus `rows` ordinary rows packed from the row
// area, with matching slot directory, free-space end and record count.
func buildHealthyRowPage(page []byte, pageSize uint32, groupID uint16, fileID uint16, pageNo uint32, storageID uint32, rows int) {
	putTestDMPageHeader(page, groupID, fileID, pageNo, dmPageKindRowData, storageID)
	const rowLen = 8
	off := uint16(dataRowAreaStart)
	rowOffsets := make([]uint16, 0, rows)
	for i := 0; i < rows; i++ {
		binary.BigEndian.PutUint16(page[off:], rowLen) // length/status word
		binary.LittleEndian.PutUint32(page[off+3:], uint32(1000+i))
		rowOffsets = append(rowOffsets, off)
		off += rowLen
	}
	freeEnd := off
	nSlot := uint16(rows + 1) // +1 boundary-key slot
	binary.LittleEndian.PutUint16(page[dataPageSlotCountOff:], nSlot)
	binary.LittleEndian.PutUint16(page[dataPageFreeEndOff:], freeEnd)
	binary.LittleEndian.PutUint16(page[dataPageRecordCountOff:], uint16(rows))
	binary.LittleEndian.PutUint16(page[dataPageTreeLevelOff:], 0)
	// Slot directory grows down from the tail: slot 1 is the boundary key.
	slotArrayStart := int(pageSize) - pageSlotTrailerLen - int(nSlot)*2
	binary.LittleEndian.PutUint16(page[slotArrayStart:], 0x5A) // boundary key
	for i, rowOff := range rowOffsets {
		binary.LittleEndian.PutUint16(page[slotArrayStart+2+i*2:], rowOff)
	}
}

func TestCheckRowPageStructureAcceptsHealthyPage(t *testing.T) {
	page := make([]byte, 8192)
	buildHealthyRowPage(page, 8192, 12, 0, 48, 33555845, 100)
	if detail, ok := checkRowPageStructure(page, 8192); !ok {
		t.Fatalf("healthy page rejected: %s", detail)
	}
	if _, _, ok := classifyPageCorruption(page, dataFileKey{groupID: 12, fileID: 0}, 48, 8192, PageCheckOptions{}); !ok {
		t.Fatalf("healthy page classified as corrupt")
	}
}

func TestCheckRowPageStructureAcceptsReusableSlotWords(t *testing.T) {
	page := make([]byte, 8192)
	putTestDMPageHeader(page, 12, 0, 48, dmPageKindRowData, 33555845)
	binary.BigEndian.PutUint16(page[dataRowAreaStart:], 8)
	binary.LittleEndian.PutUint32(page[dataRowAreaStart+3:], 1001)
	binary.LittleEndian.PutUint16(page[dataPageSlotCountOff:], 3)
	binary.LittleEndian.PutUint16(page[dataPageFreeEndOff:], dataRowAreaStart+8)
	binary.LittleEndian.PutUint16(page[dataPageRecordCountOff:], 1)
	start := len(page) - pageSlotTrailerLen - 3*2
	binary.LittleEndian.PutUint16(page[start:], 0x5A)
	binary.LittleEndian.PutUint16(page[start+2:], dataRowAreaStart)
	binary.LittleEndian.PutUint16(page[start+4:], 0x1FFF)

	if detail, ok := checkRowPageStructure(page, 8192); !ok {
		t.Fatalf("page with one live row and one reusable slot rejected: %s", detail)
	}
}

func TestClassifyPageCorruptionDetectsInjectedDamage(t *testing.T) {
	key := dataFileKey{groupID: 12, fileID: 0}
	tests := []struct {
		name    string
		damage  func(page []byte)
		want    PageCorruptionKind
		checkOK bool // true means the page should be classified OK
	}{
		{
			name:   "header self-id smash",
			damage: func(p []byte) { binary.LittleEndian.PutUint32(p[4:], 0xEFBEADDE) },
			want:   PageCorruptionHeader,
		},
		{
			name:   "slot count insane",
			damage: func(p []byte) { binary.LittleEndian.PutUint16(p[dataPageSlotCountOff:], 0x0FFF) },
			want:   PageCorruptionStructure,
		},
		{
			name: "row data corrupted",
			damage: func(p []byte) {
				for i := 0; i < 64; i++ {
					p[0x100+i] = 'Z'
				}
			},
			want: PageCorruptionStructure,
		},
		{
			name: "zeroed after formatting",
			damage: func(p []byte) {
				for i := 16; i < len(p); i++ {
					p[i] = 0
				}
			},
			want: PageCorruptionHeader,
		},
		{
			name:    "untouched healthy page",
			damage:  func(p []byte) {},
			checkOK: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			page := make([]byte, 8192)
			buildHealthyRowPage(page, 8192, 12, 0, 48, 33555845, 100)
			tt.damage(page)
			kind, detail, ok := classifyPageCorruption(page, key, 48, 8192, PageCheckOptions{})
			if tt.checkOK {
				if !ok {
					t.Fatalf("expected OK, got %s: %s", kind, detail)
				}
				return
			}
			if ok {
				t.Fatalf("expected corruption %s, got OK", tt.want)
			}
			if kind != tt.want {
				t.Fatalf("expected %s, got %s: %s", tt.want, kind, detail)
			}
		})
	}
}

func TestClassifyPageCorruptionSkipsReservedAndFillPages(t *testing.T) {
	key := dataFileKey{groupID: 12, fileID: 0}
	// Reserved page: valid self-id, non-zero fill kind, blank body.
	reserved := make([]byte, 8192)
	binary.LittleEndian.PutUint16(reserved[0:], 12)
	binary.LittleEndian.PutUint16(reserved[2:], 0)
	binary.LittleEndian.PutUint32(reserved[4:], 47)
	binary.LittleEndian.PutUint32(reserved[dmPageKindOff:], 0xFFFF00FF)
	if _, _, ok := classifyPageCorruption(reserved, key, 47, 8192, PageCheckOptions{}); !ok {
		t.Fatalf("reserved fill page must not be flagged as corrupt")
	}
	// A legitimate internal page with a valid storage id but a non-data kind.
	internal := make([]byte, 8192)
	putTestDMPageHeader(internal, 12, 0, 17, 0x1A1A001A, 33555845)
	binary.LittleEndian.PutUint16(internal[dataPageSlotCountOff:], 2)
	binary.LittleEndian.PutUint16(internal[dataPageFreeEndOff:], dataRowAreaStart)
	if _, _, ok := classifyPageCorruption(internal, key, 17, 8192, PageCheckOptions{}); !ok {
		t.Fatalf("internal non-data page must not be flagged as corrupt")
	}
}

func TestCheckPagesReportsFileSizeAndBadPages(t *testing.T) {
	dir := t.TempDir()
	// One tablespace file: page 0 healthy, page 1 header-smashed.
	raw := make([]byte, 3*8192)
	buildHealthyRowPage(raw[0:8192], 8192, 12, 0, 0, 33555845, 10)
	buildHealthyRowPage(raw[8192:16384], 8192, 12, 0, 1, 33555845, 10)
	binary.LittleEndian.PutUint32(raw[8192+4:], 0xDEADBEEF) // smash page 1 self-id
	// page 2 left all-zero (empty).
	path := filepath.Join(dir, "MAIN.DBF")
	if err := os.WriteFile(path, raw, 0644); err != nil {
		t.Fatal(err)
	}
	result, err := CheckPages(PageCheckOptions{DataDir: dir, PageSize: 8192})
	if err != nil {
		t.Fatalf("CheckPages failed: %v", err)
	}
	if result.PagesChecked != 3 || result.PagesEmpty != 1 {
		t.Fatalf("unexpected page counts: %+v", result)
	}
	if result.BadPagesTotal != 1 || result.Corruption[PageCorruptionHeader] != 1 {
		t.Fatalf("expected exactly one header-invalid page, got %+v", result.Corruption)
	}
	bad := result.SortedBadPages()
	if len(bad) != 1 || bad[0].PageNo != 1 || bad[0].Kind != PageCorruptionHeader {
		t.Fatalf("unexpected bad page: %+v", bad)
	}
}

func TestCheckPagesStreamsAllBadPagesAndKeepsExactImpactCounts(t *testing.T) {
	dir := t.TempDir()
	raw := make([]byte, 5*8192)
	for pageNo := 0; pageNo < 5; pageNo++ {
		page := raw[pageNo*8192 : (pageNo+1)*8192]
		buildHealthyRowPage(page, 8192, 12, 0, uint32(pageNo), 33555845, 10)
		if pageNo > 0 {
			binary.LittleEndian.PutUint32(page[4:], 0xDEADBEEF)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "MAIN.DBF"), raw, 0644); err != nil {
		t.Fatal(err)
	}
	dict := &DictionaryInfo{Tables: []DictionaryTable{{
		ID: 42, Owner: "APP", Name: "T_BAD", Tablespace: "MAIN", GroupID: 12,
		HeaderFile: 0, HeaderBlock: 1, Blocks: 4, Bytes: uint64(len(raw)), StorageID: 33555845,
	}}}
	streamed := 0
	result, err := CheckPages(PageCheckOptions{
		DataDir: dir, PageSize: 8192, MaxReported: 1, Dictionary: dict,
		OnBadPage: func(bad BadPage) error {
			streamed++
			if bad.Owner != "APP" || bad.Table != "T_BAD" || bad.Attribution != PageAttributionStorageID {
				t.Fatalf("callback received unattributed page: %+v", bad)
			}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("CheckPages failed: %v", err)
	}
	if streamed != 4 || result.BadPagesTotal != 4 || result.Corruption[PageCorruptionHeader] != 4 {
		t.Fatalf("full counts must ignore retained-detail cap: streamed=%d result=%+v", streamed, result)
	}
	if len(result.Files) != 1 || len(result.Files[0].Bad) != 1 || !result.Files[0].ReportTruncated {
		t.Fatalf("console detail cap not applied as expected: %+v", result.Files)
	}
	if result.AttributedBadPages != 4 || result.UnattributedBadPages != 0 ||
		result.AffectedTables != 1 || result.TotalTables != 1 || len(result.AffectedObjects) != 1 {
		t.Fatalf("unexpected impact summary: %+v", result)
	}
	object := result.AffectedObjects[0]
	if object.BadPages != 4 || object.SegmentHeaderBadPages != 1 || object.HeaderInvalid != 4 || object.StorageID != 33555845 ||
		object.Attribution != string(PageAttributionStorageID) {
		t.Fatalf("unexpected affected object: %+v", object)
	}
	if result.AffectedTableBytes != uint64(len(raw)) || result.TotalTableBytes != uint64(len(raw)) {
		t.Fatalf("unexpected table-byte totals: affected=%d total=%d", result.AffectedTableBytes, result.TotalTableBytes)
	}
}

func TestCheckPhysicalPageSourceSupportsLogicalReader(t *testing.T) {
	raw := make([]byte, 3*8192)
	for pageNo := 0; pageNo < 3; pageNo++ {
		page := raw[pageNo*8192 : (pageNo+1)*8192]
		buildHealthyRowPage(page, 8192, 0, 0, uint32(pageNo), 500, 5)
	}
	binary.LittleEndian.PutUint32(raw[2*8192+4:], 999)
	streamed := 0
	result, err := CheckPhysicalPageSource(OfflineDataSource{
		GroupID: 0, FileID: 0, Tablespace: "SYSTEM", Path: "+DATA/DMDB/SYSTEM.DBF",
		Reader: fixedSizeReaderAt{ReaderAt: bytes.NewReader(raw), size: int64(len(raw))},
	}, PageCheckOptions{
		PageSize: 8192,
		OnBadPage: func(bad BadPage) error {
			streamed++
			return nil
		},
	})
	if err != nil {
		t.Fatalf("CheckPhysicalPageSource failed: %v", err)
	}
	if result.DictionaryUsed || result.FilesChecked != 1 || result.PagesChecked != 3 ||
		result.BadPagesTotal != 1 || result.Corruption[PageCorruptionHeader] != 1 || streamed != 1 {
		t.Fatalf("unexpected physical source result: streamed=%d result=%+v", streamed, result)
	}
	bad := result.SortedBadPages()
	if len(bad) != 1 || bad[0].Path != "+DATA/DMDB/SYSTEM.DBF" ||
		bad[0].UnattributedReason != "dictionary_not_loaded" {
		t.Fatalf("unexpected logical-source bad page: %+v", bad)
	}
}

func TestCheckPagesFlagsTruncatedFile(t *testing.T) {
	dir := t.TempDir()
	// 8192 + 100 bytes: not a page-size multiple.
	raw := make([]byte, 8192+100)
	buildHealthyRowPage(raw[0:8192], 8192, 12, 0, 0, 33555845, 5)
	path := filepath.Join(dir, "MAIN.DBF")
	if err := os.WriteFile(path, raw, 0644); err != nil {
		t.Fatal(err)
	}
	result, err := CheckPages(PageCheckOptions{DataDir: dir, PageSize: 8192})
	if err != nil {
		t.Fatalf("CheckPages failed: %v", err)
	}
	if len(result.Files) != 1 || !result.Files[0].SizeInvalid {
		t.Fatalf("expected size-invalid file, got %+v", result.Files)
	}
}

func TestCheckPagesDataDirOnlyIgnoresControlAbsolutePaths(t *testing.T) {
	// A "live" directory holds a clean file; the snapshot dir holds a
	// corrupted copy of the same tablespace file. Default (data_dir-only)
	// must scan the snapshot, not follow any absolute path to the clean file.
	live := t.TempDir()
	snap := t.TempDir()
	clean := make([]byte, 2*8192)
	buildHealthyRowPage(clean[0:8192], 8192, 12, 0, 0, 33555845, 10)
	buildHealthyRowPage(clean[8192:16384], 8192, 12, 0, 1, 33555845, 10)
	if err := os.WriteFile(filepath.Join(live, "TBS.DBF"), clean, 0644); err != nil {
		t.Fatal(err)
	}
	corrupt := append([]byte(nil), clean...)
	binary.LittleEndian.PutUint32(corrupt[8192+4:], 0xDEADBEEF) // smash page 1 self-id
	if err := os.WriteFile(filepath.Join(snap, "TBS.DBF"), corrupt, 0644); err != nil {
		t.Fatal(err)
	}
	result, err := CheckPages(PageCheckOptions{DataDir: snap, PageSize: 8192})
	if err != nil {
		t.Fatalf("CheckPages failed: %v", err)
	}
	if result.BadPagesTotal != 1 {
		t.Fatalf("data_dir-only must scan the corrupted snapshot copy, got bad=%d", result.BadPagesTotal)
	}
	if len(result.Files) != 1 || filepath.Dir(result.Files[0].Path) != snap {
		t.Fatalf("scanned the wrong file: %+v", result.Files)
	}
}
