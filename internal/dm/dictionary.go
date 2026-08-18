package dm

import (
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
)

type DictionaryOptions struct {
	SystemPath     string
	SystemReader   io.ReaderAt
	SystemSize     int64
	DataSources    []OfflineDataSource
	ControlPath    string
	ControlDULPath string
	OwnerFilter    string
	Charset        string
	Diagnostic     func(BootstrapDiagnostic)
}

type BootstrapDiagnostic struct {
	Stage     int
	Phase     string
	Name      string
	Mode      string
	Status    string
	RootFile  int16
	RootPage  uint32
	StorageID uint32
	Pages     int
	Rows      int
	Reason    string
}

type DictionaryInfo struct {
	SystemPath          string
	ControlPath         string
	Source              string
	DictionaryDir       string
	ExtentSize          uint32
	ExtentSizeSource    string
	PageSize            uint32
	PageCount           uint32
	Charset             string
	CharsetSource       string
	CaseSensitive       bool
	CaseSensitiveSource string
	HasCaseSensitive    bool
	ObjectCount         int
	UserCount           int
	SchemaCount         int
	TableCount          int
	ColumnCount         int
	ViewCount           int
	SequenceCount       int
	RoutineCount        int
	TriggerCount        int
	SynonymCount        int
	TabPrivilegeCount   int
	PartitionCount      int
	PartitionKeyCount   int
	BootstrapMode       string
	BootstrapFallback   bool
	Diagnostics         []BootstrapDiagnostic
	Users               []DictionaryUser
	Schemas             []DictionarySchema
	Tables              []DictionaryTable
	Columns             []DictionaryColumn
	Views               []DictionaryView
	Sequences           []DictionarySequence
	Routines            []DictionaryRoutine
	Triggers            []DictionaryTrigger
	Synonyms            []DictionarySynonym
	TabPrivileges       []DictionaryTabPrivilege
	Partitions          []DictionaryPartition
	PartitionKeys       []DictionaryPartitionKey
}

type DictionaryUser struct {
	ID   uint32
	Name string
}

// DictionarySchema preserves the distinction between a database user and a
// schema. DM creates one default schema per user, but one user can own more
// than one schema; OWNER and SCHEMAS logical exports therefore cannot be
// implemented correctly from table owner names alone.
type DictionarySchema struct {
	ID      uint32
	Name    string
	OwnerID uint32
	Owner   string
}

type DictionaryTable struct {
	ID              uint32
	Owner           string
	Name            string
	ColumnCount     int
	Tablespace      string
	GroupID         uint32
	HeaderFile      int16
	HeaderBlock     uint32
	Bytes           uint64
	Blocks          uint32
	Extents         uint32
	Temporary       bool
	Storage         string
	Partitioned     bool
	StorageID       uint32
	RootFile        int16
	RootPage        uint32
	AssistIDs       []uint32
	Huge            bool
	HugeWithDelta   bool
	HugeSectionRows uint32
	HugeFileSizeMB  uint32
	HugeAuxTableID  uint32
	HugeRAuxTableID uint32
	HugeDAuxTableID uint32
	HugeUAuxTableID uint32
}

type DictionaryColumn struct {
	TableID    uint32
	TableOwner string
	TableName  string
	ColID      uint16
	Name       string
	DataType   string
	Length     uint32
	Scale      int16
	Nullable   string
	Default    string
}

// DictionaryPartition is the durable representation of one
// SYSHPARTTABLEINFO row. HighValue keeps the complete binary value so a
// recovered dictionary can be edited and reused without rescanning SYSTEM.DBF.
type DictionaryPartition struct {
	BaseTableID uint32
	Owner       string
	TableName   string
	Position    uint32
	Type        string
	Name        string
	PartTableID uint32
	HighValue   []byte
	PageNo      uint32
	SlotNo      uint16
	SlotOffset  uint16
	RowOffset   uint64
}

// DictionaryPartitionKey is decoded from SYSOBJINFOS.TYPE$='TABPART'.
type DictionaryPartitionKey struct {
	TableID    uint32
	Owner      string
	TableName  string
	Position   uint32
	ColID      uint16
	ColumnName string
}

type DictionaryView struct {
	ID       uint32
	Owner    string
	Name     string
	Valid    string
	SQL      string
	QuerySQL string
}

type DictionarySequence struct {
	ID                uint32
	Owner             string
	Name              string
	Valid             string
	StartWith         int64
	HasStartWith      bool
	MinValue          int64
	HasMinValue       bool
	MaxValue          int64
	HasMaxValue       bool
	IncrementBy       int64
	CycleFlag         string
	OrderFlag         string
	CacheSize         uint32
	LastNumber        int64
	HasLastNumber     bool
	RuntimeFile       uint16
	RuntimePage       uint32
	RuntimeSlot       uint16
	RuntimeState      uint8
	HasRuntimeLocator bool
	SQL               string
}

type DictionaryRoutine struct {
	ID         uint32
	Owner      string
	Name       string
	ObjectType string
	SeqNo      uint32
	Valid      string
	SQL        string
}

type DictionaryTrigger struct {
	ID         uint32
	Owner      string
	Name       string
	TableOwner string
	TableName  string
	Valid      string
	SQL        string
}

type DictionarySynonym struct {
	ID         uint32
	Owner      string
	Name       string
	TableOwner string
	TableName  string
	Public     bool
}

type DictionaryTabPrivilege struct {
	Grantee    string
	Owner      string
	ObjectName string
	ObjectType string
	Privilege  string
	Grantable  string
}

func LoadDictionary(opts DictionaryOptions) (*DictionaryInfo, error) {
	if opts.SystemPath == "" {
		return nil, fmt.Errorf("bootstrap requires SYSTEM.DBF path")
	}
	var stream *systemPageStream
	var err error
	if opts.SystemReader != nil {
		stream, err = openSystemPageStreamReader(opts.SystemPath, opts.SystemReader, opts.SystemSize)
	} else {
		stream, err = openSystemPageStream(opts.SystemPath)
	}
	if err != nil {
		return nil, err
	}
	defer stream.close()

	pageSize := stream.pageSize
	pageCount := stream.pageCount
	extentSize, extentSizeSource := stream.extentSize, stream.extentSrc

	preferredCharset := strings.ToLower(strings.TrimSpace(opts.Charset))
	charsetDisplay := defaultIfEmpty(opts.Charset, "auto")
	charsetSource := "decoder setting"
	if charset, ok := stream.charset(); ok {
		charsetDisplay = charset.DisplayName
		charsetSource = charset.Source
		if preferredCharset == "" || preferredCharset == "auto" {
			preferredCharset = charset.DecoderName
		}
	}
	caseSensitive, hasCaseSensitive := stream.caseSensitive()
	caseSensitiveSource := ""
	if hasCaseSensitive {
		caseSensitiveSource = "SYSTEM.DBF page 4 + 0x2C"
	}
	decoder := textDecoder{preferred: preferredCharset}
	ownerMatcher := newOwnerMatcher(opts.OwnerFilter)

	catalog, fallbackReason := loadStandardBootstrapCatalog(stream, decoder, opts.Diagnostic)
	bootstrapMode := "standard-two-stage"
	bootstrapFallback := fallbackReason != ""
	var objects map[uint32]dictionaryObject
	if !bootstrapFallback {
		objects = catalog.objects
	} else {
		bootstrapMode = "stream-scan-fallback"
		catalog.addDiagnostic(BootstrapDiagnostic{
			Stage: 1, Phase: "fallback", Name: "SYSOBJECTS", Mode: bootstrapMode,
			Status: "WARNING", RootFile: -1, Reason: fallbackReason,
		})
		objects, err = stream.dictionaryObjects(decoder)
		if err != nil {
			return nil, err
		}
	}
	schemaNames := schemaNamesFromDictionaryObjects(objects)
	for id, obj := range objects {
		obj.Owner = resolveSchemaName(obj.SchemaID, schemaNames)
		objects[id] = obj
	}

	tables := make(map[uint32]dictionaryObject)
	indexObjects := make(map[uint32]dictionaryObject)
	users := make(map[uint32]DictionaryUser)
	userObjects := make(map[uint32]dictionaryObject)
	roleObjects := make(map[uint32]dictionaryObject)
	for _, obj := range objects {
		switch {
		case obj.Type == "SCHOBJ" && obj.Subtype == "UTAB":
			tables[obj.ID] = obj
		case obj.Type == "TABOBJ" && obj.Subtype == "INDEX":
			indexObjects[obj.ID] = obj
		case obj.Type == "UR" && obj.Subtype == "USER" && isRealURObject(obj):
			userObjects[obj.ID] = obj
			if ownerMatcher.allowed(obj.Name) {
				users[obj.ID] = DictionaryUser{ID: obj.ID, Name: obj.Name}
			}
		case obj.Type == "UR" && obj.Subtype == "ROLE" && isRealURObject(obj):
			roleObjects[obj.ID] = obj
		}
	}
	linkHugeTableObjects(tables)
	schemaList := dictionarySchemasFromObjects(objects, userObjects, ownerMatcher)
	schemaOwners := make(map[string]string, len(schemaList))
	for _, schema := range schemaList {
		schemaOwners[strings.ToUpper(schema.Name)] = schema.Owner
	}

	columnsByTable := make(map[uint32]int)
	allColumnDefsByTable := make(map[uint32][]columnDef)
	var columnList []DictionaryColumn
	columnCount := 0
	parsedColumnRows := 0
	visitColumn := func(page []byte, pageNo uint32, slotNo uint16, slotOff uint16) {
		col, ok := parseDDLColumnRow(page, int(slotOff), pageNo, slotNo, slotOff, pageSize, decoder)
		if !ok {
			return
		}
		parsedColumnRows++
		table, ok := tables[col.TableID]
		if !ok || !ownerMatcher.allowed(table.Owner) {
			return
		}
		allColumnDefsByTable[col.TableID] = append(allColumnDefsByTable[col.TableID], col)
		if table.isHugeInternalTable() {
			return
		}
		columnsByTable[col.TableID]++
		columnList = append(columnList, DictionaryColumn{
			TableID:    col.TableID,
			TableOwner: table.Owner,
			TableName:  table.Name,
			ColID:      col.ColID,
			Name:       col.Name,
			DataType:   col.DataType,
			Length:     col.Length,
			Scale:      col.Scale,
			Nullable:   col.Nullable,
			Default:    col.Default,
		})
		columnCount++
	}
	usedStandardColumns := false
	if !bootstrapFallback {
		usedStandardColumns, err = catalog.forEachTableRow("SYSCOLUMNS", visitColumn)
	}
	if err != nil {
		return nil, err
	}
	if !usedStandardColumns {
		if err := stream.forEachDictionaryRow(visitColumn); err != nil {
			return nil, err
		}
		catalog.recordTableRows("SYSCOLUMNS", parsedColumnRows, false, defaultIfEmpty(fallbackReason, "standard table plan is unavailable"))
	} else {
		catalog.recordTableRows("SYSCOLUMNS", parsedColumnRows, true, "")
	}
	repairHugeMainColumnsFromRAux(tables, allColumnDefsByTable)
	for i := range columnList {
		for _, recovered := range allColumnDefsByTable[columnList[i].TableID] {
			if recovered.ColID == columnList[i].ColID {
				columnList[i].DataType = recovered.DataType
				columnList[i].Length = recovered.Length
				columnList[i].Scale = recovered.Scale
				break
			}
		}
	}

	indexes := catalog.indexes
	if bootstrapFallback {
		indexes = make(map[uint32]indexDef)
		if err := stream.forEachDictionaryRow(func(page []byte, pageNo uint32, slotNo uint16, slotOff uint16) {
			idx, ok := parseDDLIndexRow(page, int(slotOff), pageSize)
			if ok {
				indexes[idx.ID] = idx
			}
		}); err != nil {
			return nil, err
		}
	}
	var texts map[uint32]map[uint32]string
	usedStandardTexts := false
	if !bootstrapFallback {
		texts, usedStandardTexts, err = catalog.dictionaryTexts()
	}
	if err != nil {
		return nil, err
	}
	if !usedStandardTexts {
		texts, err = stream.dictionaryTexts(decoder)
		if err != nil {
			return nil, err
		}
		catalog.recordTableRows("SYSTEXTS", countDictionaryTextRows(texts), false, defaultIfEmpty(fallbackReason, "standard table plan is unavailable"))
	}
	viewList := scanDictionaryViews(objects, texts, ownerMatcher)
	sequenceList := scanDictionarySequences(objects, texts, ownerMatcher)
	enrichSequenceRuntimeValues(stream, sequenceList)
	if len(sequenceList) > 0 {
		recovered, pages := sequenceRuntimeRecoveryStats(sequenceList)
		status := "OK"
		reason := ""
		if recovered != len(sequenceList) {
			status = "NOTICE"
			reason = fmt.Sprintf("recovered %d/%d sequence runtime values", recovered, len(sequenceList))
		}
		catalog.addDiagnostic(BootstrapDiagnostic{
			Stage: 2, Phase: "extract", Name: "SEQUENCE_STATE", Mode: "runtime-page-slot",
			Status: status, RootFile: -1, Pages: pages, Rows: recovered, Reason: reason,
		})
	}
	rawRoutines, err := stream.rawRoutineTexts(decoder)
	if err != nil {
		return nil, err
	}
	rawTriggers, err := stream.rawTriggerTexts(decoder)
	if err != nil {
		return nil, err
	}
	routineList := scanDictionaryRoutines(objects, texts, rawRoutines, ownerMatcher)
	triggerList := scanDictionaryTriggers(objects, texts, rawTriggers, ownerMatcher)
	if len(rawRoutines) > 0 {
		catalog.addDiagnostic(BootstrapDiagnostic{Stage: 2, Phase: "source", Name: "ROUTINES", Mode: "raw-window-fallback", Status: "NOTICE", RootFile: -1, Rows: len(rawRoutines), Reason: "supplemented SYSTEXTS definitions"})
	}
	if len(rawTriggers) > 0 {
		catalog.addDiagnostic(BootstrapDiagnostic{Stage: 2, Phase: "source", Name: "TRIGGERS", Mode: "raw-window-fallback", Status: "NOTICE", RootFile: -1, Rows: len(rawTriggers), Reason: "supplemented SYSTEXTS definitions"})
	}
	synonymList := scanDictionarySynonyms(objects, ownerMatcher)
	var tabPrivilegeList []DictionaryTabPrivilege
	usedStandardGrants := false
	if !bootstrapFallback {
		tabPrivilegeList, usedStandardGrants, err = catalog.tabPrivileges(objects, userObjects, roleObjects, ownerMatcher, newTableNameMatcher("all"))
	}
	if err != nil {
		return nil, err
	}
	if !usedStandardGrants {
		tabPrivilegeList, err = stream.tabPrivileges(objects, userObjects, roleObjects, ownerMatcher, newTableNameMatcher("all"))
		if err != nil {
			return nil, err
		}
		catalog.recordTableRows("SYSGRANTS", len(tabPrivilegeList), false, defaultIfEmpty(fallbackReason, "standard table plan is unavailable"))
	}

	tablespaces := loadTablespaceNames(opts.ControlPath, opts.ControlDULPath)
	tableStorage := tableStorageByID(tables, indexObjects, indexes, tablespaces)
	assistByParentID := assistIndexesByParentID(tables, indexObjects, indexes)
	var partitionsByTable map[uint32][]PartitionInfo
	usedStandardPartitions := false
	if !bootstrapFallback {
		partitionsByTable, usedStandardPartitions, err = catalog.partitionsByTable(tables, ownerMatcher)
	}
	if err != nil {
		return nil, err
	}
	if !usedStandardPartitions {
		partitionsByTable, err = stream.partitionsByTable(decoder, tables, ownerMatcher)
		if err != nil {
			return nil, err
		}
		catalog.recordTableRows("SYSHPARTTABLEINFO", countPartitions(partitionsByTable), false, defaultIfEmpty(fallbackReason, "standard table plan is unavailable"))
	}
	var partitionKeysByTable map[uint32][]uint16
	usedStandardPartitionKeys := false
	if !bootstrapFallback {
		partitionKeysByTable, usedStandardPartitionKeys, err = catalog.partitionKeysByTable(tables, ownerMatcher)
	}
	if err != nil {
		return nil, err
	}
	if !usedStandardPartitionKeys {
		partitionKeysByTable, err = stream.partitionKeysByTable(decoder, tables, ownerMatcher)
		if err != nil {
			return nil, err
		}
		catalog.recordTableRows("SYSOBJINFOS", countPartitionKeys(partitionKeysByTable), false, defaultIfEmpty(fallbackReason, "standard table plan is unavailable"))
	}
	var tableList []DictionaryTable
	userNamesByName := make(map[string]DictionaryUser)
	for _, user := range users {
		userNamesByName[strings.ToUpper(user.Name)] = user
	}
	for id, table := range tables {
		if !ownerMatcher.allowed(table.Owner) || table.isHugeInternalTable() || columnsByTable[id] == 0 {
			continue
		}
		var groupID uint32
		var tablespace string
		if storage, ok := tableStorage[id]; ok {
			groupID = uint32(storage.GroupID)
			tablespace = tablespaces[groupID]
		}
		if table.isHugeTable() && groupID == 0 {
			groupID = table.Info2 & 0xFFFF
			tablespace = tablespaces[groupID]
		}
		storageID, rootFile, rootPage, assistIDs := dictionaryTableStorageSnapshot(id, tableStorage, assistByParentID)
		tableList = append(tableList, DictionaryTable{
			ID:              table.ID,
			Owner:           table.Owner,
			Name:            table.Name,
			ColumnCount:     columnsByTable[id],
			Tablespace:      tablespace,
			GroupID:         groupID,
			Temporary:       table.isTemporaryTable(),
			Storage:         table.tableStorageOrganization(),
			Partitioned:     len(partitionsByTable[id]) > 0,
			StorageID:       storageID,
			RootFile:        rootFile,
			RootPage:        rootPage,
			AssistIDs:       assistIDs,
			Huge:            table.isHugeTable(),
			HugeWithDelta:   table.hugeWithDelta(),
			HugeSectionRows: table.hugeSectionRows(),
			HugeFileSizeMB:  table.hugeFileSizeMB(),
			HugeAuxTableID:  table.HugeAuxID,
			HugeRAuxTableID: table.HugeRAuxID,
			HugeDAuxTableID: table.HugeDAuxID,
			HugeUAuxTableID: table.HugeUAuxID,
		})
		if _, knownSchema := schemaOwners[strings.ToUpper(table.Owner)]; !knownSchema {
			if _, ok := userNamesByName[strings.ToUpper(table.Owner)]; !ok {
				userNamesByName[strings.ToUpper(table.Owner)] = DictionaryUser{Name: table.Owner}
			}
		}
	}
	segments, err := inferDictionaryTableSegments(opts.ControlPath, opts.ControlDULPath, filepath.Dir(opts.SystemPath), opts.DataSources, pageSize, extentSize, tables, indexObjects, indexes, partitionsByTable, tableList)
	if err != nil {
		return nil, err
	}
	for i := range tableList {
		if seg, ok := segments[tableList[i].ID]; ok {
			tableList[i].HeaderFile = seg.fileID
			tableList[i].HeaderBlock = seg.headerPage
			tableList[i].Bytes = seg.bytes
			tableList[i].Blocks = seg.blocks
			tableList[i].Extents = seg.extents
			if seg.tablespaceID != 0 {
				tableList[i].GroupID = seg.tablespaceID
				if name := tablespaces[seg.tablespaceID]; name != "" {
					tableList[i].Tablespace = name
				}
			}
		}
	}

	userList := make([]DictionaryUser, 0, len(userNamesByName))
	for _, user := range userNamesByName {
		userList = append(userList, user)
	}
	sort.Slice(userList, func(i, j int) bool {
		return userList[i].Name < userList[j].Name
	})
	sort.Slice(tableList, func(i, j int) bool {
		if tableList[i].Owner == tableList[j].Owner {
			if tableList[i].Name == tableList[j].Name {
				return tableList[i].ID < tableList[j].ID
			}
			return tableList[i].Name < tableList[j].Name
		}
		return tableList[i].Owner < tableList[j].Owner
	})
	sort.Slice(columnList, func(i, j int) bool {
		if columnList[i].TableOwner != columnList[j].TableOwner {
			return columnList[i].TableOwner < columnList[j].TableOwner
		}
		if columnList[i].TableName != columnList[j].TableName {
			return columnList[i].TableName < columnList[j].TableName
		}
		if columnList[i].ColID != columnList[j].ColID {
			return columnList[i].ColID < columnList[j].ColID
		}
		return columnList[i].Name < columnList[j].Name
	})
	partitionList := dictionaryPartitionsFromMap(tables, partitionsByTable)
	partitionKeyList := dictionaryPartitionKeysFromMap(tables, partitionKeysByTable, columnList)
	if !bootstrapFallback && catalog.fallback {
		bootstrapFallback = true
		bootstrapMode = "standard-two-stage-with-fallback"
	}

	return &DictionaryInfo{
		SystemPath:          opts.SystemPath,
		ControlPath:         opts.ControlPath,
		Source:              "SYSTEM.DBF",
		ExtentSize:          extentSize,
		ExtentSizeSource:    extentSizeSource,
		PageSize:            pageSize,
		PageCount:           pageCount,
		Charset:             charsetDisplay,
		CharsetSource:       charsetSource,
		CaseSensitive:       caseSensitive,
		CaseSensitiveSource: caseSensitiveSource,
		HasCaseSensitive:    hasCaseSensitive,
		ObjectCount:         len(objects),
		UserCount:           len(userList),
		SchemaCount:         len(schemaList),
		TableCount:          len(tableList),
		ColumnCount:         columnCount,
		ViewCount:           len(viewList),
		SequenceCount:       len(sequenceList),
		RoutineCount:        len(routineList),
		TriggerCount:        len(triggerList),
		SynonymCount:        len(synonymList),
		TabPrivilegeCount:   len(tabPrivilegeList),
		PartitionCount:      len(partitionList),
		PartitionKeyCount:   len(partitionKeyList),
		BootstrapMode:       bootstrapMode,
		BootstrapFallback:   bootstrapFallback,
		Diagnostics:         append([]BootstrapDiagnostic(nil), catalog.diagnostics...),
		Users:               userList,
		Schemas:             schemaList,
		Tables:              tableList,
		Columns:             columnList,
		Views:               viewList,
		Sequences:           sequenceList,
		Routines:            routineList,
		Triggers:            triggerList,
		Synonyms:            synonymList,
		TabPrivileges:       tabPrivilegeList,
		Partitions:          partitionList,
		PartitionKeys:       partitionKeyList,
	}, nil
}

func dictionarySchemasFromObjects(objects map[uint32]dictionaryObject, users map[uint32]dictionaryObject, matcher ownerMatcher) []DictionarySchema {
	result := make([]DictionarySchema, 0)
	for _, obj := range objects {
		if obj.Type != "SCH" || obj.Valid == "N" || !isSafeShortText(obj.Name) || obj.ParentID <= 0 {
			continue
		}
		owner, ok := users[uint32(obj.ParentID)]
		if !ok || !isSafeShortText(owner.Name) {
			continue
		}
		if !matcher.allowed(obj.Name) && !matcher.allowed(owner.Name) {
			continue
		}
		result = append(result, DictionarySchema{
			ID: obj.ID, Name: obj.Name, OwnerID: owner.ID, Owner: owner.Name,
		})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Owner != result[j].Owner {
			return result[i].Owner < result[j].Owner
		}
		if result[i].Name != result[j].Name {
			return result[i].Name < result[j].Name
		}
		return result[i].ID < result[j].ID
	})
	return result
}

func dictionaryTableStorageSnapshot(tableID uint32, tableStorage map[uint32]indexDef, assistByParentID map[uint32][]indexDef) (uint32, int16, uint32, []uint32) {
	var primary indexDef
	if storage, ok := tableStorage[tableID]; ok {
		primary = storage
	} else if assists := assistByParentID[tableID]; len(assists) > 0 {
		primary = assists[0]
	}
	rootFile := int16(-1)
	if primary.RootFile >= 0 {
		rootFile = primary.RootFile
	}
	var rootPage uint32
	if primary.RootPage >= 0 {
		rootPage = uint32(primary.RootPage)
	}
	seen := make(map[uint32]bool)
	var assistIDs []uint32
	add := func(id uint32) {
		if id == 0 || seen[id] {
			return
		}
		seen[id] = true
		assistIDs = append(assistIDs, id)
	}
	add(primary.ID)
	for _, storage := range assistByParentID[tableID] {
		add(storage.ID)
	}
	add(tableDataAssistID(tableID))
	return primary.ID, rootFile, rootPage, assistIDs
}
