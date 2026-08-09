package dm

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func TestInferDictionaryTableSegmentsFromAssistPages(t *testing.T) {
	dir := t.TempDir()
	dataPath := filepath.Join(dir, "MAIN.DBF")
	const pageSize = 8192
	raw := make([]byte, pageSize*192)
	putSegmentTestPage(raw, pageSize, 32, tableDataAssistID(1001))
	putSegmentTestPage(raw, pageSize, 160, tableDataAssistID(1001))
	if err := os.WriteFile(dataPath, raw, 0644); err != nil {
		t.Fatal(err)
	}
	controlDUL := filepath.Join(dir, "control.dul")
	if err := WriteControlDUL(controlDUL, []OfflineDataFile{{GroupID: 4, FileID: 0, Tablespace: "MAIN", Path: dataPath}}); err != nil {
		t.Fatal(err)
	}

	segments, err := inferDictionaryTableSegments(
		"",
		controlDUL,
		dir,
		nil,
		pageSize,
		16,
		map[uint32]dictionaryObject{1001: {ID: 1001, Owner: "APP", Name: "T"}},
		nil,
		nil,
		nil,
		[]DictionaryTable{{ID: 1001, Owner: "APP", Name: "T", GroupID: 4, Tablespace: "MAIN"}},
	)
	if err != nil {
		t.Fatal(err)
	}

	seg, ok := segments[1001]
	if !ok {
		t.Fatal("segment was not inferred")
	}
	if seg.fileID != 0 || seg.headerPage != 32 || seg.blocks != 32 || seg.extents != 2 || seg.bytes != 32*pageSize || seg.tablespaceID != 4 {
		t.Fatalf("unexpected segment: %+v", seg)
	}
}

func TestInferDictionaryTableSegmentsFromASMPagePlan(t *testing.T) {
	const (
		pageSize = 8192
		storage  = 33555433
	)
	raw := make([]byte, pageSize*64)
	putTestDMPageHeader(raw[16*pageSize:17*pageSize], 5, 0, 16, dmPageKindBTreeLeaf, storage)
	putTestDMPageRef(raw[16*pageSize:17*pageSize], dmPageNextRefOff, 0, 33)
	putTestDMPageHeader(raw[33*pageSize:34*pageSize], 5, 0, 33, dmPageKindBTreeLeaf, storage)
	putTestDMNullPageRef(raw[33*pageSize:34*pageSize], dmPageNextRefOff)
	reader := &countingSizedReaderAt{Reader: bytes.NewReader(raw)}

	segments, err := inferDictionaryTableSegments(
		"", "", "",
		[]OfflineDataSource{{GroupID: 5, FileID: 0, Tablespace: "TBS_APP", Path: "+DATA/DB/TBS_APP.DBF", Reader: reader}},
		pageSize, 16,
		map[uint32]dictionaryObject{1001: {ID: 1001, Owner: "APP", Name: "T"}},
		map[uint32]dictionaryObject{storage: {ID: storage, ParentID: 1001, Type: "TABOBJ", Subtype: "INDEX"}},
		map[uint32]indexDef{storage: {ID: storage, GroupID: 5, RootFile: 0, RootPage: 16, Flag: 1}},
		nil,
		[]DictionaryTable{{ID: 1001, Owner: "APP", Name: "T", GroupID: 5, Tablespace: "TBS_APP", StorageID: storage, RootFile: 0, RootPage: 16}},
	)
	if err != nil {
		t.Fatal(err)
	}
	segment, ok := segments[1001]
	if !ok {
		t.Fatal("segment was not inferred from the ASM page plan")
	}
	if segment.fileID != 0 || segment.headerPage != 16 || segment.blocks != 32 || segment.extents != 2 || segment.bytes != 32*pageSize || segment.tablespaceID != 5 {
		t.Fatalf("unexpected segment: %+v", segment)
	}
	if reader.reads != 2 {
		t.Fatalf("page-plan inference read %d pages, want 2", reader.reads)
	}
}

func TestInferDictionarySegmentStatsMarksPartialPartitionPlanIncomplete(t *testing.T) {
	const (
		pageSize         = 8192
		baseTableID      = 1001
		partTableID      = 2001
		baseStorageID    = 33555433
		missingStorageID = 33555434
	)
	raw := make([]byte, pageSize*32)
	putTestDMPageHeader(raw[16*pageSize:17*pageSize], 5, 0, 16, dmPageKindBTreeLeaf, baseStorageID)
	putTestDMNullPageRef(raw[16*pageSize:17*pageSize], dmPageNextRefOff)
	reader := &countingSizedReaderAt{Reader: bytes.NewReader(raw)}

	stats, complete := inferDictionarySegmentStatsFromPagePlans(
		[]dataFileRef{{key: dataFileKey{groupID: 5, fileID: 0}, path: "MAIN.DBF", reader: reader}},
		pageSize, 16,
		map[uint32]dictionaryObject{baseTableID: {ID: baseTableID, Owner: "APP", Name: "T_PART"}},
		map[uint32]dictionaryObject{missingStorageID: {ID: missingStorageID, ParentID: int32(partTableID), Type: "TABOBJ", Subtype: "INDEX"}},
		map[uint32]indexDef{missingStorageID: {ID: missingStorageID, GroupID: 5, RootFile: 1, RootPage: 16, Flag: 1}},
		map[uint32][]PartitionInfo{baseTableID: {{BaseTableID: baseTableID, PartTableID: partTableID}}},
		[]DictionaryTable{{ID: baseTableID, Owner: "APP", Name: "T_PART", GroupID: 5, StorageID: baseStorageID, RootFile: 0, RootPage: 16}},
	)

	if stats[baseTableID] == nil {
		t.Fatal("successful base-table page plan was discarded")
	}
	if complete[baseTableID] {
		t.Fatal("table with an unavailable partition root was marked complete")
	}
	if reader.reads != 1 {
		t.Fatalf("page-plan inference read %d pages, want 1", reader.reads)
	}
}

type countingSizedReaderAt struct {
	*bytes.Reader
	reads int
}

func (r *countingSizedReaderAt) ReadAt(p []byte, off int64) (int, error) {
	r.reads++
	return r.Reader.ReadAt(p, off)
}

func putSegmentTestPage(raw []byte, pageSize int, pageNo int, assistID uint32) {
	start := pageNo * pageSize
	page := raw[start : start+pageSize]
	binary.LittleEndian.PutUint16(page[dataPageSlotCountOff:], 1)
	binary.LittleEndian.PutUint16(page[dataPageFreeEndOff:], 0x70)
	binary.LittleEndian.PutUint16(page[dataPageRecordCountOff:], 1)
	binary.LittleEndian.PutUint32(page[dataPageAssistIndexOff:], assistID)
}
