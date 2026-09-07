package dm

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const (
	defaultStorageOrg = "CLUSTERBTR"
	heapStorageOrg    = "NOBRANCH"
	// defaultRecoveredPassword must satisfy every DM PWD_POLICY flag
	// (length >= 9, upper + lower case, digit, punctuation, not a user name)
	// so the generated CREATE USER statements run on hardened instances.
	defaultRecoveredPassword       = "Dmdul_2026#Reset"
	tableIOTInfo1Mask              = 0xFFFF0
	tableTemporaryInfo3Flag        = 0x40
	tableTemporarySessionInfo3Flag = 0x10000
	// tableHugeInfo1Flag is present on a HUGE table and its HFS transaction
	// auxiliaries ($AUX/$RAUX/$DAUX/$UAUX). Verified against DM8 HUGE tables
	// with different SECTION and FILESIZE settings.
	tableHugeInfo1Flag = uint32(0x200000)
	// tableLongRowInfo3Flag is bit 50 of SYSOBJECTS.INFO3, set for tables
	// created with STORAGE(USING LONG ROW). Verified by diffing minimal
	// plain vs USING LONG ROW tables: only this bit changes.
	tableLongRowInfo3Flag            = uint64(1) << 50
	longRowStorageOrg                = "USING LONG ROW"
	hugeStorageOrg                   = "HUGE"
	defaultHugeSectionRows           = uint32(65536)
	defaultHugeFileSizeMB            = uint32(64)
	tableTemporaryDeleteRowsClause   = "ON COMMIT DELETE ROWS"
	tableTemporaryPreserveRowsClause = "ON COMMIT PRESERVE ROWS"
	sysObjectsInfo1Offset            = 0x1F
	sysObjectsInfo3Offset            = 0x27
)

var builtInUserNames = map[string]bool{
	"SYS": true, "SYSDBA": true, "SYSAUDITOR": true, "SYSSSO": true,
	"CTISYS": true, "SYSJOB": true,
}

var builtInRoleNames = map[string]bool{
	"DBA": true, "PUBLIC": true, "RESOURCE": true, "SOI": true, "SVI": true,
	"VTI": true, "SYS_ADMIN": true,
	"DB_AUDIT_ADMIN": true, "DB_AUDIT_OPER": true, "DB_AUDIT_PUBLIC": true,
	"DB_AUDIT_SOI": true, "DB_AUDIT_SVI": true, "DB_AUDIT_VTI": true,
	"DB_POLICY_ADMIN": true, "DB_POLICY_OPER": true, "DB_POLICY_PUBLIC": true,
	"DB_POLICY_SOI": true, "DB_POLICY_SVI": true, "DB_POLICY_VTI": true,
}

var regularIdentifierPattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

var reservedIdentifierNames = map[string]bool{
	"ADD": true, "ALTER": true, "AND": true, "AS": true, "BETWEEN": true,
	"BY": true, "CASE": true, "CHECK": true, "COMMENT": true, "CONSTRAINT": true,
	"CREATE": true, "DATE": true, "DEFAULT": true, "DELETE": true, "DISTINCT": true,
	"DROP": true, "ELSE": true, "END": true, "EXISTS": true, "FOREIGN": true,
	"FROM": true, "GRANT": true, "GROUP": true, "HAVING": true, "IN": true,
	"INDEX": true, "INSERT": true, "INTERSECT": true, "INTO": true, "IS": true,
	"KEY": true, "LEVEL": true, "LIKE": true, "MINUS": true, "NOT": true,
	"NULL": true, "ON": true, "OR": true, "ORDER": true, "PRIMARY": true,
	"PROCEDURE": true, "REFERENCES": true, "REVOKE": true, "ROLE": true,
	"SELECT": true, "SEQUENCE": true, "SET": true, "SIZE": true, "TABLE": true,
	"THEN": true, "TIME": true, "TIMESTAMP": true, "TRIGGER": true, "TYPE": true,
	"UNION": true, "UNIQUE": true, "UPDATE": true, "USER": true, "VALUES": true,
	"VIEW": true, "WHEN": true, "WHERE": true,
}

type DDLExportOptions struct {
	SystemPath      string
	SystemReader    SizedReaderAt
	ControlPath     string
	ControlDULPath  string
	OutputPath      string
	TableOutputPath func(owner string, table string, tableID uint32) string
	OwnerFilter     string
	TableFilter     string
	Charset         string
	TablesOnly      bool
	DMPMode         DMPExportMode
	Dictionary      *DictionaryInfo
}

type DDLTableOutput struct {
	TableID    uint32
	Owner      string
	Name       string
	OutputPath string
}

type DDLExportResult struct {
	SystemPath           string
	OutputPath           string
	ExtentSize           uint32
	ExtentSizeSource     string
	PageSize             uint32
	PageCount            uint32
	ObjectCount          int
	TableCount           int
	ColumnCount          int
	IndexCount           int
	ConstraintCount      int
	TableCommentCount    int
	ColumnCommentCount   int
	PartitionedTables    int
	PartitionCount       int
	UserCount            int
	RoleCount            int
	RoleGrantCount       int
	ViewCount            int
	SequenceCount        int
	RoutineCount         int
	TriggerCount         int
	SynonymCount         int
	TabPrivilegeCount    int
	SystemPrivilegeCount int
	TableOutputs         []DDLTableOutput
	DMPMetadata          *DMPMetadataCatalog
}

type ddlLocation struct {
	PageNo     uint32
	SlotNo     uint16
	SlotOffset uint16
	RowOffset  uint64
}

type dictionaryObject struct {
	ID                uint32
	SchemaID          uint32
	Owner             string
	ParentID          int32
	Info1             uint32
	Info2             uint32
	Info3             uint64
	Info4             int64
	Payload           []byte
	Valid             string
	Name              string
	Type              string
	Subtype           string
	TargetOwner       string
	TargetName        string
	Location          ddlLocation
	HugeAuxID         uint32
	HugeRAuxID        uint32
	HugeDAuxID        uint32
	HugeUAuxID        uint32
	HugeTableFlag     bool
	HugeWithDeltaFlag bool
}

func (obj dictionaryObject) isIOTTable() bool {
	return obj.Info1&tableIOTInfo1Mask == 0
}

func (obj dictionaryObject) isTemporaryTable() bool {
	return obj.Info3&tableTemporaryInfo3Flag != 0
}

func (obj dictionaryObject) temporaryCommitClause() string {
	if !obj.isTemporaryTable() {
		return ""
	}
	if obj.Info3&tableTemporarySessionInfo3Flag != 0 {
		return tableTemporaryPreserveRowsClause
	}
	return tableTemporaryDeleteRowsClause
}

func (obj dictionaryObject) isLongRowTable() bool {
	return obj.Info3&tableLongRowInfo3Flag != 0
}

func (obj dictionaryObject) isHugeObject() bool {
	return obj.Info1&tableHugeInfo1Flag != 0
}

func (obj dictionaryObject) isHugeInternalTable() bool {
	return obj.isHugeObject() && isHugeInternalTableName(obj.Name)
}

func (obj dictionaryObject) isSystemManagedInternalTable() bool {
	return obj.isHugeInternalTable() || isVectorInternalTableName(obj.Name)
}

func (obj dictionaryObject) isHugeMainCandidate() bool {
	return obj.isHugeObject() && !obj.isHugeInternalTable()
}

func (obj dictionaryObject) isHugeTable() bool {
	return obj.isHugeMainCandidate() && (obj.HugeTableFlag || obj.HugeAuxID != 0)
}

func (obj dictionaryObject) hugeWithDelta() bool {
	return obj.HugeWithDeltaFlag || obj.HugeRAuxID != 0 || obj.HugeDAuxID != 0 || obj.HugeUAuxID != 0
}

func (obj dictionaryObject) hugeSectionRows() uint32 {
	exponent := uint32((obj.Info3 >> 24) & 0x1F)
	if exponent < 10 || exponent > 20 {
		return defaultHugeSectionRows
	}
	return uint32(1) << exponent
}

func (obj dictionaryObject) hugeFileSizeMB() uint32 {
	exponent := uint32((obj.Info3 >> 40) & 0xFF)
	if exponent < 4 || exponent > 20 {
		return defaultHugeFileSizeMB
	}
	return uint32(1) << exponent
}

func isHugeInternalTableName(name string) bool {
	upper := strings.ToUpper(strings.TrimSpace(name))
	for _, suffix := range []string{"$RAUX", "$DAUX", "$UAUX", "$AUX"} {
		if strings.HasSuffix(upper, suffix) {
			return true
		}
	}
	return false
}

func isVectorInternalTableName(name string) bool {
	upper := strings.ToUpper(strings.TrimSpace(name))
	for _, prefix := range []string{"HNSW_GRAPH$", "HNSW_ELEMENT$", "IVFFLAT_CENTERS$", "IVFFLAT_VECTORS$"} {
		if strings.HasPrefix(upper, prefix) {
			return true
		}
	}
	return false
}

func isVectorGeneratedTriggerName(name string) bool {
	upper := strings.ToUpper(strings.TrimSpace(name))
	return strings.HasPrefix(upper, "TRIG_HNSW$") || strings.HasPrefix(upper, "TRIG_IVFFLAT$")
}

func linkHugeTableObjects(tables map[uint32]dictionaryObject) {
	byOwnerName := make(map[string]uint32, len(tables))
	for id, table := range tables {
		key := strings.ToUpper(strings.TrimSpace(table.Owner)) + "\x00" + strings.ToUpper(strings.TrimSpace(table.Name))
		byOwnerName[key] = id
	}
	for id, table := range tables {
		if !table.isHugeMainCandidate() {
			continue
		}
		prefix := strings.ToUpper(strings.TrimSpace(table.Owner)) + "\x00" + strings.ToUpper(strings.TrimSpace(table.Name))
		table.HugeAuxID = byOwnerName[prefix+"$AUX"]
		table.HugeRAuxID = byOwnerName[prefix+"$RAUX"]
		table.HugeDAuxID = byOwnerName[prefix+"$DAUX"]
		table.HugeUAuxID = byOwnerName[prefix+"$UAUX"]
		table.HugeTableFlag = table.HugeAuxID != 0
		table.HugeWithDeltaFlag = table.HugeRAuxID != 0 || table.HugeDAuxID != 0 || table.HugeUAuxID != 0
		tables[id] = table
	}
}

func removeHugeInternalTables(tables map[uint32]dictionaryObject) {
	for id, table := range tables {
		if table.isHugeInternalTable() {
			delete(tables, id)
		}
	}
}

func removeSystemManagedInternalTables(tables map[uint32]dictionaryObject) {
	for id, table := range tables {
		if table.isSystemManagedInternalTable() {
			delete(tables, id)
		}
	}
}

func (obj dictionaryObject) tableStorageOrganization() string {
	if obj.isHugeTable() {
		return hugeStorageOrg
	}
	org := heapStorageOrg
	if obj.isIOTTable() {
		org = defaultStorageOrg
	}
	if obj.isLongRowTable() {
		return org + ", " + longRowStorageOrg
	}
	return org
}

type columnDef struct {
	TableID  uint32
	ColID    uint16
	Name     string
	DataType string
	Length   uint32
	Scale    int16
	Nullable string
	Default  string
	Location ddlLocation
}

type indexKey struct {
	ColID uint16
	Order string
}

type indexDef struct {
	ID          uint32
	IsUnique    string
	GroupID     uint16
	RootFile    int16
	RootPage    int32
	Type        string
	XType       uint32
	Flag        uint32
	KeyNum      uint16
	InitExtents uint16
	BatchAlloc  uint16
	MinExtents  uint16
	KeyInfo     []byte
	Keys        []indexKey
}

type constraintDef struct {
	ID        uint32
	TableID   uint32
	ColID     int16
	Type      string
	Valid     string
	IndexID   uint32
	FIndexID  uint32
	FAction   string
	TriggerID int32
	CheckInfo string
	Location  ddlLocation
}

type tableComment struct {
	Owner     string
	TableName string
	TableType string
	Comment   string
}

type columnComment struct {
	Owner      string
	TableName  string
	ColumnName string
	TableType  string
	Comment    string
}

type roleGrantDef struct {
	GranteeID   uint32
	RoleID      uint32
	AdminOption string
	Location    ddlLocation
}

type textDecoder struct {
	preferred string
}

func ExportDDL(opts DDLExportOptions) (*DDLExportResult, error) {
	if opts.SystemPath == "" {
		return nil, fmt.Errorf("export-ddl requires SYSTEM.DBF path")
	}
	if opts.OutputPath == "" && opts.TableOutputPath == nil {
		return nil, fmt.Errorf("export-ddl requires output path")
	}

	var stream *systemPageStream
	var err error
	if opts.SystemReader != nil {
		stream, err = openSystemPageStreamReader(opts.SystemPath, opts.SystemReader, opts.SystemReader.Size())
	} else {
		stream, err = openSystemPageStream(opts.SystemPath)
	}
	if err != nil {
		return nil, err
	}
	defer stream.close()

	pageSize, pageCount := stream.pageSize, stream.pageCount
	extentSize, extentSizeSource := stream.extentSize, stream.extentSrc
	preferredCharset := strings.ToLower(strings.TrimSpace(opts.Charset))
	if preferredCharset == "" || preferredCharset == "auto" {
		if charset, ok := stream.charset(); ok && charset.DecoderName != "" {
			preferredCharset = charset.DecoderName
		}
	}
	decoder := textDecoder{preferred: preferredCharset}
	ownerMatcher := newOwnerMatcher(opts.OwnerFilter)
	tableFilter := strings.TrimSpace(opts.TableFilter)
	if tableFilter == "" {
		tableFilter = "all"
	}
	tableMatcher := newTableNameMatcher(tableFilter)
	tablespaces := loadTablespaceNames(opts.ControlPath, opts.ControlDULPath)

	objects, err := stream.dictionaryObjects(decoder)
	if err != nil {
		return nil, err
	}
	schemaNames := schemaNamesFromDictionaryObjects(objects)
	for id, obj := range objects {
		obj.Owner = resolveSchemaName(obj.SchemaID, schemaNames)
		objects[id] = obj
	}

	tables := make(map[uint32]dictionaryObject)
	indexObjects := make(map[uint32]dictionaryObject)
	constraintObjects := make(map[uint32]dictionaryObject)
	users := make(map[uint32]dictionaryObject)
	roles := make(map[uint32]dictionaryObject)
	for _, obj := range objects {
		switch {
		case obj.Type == "SCHOBJ" && obj.Subtype == "UTAB":
			tables[obj.ID] = obj
		case obj.Type == "TABOBJ" && obj.Subtype == "INDEX":
			indexObjects[obj.ID] = obj
		case obj.Type == "TABOBJ" && obj.Subtype == "CONS":
			constraintObjects[obj.ID] = obj
		case obj.Type == "UR" && obj.Subtype == "USER" && isRealURObject(obj):
			users[obj.ID] = obj
		case obj.Type == "UR" && obj.Subtype == "ROLE" && isRealURObject(obj):
			roles[obj.ID] = obj
		}
	}
	linkHugeTableObjects(tables)
	removeSystemManagedInternalTables(tables)
	applyDictionaryUserOverrides(opts.Dictionary, users)
	dictionaryTables := applyDictionaryTableOverrides(opts.Dictionary, tables, tablespaces)

	partitionsByTable, err := stream.partitionsByTable(decoder, tables, ownerMatcher)
	if err != nil {
		return nil, err
	}
	partitionKeysByTable, err := stream.partitionKeysByTable(decoder, tables, ownerMatcher)
	if err != nil {
		return nil, err
	}
	applyDictionaryPartitionOverrides(opts.Dictionary, dictionaryTables, tables, ownerMatcher, partitionsByTable, partitionKeysByTable)
	var scanErr error
	iterRows := func(visit func(page []byte, pageNo uint32, slotNo uint16, slotOff uint16)) {
		if scanErr == nil {
			scanErr = stream.forEachDictionaryRow(visit)
		}
	}

	columnsByTable := make(map[uint32][]columnDef)
	columnsByTableColID := make(map[tableColKey]columnDef)
	columnCount := 0
	iterRows(func(page []byte, pageNo uint32, slotNo uint16, slotOff uint16) {
		col, ok := parseDDLColumnRow(page, int(slotOff), pageNo, slotNo, slotOff, pageSize, decoder)
		if !ok {
			return
		}
		table, ok := tables[col.TableID]
		if !ok || !ownerMatcher.allowed(table.Owner) {
			return
		}
		columnsByTableColID[tableColKey{tableID: col.TableID, colID: col.ColID}] = col
		if !tableMatcher.allowed(table.Owner, table.Name) {
			return
		}
		columnsByTable[col.TableID] = append(columnsByTable[col.TableID], col)
		columnCount++
	})
	for tableID := range columnsByTable {
		sort.Slice(columnsByTable[tableID], func(i, j int) bool {
			return columnsByTable[tableID][i].ColID < columnsByTable[tableID][j].ColID
		})
	}
	if dictColumnsByTable, dictColumnsByTableColID, dictColumnCount, ok := dictionaryColumnMaps(opts.Dictionary, dictionaryTables, tables, ownerMatcher, tableMatcher, tableNameMatcher{}); ok {
		columnsByTable = dictColumnsByTable
		columnsByTableColID = dictColumnsByTableColID
		columnCount = dictColumnCount
	}

	indexes := make(map[uint32]indexDef)
	iterRows(func(page []byte, pageNo uint32, slotNo uint16, slotOff uint16) {
		idx, ok := parseDDLIndexRow(page, int(slotOff), pageSize)
		if ok {
			indexes[idx.ID] = idx
		}
	})

	tableStorage := tableStorageByID(tables, indexObjects, indexes, tablespaces)
	applyDictionaryTableStorage(dictionaryTables, tableStorage, tablespaces)

	var constraints []constraintDef
	iterRows(func(page []byte, pageNo uint32, slotNo uint16, slotOff uint16) {
		cons, ok := parseDDLConstraintRow(page, int(slotOff), pageNo, slotNo, slotOff, pageSize, decoder)
		if !ok {
			return
		}
		table, ok := tables[cons.TableID]
		if !ok || !ownerMatcher.allowed(table.Owner) || !tableMatcher.allowed(table.Owner, table.Name) {
			return
		}
		if _, ok := constraintObjects[cons.ID]; !ok {
			return
		}
		constraints = append(constraints, cons)
	})

	tableComments := make(map[ownerTableKey]tableComment)
	iterRows(func(page []byte, pageNo uint32, slotNo uint16, slotOff uint16) {
		comment, ok := parseDDLTableCommentRow(page, int(slotOff), pageSize, decoder)
		if !ok {
			return
		}
		tableID, ok := tableIDByOwnerName(tables, columnsByTable, ownerMatcher, comment.Owner, comment.TableName)
		if !ok || tableID == 0 {
			return
		}
		if !tableMatcher.allowed(comment.Owner, comment.TableName) {
			return
		}
		tableComments[ownerTableKey{owner: comment.Owner, table: comment.TableName}] = comment
	})

	columnComments := make(map[ownerTableColumnKey]columnComment)
	iterRows(func(page []byte, pageNo uint32, slotNo uint16, slotOff uint16) {
		comment, ok := parseDDLColumnCommentRow(page, int(slotOff), pageSize, decoder)
		if !ok {
			return
		}
		tableID, ok := tableIDByOwnerName(tables, columnsByTable, ownerMatcher, comment.Owner, comment.TableName)
		if !ok {
			return
		}
		if !tableMatcher.allowed(comment.Owner, comment.TableName) {
			return
		}
		for _, col := range columnsByTable[tableID] {
			if col.Name == comment.ColumnName {
				columnComments[ownerTableColumnKey{owner: comment.Owner, table: comment.TableName, column: comment.ColumnName}] = comment
				return
			}
		}
	})

	var roleGrants []roleGrantDef
	iterRows(func(page []byte, pageNo uint32, slotNo uint16, slotOff uint16) {
		grant, ok := parseDDLRoleGrantRow(page, int(slotOff), pageNo, slotNo, slotOff, pageSize)
		if !ok {
			return
		}
		grantee, granteeIsUser := users[grant.GranteeID]
		_, granteeIsRole := roles[grant.GranteeID]
		if !granteeIsUser && !granteeIsRole {
			return
		}
		if _, ok := roles[grant.RoleID]; !ok {
			return
		}
		if granteeIsUser && !ownerMatcher.allowed(grantee.Name) {
			return
		}
		roleGrants = append(roleGrants, grant)
	})

	if scanErr != nil {
		return nil, scanErr
	}
	tableOnlyMode := opts.TablesOnly || opts.TableOutputPath != nil
	var views []DictionaryView
	var sequences []DictionarySequence
	var routines []DictionaryRoutine
	var triggers []DictionaryTrigger
	var synonyms []DictionarySynonym
	if tableOnlyMode {
		if dictTriggers, ok := dictionaryTriggersForDDL(opts.Dictionary, ownerMatcher, tableMatcher); ok {
			triggers = dictTriggers
		} else {
			texts, textErr := stream.dictionaryTexts(decoder)
			if textErr != nil {
				return nil, textErr
			}
			rawTriggers, triggerErr := stream.rawTriggerTexts(decoder)
			if triggerErr != nil {
				return nil, triggerErr
			}
			triggers = scanDictionaryTriggers(objects, texts, rawTriggers, ownerMatcher)
		}
	} else {
		texts, textErr := stream.dictionaryTexts(decoder)
		if textErr != nil {
			return nil, textErr
		}
		views = scanDictionaryViews(objects, texts, ownerMatcher)
		if dictViews, ok := dictionaryViewsForDDL(opts.Dictionary, ownerMatcher); ok {
			views = dictViews
		}
		sequences = scanDictionarySequences(objects, texts, ownerMatcher)
		enrichSequenceRuntimeValues(stream, sequences)
		if dictSequences, ok := dictionarySequencesForDDL(opts.Dictionary, ownerMatcher); ok {
			mergeSequenceRuntimeMetadata(dictSequences, sequences)
			sequences = dictSequences
		}
		rawRoutines, routineErr := stream.rawRoutineTexts(decoder)
		if routineErr != nil {
			return nil, routineErr
		}
		routines = scanDictionaryRoutines(objects, texts, rawRoutines, ownerMatcher)
		if dictRoutines, ok := dictionaryRoutinesForDDL(opts.Dictionary, ownerMatcher); ok {
			routines = dictRoutines
		}
		rawTriggers, triggerErr := stream.rawTriggerTexts(decoder)
		if triggerErr != nil {
			return nil, triggerErr
		}
		triggers = scanDictionaryTriggers(objects, texts, rawTriggers, ownerMatcher)
		if dictTriggers, ok := dictionaryTriggersForDDL(opts.Dictionary, ownerMatcher, tableMatcher); ok {
			triggers = dictTriggers
		}
		synonyms = scanDictionarySynonyms(objects, ownerMatcher)
		if dictSynonyms, ok := dictionarySynonymsForDDL(opts.Dictionary, ownerMatcher); ok {
			synonyms = dictSynonyms
		}
	}
	tabPrivileges, err := stream.tabPrivileges(objects, users, roles, columnsByTable, ownerMatcher, tableMatcher)
	if err != nil {
		return nil, err
	}
	if dictPrivileges, ok := dictionaryTabPrivilegesForDDL(opts.Dictionary, ownerMatcher, tableMatcher); ok {
		tabPrivileges = dictPrivileges
	}
	triggers = filterDDLTriggersByTable(triggers, tableMatcher)
	tabPrivileges = filterDDLPrivilegesByTable(tabPrivileges, tableMatcher)
	var systemPrivileges []DictionarySystemPrivilege
	if !tableOnlyMode && tableMatcher.all {
		if opts.Dictionary != nil && opts.Dictionary.SystemPrivileges != nil {
			systemPrivileges = opts.Dictionary.SystemPrivileges
		} else {
			systemPrivileges, err = scanSystemPrivileges(stream, nil, users, roles)
			if err != nil {
				return nil, err
			}
		}
		systemPrivileges = selectSystemPrivileges(systemPrivileges, users, roles, roleGrants, ownerMatcher)
	}

	if tableOnlyMode {
		users = make(map[uint32]dictionaryObject)
		roles = make(map[uint32]dictionaryObject)
		roleGrants = nil
		views = nil
		sequences = nil
		routines = nil
		synonyms = nil
		tabPrivileges = filterDDLPrivilegesToTables(tabPrivileges, tables, ownerMatcher, tableMatcher)
	}

	var dmpMetadata *DMPMetadataCatalog
	if opts.DMPMode != 0 {
		dmpMetadata, err = buildDMPMetadataCatalog(
			opts.DMPMode, opts.Dictionary, objects, users, roles, roleGrants,
			tables, columnsByTable, columnsByTableColID, indexObjects, indexes,
			tableStorage, partitionsByTable, partitionKeysByTable,
			constraintObjects, constraints, tableComments, columnComments,
			views, sequences, routines, triggers, synonyms, tabPrivileges, systemPrivileges,
			ownerMatcher, tableMatcher, tablespaces,
		)
		if err != nil {
			return nil, err
		}
	}

	var tableOutputs []DDLTableOutput
	if opts.TableOutputPath != nil {
		pathOwners := make(map[string]uint32)
		for _, tableID := range sortedTableIDs(tables) {
			table := tables[tableID]
			if !ownerMatcher.allowed(table.Owner) || !tableMatcher.allowed(table.Owner, table.Name) || len(columnsByTable[tableID]) == 0 {
				continue
			}
			path := strings.TrimSpace(opts.TableOutputPath(table.Owner, table.Name, tableID))
			if path == "" {
				return nil, fmt.Errorf("empty ddl output path for %s.%s", table.Owner, table.Name)
			}
			pathKey := strings.ToUpper(filepath.Clean(path))
			if priorID, exists := pathOwners[pathKey]; exists && priorID != tableID {
				return nil, fmt.Errorf("duplicate ddl output path %s", path)
			}
			pathOwners[pathKey] = tableID
			if dir := filepath.Dir(path); dir != "." && dir != "" {
				if err := os.MkdirAll(dir, 0755); err != nil {
					return nil, fmt.Errorf("create ddl output directory: %w", err)
				}
			}
			exactOwnerMatcher := newOwnerMatcher(table.Owner)
			exactTableMatcher := newTableNameMatcher(table.Owner + "." + table.Name)
			tableTriggers := filterDDLTriggersByTable(triggers, exactTableMatcher)
			tablePrivileges := filterDDLPrivilegesByTable(tabPrivileges, exactTableMatcher)
			sql := renderDDL(objects, nil, nil, nil, tables, columnsByTable, columnsByTableColID, indexObjects, indexes, tableStorage, partitionsByTable, partitionKeysByTable, constraintObjects, constraints, tableComments, columnComments, nil, nil, nil, tableTriggers, nil, tablePrivileges, nil, exactOwnerMatcher, exactTableMatcher, tablespaces)
			if err := os.WriteFile(path, []byte(sql), 0644); err != nil {
				return nil, fmt.Errorf("write ddl output for %s.%s: %w", table.Owner, table.Name, err)
			}
			tableOutputs = append(tableOutputs, DDLTableOutput{
				TableID: tableID, Owner: table.Owner, Name: table.Name, OutputPath: path,
			})
		}
	} else {
		sql := renderDDL(objects, users, roles, roleGrants, tables, columnsByTable, columnsByTableColID, indexObjects, indexes, tableStorage, partitionsByTable, partitionKeysByTable, constraintObjects, constraints, tableComments, columnComments, views, sequences, routines, triggers, synonyms, tabPrivileges, systemPrivileges, ownerMatcher, tableMatcher, tablespaces)
		if err := os.WriteFile(opts.OutputPath, []byte(sql), 0644); err != nil {
			return nil, fmt.Errorf("write ddl output: %w", err)
		}
	}

	exportedUsers := exportedUserIDs(users, ownerMatcher)
	exportedRoles := exportedRoleIDsForScope(roles, roleGrants, users, exportedUsers, ownerMatcher)
	return &DDLExportResult{
		SystemPath:           opts.SystemPath,
		OutputPath:           opts.OutputPath,
		ExtentSize:           extentSize,
		ExtentSizeSource:     extentSizeSource,
		PageSize:             pageSize,
		PageCount:            pageCount,
		ObjectCount:          len(objects),
		TableCount:           countAllowedTables(tables, columnsByTable, ownerMatcher, tableMatcher),
		ColumnCount:          columnCount,
		IndexCount:           countDDLIndexes(tables, indexObjects, indexes, ownerMatcher, tableMatcher),
		ConstraintCount:      len(constraints),
		TableCommentCount:    len(tableComments),
		ColumnCommentCount:   len(columnComments),
		PartitionedTables:    countExportedPartitionedTables(partitionsByTable, columnsByTable),
		PartitionCount:       countExportedPartitions(partitionsByTable, columnsByTable),
		UserCount:            len(exportedUsers),
		RoleCount:            len(exportedRoles),
		RoleGrantCount:       countExportedRoleGrants(roleGrants, users, roles, exportedUsers, exportedRoles),
		ViewCount:            len(views),
		SequenceCount:        len(sequences),
		RoutineCount:         len(routines),
		TriggerCount:         len(triggers),
		SynonymCount:         len(synonyms),
		TabPrivilegeCount:    len(tabPrivileges),
		SystemPrivilegeCount: len(systemPrivileges),
		TableOutputs:         tableOutputs,
		DMPMetadata:          dmpMetadata,
	}, nil
}

type tableColKey struct {
	tableID uint32
	colID   uint16
}

type ownerTableKey struct {
	owner string
	table string
}

type ownerTableColumnKey struct {
	owner  string
	table  string
	column string
}

type ownerMatcher struct {
	allUser bool
	owners  map[string]bool
}

func newOwnerMatcher(filter string) ownerMatcher {
	filter = strings.TrimSpace(filter)
	if filter == "" {
		filter = "all"
	}
	if strings.EqualFold(filter, "all") || strings.EqualFold(filter, "*") {
		return ownerMatcher{allUser: true}
	}
	result := ownerMatcher{owners: map[string]bool{}}
	for _, part := range strings.Split(filter, ",") {
		owner := strings.ToUpper(strings.TrimSpace(part))
		if owner != "" {
			result.owners[owner] = true
		}
	}
	return result
}

func (m ownerMatcher) allowed(owner string) bool {
	owner = strings.ToUpper(owner)
	if m.allUser {
		switch owner {
		case "SYS", "CTISYS", "SYSAUDITOR", "SYSSSO", "SYSJOB":
			return false
		default:
			return !strings.HasPrefix(owner, "SCHID_")
		}
	}
	return m.owners[owner]
}
