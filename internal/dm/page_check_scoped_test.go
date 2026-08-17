package dm

import "testing"

func TestPageAttributionByStorageAndSegment(t *testing.T) {
	dict := &DictionaryInfo{
		Tables: []DictionaryTable{
			{
				ID: 1, Owner: "CHKTEST", Name: "T_CHK", StorageID: 33555845,
				GroupID: 12, HeaderFile: 0, HeaderBlock: 48, Blocks: 248,
			},
			{
				ID: 2, Owner: "APP", Name: "OTHER", StorageID: 44001122,
				AssistIDs: []uint32{44001123}, GroupID: 12, HeaderFile: 0,
			},
		},
	}
	a := newPageAttribution(dict)

	// Attribution by primary storage id.
	owner, table := a.attribute(&BadPage{GroupID: 12, FileID: 0, PageNo: 80, StorageID: 33555845})
	if owner != "CHKTEST" || table != "T_CHK" {
		t.Fatalf("storage-id attribution wrong: %s.%s", owner, table)
	}
	// Attribution by assist id.
	owner, table = a.attribute(&BadPage{GroupID: 12, FileID: 0, PageNo: 5, StorageID: 44001123})
	if owner != "APP" || table != "OTHER" {
		t.Fatalf("assist-id attribution wrong: %s.%s", owner, table)
	}
	// Zeroed page (storage id 0) attributed by segment range.
	owner, table = a.attribute(&BadPage{GroupID: 12, FileID: 0, PageNo: 120, StorageID: 0})
	if owner != "CHKTEST" || table != "T_CHK" {
		t.Fatalf("segment-range attribution wrong: %s.%s", owner, table)
	}
	// Page outside any storage or segment: unattributed.
	owner, table = a.attribute(&BadPage{GroupID: 12, FileID: 0, PageNo: 9000, StorageID: 0})
	if owner != "" || table != "" {
		t.Fatalf("unexpected attribution: %s.%s", owner, table)
	}
}

func TestPageAttributionEvidenceAndAmbiguity(t *testing.T) {
	dict := &DictionaryInfo{Tables: []DictionaryTable{
		{ID: 10, Owner: "APP", Name: "T1", Tablespace: "MAIN", StorageID: 100,
			AssistIDs: []uint32{101, 999}, GroupID: 4, HeaderFile: 0, HeaderBlock: 16, Blocks: 16, Bytes: 131072},
		{ID: 11, Owner: "APP", Name: "T2", Tablespace: "MAIN", StorageID: 200,
			AssistIDs: []uint32{999}, GroupID: 4, HeaderFile: 0, HeaderBlock: 64, Blocks: 16, Bytes: 131072},
	}}
	a := newPageAttribution(dict)

	primary := BadPage{GroupID: 4, FileID: 0, PageNo: 20, StorageID: 100}
	a.apply(&primary)
	if primary.ObjectType != PageObjectTable || primary.Attribution != PageAttributionStorageID ||
		primary.AttributionConfidence != PageAttributionHigh || primary.TableID != 10 {
		t.Fatalf("unexpected primary attribution: %+v", primary)
	}

	assist := BadPage{GroupID: 4, FileID: 0, PageNo: 200, StorageID: 101}
	a.apply(&assist)
	if assist.ObjectType != PageObjectTableAssist || assist.Attribution != PageAttributionAssistStorageID ||
		assist.ObjectStorageID != 101 {
		t.Fatalf("unexpected assist attribution: %+v", assist)
	}

	segment := BadPage{GroupID: 4, FileID: 0, PageNo: 22, StorageID: 0}
	a.apply(&segment)
	if segment.ObjectType != PageObjectTable || segment.Attribution != PageAttributionSegmentRange ||
		segment.AttributionConfidence != PageAttributionMedium {
		t.Fatalf("unexpected segment attribution: %+v", segment)
	}

	ambiguous := BadPage{GroupID: 9, FileID: 0, PageNo: 1, StorageID: 999}
	a.apply(&ambiguous)
	if ambiguous.Owner != "" || ambiguous.UnattributedReason != "ambiguous_storage_id" ||
		ambiguous.AttributionConfidence != PageAttributionNo {
		t.Fatalf("ambiguous storage must remain unattributed: %+v", ambiguous)
	}
}

func TestIsLeafChainBreakReasonExcludesRootIssues(t *testing.T) {
	rootIssues := []string{
		"storage root metadata is incomplete",
		"cannot read root page 0/16",
		"root page identity mismatch at 0/16",
		"root page storage_id=100, expected 200",
		"unsupported root page kind 0x99",
	}
	for _, r := range rootIssues {
		if isLeafChainBreakReason(r) {
			t.Fatalf("root-level reason must be excluded: %q", r)
		}
	}
	chainBreaks := []string{
		"leaf page identity mismatch at file=0 page=60",
		"leaf page cycle at file=0 page=88",
		"unexpected leaf page kind 0x0 at file=0 page=120",
		"leaf page storage_id=0, expected 33555845",
	}
	for _, r := range chainBreaks {
		if !isLeafChainBreakReason(r) {
			t.Fatalf("chain break must be reported: %q", r)
		}
	}
}

func TestCheckDictionaryConsistencyDetectsAnomalies(t *testing.T) {
	dict := &DictionaryInfo{
		Users: []DictionaryUser{{ID: 1, Name: "APP"}},
		Tables: []DictionaryTable{
			{ID: 10, Owner: "APP", Name: "T1"},
			{ID: 10, Owner: "APP", Name: "T1_DUP"},   // duplicate id
			{ID: 11, Owner: "GHOST", Name: "T_ORPH"}, // unknown owner
		},
		Columns: []DictionaryColumn{
			{TableID: 10, Name: "C1"},
			{TableID: 999, Name: "DANGLING"}, // missing table
		},
	}
	issues := checkDictionaryConsistency(dict)
	got := map[string]int{}
	for _, issue := range issues {
		got[issue.Category]++
	}
	if got["duplicate-table-id"] != 1 {
		t.Fatalf("expected 1 duplicate-table-id, got %d", got["duplicate-table-id"])
	}
	if got["orphan-table-owner"] != 1 {
		t.Fatalf("expected 1 orphan-table-owner, got %d", got["orphan-table-owner"])
	}
	if got["dangling-columns"] != 1 {
		t.Fatalf("expected 1 dangling-columns, got %d", got["dangling-columns"])
	}
}

func TestCheckDictionaryConsistencyCleanDictionary(t *testing.T) {
	dict := &DictionaryInfo{
		Users:  []DictionaryUser{{ID: 1, Name: "APP"}},
		Tables: []DictionaryTable{{ID: 10, Owner: "APP", Name: "T1"}},
		Columns: []DictionaryColumn{
			{TableID: 10, Name: "C1"},
		},
	}
	if issues := checkDictionaryConsistency(dict); len(issues) != 0 {
		t.Fatalf("clean dictionary reported issues: %+v", issues)
	}
}
