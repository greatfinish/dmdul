package dm

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRepairCatalogDataTypeFromProtectedBytes(t *testing.T) {
	raw := []byte{0xE1, 0xE2, 0xE3, 'C', 'H', 'A', 'R'}
	if got := repairCatalogDataType(raw, "损坏HAR"); got != "VARCHAR" {
		t.Fatalf("repairCatalogDataType()=%q, want VARCHAR", got)
	}
	if got := repairCatalogDataType([]byte{0xE1, 0xE2, 0xE3}, "损坏"); got != "损坏" {
		t.Fatalf("ambiguous repair should preserve decoded text, got %q", got)
	}
}

func TestParseDDLIndexRowAcceptsKeylessStorageWithoutKeyInfo(t *testing.T) {
	page := make([]byte, 8192)
	base := 128
	binary.LittleEndian.PutUint32(page[base:], 33555554)
	page[base+4] = 'N'
	binary.LittleEndian.PutUint16(page[base+5:], 4)
	binary.LittleEndian.PutUint16(page[base+7:], 0)
	binary.LittleEndian.PutUint32(page[base+9:], 203072)
	copy(page[base+13:], "BT")
	binary.LittleEndian.PutUint32(page[base+19:], 1)
	binary.LittleEndian.PutUint16(page[base+23:], 0)
	page[base+31] = 0 // nullable KEYINFO is physically omitted

	index, ok := parseDDLIndexRow(page, base, uint32(len(page)))
	if !ok {
		t.Fatal("parseDDLIndexRow rejected a keyless table-data storage row")
	}
	if index.ID != 33555554 || index.GroupID != 4 || index.RootFile != 0 || index.RootPage != 203072 || index.KeyNum != 0 {
		t.Fatalf("unexpected index: %+v", index)
	}
}

func TestHugeObjectLinkingRequiresAuxiliaryTable(t *testing.T) {
	tables := map[uint32]dictionaryObject{
		100: {ID: 100, Owner: "SYSDBA", Name: "H1", Info1: tableHugeInfo1Flag},
		101: {ID: 101, Owner: "SYSDBA", Name: "H1$AUX", Info1: tableHugeInfo1Flag},
		102: {ID: 102, Owner: "SYSDBA", Name: "H1$RAUX", Info1: tableHugeInfo1Flag},
		200: {ID: 200, Owner: "SYSDBA", Name: "ORDINARY", Info1: tableHugeInfo1Flag},
	}
	linkHugeTableObjects(tables)

	if !tables[100].isHugeTable() || tables[100].HugeAuxID != 101 || tables[100].HugeRAuxID != 102 {
		t.Fatalf("HUGE table was not linked: %+v", tables[100])
	}
	if tables[200].isHugeTable() {
		t.Fatalf("an object without $AUX was misclassified as HUGE: %+v", tables[200])
	}
}

func TestHugeAuxStorageMappingChoosesNewestRoot(t *testing.T) {
	table := dictionaryObject{
		ID: 100, Owner: "SYSDBA", Name: "H1", Info1: tableHugeInfo1Flag, Info2: 4,
		HugeAuxID: 101, HugeRAuxID: 102, HugeDAuxID: 103, HugeUAuxID: 104,
	}
	oldRoot := indexDef{ID: 33555496, GroupID: 4, RootFile: 0, RootPage: 100, Flag: 1}
	newRoot := indexDef{ID: 33555553, GroupID: 4, RootFile: 0, RootPage: 200, Flag: 1}
	indexes := map[uint32]indexDef{
		newRoot.ID:     newRoot,
		newRoot.ID + 1: {ID: newRoot.ID + 1, GroupID: 4, RootFile: 0, RootPage: 201, Flag: 1},
		newRoot.ID + 2: {ID: newRoot.ID + 2, GroupID: 4, RootFile: 0, RootPage: 202, Flag: 1},
		newRoot.ID + 3: {ID: newRoot.ID + 3, GroupID: 4, RootFile: 0, RootPage: 203, Flag: 1},
	}
	storage := make(map[uint32]indexDef)
	ensureHugeAuxStorageMappings(
		map[uint32]dictionaryObject{table.ID: table}, indexes,
		map[uint32][]indexDef{table.HugeAuxID: {oldRoot, newRoot}}, storage)

	if got := storage[table.HugeAuxID]; got.ID != newRoot.ID || got.RootPage != newRoot.RootPage {
		t.Fatalf("selected stale $AUX storage: %+v", got)
	}
	if storage[table.HugeRAuxID].ID != newRoot.ID+1 || storage[table.HugeDAuxID].ID != newRoot.ID+2 || storage[table.HugeUAuxID].ID != newRoot.ID+3 {
		t.Fatalf("consecutive auxiliary storage mapping failed: %+v", storage)
	}
}

func TestApplyHugeRowUpdatesUsesRowAndColumnKey(t *testing.T) {
	columns := []columnDef{{ColID: 0}, {ColID: 1}}
	values := map[uint16]dataValue{0: {value: int32(10)}, 1: {value: "old"}}
	updates := hugeUpdateSet{
		{rowID: 7, colID: 1}: {value: "new"},
		{rowID: 8, colID: 0}: {value: int32(99)},
	}
	applyHugeRowUpdates(values, updates, 7, columns)
	if values[0].value != int32(10) || values[1].value != "new" {
		t.Fatalf("unexpected merged row: %+v", values)
	}
}

func TestRenderCreateHugeTable(t *testing.T) {
	info3 := uint64(11)<<24 | uint64(5)<<40
	table := dictionaryObject{
		ID: 100, Owner: "SYSDBA", Name: "H1", Info1: tableHugeInfo1Flag,
		Info2: 4, Info3: info3, HugeAuxID: 101, HugeRAuxID: 102,
	}
	columns := []columnDef{
		{TableID: 100, ColID: 0, Name: "ID", DataType: "INT", Length: 4, Nullable: "N"},
		{TableID: 100, ColID: 1, Name: "VAL", DataType: "VARCHAR", Length: 20, Nullable: "Y"},
	}
	var out strings.Builder
	renderCreateTables(&out,
		map[uint32]dictionaryObject{100: table},
		map[uint32][]columnDef{100: columns},
		map[tableColKey]columnDef{}, map[uint32]indexDef{},
		map[uint32][]PartitionInfo{}, map[uint32][]uint16{},
		newOwnerMatcher("all"), newTableNameMatcher("all"), map[uint32]string{4: "MAIN"})

	ddl := out.String()
	for _, want := range []string{
		"CREATE HUGE TABLE SYSDBA.H1",
		"STORAGE(SECTION(2048), FILESIZE(32), WITH DELTA, ON MAIN);",
	} {
		if !strings.Contains(ddl, want) {
			t.Fatalf("DDL does not contain %q:\n%s", want, ddl)
		}
	}
}

func TestHugeColumnSectionReaders(t *testing.T) {
	tableDir := t.TempDir()
	decoder := textDecoder{preferred: "utf-8"}

	intPayload := make([]byte, 12)
	binary.LittleEndian.PutUint32(intPayload[0:], 1)
	binary.LittleEndian.PutUint32(intPayload[4:], 20)
	negative := int32(-7)
	binary.LittleEndian.PutUint32(intPayload[8:], uint32(negative))
	writeSyntheticHugeSection(t, tableDir, 0, 0, intPayload, nil)
	intMeta := hugeColumnSection{colID: 0, section: 0, fileID: 0, offset: 4096, count: 3, nlen: 140, cprFlag: "N", encFlag: "N"}
	intReader, _, err := openHugeColumnSection(tableDir, columnDef{Name: "ID", DataType: "INT", Nullable: "N"}, intMeta, decoder)
	if err != nil {
		t.Fatalf("open fixed section: %v", err)
	}
	defer closeHugeColumnReaders([]*hugeColumnSectionReader{intReader})
	for i, want := range []int32{1, 20, -7} {
		got, err := intReader.next()
		if err != nil || got != want {
			t.Fatalf("fixed row %d: got=%v err=%v, want=%d", i+1, got, err, want)
		}
	}

	offsets := []uint32{144, ^uint32(0), 149, 152}
	textPayload := append([]byte("alpha"), []byte("尾")...)
	writeSyntheticHugeSection(t, tableDir, 1, 0, textPayload, offsets)
	textMeta := hugeColumnSection{colID: 1, section: 0, fileID: 0, offset: 4096, count: 3, nlen: 152, cprFlag: "N", encFlag: "N"}
	textReader, _, err := openHugeColumnSection(tableDir, columnDef{Name: "VAL", DataType: "VARCHAR", Nullable: "Y"}, textMeta, decoder)
	if err != nil {
		t.Fatalf("open variable section: %v", err)
	}
	defer closeHugeColumnReaders([]*hugeColumnSectionReader{textReader})
	for i, want := range []any{"alpha", nil, "尾"} {
		got, err := textReader.next()
		if err != nil || got != want {
			t.Fatalf("variable row %d: got=%v err=%v, want=%v", i+1, got, err, want)
		}
	}
}

func TestFindHugeTableDirSupportsNestedRecoveryRootsAndRejectsAmbiguity(t *testing.T) {
	dataDir := t.TempDir()
	first := filepath.Join(dataDir, "nested", "database", "HMAIN", "SCH000000123", "TAB1063")
	if err := os.MkdirAll(first, 0755); err != nil {
		t.Fatal(err)
	}
	got, err := findHugeTableDir(dataDir, 123, 1063)
	if err != nil || got != first {
		t.Fatalf("findHugeTableDir()=%q err=%v, want %q", got, err, first)
	}

	second := filepath.Join(dataDir, "HOTHER", "SCH000000123", "TAB1063")
	if err := os.MkdirAll(second, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := findHugeTableDir(dataDir, 123, 1063); err == nil || !strings.Contains(err.Error(), "multiple HFS directories") {
		t.Fatalf("ambiguous HFS roots error=%v", err)
	}
}

func TestHugeColumnSectionRejectsUnsupportedOrInvalidLayout(t *testing.T) {
	tableDir := t.TempDir()
	column := columnDef{Name: "ID", DataType: "INT", Nullable: "N"}
	decoder := textDecoder{preferred: "utf-8"}

	if _, _, err := openHugeColumnSection(tableDir, column, hugeColumnSection{section: 1, cprFlag: "Y"}, decoder); err == nil || !strings.Contains(err.Error(), "unsupported HUGE compression") {
		t.Fatalf("compressed section error=%v", err)
	}
	if _, _, err := openHugeColumnSection(tableDir, column, hugeColumnSection{section: 1, offset: 128, cprFlag: "N", encFlag: "N"}, decoder); err == nil || !strings.Contains(err.Error(), "invalid HFS offset") {
		t.Fatalf("unaligned section error=%v", err)
	}
	if _, _, err := hugeColumnSectionLayout(columnDef{Name: "AMOUNT", DataType: "DECIMAL", Nullable: "Y"}, hugeColumnSection{section: 1, offset: 4096, cprFlag: "N", encFlag: "N"}); err == nil || !strings.Contains(err.Error(), "no verified HUGE HFS decoder") {
		t.Fatalf("unverified type error=%v", err)
	}
}

func writeSyntheticHugeSection(t *testing.T, tableDir string, colID uint16, fileID int32, payload []byte, offsets []uint32) {
	t.Helper()
	headerLength := int(hugeHFSSectionHeaderSize) + len(payload) + len(offsets)*4
	raw := make([]byte, int(hugeHFSFileHeaderSize)+headerLength)
	section := raw[hugeHFSFileHeaderSize:]
	copy(section, hugeHFSSectionMagic)
	binary.LittleEndian.PutUint32(section[4:], uint32(headerLength))
	dataPos := int(hugeHFSSectionHeaderSize)
	for _, offset := range offsets {
		binary.LittleEndian.PutUint32(section[dataPos:], offset)
		dataPos += 4
	}
	copy(section[dataPos:], payload)
	path := filepath.Join(tableDir, fmt.Sprintf("COL%04d_%010d.dta", colID, fileID))
	if err := os.WriteFile(path, raw, 0644); err != nil {
		t.Fatalf("write synthetic HFS file: %v", err)
	}
}
