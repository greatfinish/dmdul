package dm

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/korean"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/transform"
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
	SystemPath         string
	OutputPath         string
	ExtentSize         uint32
	ExtentSizeSource   string
	PageSize           uint32
	PageCount          uint32
	ObjectCount        int
	TableCount         int
	ColumnCount        int
	IndexCount         int
	ConstraintCount    int
	TableCommentCount  int
	ColumnCommentCount int
	PartitionedTables  int
	PartitionCount     int
	UserCount          int
	RoleCount          int
	RoleGrantCount     int
	ViewCount          int
	SequenceCount      int
	RoutineCount       int
	TriggerCount       int
	SynonymCount       int
	TabPrivilegeCount  int
	TableOutputs       []DDLTableOutput
	DMPMetadata        *DMPMetadataCatalog
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
	removeHugeInternalTables(tables)
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
	tabPrivileges, err := stream.tabPrivileges(objects, users, roles, ownerMatcher, tableMatcher)
	if err != nil {
		return nil, err
	}
	if dictPrivileges, ok := dictionaryTabPrivilegesForDDL(opts.Dictionary, ownerMatcher, tableMatcher); ok {
		tabPrivileges = dictPrivileges
	}
	triggers = filterDDLTriggersByTable(triggers, tableMatcher)
	tabPrivileges = filterDDLPrivilegesByTable(tabPrivileges, tableMatcher)

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
			views, sequences, routines, triggers, synonyms, tabPrivileges,
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
			sql := renderDDL(objects, nil, nil, nil, tables, columnsByTable, columnsByTableColID, indexObjects, indexes, tableStorage, partitionsByTable, partitionKeysByTable, constraintObjects, constraints, tableComments, columnComments, nil, nil, nil, tableTriggers, nil, tablePrivileges, exactOwnerMatcher, exactTableMatcher, tablespaces)
			if err := os.WriteFile(path, []byte(sql), 0644); err != nil {
				return nil, fmt.Errorf("write ddl output for %s.%s: %w", table.Owner, table.Name, err)
			}
			tableOutputs = append(tableOutputs, DDLTableOutput{
				TableID: tableID, Owner: table.Owner, Name: table.Name, OutputPath: path,
			})
		}
	} else {
		sql := renderDDL(objects, users, roles, roleGrants, tables, columnsByTable, columnsByTableColID, indexObjects, indexes, tableStorage, partitionsByTable, partitionKeysByTable, constraintObjects, constraints, tableComments, columnComments, views, sequences, routines, triggers, synonyms, tabPrivileges, ownerMatcher, tableMatcher, tablespaces)
		if err := os.WriteFile(opts.OutputPath, []byte(sql), 0644); err != nil {
			return nil, fmt.Errorf("write ddl output: %w", err)
		}
	}

	exportedUsers := exportedUserIDs(users, ownerMatcher)
	exportedRoles := exportedRoleIDs(roles, roleGrants, users, exportedUsers)
	return &DDLExportResult{
		SystemPath:         opts.SystemPath,
		OutputPath:         opts.OutputPath,
		ExtentSize:         extentSize,
		ExtentSizeSource:   extentSizeSource,
		PageSize:           pageSize,
		PageCount:          pageCount,
		ObjectCount:        len(objects),
		TableCount:         countAllowedTables(tables, columnsByTable, ownerMatcher, tableMatcher),
		ColumnCount:        columnCount,
		IndexCount:         countDDLIndexes(tables, indexObjects, indexes, ownerMatcher, tableMatcher),
		ConstraintCount:    len(constraints),
		TableCommentCount:  len(tableComments),
		ColumnCommentCount: len(columnComments),
		PartitionedTables:  countExportedPartitionedTables(partitionsByTable, columnsByTable),
		PartitionCount:     countExportedPartitions(partitionsByTable, columnsByTable),
		UserCount:          len(exportedUsers),
		RoleCount:          len(exportedRoles),
		RoleGrantCount:     countExportedRoleGrants(roleGrants, users, roles, exportedUsers, exportedRoles),
		ViewCount:          len(views),
		SequenceCount:      len(sequences),
		RoutineCount:       len(routines),
		TriggerCount:       len(triggers),
		SynonymCount:       len(synonyms),
		TabPrivilegeCount:  len(tabPrivileges),
		TableOutputs:       tableOutputs,
		DMPMetadata:        dmpMetadata,
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

func iterDictionaryRowsInPage(page []byte, pageSize uint32, pageNo uint32, visit func(page []byte, pageNo uint32, slotNo uint16, slotOff uint16)) {
	if len(page) < int(pageSize) || len(page) < sysObjectsSlotCountOff+2 {
		return
	}
	slotCount := binary.LittleEndian.Uint16(page[sysObjectsSlotCountOff:])
	if slotCount == 0 || slotCount >= 2048 {
		return
	}
	slotArrayStart := int(pageSize) - pageSlotTrailerLenForPage(page) - int(slotCount)*2
	if slotArrayStart < 0x40 || slotArrayStart >= int(pageSize) {
		return
	}
	for slotNo := uint16(0); slotNo < slotCount; slotNo++ {
		pos := slotArrayStart + int(slotNo)*2
		slotOff := binary.LittleEndian.Uint16(page[pos:])
		if slotOff == 0 || int(slotOff) >= int(pageSize) {
			continue
		}
		visit(page, pageNo, slotNo, slotOff)
	}
}

func parseDDLObjectRow(page []byte, rowOff int, pageNo uint32, slotNo uint16, slotOff uint16, pageSize uint32, decoder textDecoder) (dictionaryObject, bool) {
	if rowOff+0x50 >= int(pageSize) {
		return dictionaryObject{}, false
	}
	name, next, ok := readDDLShortString(page, rowOff+0x40, decoder, false)
	if !ok {
		return dictionaryObject{}, false
	}
	objType, next, ok := readDDLShortString(page, next, decoder, false)
	if !ok {
		return dictionaryObject{}, false
	}
	subtype, subtypeNext, ok := readOptionalDDLObjectSubtype(page, next, decoder, objType)
	if !ok {
		return dictionaryObject{}, false
	}
	if !isLikelyDictionaryType(objType, subtype) {
		return dictionaryObject{}, false
	}
	targetOwner, targetName := "", ""
	if objType == "DSYNOM" || subtype == "SYNOM" {
		targetOwner, targetName = parseDDLSynonymTarget(page, subtypeNext, decoder)
	}
	payload := parseDDLObjectPayload(page, subtypeNext)
	valid := ""
	if b := page[rowOff+0x3F]; b == 'Y' || b == 'N' {
		valid = string([]byte{b})
	}
	rowAbs := uint64(pageNo)*uint64(pageSize) + uint64(rowOff)
	schemaID := binary.LittleEndian.Uint32(page[rowOff+0x0B:])
	return dictionaryObject{
		ID:          binary.LittleEndian.Uint32(page[rowOff+0x07:]),
		SchemaID:    schemaID,
		Owner:       schemaName(schemaID),
		ParentID:    int32(binary.LittleEndian.Uint32(page[rowOff+0x0F:])),
		Info1:       binary.LittleEndian.Uint32(page[rowOff+sysObjectsInfo1Offset:]),
		Info2:       binary.LittleEndian.Uint32(page[rowOff+0x23:]),
		Info3:       binary.LittleEndian.Uint64(page[rowOff+sysObjectsInfo3Offset:]),
		Info4:       int64(binary.LittleEndian.Uint64(page[rowOff+0x2F:])),
		Payload:     payload,
		Valid:       valid,
		Name:        name,
		Type:        objType,
		Subtype:     subtype,
		TargetOwner: targetOwner,
		TargetName:  targetName,
		Location:    ddlLocation{PageNo: pageNo, SlotNo: slotNo, SlotOffset: slotOff, RowOffset: rowAbs},
	}, true
}

func isLikelyDictionaryType(objType string, subtype string) bool {
	switch objType {
	case "UR", "SCH", "DIR", "PROFILE", "SCHOBJ", "DMNOBJ", "TABOBJ", "DSYNOM":
	default:
		return false
	}
	if subtype == "" {
		return objType == "SCH" || objType == "DIR" || objType == "PROFILE" || objType == "DSYNOM"
	}
	if len(subtype) > 16 {
		return false
	}
	for _, ch := range subtype {
		if ch < 32 || ch > 126 {
			return false
		}
	}
	return true
}

func readOptionalDDLObjectSubtype(page []byte, markerOff int, decoder textDecoder, objType string) (string, int, bool) {
	if markerOff < len(page) {
		marker := page[markerOff]
		if marker >= 0x80 && marker <= 0xBF {
			value, _, ok := readDDLShortString(page, markerOff, decoder, false)
			if ok {
				_, next, _ := readDDLShortString(page, markerOff, decoder, false)
				return value, next, true
			}
		}
	}
	switch objType {
	case "SCH", "DIR", "PROFILE", "DSYNOM":
		return "", markerOff, true
	default:
		return "", markerOff, false
	}
}

func parseDDLObjectPayload(page []byte, pos int) []byte {
	if pos >= len(page) {
		return nil
	}
	marker := page[pos]
	if marker < 0x80 || marker > 0xBF {
		return nil
	}
	length := int(marker - 0x80)
	start := pos + 1
	end := start + length
	if length <= 0 || end > len(page) {
		return nil
	}
	return append([]byte(nil), page[start:end]...)
}

func parseDDLColumnRow(page []byte, rowOff int, pageNo uint32, slotNo uint16, slotOff uint16, pageSize uint32, decoder textDecoder) (columnDef, bool) {
	if rowOff+0x30 >= int(pageSize) {
		return columnDef{}, false
	}
	rowLen := binary.LittleEndian.Uint16(page[rowOff+0x01:])
	if rowLen < 0x20 || rowLen > 0x300 {
		return columnDef{}, false
	}
	nullable := ""
	if b := page[rowOff+0x11]; b == 'Y' || b == 'N' {
		nullable = string([]byte{b})
	}
	name, next, ok := readDDLShortString(page, rowOff+0x16, decoder, false)
	if !ok {
		return columnDef{}, false
	}
	typeMarkerOff := next
	dataType, next, ok := readDDLShortString(page, next, decoder, false)
	if !ok {
		return columnDef{}, false
	}
	if typeMarkerOff < len(page) && page[typeMarkerOff] >= 0x80 && page[typeMarkerOff] <= 0xBF {
		rawLength := int(page[typeMarkerOff] - 0x80)
		rawStart := typeMarkerOff + 1
		if rawStart+rawLength <= len(page) {
			dataType = repairCatalogDataType(page[rawStart:rawStart+rawLength], dataType)
		}
	}
	if !isSafeShortText(name) || !isSafeShortText(dataType) {
		return columnDef{}, false
	}
	scale := int16(binary.LittleEndian.Uint16(page[rowOff+0x0F:]))
	dataType = normalizeCatalogColumnType(dataType, scale)
	rowAbs := uint64(pageNo)*uint64(pageSize) + uint64(rowOff)
	return columnDef{
		TableID:  binary.LittleEndian.Uint32(page[rowOff+0x05:]),
		ColID:    binary.LittleEndian.Uint16(page[rowOff+0x09:]),
		Name:     name,
		DataType: dataType,
		Length:   binary.LittleEndian.Uint32(page[rowOff+0x0B:]),
		Scale:    scale,
		Nullable: nullable,
		Default:  parseDDLDefault(page, next, decoder),
		Location: ddlLocation{PageNo: pageNo, SlotNo: slotNo, SlotOffset: slotOff, RowOffset: rowAbs},
	}, true
}

func parseDDLIndexRow(page []byte, slotOff int, pageSize uint32) (indexDef, bool) {
	for delta := 0; delta < 16; delta++ {
		base := slotOff + delta
		if base+40 >= int(pageSize) {
			continue
		}
		isUnique := page[base+4]
		if isUnique != 'Y' && isUnique != 'N' {
			continue
		}
		idxType := string(page[base+13 : base+15])
		switch idxType {
		case "BT", "BM", "AR", "IF", "HS", "MP":
		default:
			continue
		}
		keyNum := binary.LittleEndian.Uint16(page[base+23:])
		if keyNum == 0 {
			// KEYINFO is nullable. DM can omit the trailing NULL value entirely
			// for a keyless table-data storage row, so there is no short-string
			// marker at base+31. Requiring 0x80 here used to drop ordinary heap
			// and HUGE $RAUX storage roots and force full-file fallbacks.
			return indexDef{
				ID:          binary.LittleEndian.Uint32(page[base:]),
				IsUnique:    string([]byte{isUnique}),
				GroupID:     binary.LittleEndian.Uint16(page[base+5:]),
				RootFile:    int16(binary.LittleEndian.Uint16(page[base+7:])),
				RootPage:    int32(binary.LittleEndian.Uint32(page[base+9:])),
				Type:        idxType,
				XType:       binary.LittleEndian.Uint32(page[base+15:]),
				Flag:        binary.LittleEndian.Uint32(page[base+19:]),
				KeyNum:      keyNum,
				InitExtents: binary.LittleEndian.Uint16(page[base+25:]),
				BatchAlloc:  binary.LittleEndian.Uint16(page[base+27:]),
				MinExtents:  binary.LittleEndian.Uint16(page[base+29:]),
			}, true
		}
		keyMarker := page[base+31]
		if keyMarker < 0x80 || keyMarker > 0xBF {
			continue
		}
		keyLen := int(keyMarker - 0x80)
		keyStart := base + 32
		keyEnd := keyStart + keyLen
		if keyEnd > int(pageSize) {
			continue
		}
		if keyNum*3 != uint16(keyLen) {
			continue
		}
		keyInfo := append([]byte(nil), page[keyStart:keyEnd]...)
		return indexDef{
			ID:          binary.LittleEndian.Uint32(page[base:]),
			IsUnique:    string([]byte{isUnique}),
			GroupID:     binary.LittleEndian.Uint16(page[base+5:]),
			RootFile:    int16(binary.LittleEndian.Uint16(page[base+7:])),
			RootPage:    int32(binary.LittleEndian.Uint32(page[base+9:])),
			Type:        idxType,
			XType:       binary.LittleEndian.Uint32(page[base+15:]),
			Flag:        binary.LittleEndian.Uint32(page[base+19:]),
			KeyNum:      keyNum,
			InitExtents: binary.LittleEndian.Uint16(page[base+25:]),
			BatchAlloc:  binary.LittleEndian.Uint16(page[base+27:]),
			MinExtents:  binary.LittleEndian.Uint16(page[base+29:]),
			KeyInfo:     keyInfo,
			Keys:        parseDDLKeyInfo(keyInfo),
		}, true
	}
	return indexDef{}, false
}

func parseDDLConstraintRow(page []byte, slotOff int, pageNo uint32, slotNo uint16, rawSlotOff uint16, pageSize uint32, decoder textDecoder) (constraintDef, bool) {
	for delta := 0; delta < 16; delta++ {
		base := slotOff + delta
		if base+0x40 >= int(pageSize) {
			continue
		}
		typ := page[base+0x0A]
		valid := page[base+0x0B]
		if !strings.ContainsRune("PCUFR", rune(typ)) {
			continue
		}
		if valid != 'Y' && valid != 'N' {
			continue
		}
		tableID := binary.LittleEndian.Uint32(page[base+0x04:])
		if tableID == 0 {
			continue
		}
		rowAbs := uint64(pageNo)*uint64(pageSize) + uint64(slotOff)
		cons := constraintDef{
			ID:        binary.LittleEndian.Uint32(page[base:]),
			TableID:   tableID,
			ColID:     int16(binary.LittleEndian.Uint16(page[base+0x08:])),
			Type:      string([]byte{typ}),
			Valid:     string([]byte{valid}),
			IndexID:   binary.LittleEndian.Uint32(page[base+0x0C:]),
			CheckInfo: parseDDLVarcharAt(page, base+0x1A, decoder),
			Location:  ddlLocation{PageNo: pageNo, SlotNo: slotNo, SlotOffset: rawSlotOff, RowOffset: rowAbs},
		}
		if typ == 'F' || typ == 'R' {
			cons.FIndexID = binary.LittleEndian.Uint32(page[base+0x10:])
			cons.FAction = strings.TrimSpace(string(page[base+0x14 : base+0x16]))
			cons.TriggerID = int32(binary.LittleEndian.Uint32(page[base+0x16:]))
		}
		return cons, true
	}
	return constraintDef{}, false
}

func parseDDLRoleGrantRow(page []byte, slotOff int, pageNo uint32, slotNo uint16, rawSlotOff uint16, pageSize uint32) (roleGrantDef, bool) {
	for delta := 0; delta < 4; delta++ {
		base := slotOff + delta
		if base+44 > int(pageSize) {
			continue
		}
		rowLen := binary.LittleEndian.Uint16(page[base+1:])
		if rowLen != 44 {
			continue
		}
		colID := int32(binary.LittleEndian.Uint32(page[base+12:]))
		privID := int32(binary.LittleEndian.Uint32(page[base+16:]))
		grantor := int32(binary.LittleEndian.Uint32(page[base+20:]))
		grantable := page[base+24]
		if colID != -1 || privID != -1 || grantor != -1 {
			continue
		}
		if grantable != 'Y' && grantable != 'N' {
			continue
		}
		granteeID := binary.LittleEndian.Uint32(page[base+4:])
		roleID := binary.LittleEndian.Uint32(page[base+8:])
		if granteeID == 0 || roleID == 0 || granteeID == roleID {
			continue
		}
		rowAbs := uint64(pageNo)*uint64(pageSize) + uint64(base)
		return roleGrantDef{
			GranteeID:   granteeID,
			RoleID:      roleID,
			AdminOption: string([]byte{grantable}),
			Location:    ddlLocation{PageNo: pageNo, SlotNo: slotNo, SlotOffset: rawSlotOff, RowOffset: rowAbs},
		}, true
	}
	return roleGrantDef{}, false
}

func parseDDLTableCommentRow(page []byte, slotOff int, pageSize uint32, decoder textDecoder) (tableComment, bool) {
	for deltaBase := 0; deltaBase < 32; deltaBase++ {
		base := slotOff + deltaBase
		if base+16 >= int(pageSize) {
			continue
		}
		rowLen := binary.LittleEndian.Uint16(page[base+0x01:])
		if rowLen < 0x10 || rowLen > 0x1000 {
			continue
		}
		for _, delta := range []int{3, 4} {
			pos := base + delta
			values := make([]string, 0, 4)
			ok := true
			for i := 0; i < 4; i++ {
				var value string
				value, pos, ok = readDDLShortString(page, pos, decoder, true)
				if !ok {
					break
				}
				values = append(values, value)
			}
			if !ok || len(values) != 4 {
				continue
			}
			if values[2] != "TABLE" && values[2] != "VIEW" {
				continue
			}
			if values[0] == "" || values[1] == "" {
				continue
			}
			return tableComment{Owner: values[0], TableName: values[1], TableType: values[2], Comment: values[3]}, true
		}
	}
	return tableComment{}, false
}

func parseDDLColumnCommentRow(page []byte, slotOff int, pageSize uint32, decoder textDecoder) (columnComment, bool) {
	for deltaBase := 0; deltaBase < 32; deltaBase++ {
		base := slotOff + deltaBase
		if base+16 >= int(pageSize) {
			continue
		}
		rowLen := binary.LittleEndian.Uint16(page[base+0x01:])
		if rowLen < 0x10 || rowLen > 0x1000 {
			continue
		}
		for _, delta := range []int{4, 3} {
			pos := base + delta
			values := make([]string, 0, 5)
			ok := true
			for i := 0; i < 5; i++ {
				var value string
				value, pos, ok = readDDLShortString(page, pos, decoder, true)
				if !ok {
					break
				}
				values = append(values, value)
			}
			if !ok || len(values) != 5 {
				continue
			}
			if values[3] != "TABLE" && values[3] != "VIEW" {
				continue
			}
			if values[0] == "" || values[1] == "" || values[2] == "" {
				continue
			}
			return columnComment{Owner: values[0], TableName: values[1], ColumnName: values[2], TableType: values[3], Comment: values[4]}, true
		}
	}
	return columnComment{}, false
}

func readDDLShortString(page []byte, pos int, decoder textDecoder, allowEmpty bool) (string, int, bool) {
	if pos >= len(page) {
		return "", pos, false
	}
	marker := page[pos]
	if marker == 0x80 {
		return "", pos + 1, allowEmpty
	}
	if marker < 0x81 || marker > 0xBF {
		return "", pos, false
	}
	n := int(marker - 0x80)
	start := pos + 1
	end := start + n
	if end > len(page) {
		return "", pos, false
	}
	value, ok := decoder.decode(page[start:end])
	if !ok {
		return "", pos, false
	}
	return value, end, true
}

func parseDDLVarcharAt(page []byte, pos int, decoder textDecoder) string {
	if pos >= len(page) {
		return ""
	}
	marker := page[pos]
	if marker == 0x80 {
		return ""
	}
	if marker < 0x81 || marker > 0xBF {
		return ""
	}
	n := int(marker - 0x80)
	start := pos + 1
	end := start + n
	if end > len(page) {
		return ""
	}
	value, ok := decoder.decode(page[start:end])
	if !ok {
		return ""
	}
	return value
}

func parseDDLDefault(page []byte, pos int, decoder textDecoder) string {
	value := strings.TrimSpace(parseDDLVarcharAt(page, pos, decoder))
	if !isSafeDefault(value) {
		return ""
	}
	return value
}

func isSafeDefault(value string) bool {
	if value == "" || strings.EqualFold(value, "NULL") {
		return false
	}
	if len(value) > 256 {
		return false
	}
	for _, ch := range value {
		if ch == utf8.RuneError || (ch < 32 && ch != '\t') {
			return false
		}
	}
	return true
}

func parseDDLKeyInfo(keyInfo []byte) []indexKey {
	var result []indexKey
	for i := 0; i+3 <= len(keyInfo); i += 3 {
		order := fmt.Sprintf("FLAG_0x%02X", keyInfo[i+2])
		switch keyInfo[i+2] {
		case 0x41:
			order = "ASC"
		case 0x44:
			order = "DESC"
		}
		result = append(result, indexKey{
			ColID: binary.LittleEndian.Uint16(keyInfo[i:]),
			Order: order,
		})
	}
	return result
}

func (d textDecoder) decode(raw []byte) (string, bool) {
	if len(raw) == 0 {
		return "", true
	}
	var candidates []string
	if d.preferred != "" && d.preferred != "auto" {
		candidates = append(candidates, d.preferred)
	}
	candidates = append(candidates, "utf-8", "gb18030", "gbk", "euc-kr")
	seen := make(map[string]bool, len(candidates))
	for _, name := range candidates {
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		value, ok := decodeWithCharset(raw, name)
		if ok && !containsBadControl(value) {
			return value, true
		}
	}
	return "", false
}

func decodeWithCharset(raw []byte, charset string) (string, bool) {
	switch strings.ReplaceAll(strings.ToLower(charset), "_", "-") {
	case "utf8", "utf-8":
		if !utf8.Valid(raw) {
			return "", false
		}
		return string(raw), true
	case "gb18030":
		return decodeWithEncoding(raw, simplifiedchinese.GB18030)
	case "gbk":
		return decodeWithEncoding(raw, simplifiedchinese.GBK)
	case "euc-kr", "euckr", "kr":
		return decodeWithEncoding(raw, korean.EUCKR)
	default:
		return "", false
	}
}

func decodeWithEncoding(raw []byte, enc encoding.Encoding) (string, bool) {
	reader := transform.NewReader(bytes.NewReader(raw), enc.NewDecoder())
	out, err := io.ReadAll(reader)
	if err != nil {
		return "", false
	}
	return string(out), true
}

func containsBadControl(value string) bool {
	for _, ch := range value {
		if ch == utf8.RuneError || (ch < 32 && ch != '\t' && ch != '\n' && ch != '\r') {
			return true
		}
	}
	return false
}

func isSafeShortText(value string) bool {
	if value == "" || len(value) > 256 {
		return false
	}
	return !containsBadControl(value)
}

func loadTablespaceNames(controlPath string, controlDULPath string) map[uint32]string {
	result := defaultTablespaceNames()
	mergeControlDULTablespaceNames(result, controlDULPath)
	if controlPath == "" {
		return result
	}
	ctl, err := InspectControlFile(controlPath)
	if err != nil {
		return result
	}
	for _, entry := range ctl.Entries {
		result[entry.ID] = entry.Name
	}
	return result
}

func defaultTablespaceNames() map[uint32]string {
	return map[uint32]string{
		0: "SYSTEM",
		1: "ROLL",
		3: "TEMP",
		4: "MAIN",
	}
}

func tableStorageByID(tables map[uint32]dictionaryObject, indexObjects map[uint32]dictionaryObject, indexes map[uint32]indexDef, tablespaces map[uint32]string) map[uint32]indexDef {
	result := make(map[uint32]indexDef)
	for indexID, obj := range indexObjects {
		tableID := uint32(obj.ParentID)
		if _, ok := tables[tableID]; !ok {
			continue
		}
		idx, ok := indexes[indexID]
		if !ok {
			continue
		}
		if idx.Flag&1 == 0 || idx.KeyNum != 0 {
			continue
		}
		result[tableID] = idx
	}
	// HUGE main and transaction auxiliary tables can have SYSINDEXES storage
	// rows without a matching TABOBJ/INDEX object. Their table-data storage id
	// follows the regular 0x02000000|table_id rule, so retain that exact catalog
	// row instead of guessing a root or scanning a data file.
	for tableID, table := range tables {
		if !table.isHugeObject() {
			continue
		}
		if idx, ok := indexes[tableDataAssistID(tableID)]; ok && idx.Flag&1 != 0 && idx.KeyNum == 0 {
			result[tableID] = idx
		}
	}
	return result
}

func tableIDByOwnerName(tables map[uint32]dictionaryObject, columnsByTable map[uint32][]columnDef, matcher ownerMatcher, owner string, name string) (uint32, bool) {
	var candidates []uint32
	for id, obj := range tables {
		if obj.Owner == owner && obj.Name == name && matcher.allowed(obj.Owner) {
			candidates = append(candidates, id)
		}
	}
	if len(candidates) == 0 {
		return 0, false
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i] < candidates[j]
	})
	for _, id := range candidates {
		if len(columnsByTable[id]) > 0 {
			return id, true
		}
	}
	return candidates[0], true
}

func countAllowedTables(tables map[uint32]dictionaryObject, columnsByTable map[uint32][]columnDef, matcher ownerMatcher, tableMatcher tableNameMatcher) int {
	count := 0
	for id, table := range tables {
		if matcher.allowed(table.Owner) && tableMatcher.allowed(table.Owner, table.Name) && len(columnsByTable[id]) > 0 {
			count++
		}
	}
	return count
}

func countDDLIndexes(tables map[uint32]dictionaryObject, indexObjects map[uint32]dictionaryObject, indexes map[uint32]indexDef, matcher ownerMatcher, tableMatcher tableNameMatcher) int {
	count := 0
	for indexID, obj := range indexObjects {
		table, ok := tables[uint32(obj.ParentID)]
		if !ok || !matcher.allowed(table.Owner) || !tableMatcher.allowed(table.Owner, table.Name) {
			continue
		}
		idx, ok := indexes[indexID]
		if !ok || idx.Flag&1 != 0 || idx.KeyNum == 0 || idx.Type != "BT" {
			continue
		}
		count++
	}
	return count
}

func countExportedPartitionedTables(partitionsByTable map[uint32][]PartitionInfo, columnsByTable map[uint32][]columnDef) int {
	count := 0
	for tableID, parts := range partitionsByTable {
		if len(parts) > 0 && len(columnsByTable[tableID]) > 0 {
			count++
		}
	}
	return count
}

func countExportedPartitions(partitionsByTable map[uint32][]PartitionInfo, columnsByTable map[uint32][]columnDef) int {
	count := 0
	for tableID, parts := range partitionsByTable {
		if len(columnsByTable[tableID]) > 0 {
			count += len(parts)
		}
	}
	return count
}

func countExportedRoleGrants(roleGrants []roleGrantDef, users map[uint32]dictionaryObject, roles map[uint32]dictionaryObject, userIDs []uint32, roleIDs []uint32) int {
	return len(renderRoleGrantLines(roleGrants, users, roles, userIDs, roleIDs))
}

func exportedUserIDs(users map[uint32]dictionaryObject, matcher ownerMatcher) []uint32 {
	var ids []uint32
	for id, user := range users {
		if isBuiltInUserName(user.Name) || !matcher.allowed(user.Name) {
			continue
		}
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		a := users[ids[i]]
		b := users[ids[j]]
		if a.Name == b.Name {
			return ids[i] < ids[j]
		}
		return a.Name < b.Name
	})
	return ids
}

func exportedRoleIDs(roles map[uint32]dictionaryObject, roleGrants []roleGrantDef, users map[uint32]dictionaryObject, userIDs []uint32) []uint32 {
	userSet := idSet(userIDs)
	wanted := make(map[uint32]bool)
	for _, grant := range roleGrants {
		if !userSet[grant.GranteeID] {
			continue
		}
		role, ok := roles[grant.RoleID]
		if !ok || isBuiltInRoleName(role.Name) {
			continue
		}
		wanted[grant.RoleID] = true
	}
	var ids []uint32
	for id := range wanted {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		a := roles[ids[i]]
		b := roles[ids[j]]
		if a.Name == b.Name {
			return ids[i] < ids[j]
		}
		return a.Name < b.Name
	})
	return ids
}

func idSet(ids []uint32) map[uint32]bool {
	result := make(map[uint32]bool, len(ids))
	for _, id := range ids {
		result[id] = true
	}
	return result
}

func isRealURObject(obj dictionaryObject) bool {
	return obj.SchemaID == 0 && obj.Valid != "N" && isSafeShortText(obj.Name)
}

func isBuiltInUserName(name string) bool {
	return builtInUserNames[strings.ToUpper(strings.TrimSpace(name))]
}

func isBuiltInRoleName(name string) bool {
	return builtInRoleNames[strings.ToUpper(strings.TrimSpace(name))]
}

func filterDDLTriggersByTable(triggers []DictionaryTrigger, matcher tableNameMatcher) []DictionaryTrigger {
	if !matcher.hasRules || matcher.all {
		return triggers
	}
	filtered := make([]DictionaryTrigger, 0, len(triggers))
	for _, trigger := range triggers {
		if matcher.allowed(trigger.TableOwner, trigger.TableName) {
			filtered = append(filtered, trigger)
		}
	}
	return filtered
}

func filterDDLPrivilegesByTable(privileges []DictionaryTabPrivilege, matcher tableNameMatcher) []DictionaryTabPrivilege {
	if !matcher.hasRules || matcher.all {
		return privileges
	}
	filtered := make([]DictionaryTabPrivilege, 0, len(privileges))
	for _, privilege := range privileges {
		if matcher.allowed(privilege.Owner, privilege.ObjectName) {
			filtered = append(filtered, privilege)
		}
	}
	return filtered
}

func filterDDLPrivilegesToTables(privileges []DictionaryTabPrivilege, tables map[uint32]dictionaryObject, ownerMatcher ownerMatcher, tableMatcher tableNameMatcher) []DictionaryTabPrivilege {
	tableKeys := make(map[string]bool)
	for _, table := range tables {
		if ownerMatcher.allowed(table.Owner) && tableMatcher.allowed(table.Owner, table.Name) {
			tableKeys[qualifiedObjectKey(table.Owner, table.Name)] = true
		}
	}
	filtered := make([]DictionaryTabPrivilege, 0, len(privileges))
	for _, privilege := range privileges {
		if tableKeys[qualifiedObjectKey(privilege.Owner, privilege.ObjectName)] {
			filtered = append(filtered, privilege)
		}
	}
	return filtered
}

func userDefaultTablespaceName(user dictionaryObject, tablespaces map[uint32]string) string {
	groupID := uint32(user.Info3 & 0xFFFF)
	if groupID == 0 {
		groupID = 0
	}
	return tablespaces[groupID]
}

func renderDDL(objects map[uint32]dictionaryObject, users map[uint32]dictionaryObject, roles map[uint32]dictionaryObject, roleGrants []roleGrantDef, tables map[uint32]dictionaryObject, columnsByTable map[uint32][]columnDef, columnsByTableColID map[tableColKey]columnDef, indexObjects map[uint32]dictionaryObject, indexes map[uint32]indexDef, tableStorage map[uint32]indexDef, partitionsByTable map[uint32][]PartitionInfo, partitionKeysByTable map[uint32][]uint16, constraintObjects map[uint32]dictionaryObject, constraints []constraintDef, tableComments map[ownerTableKey]tableComment, columnComments map[ownerTableColumnKey]columnComment, views []DictionaryView, sequences []DictionarySequence, routines []DictionaryRoutine, triggers []DictionaryTrigger, synonyms []DictionarySynonym, tabPrivileges []DictionaryTabPrivilege, matcher ownerMatcher, tableMatcher tableNameMatcher, tablespaces map[uint32]string) string {
	var out strings.Builder
	out.WriteString("-- Generated by dmdul export-ddl. Review before running.\n\n")
	renderUsersAndRoles(&out, users, roles, roleGrants, matcher, tablespaces)
	renderCreateSchemas(&out, objects, users, matcher)
	renderCreateTables(&out, tables, columnsByTable, columnsByTableColID, tableStorage, partitionsByTable, partitionKeysByTable, matcher, tableMatcher, tablespaces)
	renderSequences(&out, sequences)
	renderRoutines(&out, routines)
	renderViews(&out, views)
	renderIndexes(&out, tables, columnsByTableColID, indexObjects, indexes, matcher, tableMatcher, tablespaces)
	renderConstraints(&out, objects, tables, columnsByTableColID, constraintObjects, indexes, constraints, matcher, tableMatcher)
	renderComments(&out, tables, columnsByTable, tableComments, columnComments, matcher, tableMatcher)
	renderTriggers(&out, triggers)
	renderSynonyms(&out, synonyms)
	renderTabPrivileges(&out, tabPrivileges)
	return out.String()
}

// renderCreateSchemas emits CREATE SCHEMA for every non-default schema in
// scope. In DM one user owns one or more schemas; the same-named default schema
// is created automatically by CREATE USER, but additional schemas
// (name != owner) must be recreated explicitly or the CREATE TABLE statements
// that follow would target a missing schema. DM's CREATE SCHEMA absorbs the
// following statements as schema elements until a "/" batch terminator, so each
// one is terminated with "/" (verified on DM8: a bare ";" swallows the next
// CREATE TABLE and the schema is not created).
func renderCreateSchemas(out *strings.Builder, objects map[uint32]dictionaryObject, users map[uint32]dictionaryObject, matcher ownerMatcher) {
	schemas := dictionarySchemasFromObjects(objects, users, matcher)
	var lines []string
	for _, schema := range schemas {
		if strings.EqualFold(schema.Name, schema.Owner) {
			continue // default same-name schema is created by CREATE USER
		}
		lines = append(lines, fmt.Sprintf("CREATE SCHEMA %s AUTHORIZATION %s;\n/", quoteIdent(schema.Name), quoteIdent(schema.Owner)))
	}
	if len(lines) == 0 {
		return
	}
	out.WriteString("-- Schemas\n")
	for _, line := range lines {
		out.WriteString(line)
		out.WriteString("\n\n")
	}
}

func renderUsersAndRoles(out *strings.Builder, users map[uint32]dictionaryObject, roles map[uint32]dictionaryObject, roleGrants []roleGrantDef, matcher ownerMatcher, tablespaces map[uint32]string) {
	userIDs := exportedUserIDs(users, matcher)
	roleIDs := exportedRoleIDs(roles, roleGrants, users, userIDs)
	if len(userIDs) == 0 && len(roleIDs) == 0 {
		return
	}

	out.WriteString("-- Users and roles\n")
	if len(userIDs) > 0 {
		out.WriteString("-- Password hashes are not exported. Change the placeholder password after import.\n")
		for _, userID := range userIDs {
			user := users[userID]
			out.WriteString(renderCreateUser(user, tablespaces))
			out.WriteByte('\n')
		}
		if len(roleIDs) > 0 {
			out.WriteByte('\n')
		}
	}

	for _, roleID := range roleIDs {
		role := roles[roleID]
		out.WriteString(fmt.Sprintf("CREATE ROLE %s;\n", quoteIdent(role.Name)))
	}

	grantLines := renderRoleGrantLines(roleGrants, users, roles, userIDs, roleIDs)
	if len(grantLines) > 0 {
		if len(roleIDs) > 0 || len(userIDs) > 0 {
			out.WriteByte('\n')
		}
		for _, line := range grantLines {
			out.WriteString(line)
			out.WriteByte('\n')
		}
	}
	out.WriteByte('\n')
}

func renderCreateUser(user dictionaryObject, tablespaces map[uint32]string) string {
	parts := []string{
		"CREATE USER " + quoteIdent(user.Name),
		"IDENTIFIED BY " + passwordLiteral(defaultRecoveredPassword),
	}
	if defaultTS := userDefaultTablespaceName(user, tablespaces); defaultTS != "" {
		parts = append(parts, "DEFAULT TABLESPACE "+quoteStorageName(defaultTS))
	}
	if tempTS := tablespaces[3]; tempTS != "" {
		parts = append(parts, "TEMPORARY TABLESPACE "+quoteStorageName(tempTS))
	}
	return strings.Join(parts, " ") + ";"
}

func renderRoleGrantLines(roleGrants []roleGrantDef, users map[uint32]dictionaryObject, roles map[uint32]dictionaryObject, userIDs []uint32, roleIDs []uint32) []string {
	userSet := idSet(userIDs)
	roleSet := idSet(roleIDs)
	linesByKey := make(map[string]string)
	for _, grant := range roleGrants {
		role, ok := roles[grant.RoleID]
		if !ok {
			continue
		}
		granteeName := ""
		if userSet[grant.GranteeID] {
			granteeName = users[grant.GranteeID].Name
		} else if roleSet[grant.GranteeID] {
			granteeName = roles[grant.GranteeID].Name
		}
		if granteeName == "" {
			continue
		}
		line := fmt.Sprintf("GRANT %s TO %s", quoteIdent(role.Name), quoteIdent(granteeName))
		if grant.AdminOption == "Y" {
			line += " WITH ADMIN OPTION"
		}
		line += ";"
		key := role.Name + "\x00" + granteeName + "\x00" + grant.AdminOption
		linesByKey[key] = line
	}
	keys := make([]string, 0, len(linesByKey))
	for key := range linesByKey {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	lines := make([]string, 0, len(keys))
	for _, key := range keys {
		lines = append(lines, linesByKey[key])
	}
	return lines
}

func renderCreateTables(out *strings.Builder, tables map[uint32]dictionaryObject, columnsByTable map[uint32][]columnDef, columnsByTableColID map[tableColKey]columnDef, tableStorage map[uint32]indexDef, partitionsByTable map[uint32][]PartitionInfo, partitionKeysByTable map[uint32][]uint16, matcher ownerMatcher, tableMatcher tableNameMatcher, tablespaces map[uint32]string) {
	out.WriteString("-- Tables\n")
	tableIDs := sortedTableIDs(tables)
	for _, tableID := range tableIDs {
		table := tables[tableID]
		if !matcher.allowed(table.Owner) || !tableMatcher.allowed(table.Owner, table.Name) || len(columnsByTable[tableID]) == 0 {
			continue
		}
		createKind := "CREATE TABLE"
		if table.isHugeTable() {
			createKind = "CREATE HUGE TABLE"
		} else if table.isTemporaryTable() {
			createKind = "CREATE GLOBAL TEMPORARY TABLE"
		}
		out.WriteString(fmt.Sprintf("%s %s.%s (\n", createKind, quoteIdent(table.Owner), quoteIdent(table.Name)))
		var lines []string
		for _, col := range columnsByTable[tableID] {
			line := fmt.Sprintf("    %s %s", quoteIdent(col.Name), formatColumnType(col.DataType, col.Length, col.Scale))
			if col.Default != "" {
				line += " DEFAULT " + col.Default
			}
			if col.Nullable == "N" {
				line += " NOT NULL"
			}
			lines = append(lines, line)
		}
		out.WriteString(strings.Join(lines, ",\n"))
		out.WriteString("\n)")
		if commitClause := table.temporaryCommitClause(); commitClause != "" {
			out.WriteString("\n")
			out.WriteString(commitClause)
		}
		partitionClause := renderPartitionClause(tableID, partitionsByTable[tableID], partitionKeysByTable[tableID], columnsByTableColID)
		if partitionClause != "" && !table.isTemporaryTable() {
			out.WriteString(partitionClause)
		}
		if table.isHugeTable() {
			groupID := uint32(table.Info2 & 0xFFFF)
			if storage, ok := tableStorage[tableID]; ok && storage.GroupID != 0 {
				groupID = uint32(storage.GroupID)
			}
			out.WriteString(hugeStorageClause(table, groupID, tablespaces))
		} else if storage, ok := tableStorage[tableID]; ok && !table.isTemporaryTable() && partitionClause == "" {
			out.WriteString(storageClause(uint32(storage.GroupID), tablespaces, table.tableStorageOrganization()))
		}
		out.WriteString(";\n\n")
	}
}

func hugeStorageClause(table dictionaryObject, groupID uint32, tablespaces map[uint32]string) string {
	delta := "WITHOUT DELTA"
	if table.hugeWithDelta() {
		delta = "WITH DELTA"
	}
	clause := fmt.Sprintf("\nSTORAGE(SECTION(%d), FILESIZE(%d), %s", table.hugeSectionRows(), table.hugeFileSizeMB(), delta)
	if tablespace := strings.TrimSpace(tablespaces[groupID]); tablespace != "" {
		clause += ", ON " + quoteIdent(tablespace)
	}
	return clause + ")"
}

func renderPartitionClause(tableID uint32, parts []PartitionInfo, keyColIDs []uint16, columnsByTableColID map[tableColKey]columnDef) string {
	if len(parts) == 0 {
		return ""
	}
	partType := partitionTypeSummary(parts)
	if partType == "" || strings.Contains(partType, ",") {
		return ""
	}
	keyColumns := partitionKeyColumnNames(tableID, keyColIDs, columnsByTableColID)
	if len(keyColumns) == 0 {
		return ""
	}
	keyList := ddlColumns(keyColumns, false)
	if keyList == "" {
		return ""
	}

	switch partType {
	case "RANGE":
		return renderRangePartitionClause(parts, keyList, firstPartitionKeyColumn(tableID, keyColIDs, columnsByTableColID))
	case "LIST":
		return renderListPartitionClause(parts, keyList)
	case "HASH":
		return fmt.Sprintf("\nPARTITION BY HASH (%s)\nPARTITIONS %d", keyList, len(parts))
	default:
		return ""
	}
}

func partitionKeyColumnNames(tableID uint32, keyColIDs []uint16, columnsByTableColID map[tableColKey]columnDef) []namedIndexKey {
	keys := make([]namedIndexKey, 0, len(keyColIDs))
	for _, colID := range keyColIDs {
		col, ok := columnsByTableColID[tableColKey{tableID: tableID, colID: colID}]
		if !ok || col.Name == "" {
			continue
		}
		keys = append(keys, namedIndexKey{Name: col.Name})
	}
	return keys
}

func firstPartitionKeyColumn(tableID uint32, keyColIDs []uint16, columnsByTableColID map[tableColKey]columnDef) columnDef {
	if len(keyColIDs) == 0 {
		return columnDef{}
	}
	return columnsByTableColID[tableColKey{tableID: tableID, colID: keyColIDs[0]}]
}

func renderRangePartitionClause(parts []PartitionInfo, keyList string, keyColumn columnDef) string {
	var lines []string
	for _, part := range parts {
		boundary, ok := rangePartitionBoundary(part, keyColumn)
		if !ok {
			return ""
		}
		lines = append(lines, fmt.Sprintf("    PARTITION %s VALUES LESS THAN (%s)", quoteIdent(part.Name), boundary))
	}
	return "\nPARTITION BY RANGE (" + keyList + ")\n(\n" + strings.Join(lines, ",\n") + "\n)"
}

func renderListPartitionClause(parts []PartitionInfo, keyList string) string {
	var lines []string
	for _, part := range parts {
		values := listPartitionValues(part)
		if values == "" {
			return ""
		}
		lines = append(lines, fmt.Sprintf("    PARTITION %s VALUES (%s)", quoteIdent(part.Name), values))
	}
	return "\nPARTITION BY LIST (" + keyList + ")\n(\n" + strings.Join(lines, ",\n") + "\n)"
}

func rangePartitionBoundary(part PartitionInfo, keyColumn columnDef) (string, bool) {
	if isMaxValuePartition(part) {
		return "MAXVALUE", true
	}
	upperType := strings.ToUpper(strings.TrimSpace(keyColumn.DataType))
	switch upperType {
	case "BYTE", "TINYINT", "SMALLINT", "INT", "INTEGER", "BIGINT":
		value, ok := decodePartitionIntegerValue(part.HighValue)
		if !ok {
			return "", false
		}
		return value, true
	case "DATE", "DATETIME", "TIMESTAMP", "TIME":
		value, ok := decodePartitionDateValue(part.HighValue)
		if !ok {
			return "", false
		}
		return sqlLiteral(value), true
	default:
		value, ok := decodePartitionDateValue(part.HighValue)
		if ok {
			return sqlLiteral(value), true
		}
		tokens := partitionHighValueTokens(part.HighValue)
		if len(tokens) == 1 {
			return sqlLiteral(tokens[0]), true
		}
		return "", false
	}
}

func isMaxValuePartition(part PartitionInfo) bool {
	if marker, ok := partitionHighValueMarker(part.HighValue); ok && marker == 0x02 {
		return true
	}
	name := strings.ToUpper(part.Name)
	return strings.Contains(name, "MAX") || strings.Contains(name, "DEFAULT")
}

func partitionHighValueMarker(raw []byte) (byte, bool) {
	raw = normalizePartitionHighValue(raw)
	// Observed SYSHPARTTABLEINFO values use a 25-byte descriptor followed by
	// a one-byte boundary marker: 1=value, 2=MAXVALUE/DEFAULT.
	if len(raw) < 26 || raw[0] != 0x01 || raw[4] != 0x03 {
		return 0, false
	}
	if raw[25] != 0x01 && raw[25] != 0x02 {
		return 0, false
	}
	return raw[25], true
}

func decodePartitionIntegerValue(raw []byte) (string, bool) {
	raw = normalizePartitionHighValue(raw)
	marker, ok := partitionHighValueMarker(raw)
	if !ok || marker != 0x01 || len(raw) < 34 {
		return "", false
	}
	return strconv.FormatInt(int64(binary.LittleEndian.Uint64(raw[26:34])), 10), true
}

func decodePartitionDateValue(raw []byte) (string, bool) {
	raw = normalizePartitionHighValue(raw)
	for i := 0; i+4 <= len(raw); i++ {
		year := int(binary.LittleEndian.Uint16(raw[i:]))
		month := int(raw[i+2])
		day := int(raw[i+3])
		if year < 1900 || year > 9999 || month < 1 || month > 12 || day < 1 || day > daysInMonth(year, month) {
			continue
		}
		return fmt.Sprintf("%04d-%02d-%02d", year, month, day), true
	}
	return "", false
}

func daysInMonth(year int, month int) int {
	switch month {
	case 1, 3, 5, 7, 8, 10, 12:
		return 31
	case 4, 6, 9, 11:
		return 30
	case 2:
		if (year%4 == 0 && year%100 != 0) || year%400 == 0 {
			return 29
		}
		return 28
	default:
		return 0
	}
}

func listPartitionValues(part PartitionInfo) string {
	tokens := partitionHighValueTokens(part.HighValue)
	if len(tokens) == 0 {
		return "DEFAULT"
	}
	values := make([]string, 0, len(tokens))
	for _, token := range tokens {
		values = append(values, sqlLiteral(token))
	}
	return strings.Join(values, ", ")
}

func partitionTypeSummary(parts []PartitionInfo) string {
	seen := make(map[string]bool)
	var types []string
	for _, part := range parts {
		if part.Type == "" || seen[part.Type] {
			continue
		}
		seen[part.Type] = true
		types = append(types, part.Type)
	}
	return strings.Join(types, ",")
}

func renderViews(out *strings.Builder, views []DictionaryView) {
	if len(views) == 0 {
		return
	}
	out.WriteString("-- Views\n")
	for _, view := range views {
		sql := strings.TrimSpace(view.SQL)
		if sql != "" {
			sql = qualifyRecoveredViewName(sql, view.Owner, view.Name)
		}
		if sql == "" && strings.TrimSpace(view.QuerySQL) != "" {
			sql = fmt.Sprintf("CREATE OR REPLACE VIEW %s.%s AS\n%s",
				quoteIdent(view.Owner), quoteIdent(view.Name), strings.TrimSpace(view.QuerySQL))
		}
		if sql == "" {
			fmt.Fprintf(out, "-- WARNING: view text not recovered for %s.%s\n", quoteIdent(view.Owner), quoteIdent(view.Name))
			continue
		}
		out.WriteString(ensureSQLTerminator(sql))
		out.WriteString("\n\n")
	}
}

var recoveredViewHeaderPattern = regexp.MustCompile(`(?is)^(\s*CREATE\s+(?:OR\s+REPLACE\s+)?(?:FORCE\s+)?VIEW\s+)(?:"(?:[^"]|"")*"|[^\s(]+)`)

func qualifyRecoveredViewName(sql, owner, name string) string {
	match := recoveredViewHeaderPattern.FindStringSubmatchIndex(sql)
	if len(match) < 4 {
		return sql
	}
	qualified := quoteIdent(owner) + "." + quoteIdent(name)
	return sql[:match[3]] + qualified + sql[match[1]:]
}

func renderSequences(out *strings.Builder, sequences []DictionarySequence) {
	if len(sequences) == 0 {
		return
	}
	out.WriteString("-- Sequences\n")
	for _, seq := range sequences {
		sql := strings.TrimSpace(seq.SQL)
		if sql == "" {
			if !seq.HasLastNumber {
				fmt.Fprintf(out, "-- WARNING: current value of %s.%s was not recovered; START WITH uses the persisted initial value\n",
					quoteIdent(seq.Owner), quoteIdent(seq.Name))
			}
			if _, _, exhausted := recoveredSequenceStart(seq); exhausted {
				fmt.Fprintf(out, "-- NOTICE: %s.%s was exhausted; the following NEXTVAL preserves its exhausted state\n",
					quoteIdent(seq.Owner), quoteIdent(seq.Name))
			}
			sql = renderRecoveredSequenceSQL(seq)
		}
		out.WriteString(ensureSQLTerminator(sql))
		out.WriteString("\n\n")
	}
}

func renderRecoveredSequenceSQL(seq DictionarySequence) string {
	increment := seq.IncrementBy
	if increment == 0 {
		increment = 1
	}
	var lines []string
	lines = append(lines, fmt.Sprintf("CREATE SEQUENCE %s.%s", quoteIdent(seq.Owner), quoteIdent(seq.Name)))
	startWith, hasStartWith, exhausted := recoveredSequenceStart(seq)
	if hasStartWith {
		lines = append(lines, fmt.Sprintf("START WITH %d", startWith))
	}
	lines = append(lines, fmt.Sprintf("INCREMENT BY %d", increment))
	if seq.HasMinValue || seq.MinValue != 0 {
		lines = append(lines, fmt.Sprintf("MINVALUE %d", seq.MinValue))
	}
	if seq.HasMaxValue || seq.MaxValue != 0 {
		lines = append(lines, fmt.Sprintf("MAXVALUE %d", seq.MaxValue))
	}
	if strings.EqualFold(seq.CycleFlag, "Y") || strings.EqualFold(seq.CycleFlag, "YES") {
		lines = append(lines, "CYCLE")
	} else {
		lines = append(lines, "NOCYCLE")
	}
	if seq.CacheSize > 0 {
		lines = append(lines, fmt.Sprintf("CACHE %d", seq.CacheSize))
	} else {
		lines = append(lines, "NOCACHE")
	}
	if strings.EqualFold(seq.OrderFlag, "Y") || strings.EqualFold(seq.OrderFlag, "YES") {
		lines = append(lines, "ORDER")
	} else {
		lines = append(lines, "NOORDER")
	}
	sql := strings.Join(lines, "\n")
	if exhausted {
		sql += fmt.Sprintf(";\nSELECT %s.%s.NEXTVAL", quoteIdent(seq.Owner), quoteIdent(seq.Name))
	}
	return sql
}

func recoveredSequenceStart(seq DictionarySequence) (int64, bool, bool) {
	start := seq.StartWith
	hasStart := seq.HasStartWith || seq.StartWith != 0
	if !seq.HasLastNumber {
		return start, hasStart, false
	}
	start = seq.LastNumber
	hasStart = true
	increment := seq.IncrementBy
	if increment == 0 {
		increment = 1
	}
	cycle := strings.EqualFold(seq.CycleFlag, "Y") || strings.EqualFold(seq.CycleFlag, "YES")
	if increment > 0 && (seq.HasMaxValue || seq.MaxValue != 0) && start > seq.MaxValue {
		if cycle && (seq.HasMinValue || seq.MinValue != 0) {
			return seq.MinValue, true, false
		}
		return seq.MaxValue, true, true
	}
	if increment < 0 && (seq.HasMinValue || seq.MinValue != 0) && start < seq.MinValue {
		if cycle && (seq.HasMaxValue || seq.MaxValue != 0) {
			return seq.MaxValue, true, false
		}
		return seq.MinValue, true, true
	}
	return start, hasStart, false
}

func renderRoutines(out *strings.Builder, routines []DictionaryRoutine) {
	if len(routines) == 0 {
		return
	}
	out.WriteString("-- Stored routines\n")
	for _, routine := range routines {
		sql := strings.TrimSpace(routine.SQL)
		if sql == "" {
			fmt.Fprintf(out, "-- WARNING: %s text not recovered for %s.%s\n",
				normalizeRoutineObjectType(routine.ObjectType),
				quoteIdent(routine.Owner),
				quoteIdent(routine.Name))
			continue
		}
		out.WriteString(ensurePLSQLBlockTerminator(sql))
		out.WriteString("\n\n")
	}
}

func renderTriggers(out *strings.Builder, triggers []DictionaryTrigger) {
	if len(triggers) == 0 {
		return
	}
	out.WriteString("-- Triggers\n")
	for _, trigger := range triggers {
		sql := strings.TrimSpace(trigger.SQL)
		if sql == "" {
			fmt.Fprintf(out, "-- WARNING: trigger text not recovered for %s.%s\n", quoteIdent(trigger.Owner), quoteIdent(trigger.Name))
			continue
		}
		out.WriteString(ensurePLSQLBlockTerminator(sql))
		out.WriteString("\n\n")
	}
}

func renderSynonyms(out *strings.Builder, synonyms []DictionarySynonym) {
	if len(synonyms) == 0 {
		return
	}
	out.WriteString("-- Synonyms\n")
	for _, syn := range synonyms {
		if syn.Public || strings.EqualFold(syn.Owner, "PUBLIC") {
			fmt.Fprintf(out, "CREATE OR REPLACE PUBLIC SYNONYM %s FOR %s.%s;\n",
				quoteIdent(syn.Name), quoteIdent(syn.TableOwner), quoteIdent(syn.TableName))
			continue
		}
		fmt.Fprintf(out, "CREATE OR REPLACE SYNONYM %s.%s FOR %s.%s;\n",
			quoteIdent(syn.Owner), quoteIdent(syn.Name), quoteIdent(syn.TableOwner), quoteIdent(syn.TableName))
	}
	out.WriteByte('\n')
}

func renderTabPrivileges(out *strings.Builder, privileges []DictionaryTabPrivilege) {
	if len(privileges) == 0 {
		return
	}
	out.WriteString("-- Object privileges\n")
	for _, priv := range privileges {
		fmt.Fprintf(out, "GRANT %s ON %s.%s TO %s",
			strings.ToUpper(strings.TrimSpace(priv.Privilege)),
			quoteIdent(priv.Owner),
			quoteIdent(priv.ObjectName),
			quoteIdent(priv.Grantee))
		if strings.EqualFold(priv.Grantable, "Y") || strings.EqualFold(priv.Grantable, "YES") {
			out.WriteString(" WITH GRANT OPTION")
		}
		out.WriteString(";\n")
	}
	out.WriteByte('\n')
}

func ensureSQLTerminator(sql string) string {
	sql = cleanRecoveredSQLText(sql)
	if strings.HasSuffix(sql, ";") {
		return sql
	}
	return sql + ";"
}

// ensurePLSQLBlockTerminator ends a PL/SQL object body (trigger, procedure,
// function, package) with a "/" batch terminator on its own line. In disql the
// trailing ";" belongs to the block body, so without "/" every statement that
// follows is silently appended to the block buffer instead of being executed.
// DMP metadata records are executed per record by dimp and must NOT carry "/".
func ensurePLSQLBlockTerminator(sql string) string {
	return ensureSQLTerminator(sql) + "\n/"
}

func cleanRecoveredSQLText(sql string) string {
	sql = strings.TrimSpace(sql)
	for i, ch := range sql {
		if ch == utf8.RuneError || (ch < 32 && ch != '\t' && ch != '\n' && ch != '\r') {
			sql = sql[:i]
			break
		}
	}
	return strings.TrimSpace(sql)
}

func renderIndexes(out *strings.Builder, tables map[uint32]dictionaryObject, columnsByTableColID map[tableColKey]columnDef, indexObjects map[uint32]dictionaryObject, indexes map[uint32]indexDef, matcher ownerMatcher, tableMatcher tableNameMatcher, tablespaces map[uint32]string) {
	out.WriteString("-- Indexes\n")
	var indexIDs []uint32
	for id := range indexObjects {
		indexIDs = append(indexIDs, id)
	}
	sort.Slice(indexIDs, func(i, j int) bool {
		a := indexObjects[indexIDs[i]]
		b := indexObjects[indexIDs[j]]
		if a.Owner == b.Owner {
			return a.Name < b.Name
		}
		return a.Owner < b.Owner
	})
	for _, indexID := range indexIDs {
		obj := indexObjects[indexID]
		table, ok := tables[uint32(obj.ParentID)]
		if !ok || !matcher.allowed(table.Owner) || !tableMatcher.allowed(table.Owner, table.Name) {
			continue
		}
		idx, ok := indexes[indexID]
		if !ok || idx.Flag&1 != 0 || idx.KeyNum == 0 || idx.Type != "BT" {
			continue
		}
		cols := ddlColumns(columnsFromIndex(indexID, uint32(obj.ParentID), indexes, columnsByTableColID), true)
		if cols == "" {
			continue
		}
		unique := ""
		if idx.IsUnique == "Y" {
			unique = "UNIQUE "
		}
		out.WriteString(fmt.Sprintf("CREATE %sINDEX %s ON %s.%s (%s)%s;\n",
			unique,
			quoteIdent(obj.Name),
			quoteIdent(table.Owner),
			quoteIdent(table.Name),
			cols,
			storageClause(uint32(idx.GroupID), tablespaces, defaultStorageOrg),
		))
	}
	out.WriteString("\n")
}

func renderConstraints(out *strings.Builder, objects map[uint32]dictionaryObject, tables map[uint32]dictionaryObject, columnsByTableColID map[tableColKey]columnDef, constraintObjects map[uint32]dictionaryObject, indexes map[uint32]indexDef, constraints []constraintDef, matcher ownerMatcher, tableMatcher tableNameMatcher) {
	out.WriteString("-- Constraints\n")
	ddlOrder := map[string]int{"P": 1, "U": 2, "C": 3, "F": 4, "R": 5}
	sort.Slice(constraints, func(i, j int) bool {
		oi := ddlOrder[constraints[i].Type]
		oj := ddlOrder[constraints[j].Type]
		if oi == oj {
			ti := tables[constraints[i].TableID]
			tj := tables[constraints[j].TableID]
			if ti.Owner != tj.Owner {
				return ti.Owner < tj.Owner
			}
			if ti.Name != tj.Name {
				return ti.Name < tj.Name
			}
			return constraints[i].ID < constraints[j].ID
		}
		return oi < oj
	})
	for _, cons := range constraints {
		if cons.Valid != "Y" {
			continue
		}
		table, ok := tables[cons.TableID]
		if !ok || !matcher.allowed(table.Owner) || !tableMatcher.allowed(table.Owner, table.Name) {
			continue
		}
		consObj, ok := constraintObjects[cons.ID]
		if !ok {
			continue
		}
		owner := quoteIdent(table.Owner)
		tableName := quoteIdent(table.Name)
		consName := recoveredConstraintNameClause(consObj.Name)
		switch cons.Type {
		case "P", "U":
			cols := ddlColumns(columnsFromIndex(cons.IndexID, cons.TableID, indexes, columnsByTableColID), false)
			if cols == "" {
				continue
			}
			kind := "PRIMARY KEY"
			if cons.Type == "U" {
				kind = "UNIQUE"
			}
			out.WriteString(fmt.Sprintf("ALTER TABLE %s.%s ADD %s%s (%s);\n", owner, tableName, consName, kind, cols))
		case "C":
			if cons.CheckInfo == "" {
				continue
			}
			out.WriteString(fmt.Sprintf("ALTER TABLE %s.%s ADD %sCHECK (%s);\n", owner, tableName, consName, cons.CheckInfo))
		case "F":
			childCols := ddlColumns(columnsFromIndex(cons.IndexID, cons.TableID, indexes, columnsByTableColID), false)
			parentIndexObj, ok := objects[cons.FIndexID]
			if !ok || childCols == "" {
				continue
			}
			parentTable, ok := tables[uint32(parentIndexObj.ParentID)]
			if !ok {
				continue
			}
			parentCols := ddlColumns(columnsFromIndex(cons.FIndexID, uint32(parentIndexObj.ParentID), indexes, columnsByTableColID), false)
			if parentCols == "" {
				continue
			}
			out.WriteString(fmt.Sprintf("ALTER TABLE %s.%s ADD %sFOREIGN KEY (%s) REFERENCES %s.%s (%s);\n",
				owner, tableName, consName, childCols, quoteIdent(parentTable.Owner), quoteIdent(parentTable.Name), parentCols))
		}
	}
	out.WriteString("\n")
}

func recoveredConstraintNameClause(name string) string {
	name = strings.TrimSpace(name)
	if name == "" || isDMGeneratedConstraintName(name) {
		return ""
	}
	return "CONSTRAINT " + quoteIdent(name) + " "
}

func isDMGeneratedConstraintName(name string) bool {
	upper := strings.ToUpper(strings.TrimSpace(name))
	if len(upper) <= len("CONS") || !strings.HasPrefix(upper, "CONS") {
		return false
	}
	for _, ch := range upper[len("CONS"):] {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return true
}

func renderComments(out *strings.Builder, tables map[uint32]dictionaryObject, columnsByTable map[uint32][]columnDef, tableComments map[ownerTableKey]tableComment, columnComments map[ownerTableColumnKey]columnComment, matcher ownerMatcher, tableMatcher tableNameMatcher) {
	out.WriteString("-- Comments\n")
	tableIDs := sortedTableIDs(tables)
	wroteTableComment := false
	for _, tableID := range tableIDs {
		table := tables[tableID]
		if !matcher.allowed(table.Owner) || !tableMatcher.allowed(table.Owner, table.Name) || len(columnsByTable[tableID]) == 0 {
			continue
		}
		key := ownerTableKey{owner: table.Owner, table: table.Name}
		comment, ok := tableComments[key]
		if !ok {
			continue
		}
		out.WriteString(fmt.Sprintf("COMMENT ON TABLE %s.%s IS %s;\n", quoteIdent(comment.Owner), quoteIdent(comment.TableName), sqlLiteral(comment.Comment)))
		wroteTableComment = true
	}

	if wroteTableComment && len(columnComments) > 0 {
		out.WriteString("\n")
	}

	for _, tableID := range tableIDs {
		table := tables[tableID]
		if !matcher.allowed(table.Owner) || !tableMatcher.allowed(table.Owner, table.Name) || len(columnsByTable[tableID]) == 0 {
			continue
		}
		for _, col := range columnsByTable[tableID] {
			key := ownerTableColumnKey{owner: table.Owner, table: table.Name, column: col.Name}
			comment, ok := columnComments[key]
			if !ok {
				continue
			}
			out.WriteString(fmt.Sprintf("COMMENT ON COLUMN %s.%s.%s IS %s;\n", quoteIdent(comment.Owner), quoteIdent(comment.TableName), quoteIdent(comment.ColumnName), sqlLiteral(comment.Comment)))
		}
	}
}

func sortedTableIDs(tables map[uint32]dictionaryObject) []uint32 {
	ids := make([]uint32, 0, len(tables))
	for id := range tables {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		a := tables[ids[i]]
		b := tables[ids[j]]
		if a.Owner == b.Owner {
			if a.Name == b.Name {
				return ids[i] < ids[j]
			}
			return a.Name < b.Name
		}
		return a.Owner < b.Owner
	})
	return ids
}

func columnsFromIndex(indexID uint32, tableID uint32, indexes map[uint32]indexDef, columns map[tableColKey]columnDef) []namedIndexKey {
	idx, ok := indexes[indexID]
	if !ok {
		return nil
	}
	var result []namedIndexKey
	for _, key := range idx.Keys {
		name := fmt.Sprintf("COLID_%d", key.ColID)
		if col, ok := columns[tableColKey{tableID: tableID, colID: key.ColID}]; ok {
			name = col.Name
		}
		result = append(result, namedIndexKey{Name: name, Order: key.Order})
	}
	return result
}

type namedIndexKey struct {
	Name  string
	Order string
}

func ddlColumns(keys []namedIndexKey, includeOrder bool) string {
	var parts []string
	for _, key := range keys {
		if key.Name == "" {
			continue
		}
		part := quoteIdent(key.Name)
		if includeOrder && key.Order != "" && key.Order != "ASC" {
			part += " /* " + key.Order + " */"
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, ", ")
}

func quoteIdent(name string) string {
	if regularIdentifierPattern.MatchString(name) && !reservedIdentifierNames[strings.ToUpper(name)] {
		return name
	}
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func quoteStorageName(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func sqlLiteral(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func passwordLiteral(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}

func storageClause(groupID uint32, tablespaces map[uint32]string, organization string) string {
	name := tablespaces[groupID]
	if name == "" {
		return ""
	}
	if organization == "" {
		return fmt.Sprintf("\nSTORAGE(ON %s)", quoteStorageName(name))
	}
	return fmt.Sprintf("\nSTORAGE(ON %s, %s)", quoteStorageName(name), organization)
}

func formatColumnType(dataType string, length uint32, scale int16) string {
	dt := strings.TrimSpace(dataType)
	upper := normalizeDataType(dt)
	charTypes := map[string]bool{
		"CHAR": true, "CHARACTER": true, "VARCHAR": true, "VARCHAR2": true,
		"NCHAR": true, "NVARCHAR": true, "NVARCHAR2": true, "VARCHARACTER": true,
		"CHARACTER VARYING": true, "NATIONAL CHAR": true, "NATIONAL CHARACTER": true,
		"NATIONAL CHAR VARYING": true, "NATIONAL CHARACTER VARYING": true, "NCHAR VARYING": true,
	}
	binaryTypes := map[string]bool{"BINARY": true, "VARBINARY": true, "RAW": true}
	noLengthTypes := map[string]bool{
		"INT": true, "INTEGER": true, "PLS_INTEGER": true, "BIGINT": true, "SMALLINT": true, "TINYINT": true, "BYTE": true,
		"DATE": true,
		"TEXT": true, "LONGVARCHAR": true, "LONGVARBINARY": true, "LONG RAW": true, "CLOB": true, "NCLOB": true, "BLOB": true, "IMAGE": true,
		"BFILE": true, "JSON": true, "JSONB": true,
		"XMLTYPE": true,
		"BIT":     true, "BOOLEAN": true, "BOOL": true,
		"REAL": true, "BINARY_FLOAT": true, "FLOAT": true, "DOUBLE": true, "DOUBLE PRECISION": true, "BINARY_DOUBLE": true,
		"ROWID": true,
	}
	timePrecisionTypes := map[string]bool{
		"DATETIME": true, "TIME": true, "TIMESTAMP": true,
		"DATETIME WITH TIME ZONE": true, "TIME WITH TIME ZONE": true,
		"TIMESTAMP WITH TIME ZONE": true, "TIMESTAMP WITH LOCAL TIME ZONE": true,
	}
	numberTypes := map[string]bool{"NUMBER": true, "NUMERIC": true, "DEC": true, "DECIMAL": true}
	switch {
	case isYearMonthIntervalDataType(upper) || isDayTimeIntervalDataType(upper):
		formatted, _ := formatIntervalColumnType(upper, scale)
		return formatted
	case charTypes[upper]:
		if length > 0 {
			return fmt.Sprintf("%s(%d)", dt, length)
		}
		return dt
	case binaryTypes[upper]:
		if length > 0 {
			return fmt.Sprintf("%s(%d)", dt, length)
		}
		return dt
	case noLengthTypes[upper]:
		return dt
	case timePrecisionTypes[upper]:
		precision := timeFractionalPrecision(scale)
		if precision > 0 && precision <= 9 {
			if idx := strings.Index(strings.ToUpper(dt), " WITH "); idx >= 0 {
				base := strings.TrimSpace(dt[:idx])
				suffix := strings.TrimSpace(dt[idx+6:])
				return fmt.Sprintf("%s(%d) WITH %s", base, precision, suffix)
			}
			return fmt.Sprintf("%s(%d)", dt, precision)
		}
		return dt
	case numberTypes[upper]:
		if length > 0 && !(length == 38 && scale == 0) {
			if scale > 0 {
				return fmt.Sprintf("%s(%d,%d)", dt, length, scale)
			}
			return fmt.Sprintf("%s(%d)", dt, length)
		}
		return dt
	default:
		if length > 0 && length != 4 && length != 8 {
			return fmt.Sprintf("%s(%d)", dt, length)
		}
		return dt
	}
}
