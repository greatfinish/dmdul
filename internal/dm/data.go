package dm

import (
	"encoding/binary"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

const (
	dataRowAreaStart       = 0x62
	dataPageSlotCountOff   = 0x24
	dataPageFreeEndOff     = 0x26
	dataPageRecordCountOff = 0x2C
	dataPageTreeLevelOff   = 0x38
	dataPageAssistIndexOff = 0x3A
	dmPageKindLOBData      = 0x20
	dmPageKindLongRowData  = 0x22
	dmPageKindRowData      = 0x14
	dmPageKindRowOverflow  = 0x16
	dmLOBPagePayloadOff    = 0x38
	dmLOBPageIDOff         = 0x24
	dmLOBPagePayloadLenOff = 0x2C
	dmLOBLocatorSize       = 21
	maxOpenSplitDataFiles  = 32
	dataRowDeletedMask     = uint16(0x8000)
	dataRowControlTailLen  = 19
)

var errUnsupportedRowMetadataState = errors.New("unsupported row metadata state")

type DataExportOptions struct {
	SystemPath          string
	SystemReader        SizedReaderAt
	DataSources         []OfflineDataSource
	ControlPath         string
	ControlDULPath      string
	DataDir             string
	OutputPath          string
	TableOutputPath     func(owner string, table string, tableID uint32) string
	OwnerFilter         string
	TableFilter         string
	ExcludeTables       string
	Charset             string
	OutputFormat        string
	DMPCaseSensitive    *bool
	MaxRows             int
	WriteFailedComments bool
	RecoveryMode        bool
	Dictionary          *DictionaryInfo
}

type DataExportResult struct {
	SystemPath           string
	OutputPath           string
	DataDir              string
	PageSize             uint32
	ObjectCount          int
	TableCount           int
	ColumnCount          int
	AssistIndexCount     int
	DataFileCount        int
	PagesScanned         int
	PlannedPages         int
	DirectPagesRead      int
	FallbackPagesScanned int
	FallbackReasons      []string
	RowsLocated          int
	RowsExported         int
	RowsFailed           int
	TablesWithRows       int
	TablesWithoutRows    int
	TableRowCounts       []DataTableRowCount
	TableOutputs         []DataTableOutput
	RecoverySources      []DataRecoverySource
	OutputFormat         string
	TimeFractionLoss     int
	HugeTableCount       int
	HugeSectionsRead     int
	HugeFilesRead        int
	// OversizedSQLStatements counts generated SQL INSERT statements longer
	// than the portable disql stdin line limit. DM9 silently skips such input
	// after reporting DISQL-10053, losing the row on import.
	OversizedSQLStatements int
	OversizedSQLTables     []string
}

type DataTableOutput struct {
	TableID    uint32
	Owner      string
	Name       string
	OutputPath string
}

type DataTableRowCount struct {
	Owner        string
	Name         string
	RowsLocated  int
	RowsExported int
	RowsFailed   int
}

// DataRecoverySource records the physical source accepted by recover table.
// Heuristic is true only when the source storage id is absent from the live
// dictionary and therefore cannot be attributed to the target with certainty.
type DataRecoverySource struct {
	Owner        string
	Name         string
	GroupID      uint32
	FileID       int16
	StorageID    uint32
	FirstPage    uint32
	LastPage     uint32
	Pages        int
	RowsLocated  int
	RowsExported int
	RowsFailed   int
	Heuristic    bool
}

type dataRecoverySourceKey struct {
	tableID   uint32
	groupID   uint32
	fileID    int16
	storageID uint32
	heuristic bool
}

type dataFileKey struct {
	groupID uint32
	fileID  int16
}

type dataFileRef struct {
	key            dataFileKey
	path           string
	tablespaceName string
	reader         SizedReaderAt
}

type dataPageRef struct {
	key    dataFileKey
	pageNo uint32
}

type dataTableInfo struct {
	table           dictionaryObject
	columns         []columnDef
	storage         indexDef
	storageKnown    bool
	dataStorageID   uint32
	historicalRows  bool
	lobReader       *dmLOBReader
	pagePlan        map[dataPageRef]bool
	pagePlanKnown   bool
	storageUnitID   uint32
	scanGroupOnly   bool
	scanGroupID     uint32
	recoveryMode    bool
	orphanRecovery  bool
	recoveryGroupID uint32
	segment         tableSegment
	segmentKnown    bool
	huge            bool
	// sqlInsertPrefix caches `INSERT INTO "O"."T" ("c1", ...) VALUES (` so
	// the per-row SQL renderer does not rebuild and re-quote the identical
	// column list for every row.
	sqlInsertPrefix string
	// fldrDialect is the delimiter pair the table's dmfldr .txt and .ctl agree
	// on, decided once from the column types (see fldrDialectForColumns).
	fldrDialect fldrDialect
	// coverage tracks per-row column-prefix keys, which deduplicate
	// ALTER-history partial rows against full rows when the same logical row
	// can be visited through more than one storage (historical assists,
	// orphan recovery, recover mode). Plain direct-read exports visit each
	// physical row exactly once (page-level dedup), so the pointer stays nil
	// there; tracking would burn O(columns) strings per row, gigabytes on
	// 10M-row tables. The state is shared by all candidates of one table and
	// self-disables once maxCoverageKeysPerTable is reached.
	coverage *tableCoverageState
}

// tableCoverageState bounds row-coverage memory for one table. Once the key
// count reaches maxCoverageKeysPerTable the map is dropped and key generation
// stops; pending partial rows are then emitted without full-row dedup and the
// export reports it, instead of the process dying on multi-GB key maps.
// overflow is atomic because parallel decode workers consult active() while
// the writer goroutine marks keys; the keys map itself is writer-only.
type tableCoverageState struct {
	keys     map[string]bool
	overflow atomic.Bool
}

const maxCoverageKeysPerTable = 2_000_000

func (s *tableCoverageState) active() bool {
	return s != nil && !s.overflow.Load()
}

func (s *tableCoverageState) mark(keys []string) {
	if s == nil || s.overflow.Load() || len(keys) == 0 {
		return
	}
	if s.keys == nil {
		s.keys = make(map[string]bool)
	}
	for _, key := range keys {
		if key != "" {
			s.keys[key] = true
		}
	}
	if len(s.keys) >= maxCoverageKeysPerTable {
		s.overflow.Store(true)
		s.keys = nil
	}
}

func (s *tableCoverageState) covered(keys ...string) bool {
	if s == nil || s.keys == nil {
		return false
	}
	for _, key := range keys {
		if key != "" && s.keys[key] {
			return true
		}
	}
	return false
}

type tableSegment struct {
	fileID       int16
	headerPage   uint32
	blocks       uint32
	extents      uint32
	bytes        uint64
	tablespace   string
	tablespaceID uint32
}

type dataValue struct {
	value any
}

type dmNumber string

type dmBinary []byte

type dmRowID string

type dmJSON string

type dmVectorValue struct {
	text string
	raw  dmBinary
}

func (v dmVectorValue) String() string { return v.text }

type dmJSONValue struct {
	value   any
	binary  bool
	decoder textDecoder
}

type dataRowRenderMeta struct {
	partial       bool
	prefixKey     string
	weakPrefixKey string
	coverageKeys  []string
	presentColIDs []uint16
}

type pendingPartialDataRow struct {
	tableID          uint32
	line             string
	record           []string
	fields           []DMPField
	timeFractionLoss bool
	stats            *DataTableRowCount
	meta             dataRowRenderMeta
}

func ExportData(opts DataExportOptions) (*DataExportResult, error) {
	if opts.SystemPath == "" {
		return nil, fmt.Errorf("export-data requires SYSTEM.DBF path")
	}
	if opts.OutputPath == "" && opts.TableOutputPath == nil {
		return nil, fmt.Errorf("export-data requires output path")
	}

	dataDir := strings.TrimSpace(opts.DataDir)
	if dataDir == "" {
		dataDir = filepath.Dir(opts.SystemPath)
		if dataDir == "" {
			dataDir = "."
		}
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
	pageSize := stream.pageSize

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
	excludeMatcher := newTableNameMatcher(opts.ExcludeTables)
	outputFormat := normalizeDataOutputFormat(opts.OutputFormat)
	if outputFormat == "" {
		return nil, fmt.Errorf("unsupported data output format %q", opts.OutputFormat)
	}
	dmpConfig := dataDMPOutputConfig{}
	if outputFormat == "dmp" {
		dmpCharset, err := dmpCharsetForDataExport(preferredCharset)
		if err != nil {
			return nil, err
		}
		caseSensitive := opts.DMPCaseSensitive
		if caseSensitive == nil {
			if detected, ok := stream.caseSensitive(); ok {
				caseSensitive = &detected
			} else {
				caseSensitive = detectDMPCaseSensitive(opts.SystemPath, dataDir, pageSize)
			}
		}
		dmpConfig = dataDMPOutputConfig{
			charset: dmpCharset, extentSize: stream.extentSize, pageSize: pageSize,
			caseSensitive: caseSensitive,
		}
	}
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
	for _, obj := range objects {
		switch {
		case obj.Type == "SCHOBJ" && obj.Subtype == "UTAB":
			tables[obj.ID] = obj
		case obj.Type == "TABOBJ" && obj.Subtype == "INDEX":
			indexObjects[obj.ID] = obj
		}
	}
	linkHugeTableObjects(tables)
	dictionaryTables := applyDictionaryTableOverrides(opts.Dictionary, tables, nil)
	hugeAuxTableIDs := selectedHugeAuxTableIDs(tables, ownerMatcher, tableMatcher, excludeMatcher)

	columnsByTable := make(map[uint32][]columnDef)
	columnCount := 0
	if err := stream.forEachDictionaryRow(func(page []byte, pageNo uint32, slotNo uint16, slotOff uint16) {
		col, ok := parseDDLColumnRow(page, int(slotOff), pageNo, slotNo, slotOff, pageSize, decoder)
		if !ok {
			return
		}
		table, ok := tables[col.TableID]
		if !ok || !ownerMatcher.allowed(table.Owner) {
			return
		}
		selectedUserTable := !table.isSystemManagedInternalTable() && tableMatcher.allowed(table.Owner, table.Name) && !excludeMatcher.allowed(table.Owner, table.Name)
		if !selectedUserTable && !hugeAuxTableIDs[col.TableID] {
			return
		}
		columnsByTable[col.TableID] = append(columnsByTable[col.TableID], col)
		if selectedUserTable {
			columnCount++
		}
	}); err != nil {
		return nil, err
	}
	for tableID := range columnsByTable {
		sort.Slice(columnsByTable[tableID], func(i, j int) bool {
			return columnsByTable[tableID][i].ColID < columnsByTable[tableID][j].ColID
		})
	}
	if dictColumnsByTable, _, dictColumnCount, ok := dictionaryColumnMaps(opts.Dictionary, dictionaryTables, tables, ownerMatcher, tableMatcher, excludeMatcher); ok {
		for tableID, columns := range dictColumnsByTable {
			columnsByTable[tableID] = columns
		}
		columnCount = dictColumnCount
	}
	ensureHugeAuxColumnDefinitions(tables, columnsByTable)

	indexes := make(map[uint32]indexDef)
	if catalog, fallbackReason := loadStandardBootstrapCatalog(stream, decoder, nil); fallbackReason == "" && len(catalog.indexes) > 0 {
		for id, index := range catalog.indexes {
			indexes[id] = index
		}
		for id, obj := range catalog.objects {
			if obj.Type != "TABOBJ" || obj.Subtype != "INDEX" {
				continue
			}
			obj.Owner = resolveSchemaName(obj.SchemaID, schemaNames)
			indexObjects[id] = obj
		}
	} else if err := stream.forEachDictionaryRow(func(page []byte, pageNo uint32, slotNo uint16, slotOff uint16) {
		idx, ok := parseDDLIndexRow(page, int(slotOff), pageSize)
		if ok {
			indexes[idx.ID] = idx
		}
	}); err != nil {
		return nil, err
	}

	assistByParentID := assistIndexesByParentID(tables, indexObjects, indexes)
	mergeDictionaryStorageRoots(assistByParentID, dictionaryTables)
	partitionsByTable, err := stream.partitionsByTable(decoder, tables, ownerMatcher)
	if err != nil {
		return nil, err
	}
	applyDictionaryPartitionOverrides(opts.Dictionary, dictionaryTables, tables, ownerMatcher, partitionsByTable, nil)
	dataStorageByTable := tableStorageByID(tables, indexObjects, indexes, nil)
	ensureHugeAuxStorageMappings(tables, indexes, assistByParentID, dataStorageByTable)
	secondaryIndexStorageIDs := secondaryIndexStorageIDSet(indexObjects, indexes)
	var dataFiles []dataFileRef
	if len(opts.DataSources) > 0 {
		dataFiles, err = dataFileRefsFromSources(opts.DataSources)
		if err != nil {
			return nil, err
		}
	} else {
		dataFiles, err = resolveDataFiles(opts.ControlPath, opts.ControlDULPath, dataDir)
		if err != nil {
			return nil, err
		}
	}
	dataFilePages := newDataFilePageCache(dataFiles, pageSize)
	lobReader := &dmLOBReader{cache: dataFilePages}
	selectedTables := make(map[uint32]dataTableInfo)
	hugeTables := make(map[uint32]dataTableInfo)
	storageUnits := make(map[uint32]dataTableInfo)
	assistByID := make(map[uint32][]dataTableInfo)
	planFailureReasons := make(map[uint32][]string)
	for tableID, table := range tables {
		if !ownerMatcher.allowed(table.Owner) || !tableMatcher.allowed(table.Owner, table.Name) || excludeMatcher.allowed(table.Owner, table.Name) || table.isSystemManagedInternalTable() {
			continue
		}
		if table.isTemporaryTable() || len(columnsByTable[tableID]) == 0 {
			continue
		}
		baseInfo := dataTableInfo{
			table:           table,
			columns:         columnsByTable[tableID],
			dataStorageID:   dataStorageIDForTable(dictionaryTables, dataStorageByTable, tableID),
			lobReader:       lobReader,
			storageUnitID:   tableID,
			recoveryMode:    opts.RecoveryMode,
			recoveryGroupID: dictionaryTableGroupID(dictionaryTables, tableID),
			segment:         segmentByTableID(opts.Dictionary, tableID),
			segmentKnown:    hasSegmentRange(opts.Dictionary, tableID),
			sqlInsertPrefix: sqlInsertPrefixForTable(table, columnsByTable[tableID]),
			fldrDialect:     fldrDialectForColumns(columnsByTable[tableID]),
			huge:            table.isHugeTable(),
		}
		selectedTables[tableID] = baseInfo
		if baseInfo.huge {
			hugeTables[tableID] = baseInfo
			continue
		}
		// Partitioned base objects describe the logical table; rows belong to
		// leaf partition storage objects. Treating an unresolved parent storage
		// as another row source forces an unnecessary group/segment fallback and
		// can duplicate rows. Build physical units only for the partitions.
		if len(partitionsByTable[tableID]) == 0 {
			storageUnits[tableID] = baseInfo
			for _, storage := range assistByParentID[tableID] {
				if baseInfo.dataStorageID != 0 && storage.ID != baseInfo.dataStorageID {
					continue
				}
				var pagePlan map[dataPageRef]bool
				var reason string
				if !opts.RecoveryMode {
					pagePlan, reason = buildStoragePagePlanDetailed(storage, dataFilePages)
				}
				addKnownDataAssistID(assistByID, baseInfo, storage.ID, storage, pagePlan)
				if !opts.RecoveryMode && len(pagePlan) == 0 {
					planFailureReasons[tableID] = append(planFailureReasons[tableID], formatStoragePlanFailure(baseInfo, storage.ID, reason))
				}
			}
			for _, assistID := range dictionaryDataAssistIDs(dictionaryTables, tableID) {
				// Secondary index storages hold key/rowid entries, not table
				// rows; scanning them yields garbage rows shaped like the table.
				if assistID != baseInfo.dataStorageID && secondaryIndexStorageIDs[assistID] {
					continue
				}
				addHistoricalDataAssistID(assistByID, baseInfo, assistID)
				if opts.RecoveryMode {
					addRecoveryDataAssistID(assistByID, baseInfo, assistID)
				}
			}
			addHiddenIndexObjectAssistIDs(assistByID, baseInfo, tableID, indexObjects, indexes)
			if baseInfo.dataStorageID == 0 {
				// The 0x02000000|table_id guess can collide with unrelated live
				// storages, so it is only worth scanning when the dictionary has
				// no real storage id for the table.
				addUnknownDataAssistID(assistByID, baseInfo, tableDataAssistID(tableID))
			}
		}
		for _, part := range partitionsByTable[tableID] {
			partInfo := baseInfo
			partInfo.storageUnitID = part.PartTableID
			partitionStorageID := dataStorageIDForTable(dictionaryTables, dataStorageByTable, part.PartTableID)
			partInfo.dataStorageID = partitionStorageID
			storageUnits[part.PartTableID] = partInfo
			for _, storage := range assistByParentID[part.PartTableID] {
				if partitionStorageID != 0 && storage.ID != partitionStorageID {
					continue
				}
				partInfo.recoveryGroupID = uint32(storage.GroupID)
				storageUnits[part.PartTableID] = partInfo
				var pagePlan map[dataPageRef]bool
				var reason string
				if !opts.RecoveryMode {
					pagePlan, reason = buildStoragePagePlanDetailed(storage, dataFilePages)
				}
				addKnownDataAssistID(assistByID, partInfo, storage.ID, storage, pagePlan)
				if !opts.RecoveryMode && len(pagePlan) == 0 {
					planFailureReasons[part.PartTableID] = append(planFailureReasons[part.PartTableID], formatStoragePlanFailure(partInfo, storage.ID, reason))
				}
			}
			addHiddenIndexObjectAssistIDs(assistByID, partInfo, part.PartTableID, indexObjects, indexes)
			if partInfo.dataStorageID == 0 {
				addUnknownDataAssistID(assistByID, partInfo, tableDataAssistID(part.PartTableID))
			}
		}
	}

	coverageStates := stampRowCoverageTracking(assistByID, storageUnits, opts.RecoveryMode)
	directCandidates, plannedRefs, plannedUnits := buildDirectDataPageCandidates(assistByID)

	result := &DataExportResult{
		SystemPath:       opts.SystemPath,
		OutputPath:       opts.OutputPath,
		DataDir:          dataDir,
		PageSize:         pageSize,
		OutputFormat:     outputFormat,
		ObjectCount:      len(objects),
		TableCount:       len(selectedTables),
		ColumnCount:      columnCount,
		AssistIndexCount: len(assistByID),
		DataFileCount:    0,
		PlannedPages:     len(plannedRefs),
		HugeTableCount:   len(hugeTables),
	}
	rowStats := initDataTableRowStats(selectedTables)
	if (outputFormat == "fldr" || outputFormat == "dmp") && opts.TableOutputPath == nil && len(selectedTables) > 1 {
		return nil, fmt.Errorf("%s export requires exactly one table or per-table output paths; selected %d tables", outputFormat, len(selectedTables))
	}

	output, err := newDataOutputRouter(opts, outputFormat, selectedTables, dmpConfig)
	if err != nil {
		return nil, err
	}
	outputClosed := false
	defer func() {
		if !outputClosed {
			_ = output.close()
		}
	}()

	hugeContext := hugeDataExportContext{
		dataDir:        dataDir,
		tables:         tables,
		columnsByTable: columnsByTable,
		storageByTable: dataStorageByTable,
		dataFiles:      dataFiles,
		pageCache:      dataFilePages,
		pageSize:       pageSize,
		decoder:        decoder,
		outputFormat:   outputFormat,
		dmpCharset:     dmpConfig.charset,
		maxRows:        opts.MaxRows,
	}
	for _, tableID := range sortedDataTableIDs(hugeTables) {
		info := hugeTables[tableID]
		stats, exportErr := exportHugeTableData(hugeContext, info, output, rowStats[tableID], result)
		result.HugeSectionsRead += stats.sectionsRead
		result.HugeFilesRead += stats.filesRead
		if exportErr == nil {
			continue
		}
		if len(selectedTables) == 1 {
			return nil, fmt.Errorf("export HUGE table %s.%s: %w", info.table.Owner, info.table.Name, exportErr)
		}
		result.RowsFailed++
		if rowStats[tableID] != nil {
			rowStats[tableID].RowsFailed++
		}
		result.FallbackReasons = append(result.FallbackReasons,
			fmt.Sprintf("HUGE table %s.%s was not exported: %v", info.table.Owner, info.table.Name, exportErr))
	}

	ordinaryTableCount := len(selectedTables) - len(hugeTables)
	if ordinaryTableCount == 0 {
		result.TableRowCounts = finalizeDataTableRowStats(rowStats)
		for _, item := range result.TableRowCounts {
			if item.RowsLocated > 0 {
				result.TablesWithRows++
			} else {
				result.TablesWithoutRows++
			}
		}
		result.TableOutputs = output.tableOutputs()
		result.OversizedSQLStatements = output.oversizedSQLRows
		result.OversizedSQLTables = sortedOversizedSQLTables(output.oversizedSQLTableIDs)
		if (outputFormat == "fldr" || outputFormat == "dmp") && opts.TableOutputPath == nil && result.RowsExported == 0 {
			result.OutputPath = ""
		}
		if err := output.close(); err != nil {
			return nil, fmt.Errorf("finalize %s data output: %w", outputFormat, err)
		}
		outputClosed = true
		return result, nil
	}

	if len(assistByID) == 0 || len(dataFiles) == 0 {
		if len(assistByID) == 0 {
			result.FallbackReasons = append(result.FallbackReasons, "no table data storage mapping is available for the selected tables")
		}
		if len(dataFiles) == 0 {
			result.FallbackReasons = append(result.FallbackReasons, "no user data files are available; selected tables were not scanned")
		}
		result.TableRowCounts = finalizeDataTableRowStats(rowStats)
		for _, item := range result.TableRowCounts {
			if item.RowsLocated > 0 {
				result.TablesWithRows++
			} else {
				result.TablesWithoutRows++
			}
		}
		result.TableOutputs = output.tableOutputs()
		if (outputFormat == "fldr" || outputFormat == "dmp") && opts.TableOutputPath == nil {
			result.OutputPath = ""
		}
		if err := output.close(); err != nil {
			return nil, fmt.Errorf("finalize %s data output: %w", outputFormat, err)
		}
		outputClosed = true
		return result, nil
	}

	stop := opts.MaxRows > 0 && result.RowsLocated >= opts.MaxRows
	var pendingPartialRows []pendingPartialDataRow
	touchedFiles := make(map[dataFileKey]bool)
	processedDirectPages := make(map[dataPageRef]bool)
	failedPlanUnits := make(map[uint32]bool)
	fallbackReasonSeen := make(map[string]bool)
	for _, reason := range result.FallbackReasons {
		fallbackReasonSeen[reason] = true
	}
	recoverySources := make(map[dataRecoverySourceKey]*DataRecoverySource)
	addFallbackReason := func(reason string) {
		reason = strings.TrimSpace(reason)
		if reason == "" || fallbackReasonSeen[reason] {
			return
		}
		fallbackReasonSeen[reason] = true
		result.FallbackReasons = append(result.FallbackReasons, reason)
	}
	recordRecoverySource := func(info dataTableInfo, file dataFileRef, pageNo uint32, storageID uint32, located int, exported int, failed int) {
		if !opts.RecoveryMode {
			return
		}
		key := dataRecoverySourceKey{
			tableID:   info.table.ID,
			groupID:   file.key.groupID,
			fileID:    file.key.fileID,
			storageID: storageID,
			heuristic: info.orphanRecovery,
		}
		source := recoverySources[key]
		if source == nil {
			source = &DataRecoverySource{
				Owner:     info.table.Owner,
				Name:      info.table.Name,
				GroupID:   file.key.groupID,
				FileID:    file.key.fileID,
				StorageID: storageID,
				FirstPage: pageNo,
				LastPage:  pageNo,
				Heuristic: info.orphanRecovery,
			}
			recoverySources[key] = source
		}
		if pageNo < source.FirstPage {
			source.FirstPage = pageNo
		}
		if pageNo > source.LastPage {
			source.LastPage = pageNo
		}
		source.Pages++
		source.RowsLocated += located
		source.RowsExported += exported
		source.RowsFailed += failed
	}
	processPage := func(file dataFileRef, pageNo uint32, page []byte, candidates []dataTableInfo) error {
		if stop {
			return errStopPageScan
		}
		if len(candidates) == 0 || !isProbableDMDataPage(page, pageSize) {
			return nil
		}
		nRec := int(binary.LittleEndian.Uint16(page[dataPageRecordCountOff:]))
		rows := locateRowsInDataPage(page, pageSize, nRec)
		if opts.RecoveryMode {
			rows = locateRowsInDataPageForRecovery(page, pageSize)
		}
		info, ok := selectDataPageCandidate(candidates, file, pageNo, page, pageSize, rows, decoder)
		if !ok {
			return nil
		}
		if info.orphanRecovery {
			addFallbackReason(fmt.Sprintf("%s.%s orphan storage recovery is heuristic; verify recovery source group/file/storage_id/page range before import", info.table.Owner, info.table.Name))
		}
		locatedBefore := result.RowsLocated
		exportedBefore := result.RowsExported
		failedBefore := result.RowsFailed
		for _, row := range rows {
			if opts.MaxRows > 0 && result.RowsLocated >= opts.MaxRows {
				stop = true
				break
			}
			result.RowsLocated++
			rowStart := int(row.offset)
			rowEnd := rowStart + int(row.length)
			rowBytes := append([]byte(nil), page[rowStart:rowEnd]...)
			line, record, fields, meta, timeFractionLoss, err := renderDataRowForExport(outputFormat, info, rowBytes, decoder, dmpConfig.charset)
			stats := rowStats[info.table.ID]
			if stats != nil {
				stats.RowsLocated++
			}
			if err != nil {
				result.RowsFailed++
				if stats != nil {
					stats.RowsFailed++
				}
				if opts.WriteFailedComments {
					message := fmt.Sprintf("-- FAILED %s.%s page=%d slot=%d off=0x%X len=%d: %v",
						quoteIdent(info.table.Owner), quoteIdent(info.table.Name), pageNo, row.slotNo, row.offset, row.length, err)
					if writeErr := output.writeFailure(info, message); writeErr != nil {
						return writeErr
					}
				}
				continue
			}
			if meta.partial {
				pendingPartialRows = append(pendingPartialRows, pendingPartialDataRow{
					tableID:          info.table.ID,
					line:             line,
					record:           record,
					fields:           fields,
					timeFractionLoss: timeFractionLoss,
					stats:            stats,
					meta:             meta,
				})
				continue
			}
			if timeFractionLoss {
				result.TimeFractionLoss++
			}
			coverageStates[info.table.ID].mark(meta.coverageKeys)
			result.RowsExported++
			if stats != nil {
				stats.RowsExported++
			}
			if err := output.writeRow(info, line, record, fields); err != nil {
				return err
			}
		}
		recordRecoverySource(
			info,
			file,
			pageNo,
			dataPageStorageID(page),
			result.RowsLocated-locatedBefore,
			result.RowsExported-exportedBefore,
			result.RowsFailed-failedBefore,
		)
		if stop {
			return errStopPageScan
		}
		return nil
	}

	// applyDirectResult replays one decoded page's bookkeeping and output on
	// the writer side, in plan order, mirroring the sequential loop exactly.
	applyDirectResult := func(res *directPageResult) error {
		ref := res.ref
		candidates := directCandidates[ref]
		if res.readErr != nil {
			markFailedPlanUnits(failedPlanUnits, candidates)
			addFallbackReason(formatDirectPageFailure(ref, res.readErr))
			return nil
		}
		result.DirectPagesRead++
		result.PagesScanned++
		touchedFiles[ref.key] = true
		if res.validationFailed {
			markFailedPlanUnits(failedPlanUnits, candidates)
			addFallbackReason(formatDirectPageFailure(ref, fmt.Errorf("page identity, kind, or storage_id validation failed")))
			return nil
		}
		if res.skipped {
			processedDirectPages[ref] = true
			return nil
		}
		if res.orphanReason != "" {
			addFallbackReason(res.orphanReason)
		}
		info := res.info
		stats := rowStats[info.table.ID]
		located, exported, failed := 0, 0, 0
		for i := range res.rows {
			row := &res.rows[i]
			result.RowsLocated++
			located++
			if stats != nil {
				stats.RowsLocated++
			}
			if row.failed {
				result.RowsFailed++
				failed++
				if stats != nil {
					stats.RowsFailed++
				}
				if opts.WriteFailedComments && row.failMsg != "" {
					if err := output.writeFailure(info, row.failMsg); err != nil {
						return err
					}
				}
				continue
			}
			if row.meta.partial {
				pendingPartialRows = append(pendingPartialRows, pendingPartialDataRow{
					tableID:          info.table.ID,
					line:             row.line,
					record:           row.record,
					fields:           row.fields,
					timeFractionLoss: row.timeFractionLoss,
					stats:            stats,
					meta:             row.meta,
				})
				continue
			}
			if row.timeFractionLoss {
				result.TimeFractionLoss++
			}
			coverageStates[info.table.ID].mark(row.meta.coverageKeys)
			result.RowsExported++
			exported++
			if stats != nil {
				stats.RowsExported++
			}
			if row.dmpSegments != nil {
				if err := output.writeDMPSegments(info, row.dmpSegments); err != nil {
					return err
				}
			} else if err := output.writeRow(info, row.line, row.record, row.fields); err != nil {
				return err
			}
		}
		recordRecoverySource(info, dataFileRefForKey(dataFiles, ref.key), ref.pageNo, res.storageID, located, exported, failed)
		processedDirectPages[ref] = true
		return nil
	}

	// runParallelDirect fans page decoding out to workers and applies chunks
	// in plan order so output stays byte-identical to the sequential path.
	// Workers cut each 256-page job into byte-bounded sub-chunks and a shared
	// byte budget throttles look-ahead, so total decode memory stays bounded
	// even when SQL/CSV rows materialize very large LOBs.
	runParallelDirect := func(refs []dataPageRef, workers int) error {
		type directJob struct {
			idx int
			lo  int
			hi  int
		}
		batchCount := (len(refs) + directDecodeBatchPages - 1) / directDecodeBatchPages
		chunkByteCap := directDecodeChunkBytes()
		budget := newByteBudget(directUnloadMemoryLimit())
		jobs := make(chan directJob)
		resultsCh := make(chan directChunk, workers*2)
		var wg sync.WaitGroup
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				reader := newDataFilePageReader(dataFiles, pageSize)
				defer func() { _ = reader.close() }()
				for job := range jobs {
					sub := 0
					chunk := directChunk{jobIdx: job.idx}
					emit := func(last bool) {
						chunk.last = last
						budget.acquire(chunk.bytes, job.idx)
						resultsCh <- chunk
						sub++
						chunk = directChunk{jobIdx: job.idx, subIdx: sub}
					}
					for i := job.lo; i < job.hi; i++ {
						ref := refs[i]
						res := decodeDirectPlannedRef(
							reader, ref, dataFileRefForKey(dataFiles, ref.key), directCandidates[ref],
							pageSize, decoder, outputFormat, dmpConfig.charset, opts.WriteFailedComments)
						chunk.bytes += res.approxBytes()
						chunk.results = append(chunk.results, res)
						if chunk.bytes >= chunkByteCap {
							emit(false)
						}
					}
					// Always emit a final (possibly empty) chunk so the writer
					// sees the job's last marker and advances the exempt job.
					emit(true)
				}
			}()
		}
		go func() {
			for i := 0; i < batchCount; i++ {
				lo := i * directDecodeBatchPages
				hi := lo + directDecodeBatchPages
				if hi > len(refs) {
					hi = len(refs)
				}
				jobs <- directJob{idx: i, lo: lo, hi: hi}
			}
			close(jobs)
		}()
		go func() {
			wg.Wait()
			close(resultsCh)
		}()
		// Apply chunks strictly in (jobIdx, subIdx) order; release each
		// chunk's budget on apply and advance the exempt job on its last
		// chunk so a blocked worker for that job can proceed.
		reorder := make(map[[2]int]directChunk)
		curJob, curSub := 0, 0
		var applyErr error
		for chunk := range resultsCh {
			reorder[[2]int{chunk.jobIdx, chunk.subIdx}] = chunk
			for {
				c, ok := reorder[[2]int{curJob, curSub}]
				if !ok {
					break
				}
				delete(reorder, [2]int{curJob, curSub})
				if applyErr == nil {
					for i := range c.results {
						if err := applyDirectResult(&c.results[i]); err != nil {
							applyErr = err
							break
						}
					}
				}
				budget.release(c.bytes)
				if c.last {
					curJob++
					curSub = 0
					budget.setApplyingJob(curJob)
				} else {
					curSub++
				}
			}
		}
		return applyErr
	}

	pageReader := newDataFilePageReader(dataFiles, pageSize)
	defer func() { _ = pageReader.close() }()

	if opts.RecoveryMode {
		addFallbackReason("recovery mode requested a full-file residual page scan")
		// TRUNCATE and DROP allocate fresh storage ids, so residual pages
		// carry ids no live object owns. Offer the recovery targets as
		// candidates for such orphaned pages; selectDataPageCandidate still
		// demands that the page rows parse with the target table's columns.
		liveStorageIDs := make(map[uint32]bool, len(indexes))
		for storageID := range indexes {
			liveStorageIDs[storageID] = true
		}
		for _, table := range dictionaryTables {
			if table.StorageID != 0 {
				liveStorageIDs[table.StorageID] = true
			}
			for _, assistID := range table.AssistIDs {
				if assistID != 0 {
					liveStorageIDs[assistID] = true
				}
			}
		}
		orphanCandidates, orphanReason := buildOrphanRecoveryCandidates(storageUnits)
		if orphanReason != "" {
			addFallbackReason(orphanReason)
		}
		for _, file := range dataFiles {
			if stop {
				break
			}
			touchedFiles[file.key] = true
			pagesScanned, scanErr := forEachDataFileRefPage(file, pageSize, func(page []byte, pageNo uint32) error {
				assistIndexID := dataPageStorageID(page)
				candidates := assistByID[assistIndexID]
				if len(candidates) == 0 && !liveStorageIDs[assistIndexID] {
					candidates = orphanCandidates
				}
				return processPage(file, pageNo, page, candidates)
			})
			result.FallbackPagesScanned += pagesScanned
			result.PagesScanned += pagesScanned
			if scanErr != nil && scanErr != errStopPageScan {
				return nil, fmt.Errorf("scan recovery data file %s: %w", file.path, scanErr)
			}
			if scanErr == errStopPageScan {
				stop = true
			}
		}
	} else {
		directRefs := sortedDataPageRefs(plannedRefs)
		workers := exportWorkerCount()
		// MaxRows keeps the sequential path so the early stop stays exact.
		if workers > 1 && opts.MaxRows == 0 && len(directRefs) >= directDecodeBatchPages {
			if err := runParallelDirect(directRefs, workers); err != nil {
				return nil, err
			}
		} else {
			for _, ref := range directRefs {
				if stop {
					break
				}
				candidates := directCandidates[ref]
				page, readErr := pageReader.readPage(ref)
				if readErr != nil {
					markFailedPlanUnits(failedPlanUnits, candidates)
					addFallbackReason(formatDirectPageFailure(ref, readErr))
					continue
				}
				result.DirectPagesRead++
				result.PagesScanned++
				touchedFiles[ref.key] = true
				if !plannedDataPageMatches(page, ref, candidates) {
					markFailedPlanUnits(failedPlanUnits, candidates)
					addFallbackReason(formatDirectPageFailure(ref, fmt.Errorf("page identity, kind, or storage_id validation failed")))
					continue
				}
				file := dataFileRefForKey(dataFiles, ref.key)
				if err := processPage(file, ref.pageNo, page, candidates); err != nil {
					if err == errStopPageScan {
						stop = true
						break
					}
					return nil, err
				}
				processedDirectPages[ref] = true
			}
		}

		storageCandidates, fallbackGroups, fallbackUnits := buildStorageFallbackCandidates(assistByID, plannedUnits, failedPlanUnits)
		for unitID := range fallbackUnits {
			if reasons := planFailureReasons[unitID]; len(reasons) > 0 {
				for _, reason := range reasons {
					addFallbackReason(reason)
				}
			} else if info, ok := storageUnits[unitID]; ok {
				addFallbackReason(fmt.Sprintf("%s.%s storage unit %d has no complete page plan; scanning group %d by storage_id", info.table.Owner, info.table.Name, unitID, fallbackGroupForInfo(info)))
			}
		}

		storagePagesFound := make(map[uint32]bool)
		if !stop && len(storageCandidates) > 0 {
			for _, file := range dataFiles {
				if stop {
					break
				}
				if !fallbackGroups[file.key.groupID] {
					continue
				}
				touchedFiles[file.key] = true
				pagesScanned, scanErr := forEachDataFileRefPage(file, pageSize, func(page []byte, pageNo uint32) error {
					ref := dataPageRef{key: file.key, pageNo: pageNo}
					if processedDirectPages[ref] || !pageHeaderMatchesRef(page, ref) || !isProbableDMDataPage(page, pageSize) {
						return nil
					}
					assistIndexID := dataPageStorageID(page)
					candidates := storageCandidates[assistIndexID]
					if len(candidates) == 0 {
						return nil
					}
					matched := candidates[:0]
					for _, candidate := range candidates {
						if candidateMatchesFile(candidate, file, pageNo) {
							storagePagesFound[candidate.storageUnitID] = true
							matched = append(matched, candidate)
						}
					}
					if len(matched) > 0 {
						processedDirectPages[ref] = true
					}
					return processPage(file, pageNo, page, matched)
				})
				result.FallbackPagesScanned += pagesScanned
				result.PagesScanned += pagesScanned
				if scanErr != nil && scanErr != errStopPageScan {
					return nil, fmt.Errorf("scan storage fallback file %s: %w", file.path, scanErr)
				}
				if scanErr == errStopPageScan {
					stop = true
				}
			}
		}

		unresolvedUnits := unresolvedStorageUnits(storageUnits, plannedUnits, failedPlanUnits, fallbackUnits, storagePagesFound)
		segmentPages := buildSegmentFallbackPages(storageUnits, unresolvedUnits)
		for unitID := range unresolvedUnits {
			info := storageUnits[unitID]
			if info.segmentKnown {
				addFallbackReason(fmt.Sprintf("%s.%s storage unit %d has no matching storage page; scanning segment file=%d header=%d blocks=%d", info.table.Owner, info.table.Name, unitID, info.segment.fileID, info.segment.headerPage, info.segment.blocks))
			} else {
				addFallbackReason(fmt.Sprintf("%s.%s storage unit %d has no matching storage page or usable segment range", info.table.Owner, info.table.Name, unitID))
			}
		}
		for _, ref := range sortedSegmentPageRefs(segmentPages) {
			if stop {
				break
			}
			page, readErr := pageReader.readPage(ref)
			if readErr != nil {
				addFallbackReason(formatDirectPageFailure(ref, readErr))
				continue
			}
			result.FallbackPagesScanned++
			result.PagesScanned++
			touchedFiles[ref.key] = true
			if processedDirectPages[ref] || !pageHeaderMatchesRef(page, ref) {
				continue
			}
			processedDirectPages[ref] = true
			file := dataFileRefForKey(dataFiles, ref.key)
			if err := processPage(file, ref.pageNo, page, segmentPages[ref]); err != nil {
				if err == errStopPageScan {
					stop = true
					break
				}
				return nil, err
			}
		}
	}
	result.DataFileCount = len(touchedFiles)
	result.RecoverySources = finalizeDataRecoverySources(recoverySources)
	sort.Strings(result.FallbackReasons)
	coverageOverflowWarned := make(map[uint32]bool)
	for _, pending := range pendingPartialRows {
		state := coverageStates[pending.tableID]
		if state.covered(pending.meta.prefixKey, pending.meta.weakPrefixKey) {
			continue
		}
		if state != nil && state.overflow.Load() && !coverageOverflowWarned[pending.tableID] {
			coverageOverflowWarned[pending.tableID] = true
			if info, ok := selectedTables[pending.tableID]; ok {
				addFallbackReason(fmt.Sprintf("%s.%s row coverage tracking exceeded %d keys; partial rows were emitted without full-row dedup, verify duplicates before import", info.table.Owner, info.table.Name, maxCoverageKeysPerTable))
			}
		}
		state.mark(pending.meta.coverageKeys)
		if pending.timeFractionLoss {
			result.TimeFractionLoss++
		}
		result.RowsExported++
		if pending.stats != nil {
			pending.stats.RowsExported++
		}
		info, ok := selectedTables[pending.tableID]
		if !ok {
			continue
		}
		if err := output.writeRow(info, pending.line, pending.record, pending.fields); err != nil {
			return nil, err
		}
	}

	result.TableRowCounts = finalizeDataTableRowStats(rowStats)
	for _, item := range result.TableRowCounts {
		if item.RowsLocated > 0 {
			result.TablesWithRows++
		} else {
			result.TablesWithoutRows++
		}
	}
	result.TableOutputs = output.tableOutputs()
	result.OversizedSQLStatements = output.oversizedSQLRows
	result.OversizedSQLTables = sortedOversizedSQLTables(output.oversizedSQLTableIDs)
	if (outputFormat == "fldr" || outputFormat == "dmp") && opts.TableOutputPath == nil && result.RowsExported == 0 {
		result.OutputPath = ""
	}
	if err := output.close(); err != nil {
		return nil, fmt.Errorf("finalize %s data output: %w", outputFormat, err)
	}
	outputClosed = true
	return result, nil
}

func initDataTableRowStats(tables map[uint32]dataTableInfo) map[uint32]*DataTableRowCount {
	result := make(map[uint32]*DataTableRowCount, len(tables))
	for tableID, info := range tables {
		result[tableID] = &DataTableRowCount{
			Owner: info.table.Owner,
			Name:  info.table.Name,
		}
	}
	return result
}

func finalizeDataTableRowStats(stats map[uint32]*DataTableRowCount) []DataTableRowCount {
	var ids []uint32
	for tableID := range stats {
		ids = append(ids, tableID)
	}
	sort.Slice(ids, func(i, j int) bool {
		left := stats[ids[i]]
		right := stats[ids[j]]
		if left.Owner != right.Owner {
			return left.Owner < right.Owner
		}
		if left.Name != right.Name {
			return left.Name < right.Name
		}
		return ids[i] < ids[j]
	})
	result := make([]DataTableRowCount, 0, len(ids))
	for _, tableID := range ids {
		result = append(result, *stats[tableID])
	}
	return result
}
