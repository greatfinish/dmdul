package dm

import (
	"encoding/binary"
	"encoding/hex"
	"strings"
	"testing"
)

func TestFormatColumnTypeUsesScaleForNumericAndTimeTypes(t *testing.T) {
	tests := []struct {
		name     string
		dataType string
		length   uint32
		scale    int16
		want     string
	}{
		{name: "number with precision and scale", dataType: "NUMBER", length: 10, scale: 2, want: "NUMBER(10,2)"},
		{name: "number with precision only", dataType: "NUMBER", length: 11, scale: 0, want: "NUMBER(11)"},
		{name: "plain max precision number", dataType: "NUMBER", length: 38, scale: 0, want: "NUMBER"},
		{name: "datetime precision", dataType: "datetime", length: 8, scale: 6, want: "datetime(6)"},
		{name: "varchar length", dataType: "VARCHAR2", length: 50, scale: 0, want: "VARCHAR2(50)"},
		{name: "national character length", dataType: "NATIONAL CHARACTER", length: 8, scale: 8, want: "NATIONAL CHARACTER(8)"},
		{name: "national varying length", dataType: "NATIONAL CHARACTER VARYING", length: 20, scale: 7, want: "NATIONAL CHARACTER VARYING(20)"},
		{name: "raw length", dataType: "RAW", length: 32, scale: 0, want: "RAW(32)"},
		{name: "binary float alias", dataType: "BINARY_FLOAT", length: 4, scale: 0, want: "BINARY_FLOAT"},
		{name: "xml alias", dataType: "XMLTYPE", length: 2147483647, scale: 0, want: "XMLTYPE"},
		{name: "vector float32", dataType: "VECTOR", length: 3, scale: 0x0115, want: "VECTOR(3, FLOAT32)"},
		{name: "vector float64", dataType: "VECTOR", length: 3, scale: 0x0215, want: "VECTOR(3, FLOAT64)"},
		{name: "vector int8", dataType: "VECTOR", length: 4, scale: 0x0315, want: "VECTOR(4, INT8)"},
		{name: "vector binary", dataType: "VECTOR", length: 8, scale: 0x0415, want: "VECTOR(8, BINARY)"},
		{name: "vector sparse", dataType: "VECTOR", length: 5, scale: 0x1115, want: "VECTOR(5, FLOAT32, SPARSE)"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatColumnType(tt.dataType, tt.length, tt.scale)
			if got != tt.want {
				t.Fatalf("formatColumnType() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseDDLKeyInfoRecognizesDescendingOrder(t *testing.T) {
	keys := parseDDLKeyInfo([]byte{
		0x01, 0x00, 0x41,
		0x02, 0x00, 0x44,
		0x03, 0x00, 0x99,
	})

	if len(keys) != 3 {
		t.Fatalf("len(keys) = %d, want 3", len(keys))
	}
	if keys[0].ColID != 1 || keys[0].Order != "ASC" {
		t.Fatalf("keys[0] = %+v, want col 1 ASC", keys[0])
	}
	if keys[1].ColID != 2 || keys[1].Order != "DESC" {
		t.Fatalf("keys[1] = %+v, want col 2 DESC", keys[1])
	}
	if keys[2].ColID != 3 || keys[2].Order != "FLAG_0x99" {
		t.Fatalf("keys[2] = %+v, want col 3 FLAG_0x99", keys[2])
	}
}

func TestParseDDLRoleGrantRow(t *testing.T) {
	page := make([]byte, 256)
	rowOff := 0x40
	page[rowOff] = 0
	binary.LittleEndian.PutUint16(page[rowOff+1:], 44)
	binary.LittleEndian.PutUint32(page[rowOff+4:], 50331752)
	binary.LittleEndian.PutUint32(page[rowOff+8:], 67108866)
	binary.LittleEndian.PutUint32(page[rowOff+12:], ^uint32(0))
	binary.LittleEndian.PutUint32(page[rowOff+16:], ^uint32(0))
	binary.LittleEndian.PutUint32(page[rowOff+20:], ^uint32(0))
	page[rowOff+24] = 'N'

	grant, ok := parseDDLRoleGrantRow(page, rowOff, 208, 88, uint16(rowOff), 8192)
	if !ok {
		t.Fatal("parseDDLRoleGrantRow() returned false")
	}
	if grant.GranteeID != 50331752 || grant.RoleID != 67108866 || grant.AdminOption != "N" {
		t.Fatalf("grant = %+v", grant)
	}
}

func TestParseDDLObjectPrivilegeRow(t *testing.T) {
	page := make([]byte, 256)
	rowOff := 0x40
	page[rowOff] = 0
	binary.LittleEndian.PutUint16(page[rowOff+1:], 44)
	binary.LittleEndian.PutUint32(page[rowOff+4:], 50331752)
	binary.LittleEndian.PutUint32(page[rowOff+8:], 1063)
	binary.LittleEndian.PutUint32(page[rowOff+12:], ^uint32(0))
	binary.LittleEndian.PutUint32(page[rowOff+16:], 8192)
	binary.LittleEndian.PutUint32(page[rowOff+20:], 50331649)
	page[rowOff+24] = 'N'

	grant, ok := parseDDLObjectPrivilegeRow(page, rowOff, 208, 88, uint16(rowOff), 8192)
	if !ok {
		t.Fatal("parseDDLObjectPrivilegeRow() returned false")
	}
	if grant.GranteeID != 50331752 || grant.ObjectID != 1063 || grant.Privilege != "SELECT" || grant.Grantable != "N" {
		t.Fatalf("grant = %+v", grant)
	}
}

func TestParseDDLTextRow(t *testing.T) {
	page := make([]byte, 512)
	rowOff := 0x40
	text := []byte("CREATE OR REPLACE VIEW SYSDBA.V AS SELECT 1 AS ID")
	binary.LittleEndian.PutUint16(page[rowOff:], uint16(25+len(text)+8))
	binary.LittleEndian.PutUint32(page[rowOff+2:], 0x010004E2)
	binary.LittleEndian.PutUint32(page[rowOff+6:], 0)
	binary.LittleEndian.PutUint32(page[rowOff+21:], uint32(len(text)))
	copy(page[rowOff+25:], text)

	row, ok := parseDDLTextRow(page, rowOff, 1985, 12, uint16(rowOff), 8192, textDecoder{preferred: "utf-8"})
	if !ok {
		t.Fatal("parseDDLTextRow() returned false")
	}
	if row.ID != 0x010004E2 || row.SeqNo != 0 || row.Text != string(text) {
		t.Fatalf("row = %+v", row)
	}
}

func TestParseDDLTextRowFromStandardSYSTEXTSLayout(t *testing.T) {
	page := make([]byte, 32768)
	rowOff := 0x180
	text := []byte(strings.Repeat("CREATE OR REPLACE VIEW DMTEST.V AS SELECT 1;\n", 4))
	payload := make([]byte, 13+len(text))
	payload[0] = 0x01
	payload[2] = 0x02
	binary.LittleEndian.PutUint32(payload[9:13], uint32(len(text)))
	copy(payload[13:], text)
	rowLen := 2 + 1 + 4 + 4 + 2 + len(payload) + 20
	binary.BigEndian.PutUint16(page[rowOff:], uint16(rowLen))
	page[rowOff+2] = 0
	binary.LittleEndian.PutUint32(page[rowOff+3:], 16778487)
	binary.LittleEndian.PutUint32(page[rowOff+7:], 1)
	binary.BigEndian.PutUint16(page[rowOff+11:], uint16(len(payload)))
	copy(page[rowOff+13:], payload)

	row, ok := parseDDLTextRow(page, rowOff, 3810, 18, uint16(rowOff), 32768, textDecoder{preferred: "utf-8"})
	if !ok {
		t.Fatal("parseDDLTextRow() returned false")
	}
	if row.ID != 16778487 || row.SeqNo != 1 || row.Text != string(text) {
		t.Fatalf("row = %+v", row)
	}
}

func TestRenderCreateUserUsesPlaceholderPasswordAndTablespaces(t *testing.T) {
	user := dictionaryObject{Name: "HR_TEST", Info3: 4}
	tablespaces := map[uint32]string{3: "TEMP", 4: "MAIN"}
	got := renderCreateUser(user, tablespaces)
	want := `CREATE USER HR_TEST IDENTIFIED BY "Dmdul_2026#Reset" DEFAULT TABLESPACE "MAIN" TEMPORARY TABLESPACE "TEMP";`
	if got != want {
		t.Fatalf("renderCreateUser() = %q, want %q", got, want)
	}
}

func TestRenderRecoveredSequenceUsesRuntimeLastNumber(t *testing.T) {
	got := renderRecoveredSequenceSQL(DictionarySequence{
		Owner: "APP", Name: "SEQ_ORDER", StartWith: 100, HasStartWith: true,
		MinValue: -100, HasMinValue: true, MaxValue: 0, HasMaxValue: true,
		IncrementBy: -3, LastNumber: -15, HasLastNumber: true,
		CycleFlag: "N", OrderFlag: "N", CacheSize: 0,
	})
	for _, want := range []string{
		"CREATE SEQUENCE APP.SEQ_ORDER",
		"START WITH -15",
		"INCREMENT BY -3",
		"MINVALUE -100",
		"MAXVALUE 0",
		"NOCACHE",
		"NOORDER",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("sequence DDL is missing %q:\n%s", want, got)
		}
	}
}

func TestRenderSequencesWarnsOnlyWhenRuntimeValueIsUnknown(t *testing.T) {
	var out strings.Builder
	renderSequences(&out, []DictionarySequence{
		{Owner: "APP", Name: "SEQ_KNOWN", StartWith: 1, HasStartWith: true, LastNumber: 21, HasLastNumber: true, IncrementBy: 1},
		{Owner: "APP", Name: "SEQ_UNKNOWN", StartWith: 5, HasStartWith: true, IncrementBy: 1},
	})
	got := out.String()
	if strings.Contains(got, "current value of APP.SEQ_KNOWN") {
		t.Fatalf("known sequence emitted a runtime warning:\n%s", got)
	}
	if !strings.Contains(got, "current value of APP.SEQ_UNKNOWN was not recovered") {
		t.Fatalf("unknown sequence did not emit a runtime warning:\n%s", got)
	}
}

func TestRenderRecoveredSequenceWrapsCyclicLastNumber(t *testing.T) {
	got := renderRecoveredSequenceSQL(DictionarySequence{
		Owner: "APP", Name: "SEQ_CYCLE", IncrementBy: 1,
		MinValue: 1, HasMinValue: true, MaxValue: 5, HasMaxValue: true,
		LastNumber: 6, HasLastNumber: true, CycleFlag: "Y",
	})
	if !strings.Contains(got, "START WITH 1") || strings.Contains(got, "NEXTVAL") {
		t.Fatalf("cyclic sequence was not wrapped to MINVALUE:\n%s", got)
	}
}

func TestRenderRecoveredSequencePreservesExhaustedState(t *testing.T) {
	seq := DictionarySequence{
		Owner: "APP", Name: "SEQ_EXHAUSTED", IncrementBy: 1,
		MinValue: 1, HasMinValue: true, MaxValue: 5, HasMaxValue: true,
		LastNumber: 6, HasLastNumber: true, CycleFlag: "N",
	}
	got := renderRecoveredSequenceSQL(seq)
	if !strings.Contains(got, "START WITH 5") || !strings.Contains(got, "SELECT APP.SEQ_EXHAUSTED.NEXTVAL") {
		t.Fatalf("exhausted sequence state was not preserved:\n%s", got)
	}
	var out strings.Builder
	renderSequences(&out, []DictionarySequence{seq})
	if !strings.Contains(out.String(), "was exhausted") {
		t.Fatalf("exhausted sequence notice was not emitted:\n%s", out.String())
	}
}

func TestRenderConstraintsOmitsUnusableDMGeneratedName(t *testing.T) {
	tables := map[uint32]dictionaryObject{
		100: {ID: 100, Owner: "APP", Name: "T"},
	}
	columns := map[tableColKey]columnDef{
		{tableID: 100, colID: 1}: {TableID: 100, ColID: 1, Name: "ID", DataType: "INT"},
	}
	objects := map[uint32]dictionaryObject{
		200: {ID: 200, ParentID: 100, Name: "INDEX_200", Type: "INDEX"},
	}
	indexes := map[uint32]indexDef{
		200: {ID: 200, KeyNum: 1, Keys: []indexKey{{ColID: 1, Order: "ASC"}}},
	}
	constraints := []constraintDef{{ID: 300, TableID: 100, Type: "P", Valid: "Y", IndexID: 200}}
	constraintObjects := map[uint32]dictionaryObject{
		300: {ID: 300, Name: "CONS134218829"},
	}
	var out strings.Builder
	renderConstraints(&out, objects, tables, columns, constraintObjects, indexes, constraints, newOwnerMatcher("all"), newTableNameMatcher("all"))
	if got := out.String(); !strings.Contains(got, "ALTER TABLE APP.T ADD PRIMARY KEY (ID);") || strings.Contains(got, "CONS134218829") {
		t.Fatalf("unexpected generated constraint DDL:\n%s", got)
	}
}

func TestRecoveredConstraintNameClausePreservesUserName(t *testing.T) {
	if got := recoveredConstraintNameClause("PK_ORDERS"); got != "CONSTRAINT PK_ORDERS " {
		t.Fatalf("recoveredConstraintNameClause() = %q", got)
	}
}

func TestResolveSchemaNamePrefersDictionarySchemaRows(t *testing.T) {
	schemaNames := schemaNamesFromDictionaryObjects(map[uint32]dictionaryObject{
		150995944: {ID: 150995944, Name: "MMIS_INNOVATION", Type: "SCH", Valid: "Y"},
	})

	got := resolveSchemaName(150995944, schemaNames)
	if got != "MMIS_INNOVATION" {
		t.Fatalf("resolveSchemaName() = %q, want MMIS_INNOVATION", got)
	}
}

func TestQuoteIdentQuotesReservedWords(t *testing.T) {
	tests := map[string]string{
		"RIS":  "RIS",
		"KEY":  `"KEY"`,
		"TYPE": `"TYPE"`,
		"TIME": `"TIME"`,
		"中文字段": `"中文字段"`,
	}

	for input, want := range tests {
		if got := quoteIdent(input); got != want {
			t.Fatalf("quoteIdent(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestEnsureSQLTerminatorTrimsRecoveredBinaryTail(t *testing.T) {
	sql := "CREATE OR REPLACE TRIGGER APP.TRG BEFORE INSERT ON APP.T BEGIN NULL; END;\x00\x00garbage"
	got := ensureSQLTerminator(sql)
	want := "CREATE OR REPLACE TRIGGER APP.TRG BEFORE INSERT ON APP.T BEGIN NULL; END;"
	if got != want {
		t.Fatalf("ensureSQLTerminator() = %q, want %q", got, want)
	}
}

func TestDictionaryObjectTableFlags(t *testing.T) {
	heap := dictionaryObject{Info1: 0x10}
	if heap.isIOTTable() {
		t.Fatalf("heap.isIOTTable() = true, want false")
	}
	if got := heap.tableStorageOrganization(); got != heapStorageOrg {
		t.Fatalf("heap.tableStorageOrganization() = %q, want %q", got, heapStorageOrg)
	}

	iot := dictionaryObject{Info1: 0}
	if !iot.isIOTTable() {
		t.Fatalf("iot.isIOTTable() = false, want true")
	}
	if got := iot.tableStorageOrganization(); got != defaultStorageOrg {
		t.Fatalf("iot.tableStorageOrganization() = %q, want %q", got, defaultStorageOrg)
	}

	tempDelete := dictionaryObject{Info3: tableTemporaryInfo3Flag}
	if !tempDelete.isTemporaryTable() {
		t.Fatalf("tempDelete.isTemporaryTable() = false, want true")
	}
	if got := tempDelete.temporaryCommitClause(); got != tableTemporaryDeleteRowsClause {
		t.Fatalf("tempDelete.temporaryCommitClause() = %q, want %q", got, tableTemporaryDeleteRowsClause)
	}

	tempPreserve := dictionaryObject{Info3: tableTemporaryInfo3Flag | tableTemporarySessionInfo3Flag}
	if got := tempPreserve.temporaryCommitClause(); got != tableTemporaryPreserveRowsClause {
		t.Fatalf("tempPreserve.temporaryCommitClause() = %q, want %q", got, tableTemporaryPreserveRowsClause)
	}
}

func TestSystemManagedVectorObjectsAreFiltered(t *testing.T) {
	for _, name := range []string{
		"HNSW_GRAPH$IDX_V",
		"HNSW_ELEMENT$IDX_V",
		"IVFFLAT_CENTERS$IDX_V",
		"IVFFLAT_VECTORS$IDX_V",
	} {
		if !isVectorInternalTableName(name) {
			t.Fatalf("%s should be recognized as a vector internal table", name)
		}
		if !(dictionaryObject{Name: name}).isSystemManagedInternalTable() {
			t.Fatalf("%s should be filtered from user objects", name)
		}
	}
	if isVectorInternalTableName("APP_VECTOR_DATA") {
		t.Fatal("ordinary user vector table was classified as internal")
	}
}

func TestRenderDM9VectorIndexes(t *testing.T) {
	table := dictionaryObject{Owner: "DM_TEST", Name: "T_VECTOR"}
	obj := dictionaryObject{Name: "IDX_VECTOR"}
	tests := []struct {
		indexType string
		want      string
	}{
		{indexType: "HS", want: `CREATE VECTOR INDEX IDX_VECTOR ON DM_TEST.T_VECTOR (EMBEDDING) ORGANIZATION NEIGHBOR GRAPH;`},
		{indexType: "IF", want: `CREATE VECTOR INDEX IDX_VECTOR ON DM_TEST.T_VECTOR (EMBEDDING) ORGANIZATION NEIGHBOR PARTITIONS;`},
	}
	for _, tc := range tests {
		got, ok := renderCreateIndexSQL(obj, table, indexDef{Type: tc.indexType}, "EMBEDDING", nil)
		if !ok || got != tc.want {
			t.Fatalf("renderCreateIndexSQL(%s) = %q, %v; want %q", tc.indexType, got, ok, tc.want)
		}
	}
}

func TestTableStorageByIDSelectsNewestCandidate(t *testing.T) {
	tables := map[uint32]dictionaryObject{
		1023: {ID: 1023, Info1: 0x10},
	}
	indexObjects := map[uint32]dictionaryObject{
		33555455: {ID: 33555455, ParentID: 1023, Type: "TABOBJ", Subtype: "INDEX"},
		33555495: {ID: 33555495, ParentID: 1023, Type: "TABOBJ", Subtype: "INDEX"},
	}
	indexes := map[uint32]indexDef{
		33555455: {ID: 33555455, Flag: 1, KeyNum: 0, RootFile: 0, RootPage: 3392},
		33555495: {ID: 33555495, Flag: 1, KeyNum: 0, RootFile: 0, RootPage: 3168},
	}

	got := tableStorageByID(tables, indexObjects, indexes, nil)[1023]
	if got.ID != 33555495 || got.RootPage != 3168 {
		t.Fatalf("selected storage = id %d root %d, want id 33555495 root 3168", got.ID, got.RootPage)
	}
}

func TestTableStorageByIDPrefersNewestEmptyStorageAfterTruncate(t *testing.T) {
	tables := map[uint32]dictionaryObject{
		1023: {ID: 1023, Info1: 0x10},
	}
	indexObjects := map[uint32]dictionaryObject{
		33555455: {ID: 33555455, ParentID: 1023, Type: "TABOBJ", Subtype: "INDEX"},
		33555495: {ID: 33555495, ParentID: 1023, Type: "TABOBJ", Subtype: "INDEX"},
	}
	indexes := map[uint32]indexDef{
		33555455: {ID: 33555455, Flag: 1, KeyNum: 0, RootFile: 0, RootPage: 3168},
		33555495: {ID: 33555495, Flag: 1, KeyNum: 0, RootFile: -1, RootPage: -1},
	}

	got := tableStorageByID(tables, indexObjects, indexes, nil)[1023]
	if got.ID != 33555495 || got.RootFile != -1 || got.RootPage != -1 {
		t.Fatalf("selected storage = %#v, want newest empty storage 33555495", got)
	}
}

func TestTableStorageByIDDoesNotApplyHugeFallbackWithoutAuxiliaryTable(t *testing.T) {
	const tableID = uint32(1023)
	tables := map[uint32]dictionaryObject{
		tableID: {ID: tableID, Info1: tableHugeInfo1Flag},
	}
	currentID := uint32(33555495)
	derivedID := tableDataAssistID(tableID)
	indexObjects := map[uint32]dictionaryObject{
		currentID: {ID: currentID, ParentID: int32(tableID), Type: "TABOBJ", Subtype: "INDEX"},
	}
	indexes := map[uint32]indexDef{
		derivedID: {ID: derivedID, Flag: 1, KeyNum: 0, RootFile: 0, RootPage: 3392},
		currentID: {ID: currentID, Flag: 1, KeyNum: 0, RootFile: 0, RootPage: 3168},
	}

	got := tableStorageByID(tables, indexObjects, indexes, nil)[tableID]
	if got.ID != currentID || got.RootPage != 3168 {
		t.Fatalf("selected storage = id %d root %d, want non-HUGE current storage %d root 3168", got.ID, got.RootPage, currentID)
	}
}

func TestStorageClauseDistinguishesHeapAndIOTTables(t *testing.T) {
	tablespaces := map[uint32]string{4: "MAIN"}

	if got := storageClause(4, tablespaces, defaultStorageOrg); got != "\nSTORAGE(ON \"MAIN\", CLUSTERBTR)" {
		t.Fatalf("IOT storageClause() = %q", got)
	}
	if got := storageClause(4, tablespaces, heapStorageOrg); got != "\nSTORAGE(ON \"MAIN\", NOBRANCH)" {
		t.Fatalf("heap storageClause() = %q", got)
	}
}

func TestParseDDLPartitionRow(t *testing.T) {
	page := make([]byte, 8192)
	rowOff := 0x100
	page[rowOff] = 0x2C
	page[rowOff+1] = 0x40
	page[rowOff+2] = 0x00
	putUint32LE(page[rowOff+sysHPartBaseTableIDOffset:], 1044)
	putUint32LE(page[rowOff+sysHPartPartTableIDOffset:], 1045)
	pos := rowOff + sysHPartTypeOffset
	page[pos] = 0x85
	copy(page[pos+1:], []byte("RANGE"))
	pos += 6
	page[pos] = 0x87
	copy(page[pos+1:], []byte("P202401"))
	pos += 8
	copy(page[pos:], []byte("2024-02-01"))

	part, ok := parseDDLPartitionRow(page, rowOff, 10, 2, uint16(rowOff), 8192, textDecoder{preferred: "utf-8"})
	if !ok {
		t.Fatal("parseDDLPartitionRow() returned false")
	}
	if part.BaseTableID != 1044 || part.PartTableID != 1045 {
		t.Fatalf("partition ids = %d/%d, want 1044/1045", part.BaseTableID, part.PartTableID)
	}
	if part.Type != "RANGE" || part.Name != "P202401" {
		t.Fatalf("partition type/name = %q/%q, want RANGE/P202401", part.Type, part.Name)
	}
	if got := partitionHighValuePreview(part.HighValue); got != "2024-02-01" {
		t.Fatalf("partitionHighValuePreview() = %q, want 2024-02-01", got)
	}
}

func TestParseTabPartInfoRow(t *testing.T) {
	page := make([]byte, 256)
	pos := 64
	binary.BigEndian.PutUint16(page[pos:], 47)
	page[pos+2] = 0xC0 // STR_VALUE is NULL; the other four columns are present.
	putUint32LE(page[pos+4:], 1049)
	typeOff := pos + 12
	page[typeOff] = 0x87
	copy(page[typeOff+1:], []byte("TABPART"))
	binOff := typeOff + 8
	page[binOff] = 0x87
	copy(page[binOff+1:], []byte{0x01, 0x00, 0x02, 0x00, 0x00, 0x00, 0x00})

	tableID, colIDs, ok := parseTabPartInfoRow(page, pos, uint32(len(page)), textDecoder{preferred: "utf-8"})
	if !ok {
		t.Fatal("parseTabPartInfoRow() returned false")
	}
	if tableID != 1049 {
		t.Fatalf("tableID = %d, want 1049", tableID)
	}
	if len(colIDs) != 1 || colIDs[0] != 2 {
		t.Fatalf("colIDs = %+v, want [2]", colIDs)
	}
}

func TestRenderRangePartitionClauseWithIntegerBoundaries(t *testing.T) {
	decode := func(value string) []byte {
		raw, err := hex.DecodeString(value)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	parts := []PartitionInfo{
		{BaseTableID: 1169, PartTableID: 1170, Type: "RANGE", Name: "p1", HighValue: decode("0100000003003000000030000000000700070007000000000001E8030000000000000192040000000000000100000000")},
		{BaseTableID: 1169, PartTableID: 1171, Type: "RANGE", Name: "p2", HighValue: decode("0100000003003000000030000000000700070007000000000001D0070000000000000193040000000000000100000000")},
		{BaseTableID: 1169, PartTableID: 1172, Type: "RANGE", Name: "p3", HighValue: decode("010000000300300000003000000000070007000700000000000200000000000000000194040000000000000100000000")},
	}
	wrapped := append([]byte{0xB0}, parts[0].HighValue...)
	wrapped = append(wrapped, make([]byte, dataRowControlTailLen)...)
	normalized := normalizePartitionHighValue(wrapped)
	if hex.EncodeToString(normalized) != hex.EncodeToString(parts[0].HighValue) {
		t.Fatalf("normalized HIGH_VALUE lost bytes: got=%X want=%X", normalized, parts[0].HighValue)
	}
	columns := map[tableColKey]columnDef{
		{tableID: 1169, colID: 0}: {TableID: 1169, ColID: 0, Name: "id", DataType: "INT"},
	}
	got := renderPartitionClause(1169, parts, []uint16{0}, columns)
	want := "\nPARTITION BY RANGE (\"id\")\n(\n" +
		"    PARTITION \"p1\" VALUES LESS THAN (1000),\n" +
		"    PARTITION \"p2\" VALUES LESS THAN (2000),\n" +
		"    PARTITION \"p3\" VALUES LESS THAN (MAXVALUE)\n)"
	if got != want {
		t.Fatalf("renderPartitionClause() =\n%s\nwant:\n%s", got, want)
	}
}

func putUint32LE(dst []byte, value uint32) {
	dst[0] = byte(value)
	dst[1] = byte(value >> 8)
	dst[2] = byte(value >> 16)
	dst[3] = byte(value >> 24)
}

func TestRenderTriggersAndRoutinesEmitPLSQLSlashTerminator(t *testing.T) {
	var out strings.Builder
	renderTriggers(&out, []DictionaryTrigger{{
		Owner: "APP", Name: "TRG1", TableOwner: "APP", TableName: "T1",
		SQL: "create or replace trigger APP.TRG1 before insert on APP.T1 for each row\nbegin\n  null;\nend;",
	}})
	renderRoutines(&out, []DictionaryRoutine{{
		Owner: "APP", Name: "P1", ObjectType: "PROCEDURE",
		SQL: "create or replace procedure APP.P1 as\nbegin\n  null;\nend;",
	}})
	got := out.String()
	if !strings.Contains(got, "end;\n/\n") {
		t.Fatalf("PL/SQL blocks must be followed by a '/' line for disql, got:\n%s", got)
	}
	if strings.Count(got, "\n/\n") != 2 {
		t.Fatalf("expected 2 '/' terminators (trigger + procedure), got:\n%s", got)
	}
}

func TestRenderViewsQualifiesRecoveredUnqualifiedName(t *testing.T) {
	var out strings.Builder
	renderViews(&out, []DictionaryView{{
		Owner: "DMTEST", Name: "V_EMPLOYEE_ORDER_SUMMARY",
		SQL: "CREATE OR REPLACE  VIEW V_EMPLOYEE_ORDER_SUMMARY AS\nSELECT 1 AS ID;",
	}})
	got := out.String()
	if !strings.Contains(got, "CREATE OR REPLACE  VIEW DMTEST.V_EMPLOYEE_ORDER_SUMMARY AS") {
		t.Fatalf("recovered view must be qualified with its dictionary owner, got:\n%s", got)
	}
	if strings.Contains(got, "VIEW V_EMPLOYEE_ORDER_SUMMARY AS") {
		t.Fatalf("unqualified recovered view name remains in output:\n%s", got)
	}
}

func TestTableStorageOrganizationLongRowFlag(t *testing.T) {
	// bit 50 of INFO3 marks STORAGE(USING LONG ROW).
	longRowInfo3 := uint64(1) << 50
	tests := []struct {
		name  string
		info1 uint32
		info3 uint64
		want  string
	}{
		// isIOTTable: Info1 & 0xFFFF0 == 0 means IOT (CLUSTERBTR); any bit in
		// that mask makes it a heap table (NOBRANCH).
		{name: "iot plain", info1: 0, info3: 0, want: "CLUSTERBTR"},
		{name: "heap plain", info1: 0x10, info3: 0, want: "NOBRANCH"},
		{name: "iot long row", info1: 0, info3: longRowInfo3, want: "CLUSTERBTR, USING LONG ROW"},
		{name: "heap long row", info1: 0x10, info3: longRowInfo3, want: "NOBRANCH, USING LONG ROW"},
		{name: "long row plus other info3 bits", info1: 0, info3: longRowInfo3 | 0x1010000000010000, want: "CLUSTERBTR, USING LONG ROW"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obj := dictionaryObject{Info1: tt.info1, Info3: tt.info3}
			if got := obj.tableStorageOrganization(); got != tt.want {
				t.Fatalf("tableStorageOrganization() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRenderCreateSchemasEmitsNonDefaultOnly(t *testing.T) {
	objects := map[uint32]dictionaryObject{
		200: {ID: 200, Type: "SCH", Name: "APP", ParentID: 100, Valid: "Y"},         // default (same name)
		201: {ID: 201, Type: "SCH", Name: "APP_EXTRA", ParentID: 100, Valid: "Y"},   // extra schema
		202: {ID: 202, Type: "SCH", Name: "APP_INVALID", ParentID: 100, Valid: "N"}, // dropped
	}
	users := map[uint32]dictionaryObject{
		100: {ID: 100, Type: "USER", Name: "APP"},
	}
	var out strings.Builder
	renderCreateSchemas(&out, objects, users, newOwnerMatcher("all"))
	got := out.String()
	if !strings.Contains(got, "CREATE SCHEMA APP_EXTRA AUTHORIZATION APP;\n/") {
		t.Fatalf("non-default schema must be created with a / terminator, got:\n%s", got)
	}
	if strings.Contains(got, "CREATE SCHEMA APP AUTHORIZATION") {
		t.Fatalf("default same-name schema must not be re-created (CREATE USER makes it), got:\n%s", got)
	}
	if strings.Contains(got, "APP_INVALID") {
		t.Fatalf("dropped schema must not be emitted, got:\n%s", got)
	}
}
