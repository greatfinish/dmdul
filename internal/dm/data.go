package dm

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	pathpkg "path"
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
	// than disql's 160 KiB input buffer; disql aborts such statements with
	// "input too long", silently losing the row on import.
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

type dmJSONValue struct {
	value   any
	binary  bool
	decoder textDecoder
}

type dmLOBValue struct {
	reader  *dmLOBReader
	locator dmLOBLocator
	kind    uint32
	text    bool
	decoder textDecoder
}

type dmLOBReader struct {
	cache *dataFilePageCache
}

type dmLOBLocator struct {
	raw       []byte
	lobID     uint32
	byteLen   uint32
	groupID   uint32
	firstPage uint32
}

type dmLOBChainReader struct {
	owner       *dmLOBReader
	locator     dmLOBLocator
	kind        uint32
	current     dataPageRef
	hasCurrent  bool
	remaining   uint64
	payload     []byte
	payloadPos  int
	seen        map[dataPageRef]bool
	steps       int
	maxSteps    int
	terminalErr error
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

type dataOutputFile struct {
	path      string
	file      *os.File
	writer    *bufio.Writer
	dmpWriter *DMPDataWriter
}

type dataDMPOutputConfig struct {
	charset       dmpCharsetHeader
	extentSize    uint32
	pageSize      uint32
	caseSensitive *bool
}

type dataOutputRouter struct {
	format           string
	split            bool
	mainPath         string
	mainTable        dataTableInfo
	main             *dataOutputFile
	pathsByTable     map[uint32]string
	filesByTable     map[uint32]*dataOutputFile
	initializedByID  map[uint32]bool
	lastUsedByID     map[uint32]uint64
	useClock         uint64
	tableOutputsByID map[uint32]DataTableOutput
	dmpConfig        dataDMPOutputConfig
	dmpWritersByID   map[uint32]*DMPDataWriter
	// charset is the recovered database character set, echoed into dmfldr
	// control files so the loader is invoked with a matching CHARACTER_CODE.
	charset string

	oversizedSQLRows     int
	oversizedSQLTableIDs map[uint32]string
}

// disqlMaxStatementBytes is disql's single-statement input buffer (160 KiB,
// measured against DM8 build 2025-01-17: 163840-byte statements execute,
// longer ones abort with "input too long"). SQL exports whose INSERT exceeds
// this cannot be replayed through disql; DMP/dimp has no such limit.
const disqlMaxStatementBytes = 160 << 10

func newDataOutputRouter(opts DataExportOptions, outputFormat string, selectedTables map[uint32]dataTableInfo, dmpConfigs ...dataDMPOutputConfig) (*dataOutputRouter, error) {
	router := &dataOutputRouter{
		format:           outputFormat,
		split:            opts.TableOutputPath != nil,
		mainPath:         opts.OutputPath,
		pathsByTable:     make(map[uint32]string),
		filesByTable:     make(map[uint32]*dataOutputFile),
		initializedByID:  make(map[uint32]bool),
		lastUsedByID:     make(map[uint32]uint64),
		tableOutputsByID: make(map[uint32]DataTableOutput),
		dmpWritersByID:   make(map[uint32]*DMPDataWriter),
		charset:          opts.Charset,
	}
	if len(dmpConfigs) > 0 {
		router.dmpConfig = dmpConfigs[0]
	}

	if !router.split {
		if table, ok := singleSelectedDataTable(selectedTables); ok {
			router.mainTable = table
		}
		if outputFormat == "fldr" || outputFormat == "dmp" {
			_ = os.Remove(opts.OutputPath)
			return router, nil
		}
		file, err := openDataOutputFile(opts.OutputPath, outputFormat, router.mainTable, router.charset)
		if err != nil {
			return nil, err
		}
		router.main = file
		return router, nil
	}

	pathOwners := make(map[string]uint32)
	for _, tableID := range sortedDataTableIDs(selectedTables) {
		table := selectedTables[tableID]
		path := strings.TrimSpace(opts.TableOutputPath(table.table.Owner, table.table.Name, tableID))
		if path == "" {
			return nil, fmt.Errorf("empty data output path for %s.%s", table.table.Owner, table.table.Name)
		}
		pathKey := strings.ToUpper(filepath.Clean(path))
		if priorID, exists := pathOwners[pathKey]; exists && priorID != tableID {
			return nil, fmt.Errorf("duplicate data output path %s", path)
		}
		pathOwners[pathKey] = tableID
		router.pathsByTable[tableID] = path
		if outputFormat == "fldr" || outputFormat == "dmp" {
			_ = os.Remove(path)
			continue
		}
		file, err := openDataOutputFile(path, outputFormat, table, router.charset)
		if err != nil {
			_ = router.close()
			return nil, err
		}
		if err := closeDataOutputFile(file); err != nil {
			_ = router.close()
			return nil, err
		}
		router.initializedByID[tableID] = true
		router.tableOutputsByID[tableID] = DataTableOutput{
			TableID: tableID, Owner: table.table.Owner, Name: table.table.Name, OutputPath: path,
		}
	}
	return router, nil
}

func sortedDataTableIDs(tables map[uint32]dataTableInfo) []uint32 {
	ids := make([]uint32, 0, len(tables))
	for id := range tables {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		left, right := tables[ids[i]].table, tables[ids[j]].table
		if left.Owner != right.Owner {
			return left.Owner < right.Owner
		}
		if left.Name != right.Name {
			return left.Name < right.Name
		}
		return ids[i] < ids[j]
	})
	return ids
}

func openDataOutputFile(path string, outputFormat string, table dataTableInfo, charset string) (*dataOutputFile, error) {
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("create data output directory: %w", err)
		}
	}
	out, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("create data output: %w", err)
	}
	target := &dataOutputFile{path: path, file: out, writer: bufio.NewWriter(out)}
	if outputFormat == "fldr" {
		// The data file carries rows only; the companion control file tells
		// dmfldr how to read them, so it is written alongside on creation.
		if err := WriteFldrControlFile(fldrControlFilePath(path), path,
			table.table.Owner, table.table.Name, table.columns, charset); err != nil {
			_ = out.Close()
			return nil, err
		}
		return target, nil
	}
	fmt.Fprintln(target.writer, "-- Generated by dmdul export-data. Review before running.")
	fmt.Fprintln(target.writer, "-- Current decoder targets ordinary in-row heap/cluster/IOT rows.")
	fmt.Fprintln(target.writer)
	return target, nil
}

func openExistingDataOutputFile(path string, outputFormat string) (*dataOutputFile, error) {
	out, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, fmt.Errorf("open data output for append: %w", err)
	}
	return &dataOutputFile{path: path, file: out, writer: bufio.NewWriter(out)}, nil
}

func (r *dataOutputRouter) targetForTable(table dataTableInfo) (*dataOutputFile, error) {
	if r.format == "dmp" {
		return r.dmpTargetForTable(table)
	}
	if !r.split {
		if r.main != nil {
			return r.main, nil
		}
		file, err := openDataOutputFile(r.mainPath, r.format, r.mainTable, r.charset)
		if err != nil {
			return nil, err
		}
		r.main = file
		return file, nil
	}
	if file := r.filesByTable[table.table.ID]; file != nil {
		r.touch(table.table.ID)
		return file, nil
	}
	if len(r.filesByTable) >= maxOpenSplitDataFiles {
		if err := r.evictLeastRecentlyUsed(); err != nil {
			return nil, err
		}
	}
	path := r.pathsByTable[table.table.ID]
	var file *dataOutputFile
	var err error
	if r.initializedByID[table.table.ID] {
		file, err = openExistingDataOutputFile(path, r.format)
	} else {
		file, err = openDataOutputFile(path, r.format, table, r.charset)
	}
	if err != nil {
		return nil, err
	}
	r.filesByTable[table.table.ID] = file
	r.initializedByID[table.table.ID] = true
	r.touch(table.table.ID)
	r.tableOutputsByID[table.table.ID] = DataTableOutput{
		TableID: table.table.ID, Owner: table.table.Owner, Name: table.table.Name, OutputPath: path,
	}
	return file, nil
}

func (r *dataOutputRouter) dmpTargetForTable(table dataTableInfo) (*dataOutputFile, error) {
	if !r.split {
		if r.main != nil {
			return r.main, nil
		}
		file, err := r.newDMPOutputFile(r.mainPath, table)
		if err != nil {
			return nil, err
		}
		r.main = file
		return file, nil
	}
	if file := r.filesByTable[table.table.ID]; file != nil {
		r.touch(table.table.ID)
		return file, nil
	}
	if len(r.filesByTable) >= maxOpenSplitDataFiles {
		if err := r.evictLeastRecentlyUsed(); err != nil {
			return nil, err
		}
	}
	path := r.pathsByTable[table.table.ID]
	writer := r.dmpWritersByID[table.table.ID]
	if writer == nil {
		file, err := r.newDMPOutputFile(path, table)
		if err != nil {
			return nil, err
		}
		writer = file.dmpWriter
	} else if err := writer.resume(); err != nil {
		return nil, err
	}
	file := &dataOutputFile{path: path, dmpWriter: writer}
	r.filesByTable[table.table.ID] = file
	r.touch(table.table.ID)
	return file, nil
}

func (r *dataOutputRouter) newDMPOutputFile(path string, table dataTableInfo) (*dataOutputFile, error) {
	writer, err := NewDMPDataWriter(DMPDataOptions{
		OutputPath:    path,
		Charset:       r.dmpConfig.charset.Name,
		ExtentSize:    r.dmpConfig.extentSize,
		PageSize:      r.dmpConfig.pageSize,
		CaseSensitive: r.dmpConfig.caseSensitive,
		Schema:        table.table.Owner,
		Table:         table.table.Name,
		TableID:       table.table.ID,
		ColumnCount:   uint16(len(table.columns)),
	})
	if err != nil {
		return nil, err
	}
	if r.split {
		r.dmpWritersByID[table.table.ID] = writer
	}
	r.initializedByID[table.table.ID] = true
	r.tableOutputsByID[table.table.ID] = DataTableOutput{
		TableID: table.table.ID, Owner: table.table.Owner, Name: table.table.Name, OutputPath: path,
	}
	return &dataOutputFile{path: path, dmpWriter: writer}, nil
}

func (r *dataOutputRouter) touch(tableID uint32) {
	r.useClock++
	r.lastUsedByID[tableID] = r.useClock
}

func (r *dataOutputRouter) evictLeastRecentlyUsed() error {
	var oldestID uint32
	var oldestUse uint64
	found := false
	for tableID := range r.filesByTable {
		used := r.lastUsedByID[tableID]
		if !found || used < oldestUse {
			oldestID = tableID
			oldestUse = used
			found = true
		}
	}
	if !found {
		return nil
	}
	if r.format == "dmp" {
		if err := r.filesByTable[oldestID].dmpWriter.suspend(); err != nil {
			return err
		}
	} else {
		if err := closeDataOutputFile(r.filesByTable[oldestID]); err != nil {
			return err
		}
	}
	delete(r.filesByTable, oldestID)
	delete(r.lastUsedByID, oldestID)
	return nil
}

func (r *dataOutputRouter) writeRow(table dataTableInfo, line string, record []string, dmpRows ...[]DMPField) error {
	target, err := r.targetForTable(table)
	if err != nil {
		return err
	}
	if r.format == "fldr" {
		dialect := table.fldrDialect.resolved()
		if _, err := target.writer.WriteString(strings.Join(record, dialect.fieldSep) + dialect.rowTerm); err != nil {
			return fmt.Errorf("write fldr row: %w", err)
		}
		return nil
	}
	if r.format == "dmp" {
		if len(dmpRows) != 1 {
			return fmt.Errorf("missing dmp fields for %s.%s", table.table.Owner, table.table.Name)
		}
		if err := target.dmpWriter.WriteRow(dmpRows[0]); err != nil {
			_ = target.dmpWriter.Abort()
			return fmt.Errorf("write dmp row: %w", err)
		}
		return nil
	}
	if len(line) > disqlMaxStatementBytes {
		r.oversizedSQLRows++
		if r.oversizedSQLTableIDs == nil {
			r.oversizedSQLTableIDs = make(map[uint32]string)
		}
		r.oversizedSQLTableIDs[table.table.ID] = table.table.Owner + "." + table.table.Name
	}
	if _, err := fmt.Fprintln(target.writer, line); err != nil {
		return fmt.Errorf("write sql row: %w", err)
	}
	return nil
}

// writeDMPSegments appends a worker-pre-encoded row to the table's DMP
// writer, mirroring writeRow's dmp error handling.
func (r *dataOutputRouter) writeDMPSegments(table dataTableInfo, segments []dmpRowSegment) error {
	target, err := r.targetForTable(table)
	if err != nil {
		return err
	}
	if err := target.dmpWriter.WriteEncodedRow(segments); err != nil {
		_ = target.dmpWriter.Abort()
		return fmt.Errorf("write dmp row: %w", err)
	}
	return nil
}

func (r *dataOutputRouter) writeFailure(table dataTableInfo, message string) error {
	if r.format == "fldr" || r.format == "dmp" {
		return nil
	}
	target, err := r.targetForTable(table)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(target.writer, message); err != nil {
		return fmt.Errorf("write failed-row comment: %w", err)
	}
	return nil
}

func sortedOversizedSQLTables(byID map[uint32]string) []string {
	if len(byID) == 0 {
		return nil
	}
	names := make([]string, 0, len(byID))
	for _, name := range byID {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (r *dataOutputRouter) tableOutputs() []DataTableOutput {
	outputs := make([]DataTableOutput, 0, len(r.tableOutputsByID))
	for _, output := range r.tableOutputsByID {
		outputs = append(outputs, output)
	}
	sort.Slice(outputs, func(i, j int) bool {
		if outputs[i].Owner != outputs[j].Owner {
			return outputs[i].Owner < outputs[j].Owner
		}
		if outputs[i].Name != outputs[j].Name {
			return outputs[i].Name < outputs[j].Name
		}
		return outputs[i].TableID < outputs[j].TableID
	})
	return outputs
}

func (r *dataOutputRouter) close() error {
	var firstErr error
	if r.format == "dmp" {
		if r.split {
			for _, tableID := range sortedDMPWriterIDs(r.dmpWritersByID) {
				if _, err := r.dmpWritersByID[tableID].Close(); err != nil && firstErr == nil {
					firstErr = err
				}
			}
		} else if r.main != nil && r.main.dmpWriter != nil {
			if _, err := r.main.dmpWriter.Close(); err != nil {
				firstErr = err
			}
		}
		return firstErr
	}
	if r.split {
		for _, tableID := range sortedDataOutputFileIDs(r.filesByTable) {
			if err := closeDataOutputFile(r.filesByTable[tableID]); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	} else {
		if err := closeDataOutputFile(r.main); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func sortedDMPWriterIDs(writers map[uint32]*DMPDataWriter) []uint32 {
	ids := make([]uint32, 0, len(writers))
	for id := range writers {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func closeDataOutputFile(target *dataOutputFile) error {
	if target == nil {
		return nil
	}
	var firstErr error
	if err := target.writer.Flush(); err != nil && firstErr == nil {
		firstErr = err
	}
	if err := target.file.Close(); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

func sortedDataOutputFileIDs(files map[uint32]*dataOutputFile) []uint32 {
	ids := make([]uint32, 0, len(files))
	for id := range files {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
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
		selectedUserTable := !table.isHugeInternalTable() && tableMatcher.allowed(table.Owner, table.Name) && !excludeMatcher.allowed(table.Owner, table.Name)
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
		if !ownerMatcher.allowed(table.Owner) || !tableMatcher.allowed(table.Owner, table.Name) || excludeMatcher.allowed(table.Owner, table.Name) || table.isHugeInternalTable() {
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

func formatStoragePlanFailure(info dataTableInfo, storageID uint32, reason string) string {
	if strings.TrimSpace(reason) == "" {
		reason = "storage root did not produce a complete leaf chain"
	}
	return fmt.Sprintf("%s.%s storage unit %d storage_id=%d: %s; scanning the same group by storage_id", info.table.Owner, info.table.Name, info.storageUnitID, storageID, reason)
}

func buildDirectDataPageCandidates(assistByID map[uint32][]dataTableInfo) (map[dataPageRef][]dataTableInfo, map[dataPageRef]bool, map[uint32]bool) {
	pages := make(map[dataPageRef][]dataTableInfo)
	refs := make(map[dataPageRef]bool)
	units := make(map[uint32]bool)
	for _, candidates := range assistByID {
		for _, candidate := range candidates {
			if !candidate.pagePlanKnown || len(candidate.pagePlan) == 0 {
				continue
			}
			units[candidate.storageUnitID] = true
			for ref := range candidate.pagePlan {
				refs[ref] = true
				pages[ref] = appendUniqueDataCandidate(pages[ref], candidate)
			}
		}
	}
	return pages, refs, units
}

func sortedDataPageRefs(refs map[dataPageRef]bool) []dataPageRef {
	result := make([]dataPageRef, 0, len(refs))
	for ref := range refs {
		result = append(result, ref)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].key.groupID != result[j].key.groupID {
			return result[i].key.groupID < result[j].key.groupID
		}
		if result[i].key.fileID != result[j].key.fileID {
			return result[i].key.fileID < result[j].key.fileID
		}
		return result[i].pageNo < result[j].pageNo
	})
	return result
}

func finalizeDataRecoverySources(items map[dataRecoverySourceKey]*DataRecoverySource) []DataRecoverySource {
	result := make([]DataRecoverySource, 0, len(items))
	for _, source := range items {
		if source != nil {
			result = append(result, *source)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Owner != result[j].Owner {
			return result[i].Owner < result[j].Owner
		}
		if result[i].Name != result[j].Name {
			return result[i].Name < result[j].Name
		}
		if result[i].GroupID != result[j].GroupID {
			return result[i].GroupID < result[j].GroupID
		}
		if result[i].FileID != result[j].FileID {
			return result[i].FileID < result[j].FileID
		}
		if result[i].StorageID != result[j].StorageID {
			return result[i].StorageID < result[j].StorageID
		}
		return !result[i].Heuristic && result[j].Heuristic
	})
	return result
}

func sortedSegmentPageRefs(pages map[dataPageRef][]dataTableInfo) []dataPageRef {
	refs := make(map[dataPageRef]bool, len(pages))
	for ref := range pages {
		refs[ref] = true
	}
	return sortedDataPageRefs(refs)
}

func appendUniqueDataCandidate(items []dataTableInfo, candidate dataTableInfo) []dataTableInfo {
	for _, existing := range items {
		if existing.table.ID == candidate.table.ID && existing.storageUnitID == candidate.storageUnitID && existing.storage.ID == candidate.storage.ID && existing.pagePlanKnown == candidate.pagePlanKnown && existing.scanGroupOnly == candidate.scanGroupOnly && existing.historicalRows == candidate.historicalRows && existing.orphanRecovery == candidate.orphanRecovery {
			return items
		}
	}
	return append(items, candidate)
}

func markFailedPlanUnits(failed map[uint32]bool, candidates []dataTableInfo) {
	for _, candidate := range candidates {
		if candidate.storageUnitID != 0 {
			failed[candidate.storageUnitID] = true
		}
	}
}

func plannedDataPageMatches(page []byte, ref dataPageRef, candidates []dataTableInfo) bool {
	if !pageHeaderMatchesRef(page, ref) || dataPageKind(page) != dmPageKindRowData {
		return false
	}
	storageID := dataPageStorageID(page)
	for _, candidate := range candidates {
		if candidate.pagePlanKnown && candidate.pagePlan[ref] && candidate.storage.ID == storageID {
			return true
		}
	}
	return false
}

func formatDirectPageFailure(ref dataPageRef, err error) string {
	return fmt.Sprintf("planned page group=%d file=%d page=%d: %v; enabling storage fallback", ref.key.groupID, ref.key.fileID, ref.pageNo, err)
}

func fallbackGroupForInfo(info dataTableInfo) uint32 {
	if info.storageKnown {
		return uint32(info.storage.GroupID)
	}
	if info.recoveryGroupID != 0 {
		return info.recoveryGroupID
	}
	return info.segment.tablespaceID
}

func buildStorageFallbackCandidates(assistByID map[uint32][]dataTableInfo, plannedUnits map[uint32]bool, failedPlanUnits map[uint32]bool) (map[uint32][]dataTableInfo, map[uint32]bool, map[uint32]bool) {
	result := make(map[uint32][]dataTableInfo)
	groups := make(map[uint32]bool)
	units := make(map[uint32]bool)
	for assistID, candidates := range assistByID {
		for _, candidate := range candidates {
			needsFallback := !plannedUnits[candidate.storageUnitID] || failedPlanUnits[candidate.storageUnitID]
			if !needsFallback || candidate.pagePlanKnown || isLooseHistoricalCandidate(candidate) {
				continue
			}
			candidate.scanGroupOnly = true
			candidate.scanGroupID = fallbackGroupForInfo(candidate)
			result[assistID] = appendUniqueDataCandidate(result[assistID], candidate)
			groups[candidate.scanGroupID] = true
			units[candidate.storageUnitID] = true
		}
	}
	return result, groups, units
}

func unresolvedStorageUnits(storageUnits map[uint32]dataTableInfo, plannedUnits map[uint32]bool, failedPlanUnits map[uint32]bool, fallbackUnits map[uint32]bool, storagePagesFound map[uint32]bool) map[uint32]bool {
	result := make(map[uint32]bool)
	for unitID := range storageUnits {
		if plannedUnits[unitID] && !failedPlanUnits[unitID] {
			continue
		}
		if fallbackUnits[unitID] && storagePagesFound[unitID] {
			continue
		}
		result[unitID] = true
	}
	return result
}

func buildSegmentFallbackPages(storageUnits map[uint32]dataTableInfo, unresolved map[uint32]bool) map[dataPageRef][]dataTableInfo {
	result := make(map[dataPageRef][]dataTableInfo)
	for unitID := range unresolved {
		info, ok := storageUnits[unitID]
		if !ok || !info.segmentKnown || info.segment.blocks == 0 {
			continue
		}
		info.pagePlan = nil
		info.pagePlanKnown = false
		info.scanGroupOnly = false
		info.historicalRows = false
		groupID := info.segment.tablespaceID
		if groupID == 0 {
			groupID = info.recoveryGroupID
		}
		end := uint64(info.segment.headerPage) + uint64(info.segment.blocks)
		if end > uint64(^uint32(0)) {
			end = uint64(^uint32(0))
		}
		for pageNo := uint64(info.segment.headerPage); pageNo < end; pageNo++ {
			ref := dataPageRef{key: dataFileKey{groupID: groupID, fileID: info.segment.fileID}, pageNo: uint32(pageNo)}
			result[ref] = appendUniqueDataCandidate(result[ref], info)
			if info.dataStorageID != 0 {
				historical := info
				historical.historicalRows = true
				result[ref] = appendUniqueDataCandidate(result[ref], historical)
			}
		}
	}
	return result
}

func dataFileRefForKey(files []dataFileRef, key dataFileKey) dataFileRef {
	for _, file := range files {
		if file.key == key {
			return file
		}
	}
	return dataFileRef{key: key}
}

func addKnownDataAssistID(assistByID map[uint32][]dataTableInfo, info dataTableInfo, assistID uint32, storage indexDef, pagePlan map[dataPageRef]bool) {
	if storage.RootFile < 0 {
		return
	}
	info.storage = storage
	info.storageKnown = true
	allowHistoricalRows := shouldAllowHistoricalRows(info, storage.ID)
	if len(pagePlan) > 0 {
		exactInfo := info
		exactInfo.historicalRows = allowHistoricalRows
		exactInfo.pagePlan = pagePlan
		exactInfo.pagePlanKnown = true
		addDataAssistCandidate(assistByID, assistID, exactInfo)
	}
	info.pagePlan = nil
	info.pagePlanKnown = false
	info.historicalRows = allowHistoricalRows
	addDataAssistCandidate(assistByID, assistID, info)
}

func addUnknownDataAssistID(assistByID map[uint32][]dataTableInfo, info dataTableInfo, assistID uint32) bool {
	info.storageKnown = false
	before := len(assistByID[assistID])
	addDataAssistCandidate(assistByID, assistID, info)
	return len(assistByID[assistID]) > before
}

func addRecoveryDataAssistID(assistByID map[uint32][]dataTableInfo, info dataTableInfo, assistID uint32) bool {
	info.recoveryMode = true
	info.historicalRows = shouldAllowHistoricalRows(info, assistID)
	info.pagePlan = nil
	info.pagePlanKnown = false
	before := len(assistByID[assistID])
	addDataAssistCandidate(assistByID, assistID, info)
	return len(assistByID[assistID]) > before
}

func addHistoricalDataAssistID(assistByID map[uint32][]dataTableInfo, info dataTableInfo, assistID uint32) bool {
	if info.dataStorageID == 0 {
		return false
	}
	info.historicalRows = shouldAllowHistoricalRows(info, assistID)
	info.pagePlan = nil
	info.pagePlanKnown = false
	info.storageKnown = false
	before := len(assistByID[assistID])
	addDataAssistCandidate(assistByID, assistID, info)
	return len(assistByID[assistID]) > before
}

func addDataAssistCandidate(assistByID map[uint32][]dataTableInfo, assistID uint32, info dataTableInfo) {
	if assistID == 0 || info.table.ID == 0 {
		return
	}
	for _, existing := range assistByID[assistID] {
		if existing.table.ID == info.table.ID && existing.storageKnown == info.storageKnown && existing.storage.ID == info.storage.ID && existing.pagePlanKnown == info.pagePlanKnown && existing.recoveryMode == info.recoveryMode && existing.historicalRows == info.historicalRows && existing.orphanRecovery == info.orphanRecovery {
			return
		}
	}
	assistByID[assistID] = append(assistByID[assistID], info)
}

func addHiddenIndexObjectAssistIDs(assistByID map[uint32][]dataTableInfo, info dataTableInfo, tableID uint32, indexObjects map[uint32]dictionaryObject, indexes map[uint32]indexDef) bool {
	added := false
	for indexID, obj := range indexObjects {
		if uint32(obj.ParentID) != tableID || !isAutoHiddenIndexObject(obj) {
			continue
		}
		if _, ok := indexes[indexID]; ok {
			continue
		}
		if addUnknownDataAssistID(assistByID, info, indexID) {
			added = true
		}
	}
	return added
}

func isAutoHiddenIndexObject(obj dictionaryObject) bool {
	if obj.Type != "TABOBJ" || obj.Subtype != "INDEX" {
		return false
	}
	return strings.EqualFold(obj.Name, fmt.Sprintf("INDEX%d", obj.ID))
}

func tableDataAssistID(tableID uint32) uint32 {
	if tableID == 0 {
		return 0
	}
	return 0x02000000 | (tableID & 0x00FFFFFF)
}

// buildOrphanRecoveryCandidates creates one heuristic candidate for pages
// whose storage id no live object owns. Orphan pages cannot prove ownership,
// so broad recovery is disabled when more than one target table is selected.
func buildOrphanRecoveryCandidates(storageUnits map[uint32]dataTableInfo) ([]dataTableInfo, string) {
	byTable := make(map[uint32]dataTableInfo)
	for _, info := range storageUnits {
		if info.table.ID == 0 {
			continue
		}
		existing, ok := byTable[info.table.ID]
		if !ok || info.storageUnitID == info.table.ID || (existing.recoveryGroupID == 0 && info.recoveryGroupID != 0) {
			byTable[info.table.ID] = info
		}
	}
	if len(byTable) == 0 {
		return nil, ""
	}
	if len(byTable) != 1 {
		return nil, fmt.Sprintf("orphan storage recovery disabled for %d target tables; use recover table owner.table to avoid ambiguous attribution", len(byTable))
	}
	for _, info := range byTable {
		info.recoveryMode = true
		info.orphanRecovery = true
		info.historicalRows = info.dataStorageID != 0
		info.pagePlan = nil
		info.pagePlanKnown = false
		info.storage = indexDef{}
		info.storageKnown = false
		info.storageUnitID = info.table.ID
		return []dataTableInfo{info}, ""
	}
	return nil, ""
}

// secondaryIndexStorageIDSet collects storage ids that belong to secondary
// indexes. Table data storages carry Flag&1 == 1 with no key columns (see
// tableStorageByID); everything else with key columns stores index entries
// whose layout does not match the owning table's rows.
func secondaryIndexStorageIDSet(indexObjects map[uint32]dictionaryObject, indexes map[uint32]indexDef) map[uint32]bool {
	result := make(map[uint32]bool)
	for indexID, idx := range indexes {
		if _, ok := indexObjects[indexID]; !ok {
			continue
		}
		if idx.Flag&1 == 1 && idx.KeyNum == 0 {
			continue
		}
		result[indexID] = true
	}
	return result
}

func assistIndexesByParentID(tables map[uint32]dictionaryObject, indexObjects map[uint32]dictionaryObject, indexes map[uint32]indexDef) map[uint32][]indexDef {
	result := make(map[uint32][]indexDef)
	for indexID, obj := range indexObjects {
		idx, ok := indexes[indexID]
		if !ok {
			continue
		}
		parentID := uint32(obj.ParentID)
		table, ok := tables[parentID]
		if !ok {
			continue
		}
		if !isCandidateDataIndex(table, idx) || idx.RootFile < 0 {
			continue
		}
		result[parentID] = append(result[parentID], idx)
	}
	for parentID := range result {
		sort.Slice(result[parentID], func(i, j int) bool {
			return result[parentID][i].ID < result[parentID][j].ID
		})
	}
	return result
}

func mergeDictionaryStorageRoots(result map[uint32][]indexDef, tables map[uint32]DictionaryTable) {
	for tableID, table := range tables {
		if table.StorageID == 0 || table.RootFile < 0 || table.RootPage == 0 {
			continue
		}
		duplicate := false
		for _, storage := range result[tableID] {
			if storage.ID == table.StorageID {
				duplicate = true
				break
			}
		}
		if duplicate {
			continue
		}
		result[tableID] = append(result[tableID], indexDef{
			ID:       table.StorageID,
			GroupID:  uint16(table.GroupID),
			RootFile: table.RootFile,
			RootPage: int32(table.RootPage),
			Flag:     1,
		})
		sort.Slice(result[tableID], func(i, j int) bool {
			return result[tableID][i].ID < result[tableID][j].ID
		})
	}
}

func isCandidateDataIndex(table dictionaryObject, idx indexDef) bool {
	if idx.Flag&1 != 0 && idx.KeyNum == 0 {
		return true
	}
	return table.isIOTTable() && idx.Flag&0x4 != 0
}

func selectDataPageCandidate(candidates []dataTableInfo, file dataFileRef, pageNo uint32, page []byte, pageSize uint32, rows []locatedDataRow, decoder textDecoder) (dataTableInfo, bool) {
	if len(rows) == 0 {
		return dataTableInfo{}, false
	}
	pageKind := dataPageKind(page)
	pageStorageID := dataPageStorageID(page)
	ordered := append([]dataTableInfo(nil), candidates...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return dataCandidateRank(ordered[i]) < dataCandidateRank(ordered[j])
	})
	for _, candidate := range ordered {
		if isTableDataAssistHeaderCandidate(candidate, pageStorageID, pageKind) {
			continue
		}
		if isLooseHistoricalCandidate(candidate) {
			continue
		}
		if !candidateMatchesFile(candidate, file, pageNo) {
			continue
		}
		if candidate.orphanRecovery {
			if orphanRecoveryCandidateMatchesRows(candidate, page, pageSize, rows, decoder) {
				return candidate, true
			}
			continue
		}
		limit := len(rows)
		if limit > 3 {
			limit = 3
		}
		for i := 0; i < limit; i++ {
			row := rows[i]
			rowStart := int(row.offset)
			rowEnd := rowStart + int(row.length)
			if rowStart < 0 || rowEnd > int(pageSize) || rowEnd > len(page) {
				continue
			}
			if _, _, _, err := parseDataRowValues(page[rowStart:rowEnd], candidate.columns, decoder, candidate.historicalRows, candidate.lobReader); err == nil {
				return candidate, true
			}
		}
	}
	return dataTableInfo{}, false
}

// orphanRecoveryCandidateMatchesRows raises the confidence threshold for an
// unowned storage page. Ownership is still heuristic, but one coincidentally
// parseable row is no longer enough to attribute a whole page to the target.
func orphanRecoveryCandidateMatchesRows(candidate dataTableInfo, page []byte, pageSize uint32, rows []locatedDataRow, decoder textDecoder) bool {
	limit := len(rows)
	if limit > 16 {
		limit = 16
	}
	matched := 0
	for i := 0; i < limit; i++ {
		row := rows[i]
		rowStart := int(row.offset)
		rowEnd := rowStart + int(row.length)
		if rowStart < 0 || rowEnd > int(pageSize) || rowEnd > len(page) {
			continue
		}
		if _, _, _, err := parseDataRowValues(page[rowStart:rowEnd], candidate.columns, decoder, candidate.historicalRows, candidate.lobReader); err == nil {
			matched++
		}
	}
	if limit == 0 {
		return false
	}
	required := limit
	if limit >= 4 {
		required = (limit*3 + 3) / 4
	}
	return matched >= required
}

func isTableDataAssistHeaderCandidate(info dataTableInfo, pageStorageID uint32, pageKind uint32) bool {
	if info.recoveryMode || info.dataStorageID == 0 || pageKind != dmPageKindRowData {
		return false
	}
	tableAssistID := tableDataAssistID(info.table.ID)
	return pageStorageID == tableAssistID && pageStorageID != info.dataStorageID
}

func isLooseHistoricalCandidate(info dataTableInfo) bool {
	return info.historicalRows && !info.recoveryMode && !info.pagePlanKnown && !info.storageKnown
}

func dataCandidateRank(info dataTableInfo) int {
	switch {
	case info.pagePlanKnown:
		return 0
	case info.recoveryMode:
		return 1
	case info.segmentKnown:
		return 2
	case info.storageKnown:
		return 3
	default:
		return 4
	}
}

func candidateMatchesFile(info dataTableInfo, file dataFileRef, pageNo uint32) bool {
	if info.pagePlanKnown {
		if len(info.pagePlan) == 0 || !info.pagePlan[dataPageRef{key: file.key, pageNo: pageNo}] {
			return false
		}
		// The exact physical reference is authoritative. Segment metadata may be
		// stale after extent movement and remains an auxiliary fallback only.
		return true
	}
	if info.recoveryMode {
		return candidateMatchesRecoveryFile(info, file)
	}
	if info.scanGroupOnly {
		return file.key.groupID == info.scanGroupID
	}
	if info.segmentKnown {
		if !candidateMatchesSegmentIdentity(info, file) {
			return false
		}
		if info.segment.blocks > 0 && info.segment.extents <= 1 {
			return pageNo >= info.segment.headerPage && pageNo < info.segment.headerPage+info.segment.blocks
		}
		if info.segment.headerPage > 0 && info.segment.extents <= 1 {
			return pageNo >= info.segment.headerPage
		}
		return true
	}
	if !info.storageKnown {
		return true
	}
	return uint32(info.storage.GroupID) == file.key.groupID && info.storage.RootFile == file.key.fileID
}

func candidateMatchesRecoveryFile(info dataTableInfo, file dataFileRef) bool {
	groupID := info.recoveryGroupID
	if groupID == 0 && info.segmentKnown {
		groupID = info.segment.tablespaceID
	}
	if groupID == 0 && info.storageKnown {
		groupID = uint32(info.storage.GroupID)
	}
	if groupID != 0 && file.key.groupID != groupID {
		return false
	}
	return true
}

func candidateMatchesSegmentIdentity(info dataTableInfo, file dataFileRef) bool {
	if !info.segmentKnown {
		return true
	}
	if uint32(info.segment.fileID) != uint32(file.key.fileID) {
		return false
	}
	if info.segment.tablespaceID != 0 && info.segment.tablespaceID != file.key.groupID {
		return false
	}
	return true
}

func segmentByTableID(dict *DictionaryInfo, tableID uint32) tableSegment {
	if dict == nil {
		return tableSegment{}
	}
	for _, table := range dict.Tables {
		if table.ID != tableID || !dictionaryTableHasSegment(table) {
			continue
		}
		return tableSegment{
			fileID:       table.HeaderFile,
			headerPage:   table.HeaderBlock,
			blocks:       table.Blocks,
			extents:      table.Extents,
			bytes:        table.Bytes,
			tablespace:   table.Tablespace,
			tablespaceID: table.GroupID,
		}
	}
	return tableSegment{}
}

func hasSegmentRange(dict *DictionaryInfo, tableID uint32) bool {
	if dict == nil {
		return false
	}
	for _, table := range dict.Tables {
		if table.ID == tableID {
			return dictionaryTableHasSegment(table)
		}
	}
	return false
}

func dictionaryTableHasSegment(table DictionaryTable) bool {
	return table.HeaderBlock > 0 && table.Blocks > 0
}

func dictionaryTableGroupID(tables map[uint32]DictionaryTable, tableID uint32) uint32 {
	table, ok := tables[tableID]
	if !ok {
		return 0
	}
	return table.GroupID
}

func dataStorageIDForTable(dictionaryTables map[uint32]DictionaryTable, dataStorageByTable map[uint32]indexDef, tableID uint32) uint32 {
	if table, ok := dictionaryTables[tableID]; ok && table.StorageID != 0 {
		return table.StorageID
	}
	if storage, ok := dataStorageByTable[tableID]; ok {
		return storage.ID
	}
	return 0
}

// shouldAllowHistoricalRows reports whether an assist storage may carry
// historical (pre-ALTER) row versions of the table. The table's own primary
// storage never counts: its pages are deduplicated page-wise across the
// direct, storage-scan and segment phases, so rows from it cannot be
// revisited, and flagging it forces row-coverage tracking (O(rows) strings)
// on every table.
func shouldAllowHistoricalRows(info dataTableInfo, assistID uint32) bool {
	return info.dataStorageID != 0 && assistID != 0 && assistID != info.dataStorageID
}

func dictionaryDataAssistIDs(tables map[uint32]DictionaryTable, tableID uint32) []uint32 {
	table, ok := tables[tableID]
	if !ok {
		return nil
	}
	seen := make(map[uint32]bool)
	var result []uint32
	add := func(id uint32) {
		if id == 0 || seen[id] {
			return
		}
		seen[id] = true
		result = append(result, id)
	}
	add(table.StorageID)
	for _, id := range table.AssistIDs {
		add(id)
	}
	// The 0x02000000|table_id guess can collide with unrelated live storages
	// and forces coverage tracking on every table, so it only participates
	// when the dictionary carries no real storage id at all.
	if table.StorageID == 0 && len(table.AssistIDs) == 0 {
		add(tableDataAssistID(tableID))
	}
	return result
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

type tableNameMatcher struct {
	all       bool
	hasRules  bool
	names     map[string]bool
	qualified map[string]bool
}

func newTableNameMatcher(filter string) tableNameMatcher {
	filter = strings.TrimSpace(filter)
	if filter == "" {
		return tableNameMatcher{}
	}
	if strings.EqualFold(filter, "all") || strings.EqualFold(filter, "*") {
		return tableNameMatcher{all: true, hasRules: true}
	}
	matcher := tableNameMatcher{
		hasRules:  true,
		names:     make(map[string]bool),
		qualified: make(map[string]bool),
	}
	for _, part := range strings.Split(filter, ",") {
		token := normalizeTableFilterToken(part)
		if token == "" {
			continue
		}
		if strings.Contains(token, ".") {
			matcher.qualified[token] = true
			continue
		}
		matcher.names[token] = true
	}
	if len(matcher.names) == 0 && len(matcher.qualified) == 0 {
		return tableNameMatcher{}
	}
	return matcher
}

func (m tableNameMatcher) allowed(owner string, table string) bool {
	if !m.hasRules {
		return false
	}
	if m.all {
		return true
	}
	owner = normalizeNameFilter(owner)
	table = normalizeNameFilter(table)
	return m.names[table] || m.qualified[owner+"."+table]
}

func normalizeNameFilter(value string) string {
	parts := splitQualifiedNameFilter(value)
	if len(parts) > 1 {
		return normalizeTableFilterToken(value)
	}
	return normalizeNameFilterPart(value)
}

func normalizeTableFilterToken(value string) string {
	parts := splitQualifiedNameFilter(value)
	if len(parts) == 0 {
		return ""
	}
	for i := range parts {
		parts[i] = normalizeNameFilterPart(parts[i])
	}
	return strings.Join(parts, ".")
}

func normalizeNameFilterPart(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
		value = value[1 : len(value)-1]
	} else {
		value = strings.Trim(value, `"`)
	}
	value = strings.ReplaceAll(value, `""`, `"`)
	return strings.ToUpper(value)
}

func splitQualifiedNameFilter(value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	var parts []string
	var current strings.Builder
	inQuote := false
	for i := 0; i < len(value); i++ {
		ch := value[i]
		if ch == '"' {
			current.WriteByte(ch)
			if inQuote && i+1 < len(value) && value[i+1] == '"' {
				i++
				current.WriteByte(value[i])
				continue
			}
			inQuote = !inQuote
			continue
		}
		if ch == '.' && !inQuote {
			parts = append(parts, current.String())
			current.Reset()
			continue
		}
		current.WriteByte(ch)
	}
	parts = append(parts, current.String())
	out := parts[:0]
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func resolveDataFiles(controlPath string, controlDULPath string, dataDir string) ([]dataFileRef, error) {
	var refs []dataFileRef
	tablespaceNames := defaultTablespaceNames()
	mergeControlDULTablespaceNames(tablespaceNames, controlDULPath)
	seenKeys := make(map[dataFileKey]bool)
	addRef := func(key dataFileKey, path string, tablespaceName string) {
		if key.groupID < 4 || path == "" || seenKeys[key] {
			return
		}
		refs = append(refs, dataFileRef{
			key:            key,
			path:           path,
			tablespaceName: tablespaceName,
		})
		seenKeys[key] = true
	}
	if strings.TrimSpace(controlPath) != "" {
		ctl, err := InspectControlFile(controlPath)
		if err != nil {
			return nil, fmt.Errorf("inspect dm.ctl: %w", err)
		}
		for _, entry := range ctl.Entries {
			tablespaceNames[entry.ID] = entry.Name
			if entry.ID < 4 {
				continue
			}
			fileID := int16(0)
			for _, controlPath := range entry.Paths {
				if !strings.EqualFold(pathpkg.Ext(strings.ReplaceAll(controlPath.Value, "\\", "/")), ".DBF") {
					continue
				}
				resolved, ok := resolveDataFilePath(controlPath.Value, dataDir)
				if !ok {
					fileID++
					continue
				}
				addRef(dataFileKey{groupID: entry.ID, fileID: fileID}, resolved, entry.Name)
				fileID++
			}
		}
	}
	if files, ok := readControlDUL(controlDULPath); ok {
		for _, file := range files {
			if file.GroupID < 4 || strings.TrimSpace(file.Path) == "" {
				continue
			}
			if file.Tablespace != "" {
				tablespaceNames[file.GroupID] = file.Tablespace
			}
			resolved, ok := resolveDataFilePath(file.Path, dataDir)
			if !ok {
				continue
			}
			addRef(dataFileKey{groupID: file.GroupID, fileID: file.FileID}, resolved, tablespaceNames[file.GroupID])
		}
	}
	refs = append(refs, scanDataFilesByPageHeader(dataDir, tablespaceNames, seenKeys)...)
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].key.groupID != refs[j].key.groupID {
			return refs[i].key.groupID < refs[j].key.groupID
		}
		return refs[i].key.fileID < refs[j].key.fileID
	})
	return refs, nil
}

func dataFileRefsFromSources(sources []OfflineDataSource) ([]dataFileRef, error) {
	refs := make([]dataFileRef, 0, len(sources))
	seen := make(map[dataFileKey]string)
	for _, source := range sources {
		if source.Reader == nil || source.GroupID < 4 {
			continue
		}
		key := dataFileKey{groupID: source.GroupID, fileID: source.FileID}
		if previous, exists := seen[key]; exists {
			return nil, fmt.Errorf("duplicate data file identity group=%d file=%d: %s and %s", key.groupID, key.fileID, previous, source.Path)
		}
		seen[key] = source.Path
		refs = append(refs, dataFileRef{
			key: key, path: source.Path, tablespaceName: source.Tablespace, reader: source.Reader,
		})
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].key.groupID == refs[j].key.groupID {
			return refs[i].key.fileID < refs[j].key.fileID
		}
		return refs[i].key.groupID < refs[j].key.groupID
	})
	return refs, nil
}

func forEachDataFileRefPage(file dataFileRef, pageSize uint32, visit func(page []byte, pageNo uint32) error) (int, error) {
	if file.reader != nil {
		return forEachSizedReaderPage(file.reader, pageSize, visit)
	}
	return forEachDataFilePage(file.path, pageSize, visit)
}

func scanDataFilesByPageHeader(dataDir string, tablespaceNames map[uint32]string, seenKeys map[dataFileKey]bool) []dataFileRef {
	if dataDir == "" {
		return nil
	}
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return nil
	}
	var refs []dataFileRef
	for _, entry := range entries {
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ".DBF") {
			continue
		}
		path := filepath.Join(dataDir, entry.Name())
		key, ok := dataFileKeyFromPageHeader(path)
		if !ok || key.groupID < 4 || seenKeys[key] {
			continue
		}
		tablespaceName := tablespaceNames[key.groupID]
		if tablespaceName == "" {
			tablespaceName = inferTablespaceNameFromDataFile(path, key.groupID)
			tablespaceNames[key.groupID] = tablespaceName
		}
		refs = append(refs, dataFileRef{
			key:            key,
			path:           path,
			tablespaceName: tablespaceName,
		})
		seenKeys[key] = true
	}
	return refs
}

func dataFileKeyFromPageHeader(path string) (dataFileKey, bool) {
	f, err := os.Open(path)
	if err != nil {
		return dataFileKey{}, false
	}
	defer f.Close()
	var head [8]byte
	if _, err := f.Read(head[:]); err != nil {
		return dataFileKey{}, false
	}
	pageNo := binary.LittleEndian.Uint32(head[4:])
	if pageNo != 0 {
		return dataFileKey{}, false
	}
	fileID := binary.LittleEndian.Uint16(head[2:])
	if fileID > uint16(^uint16(0)>>1) {
		return dataFileKey{}, false
	}
	return dataFileKey{
		groupID: uint32(binary.LittleEndian.Uint16(head[0:])),
		fileID:  int16(fileID),
	}, true
}

// resolveDataFilePath maps a dm.ctl / control.dul path entry to an actual file.
// The offline files the user placed in data_dir are authoritative: a same-named
// file there wins over the recorded (often absolute) path. dm.ctl/control.dul
// therefore act only as a cross-reference for the group/file/tablespace mapping,
// never dragging the read to the live database's original location when
// recovering on the same host. The recorded absolute path is used only as a
// fallback when no matching file exists in data_dir.
func resolveDataFilePath(controlValue string, dataDir string) (string, bool) {
	base := pathpkg.Base(strings.ReplaceAll(controlValue, "\\", "/"))
	if base != "." && base != "/" && base != "" && strings.TrimSpace(dataDir) != "" {
		candidate := filepath.Join(dataDir, base)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, true
		}
	}
	if info, err := os.Stat(controlValue); err == nil && !info.IsDir() {
		return controlValue, true
	}
	return "", false
}

type locatedDataRow struct {
	slotNo   uint16
	offset   uint16
	length   uint16
	deleted  bool
	fromSlot bool
}

func isProbableDMDataPage(page []byte, pageSize uint32) bool {
	if len(page) < 0x80 || len(page) < int(pageSize) {
		return false
	}
	nSlot := binary.LittleEndian.Uint16(page[dataPageSlotCountOff:])
	freeEnd := binary.LittleEndian.Uint16(page[dataPageFreeEndOff:])
	nRec := binary.LittleEndian.Uint16(page[dataPageRecordCountOff:])
	treeLevel := binary.LittleEndian.Uint16(page[dataPageTreeLevelOff:])
	kind := dataPageKind(page)
	if nSlot >= 2048 {
		return false
	}
	if nRec > nSlot {
		return false
	}
	if treeLevel != 0 {
		return false
	}
	if kind != dmPageKindRowData && kind != dmPageKindRowOverflow {
		return false
	}
	return freeEnd >= dataRowAreaStart && uint32(freeEnd) <= pageSize
}

func locateRowsInDataPage(page []byte, pageSize uint32, expectedRecords int) []locatedDataRow {
	return locateRowsInDataPageMode(page, pageSize, expectedRecords, false)
}

func locateRowsInDataPageForRecovery(page []byte, pageSize uint32) []locatedDataRow {
	return locateRowsInDataPageMode(page, pageSize, -1, true)
}

func locateRowsInDataPageMode(page []byte, pageSize uint32, _ int, scanPhysicalRows bool) []locatedDataRow {
	freeEnd := binary.LittleEndian.Uint16(page[dataPageFreeEndOff:])
	var rows []locatedDataRow
	seenOffsets := make(map[uint16]bool)
	nSlot := binary.LittleEndian.Uint16(page[dataPageSlotCountOff:])
	slotArrayStart := int(pageSize) - pageSlotTrailerLenForPage(page) - int(nSlot)*2
	if nSlot > 0 && nSlot < 2048 && slotArrayStart >= 0x40 && slotArrayStart+int(nSlot)*2 <= int(pageSize) {
		for slotNo := uint16(1); slotNo <= nSlot; slotNo++ {
			pos := slotArrayStart + int(slotNo-1)*2
			rowOff := binary.LittleEndian.Uint16(page[pos:])
			header, ok := decodeDataRowHeader(page, rowOff, pageSize, freeEnd)
			if !ok || (header.deleted && !scanPhysicalRows) {
				continue
			}
			seenOffsets[rowOff] = true
			rows = append(rows, locatedDataRow{
				slotNo:   slotNo,
				offset:   rowOff,
				length:   header.length,
				deleted:  header.deleted,
				fromSlot: true,
			})
		}
	}

	if scanPhysicalRows {
		pos := uint16(dataRowAreaStart)
		for int(pos)+3 <= int(freeEnd) && uint32(pos) < pageSize {
			header, ok := decodeDataRowHeader(page, pos, pageSize, freeEnd)
			if !ok || header.length == 0 {
				break
			}
			if !seenOffsets[pos] {
				rows = append(rows, locatedDataRow{
					offset:  pos,
					length:  header.length,
					deleted: header.deleted,
				})
			}
			pos += header.length
		}
	}

	sort.Slice(rows, func(i, j int) bool {
		if rows[i].offset == rows[j].offset {
			return rows[i].slotNo < rows[j].slotNo
		}
		return rows[i].offset < rows[j].offset
	})
	return rows
}

type dataRowHeader struct {
	length  uint16
	deleted bool
}

func decodeDataRowHeader(page []byte, rowOff uint16, pageSize uint32, freeEnd uint16) (dataRowHeader, bool) {
	if int(rowOff)+3 > len(page) || uint32(rowOff)+3 > pageSize {
		return dataRowHeader{}, false
	}
	// The first two bytes are a big-endian length/status word. The low 15 bits
	// are the physical row length; bit 15 marks a deleted row.
	raw := binary.BigEndian.Uint16(page[rowOff:])
	rowLen := raw &^ dataRowDeletedMask
	if rowLen < 3 {
		return dataRowHeader{}, false
	}
	if uint32(rowOff)+uint32(rowLen) > uint32(freeEnd) || uint32(rowOff)+uint32(rowLen) > pageSize {
		return dataRowHeader{}, false
	}
	return dataRowHeader{length: rowLen, deleted: raw&dataRowDeletedMask != 0}, true
}

type dataRowControlTail struct {
	clusterRowID  uint64
	rollFile      uint8
	rollPage      uint32
	rollOffset    uint16
	transactionID uint64
}

func decodeDataRowControlTail(row []byte) (dataRowControlTail, bool) {
	if len(row) < dataRowControlTailLen {
		return dataRowControlTail{}, false
	}
	tail := row[len(row)-dataRowControlTailLen:]
	return dataRowControlTail{
		clusterRowID:  decodeUint48LE(tail[0:6]),
		rollFile:      tail[6],
		rollPage:      binary.LittleEndian.Uint32(tail[7:11]),
		rollOffset:    binary.LittleEndian.Uint16(tail[11:13]),
		transactionID: decodeUint48LE(tail[13:19]),
	}, true
}

func (tail dataRowControlTail) hasRollbackAddress() bool {
	return tail.rollFile != 0xFF || tail.rollPage != 0x7FFFFFFF || tail.rollOffset != 0xFFFF
}

func decodeUint48LE(raw []byte) uint64 {
	if len(raw) < 6 {
		return 0
	}
	return uint64(raw[0]) |
		uint64(raw[1])<<8 |
		uint64(raw[2])<<16 |
		uint64(raw[3])<<24 |
		uint64(raw[4])<<32 |
		uint64(raw[5])<<40
}

func renderInsertForDataRow(info dataTableInfo, row []byte, decoder textDecoder) (string, int, int, error) {
	line, dataStart, dataEnd, _, err := renderInsertForDataRowWithMeta(info, row, decoder)
	return line, dataStart, dataEnd, err
}

// sqlInsertPrefixForTable renders the per-table constant INSERT frame once so
// the hot per-row path only appends values.
func sqlInsertPrefixForTable(table dictionaryObject, columns []columnDef) string {
	var b strings.Builder
	b.WriteString("INSERT INTO ")
	b.WriteString(quoteIdent(table.Owner))
	b.WriteByte('.')
	b.WriteString(quoteIdent(table.Name))
	b.WriteString(" (")
	for i, col := range columns {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(quoteIdent(col.Name))
	}
	b.WriteString(") VALUES (")
	return b.String()
}

func renderInsertForDataRowWithMeta(info dataTableInfo, row []byte, decoder textDecoder) (string, int, int, dataRowRenderMeta, error) {
	values, dataStart, dataEnd, err := parseDataRowValues(row, info.columns, decoder, info.historicalRows, info.lobReader)
	if err != nil {
		return "", 0, 0, dataRowRenderMeta{}, err
	}
	prefix := info.sqlInsertPrefix
	if prefix == "" {
		prefix = sqlInsertPrefixForTable(info.table, info.columns)
	}
	var b strings.Builder
	b.Grow(len(prefix) + 32*len(info.columns))
	b.WriteString(prefix)
	for i, col := range info.columns {
		if i > 0 {
			b.WriteString(", ")
		}
		value, ok := values[col.ColID]
		if !ok {
			b.WriteString("NULL")
			continue
		}
		sqlValue, err := sqlValueForDataColumn(col, value.value)
		if err != nil {
			return "", 0, 0, dataRowRenderMeta{}, err
		}
		b.WriteString(sqlValue)
	}
	b.WriteString(");")
	return b.String(), dataStart, dataEnd, dataRowRenderMetaForValues(info.columns, values, info.coverage.active()), nil
}

func dataRowRenderMetaForValues(columns []columnDef, values map[uint16]dataValue, trackCoverage bool) dataRowRenderMeta {
	ordered := append([]columnDef(nil), columns...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].ColID < ordered[j].ColID })
	var present []columnDef
	for _, col := range ordered {
		if _, ok := values[col.ColID]; !ok {
			break
		}
		present = append(present, col)
	}
	meta := dataRowRenderMeta{
		partial: len(present) < len(ordered),
	}
	for _, col := range present {
		meta.presentColIDs = append(meta.presentColIDs, col.ColID)
	}
	// Coverage keys exist to deduplicate rows that can be visited via more
	// than one storage path. Partial (ALTER-history) rows always carry keys so
	// pending partials can suppress duplicates among themselves; full rows
	// carry them only when the table's storages make revisits possible.
	// Skipping them on plain direct-read exports avoids O(columns) string
	// allocations per row, which dominates memory on 10M-row tables.
	if !trackCoverage && !meta.partial {
		return meta
	}
	if len(present) > 0 {
		meta.prefixKey = dataRowPrefixKey(present, values)
		meta.weakPrefixKey = dataRowPrefixKey(present[:1], values)
	}
	for keep := 1; keep <= len(present); keep++ {
		meta.coverageKeys = append(meta.coverageKeys, dataRowPrefixKey(present[:keep], values))
	}
	return meta
}

func dataRowPrefixKey(columns []columnDef, values map[uint16]dataValue) string {
	var parts []string
	for _, col := range columns {
		value, ok := values[col.ColID]
		if !ok {
			break
		}
		parts = append(parts, fmt.Sprintf("%d=%s", col.ColID, dataValueSignature(value.value)))
	}
	return strings.Join(parts, "|")
}

func dataValueSignature(value any) string {
	switch v := value.(type) {
	case nil:
		return "NULL"
	case dmBinary:
		return "BIN:" + hex.EncodeToString(v)
	case dmLOBValue:
		return fmt.Sprintf("LOB:%d:%d:%d:%d:%t", v.locator.lobID, v.locator.groupID, v.locator.firstPage, v.locator.byteLen, v.text)
	default:
		return fmt.Sprintf("%T:%v", value, value)
	}
}

// stampRowCoverageTracking enables row coverage keys only for tables whose
// rows can be visited through more than one storage path. Everything is
// tracked in recovery mode; otherwise only tables that gained historical or
// orphan candidates need it, and their primary candidates must track too so
// full rows can suppress duplicate ALTER-history partial rows.
func stampRowCoverageTracking(assistByID map[uint32][]dataTableInfo, storageUnits map[uint32]dataTableInfo, recoveryMode bool) map[uint32]*tableCoverageState {
	coverageDebug := os.Getenv("DMDUL_DEBUG_COVERAGE") != ""
	states := make(map[uint32]*tableCoverageState)
	ensure := func(tableID uint32) *tableCoverageState {
		if states[tableID] == nil {
			states[tableID] = &tableCoverageState{}
		}
		return states[tableID]
	}
	for assistID, infos := range assistByID {
		for _, info := range infos {
			if recoveryMode || info.historicalRows || info.orphanRecovery || info.recoveryMode {
				ensure(info.table.ID)
				if coverageDebug {
					fmt.Fprintf(os.Stderr, "[coverage-debug] table=%s.%s assist=%d hist=%v orphan=%v recov=%v dataStorageID=%d storageKnown=%v\n",
						info.table.Owner, info.table.Name, assistID, info.historicalRows, info.orphanRecovery, info.recoveryMode, info.dataStorageID, info.storageKnown)
				}
			}
		}
	}
	for assistID, infos := range assistByID {
		for i := range infos {
			if state := states[infos[i].table.ID]; state != nil {
				infos[i].coverage = state
			}
		}
		assistByID[assistID] = infos
	}
	for unitID, info := range storageUnits {
		if state := states[info.table.ID]; state != nil {
			info.coverage = state
			storageUnits[unitID] = info
		}
	}
	return states
}

func singleSelectedDataTable(tables map[uint32]dataTableInfo) (dataTableInfo, bool) {
	if len(tables) != 1 {
		return dataTableInfo{}, false
	}
	for _, table := range tables {
		return table, true
	}
	return dataTableInfo{}, false
}

func normalizeDataOutputFormat(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return "sql"
	}
	switch value {
	case "sql", "dmp", "fldr":
		return value
	case "csv":
		// The former CSV export is now the dmfldr text format: a plain CSV
		// cannot be loaded back into DM, which is the point of a recovery
		// dump. "csv" stays accepted as an alias so existing scripts keep
		// working, but it produces the pipe-delimited .txt plus .ctl pair.
		return "fldr"
	default:
		return ""
	}
}

func parseDataRowValues(row []byte, columns []columnDef, decoder textDecoder, allowHistoricalRows bool, lobReader *dmLOBReader) (map[uint16]dataValue, int, int, error) {
	values, start, end, err := parseDataRowValuesForColumns(row, columns, decoder, lobReader)
	if err == nil {
		return values, start, end, nil
	}
	if errors.Is(err, errUnsupportedRowMetadataState) {
		return nil, 0, 0, err
	}
	if !allowHistoricalRows {
		return nil, 0, 0, err
	}
	firstErr := err
	for _, historicalColumns := range historicalColumnPrefixes(columns) {
		values, start, end, err = parseDataRowValuesForColumns(row, historicalColumns, decoder, lobReader)
		if err == nil {
			return values, start, end, nil
		}
	}
	return nil, 0, 0, firstErr
}

func historicalColumnPrefixes(columns []columnDef) [][]columnDef {
	if len(columns) <= 1 {
		return nil
	}
	ordered := append([]columnDef(nil), columns...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].ColID < ordered[j].ColID })
	var result [][]columnDef
	for keep := len(ordered) - 1; keep >= 1; keep-- {
		if !canOmitHistoricalColumns(ordered[keep:]) {
			break
		}
		result = append(result, append([]columnDef(nil), ordered[:keep]...))
	}
	return result
}

func canOmitHistoricalColumns(columns []columnDef) bool {
	if len(columns) == 0 {
		return false
	}
	for _, col := range columns {
		if !isNullableColumn(col) && strings.TrimSpace(col.Default) == "" {
			return false
		}
	}
	return true
}

func dataStorageColumns(columns []columnDef) []columnDef {
	ordered := append([]columnDef(nil), columns...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].ColID < ordered[j].ColID })
	var fixedCols []columnDef
	var varCols []columnDef
	for _, col := range ordered {
		switch {
		case fixedDataSizeForColumn(col) > 0:
			fixedCols = append(fixedCols, col)
		case isVariableDataType(col.DataType):
			varCols = append(varCols, col)
		default:
			varCols = append(varCols, col)
		}
	}
	return append(fixedCols, varCols...)
}

func rowMetadataLength(columnCount int) int {
	if columnCount <= 0 {
		return 0
	}
	return (columnCount + 3) / 4
}

func decodeRowColumnStates(raw []byte, columnCount int) []byte {
	states := make([]byte, columnCount)
	for i := 0; i < columnCount; i++ {
		b := raw[i/4]
		states[i] = (b >> uint((i%4)*2)) & 0x03
	}
	return states
}

func readInRowDataValue(col columnDef, row []byte, pos int, decoder textDecoder, lobReader *dmLOBReader) (any, int, error) {
	if fixedDataSizeForColumn(col) > 0 {
		// Row metadata states already distinguish NULL from present values,
		// so decode the bytes as-is instead of guessing at NULL sentinels.
		return parseFixedDataValuePresent(col, row, pos)
	}
	return readVariableDataValue(col, row, pos, decoder, lobReader)
}

func readOutOfLineDataValue(col columnDef, row []byte, pos int, decoder textDecoder, lobReader *dmLOBReader) (any, int, error) {
	locator, next, err := readDMLOBLocator(row, pos)
	if err != nil {
		return nil, pos, fmt.Errorf("%s: %w", col.Name, err)
	}
	if lobReader == nil {
		return nil, pos, fmt.Errorf("%s: out-of-line locator cannot be resolved without data files", col.Name)
	}
	if isJSONDataType(col.DataType) {
		value, err := lobReader.lazyLOBValue(locator, dmPageKindLOBData, false, decoder)
		if err != nil {
			return nil, pos, fmt.Errorf("%s: %w", col.Name, err)
		}
		return dmJSONValue{value: value, binary: normalizeDataType(col.DataType) == "JSONB", decoder: decoder}, next, nil
	}
	if isBinaryDataType(col.DataType) {
		value, err := lobReader.lazyLOBValue(locator, dmPageKindLOBData, false, decoder)
		if err != nil {
			return nil, pos, fmt.Errorf("%s: %w", col.Name, err)
		}
		return value, next, nil
	}
	if isCharacterLOBDataType(col.DataType) {
		if value, err := lobReader.lazyLOBValue(locator, dmPageKindLOBData, true, decoder); err == nil {
			return value, next, nil
		}
		payload, err := lobReader.readLongRowPayload(locator)
		if err != nil {
			return nil, pos, fmt.Errorf("%s: %w", col.Name, err)
		}
		value, err := decodeLOBTextValue(col.Name, payload, decoder)
		if err != nil {
			return nil, pos, err
		}
		return value, next, nil
	}
	if isCharacterDataType(col.DataType) {
		// An out-of-line VARCHAR/CHAR column can land in either storage: DM
		// keeps sub-page overflow columns in long-row (0x22) pages but spills
		// larger ones (~page/2 and up) into regular LOB (0x20) pages. Try both,
		// LOB first, so wide USING LONG ROW rows resolve either way.
		payload, err := lobReader.readTextLOBOrLongRowPayload(locator)
		if err != nil {
			return nil, pos, fmt.Errorf("%s: %w", col.Name, err)
		}
		value, ok := decoder.decode(payload)
		if !ok || strings.ContainsRune(value, '\uFFFD') || containsBadControl(value) {
			return nil, pos, fmt.Errorf("%s: cannot decode out-of-line long row text", col.Name)
		}
		return value, next, nil
	}
	return nil, pos, fmt.Errorf("%s: unsupported out-of-line data type %s", col.Name, col.DataType)
}

func parseDataRowValuesForColumns(row []byte, columns []columnDef, decoder textDecoder, lobReader *dmLOBReader) (map[uint16]dataValue, int, int, error) {
	values, start, end, err := parseDataRowValuesWithMetadata(row, columns, decoder, lobReader)
	if err == nil {
		return values, start, end, nil
	}
	if errors.Is(err, errUnsupportedRowMetadataState) {
		return nil, 0, 0, err
	}
	metadataErr := err
	values, start, end, err = parseDataRowValuesHeuristic(row, columns, decoder, lobReader)
	if err == nil {
		return values, start, end, nil
	}
	return nil, 0, 0, fmt.Errorf("%v; heuristic: %w", metadataErr, err)
}

func parseDataRowValuesWithMetadata(row []byte, columns []columnDef, decoder textDecoder, lobReader *dmLOBReader) (map[uint16]dataValue, int, int, error) {
	if len(columns) == 0 {
		return nil, 0, 0, fmt.Errorf("no columns")
	}
	storageColumns := dataStorageColumns(columns)
	metaLen := rowMetadataLength(len(storageColumns))
	start := 2 + metaLen
	if len(row) < start {
		return nil, 0, 0, fmt.Errorf("row too short for metadata: len=%d metadata=%d", len(row), metaLen)
	}
	states := decodeRowColumnStates(row[2:start], len(storageColumns))
	pos := start
	values := make(map[uint16]dataValue, len(columns))
parseColumns:
	for i, col := range storageColumns {
		state := states[i]
		switch state {
		case 0x03:
			if fixedDataSizeForColumn(col) > 0 {
				pos += fixedDataSizeForColumn(col)
				if pos > len(row) {
					return nil, 0, 0, fmt.Errorf("%s fixed NULL out of range", col.Name)
				}
			}
			values[col.ColID] = dataValue{value: nil}
		case 0x01:
			// State 1 marks special/overflow storage. Inline LOB values also use
			// it on some DM8 builds, so accept the normal length-prefixed form
			// first and fall back to a bare 21-byte locator.
			value, next, err := readInRowDataValue(col, row, pos, decoder, lobReader)
			if err != nil {
				value, next, err = readOutOfLineDataValue(col, row, pos, decoder, lobReader)
			}
			if err != nil {
				return nil, 0, 0, err
			}
			values[col.ColID] = dataValue{value: value}
			pos = next
		case 0x02:
			return nil, 0, 0, fmt.Errorf("%w 10 for column %s", errUnsupportedRowMetadataState, col.Name)
		case 0x00:
			value, next, err := readInRowDataValue(col, row, pos, decoder, lobReader)
			if err != nil {
				if fixedDataSizeForColumn(col) == 0 && canOmitTrailingNullVars(row, pos, storageColumns[i:]) {
					for _, nullCol := range storageColumns[i:] {
						values[nullCol.ColID] = dataValue{value: nil}
					}
					break parseColumns
				}
				return nil, 0, 0, err
			}
			values[col.ColID] = dataValue{value: value}
			pos = next
		}
	}
	trailing := len(row) - pos
	if trailing < 0 || trailing > 64 {
		return nil, 0, 0, fmt.Errorf("bad trailing length %d", trailing)
	}
	return values, start, pos, nil
}

func parseDataRowValuesHeuristic(row []byte, columns []columnDef, decoder textDecoder, lobReader *dmLOBReader) (map[uint16]dataValue, int, int, error) {
	var fixedCols []columnDef
	var varCols []columnDef
	for _, col := range columns {
		switch {
		case fixedDataSizeForColumn(col) > 0:
			fixedCols = append(fixedCols, col)
		case isVariableDataType(col.DataType):
			varCols = append(varCols, col)
		}
	}

	type candidate struct {
		score                    int
		values                   map[uint16]dataValue
		start                    int
		end                      int
		omittedTrailingNullValue bool
	}
	var best *candidate
	var errors []string
	limit := len(row)
	if limit > 16 {
		limit = 16
	}
	for start := 3; start < limit; start++ {
		pos := start
		values := make(map[uint16]dataValue)
		ok := true
		for _, col := range fixedCols {
			value, next, err := parseFixedDataValue(col, row, pos)
			if err != nil {
				errors = append(errors, fmt.Sprintf("start=%d %v", start, err))
				ok = false
				break
			}
			values[col.ColID] = dataValue{value: value}
			pos = next
		}
		if !ok {
			continue
		}
		omittedTrailingNullValue := false
		for i, col := range varCols {
			value, next, err := readVariableDataValue(col, row, pos, decoder, lobReader)
			if err != nil {
				if canOmitTrailingNullVars(row, pos, varCols[i:]) {
					for _, nullCol := range varCols[i:] {
						values[nullCol.ColID] = dataValue{value: nil}
					}
					omittedTrailingNullValue = true
					break
				}
				errors = append(errors, fmt.Sprintf("start=%d %v", start, err))
				ok = false
				break
			}
			values[col.ColID] = dataValue{value: value}
			pos = next
		}
		if !ok {
			continue
		}
		trailing := len(row) - pos
		if trailing < 0 || trailing > 64 {
			errors = append(errors, fmt.Sprintf("start=%d bad trailing length %d", start, trailing))
			continue
		}
		if trailing > 0 && trailing < 8 {
			errors = append(errors, fmt.Sprintf("start=%d short trailing length %d", start, trailing))
			continue
		}
		score := 100 - trailing - start*4
		if best == nil || score > best.score {
			best = &candidate{score: score, values: values, start: start, end: pos, omittedTrailingNullValue: omittedTrailingNullValue}
		}
	}
	if best == nil {
		if len(errors) > 5 {
			errors = errors[:5]
		}
		return nil, 0, 0, fmt.Errorf("cannot parse row; candidates errors=%v", errors)
	}
	if best.omittedTrailingNullValue {
		markTrailingNullableZeroFixedValues(best.values, fixedCols)
	}
	return best.values, best.start, best.end, nil
}

func canOmitTrailingNullVars(row []byte, pos int, cols []columnDef) bool {
	if pos < 0 || pos >= len(row) || len(cols) == 0 {
		return false
	}
	for _, col := range cols {
		if !isNullableColumn(col) {
			return false
		}
	}
	remaining := len(row) - pos
	if remaining < 8 || remaining > 64 {
		return false
	}
	marker := row[pos]
	return marker < 0x80
}

func markTrailingNullableZeroFixedValues(values map[uint16]dataValue, fixedCols []columnDef) {
	for i := len(fixedCols) - 1; i >= 0; i-- {
		col := fixedCols[i]
		if !isNullableColumn(col) {
			return
		}
		value, ok := values[col.ColID]
		if !ok || !isZeroFixedValue(value.value) {
			return
		}
		values[col.ColID] = dataValue{value: nil}
	}
}

func isNullableColumn(col columnDef) bool {
	return !strings.EqualFold(strings.TrimSpace(col.Nullable), "N")
}

func isZeroFixedValue(value any) bool {
	switch v := value.(type) {
	case int8:
		return v == 0
	case int16:
		return v == 0
	case int32:
		return v == 0
	case int64:
		return v == 0
	default:
		return false
	}
}

func isVariableDataType(dataType string) bool {
	return isCharacterDataType(dataType) || isVariableBinaryDataType(dataType) || isNumberDataType(dataType) ||
		isJSONDataType(dataType) || normalizeDataType(dataType) == "BFILE"
}

func isCharacterDataType(dataType string) bool {
	upper := normalizeDataType(dataType)
	return strings.Contains(upper, "CHAR") || strings.Contains(upper, "VARCHAR") || strings.Contains(upper, "TEXT") || strings.Contains(upper, "CLOB") || upper == "LONG" || upper == "XMLTYPE"
}

func isCharacterLOBDataType(dataType string) bool {
	upper := normalizeDataType(dataType)
	return strings.Contains(upper, "CLOB") || strings.Contains(upper, "TEXT") || upper == "LONG" || upper == "LONGVARCHAR" || upper == "XMLTYPE"
}

func isBinaryDataType(dataType string) bool {
	switch normalizeDataType(dataType) {
	case "BINARY", "VARBINARY", "RAW", "LONGVARBINARY", "LONG RAW", "BLOB", "IMAGE":
		return true
	default:
		return false
	}
}

func isVariableBinaryDataType(dataType string) bool {
	switch normalizeDataType(dataType) {
	case "VARBINARY", "RAW", "LONGVARBINARY", "LONG RAW", "BLOB", "IMAGE":
		return true
	default:
		return false
	}
}

func isNumberDataType(dataType string) bool {
	switch normalizeDataType(dataType) {
	case "NUMBER", "NUMERIC", "DEC", "DECIMAL":
		return true
	default:
		return false
	}
}

func fixedDataSizeForColumn(col columnDef) int {
	return fixedDataSizeForType(normalizeDataType(col.DataType), col.Length)
}

func fixedDataSizeForType(dataType string, length uint32) int {
	if isDayTimeIntervalDataType(dataType) {
		return 24
	}
	if isYearMonthIntervalDataType(dataType) {
		return 12
	}
	switch dataType {
	case "BIT", "BOOL", "BOOLEAN", "BYTE", "TINYINT":
		return 1
	case "SMALLINT":
		return 2
	case "INT", "INTEGER", "PLS_INTEGER":
		return 4
	case "BIGINT":
		return 8
	case "REAL", "BINARY_FLOAT":
		return 4
	case "FLOAT":
		if length == 4 {
			return 4
		}
		return 8
	case "DOUBLE", "DOUBLE PRECISION", "BINARY_DOUBLE":
		return 8
	case "BINARY":
		return int(length)
	case "CHAR", "CHARACTER":
		// DM stores single-byte CHAR(1) flags in the fixed section without a
		// short-value length marker. Wider CHAR values remain variable-width.
		if length == 1 {
			return 1
		}
		return 0
	case "DATE":
		return 3
	case "TIME":
		return 5
	case "TIME WITH TIME ZONE":
		return 7
	case "DATETIME", "TIMESTAMP", "TIMESTAMP WITH LOCAL TIME ZONE":
		if length == 9 {
			return 9
		}
		return 8
	case "DATETIME WITH TIME ZONE", "TIMESTAMP WITH TIME ZONE":
		if length == 11 {
			return 11
		}
		return 10
	case "ROWID":
		return 12
	default:
		return 0
	}
}

func normalizeDataType(dataType string) string {
	upper := strings.ToUpper(strings.TrimSpace(dataType))
	if idx := strings.IndexByte(upper, '('); idx >= 0 {
		if end := strings.IndexByte(upper[idx:], ')'); end >= 0 {
			upper = strings.TrimSpace(upper[:idx] + " " + upper[idx+end+1:])
		} else {
			upper = strings.TrimSpace(upper[:idx])
		}
	}
	return strings.Join(strings.Fields(upper), " ")
}

// parseFixedDataValue decodes a fixed-size value where no per-column NULL
// metadata is available (heuristic row parsing), so recognizable NULL
// sentinel encodings are honoured before decoding.
func parseFixedDataValue(col columnDef, row []byte, pos int) (any, int, error) {
	dataType := normalizeDataType(col.DataType)
	size := fixedDataSizeForColumn(col)
	if size > 0 && pos+size <= len(row) && isNullableColumn(col) && isFixedNullSentinel(dataType, row[pos:pos+size]) {
		return nil, pos + size, nil
	}
	return parseFixedDataValuePresent(col, row, pos)
}

// parseFixedDataValuePresent decodes a fixed-size value that row metadata has
// already marked as present. Zero-filled encodings are legitimate values here
// (for example TIME '00:00:00'), so no NULL sentinel heuristics are applied.
func parseFixedDataValuePresent(col columnDef, row []byte, pos int) (any, int, error) {
	dataType := normalizeDataType(col.DataType)
	size := fixedDataSizeForColumn(col)
	switch dataType {
	case "BIT", "BOOL", "BOOLEAN":
		if pos+1 > len(row) {
			return nil, pos, fmt.Errorf("%s out of range", dataType)
		}
		if row[pos] == 0 {
			return int8(0), pos + 1, nil
		}
		return int8(1), pos + 1, nil
	case "BYTE":
		if pos+1 > len(row) {
			return nil, pos, fmt.Errorf("BYTE out of range")
		}
		return int8(row[pos]), pos + 1, nil
	case "TINYINT":
		if pos+1 > len(row) {
			return nil, pos, fmt.Errorf("TINYINT out of range")
		}
		return int8(row[pos]), pos + 1, nil
	case "SMALLINT":
		if pos+2 > len(row) {
			return nil, pos, fmt.Errorf("SMALLINT out of range")
		}
		return int16(binary.LittleEndian.Uint16(row[pos:])), pos + 2, nil
	case "INT", "INTEGER", "PLS_INTEGER":
		if pos+4 > len(row) {
			return nil, pos, fmt.Errorf("INT out of range")
		}
		return int32(binary.LittleEndian.Uint32(row[pos:])), pos + 4, nil
	case "BIGINT":
		if pos+8 > len(row) {
			return nil, pos, fmt.Errorf("BIGINT out of range")
		}
		return int64(binary.LittleEndian.Uint64(row[pos:])), pos + 8, nil
	case "REAL", "BINARY_FLOAT":
		if pos+4 > len(row) {
			return nil, pos, fmt.Errorf("REAL out of range")
		}
		return math.Float32frombits(binary.LittleEndian.Uint32(row[pos:])), pos + 4, nil
	case "FLOAT":
		if size == 4 {
			if pos+4 > len(row) {
				return nil, pos, fmt.Errorf("FLOAT out of range")
			}
			return math.Float32frombits(binary.LittleEndian.Uint32(row[pos:])), pos + 4, nil
		}
		if pos+8 > len(row) {
			return nil, pos, fmt.Errorf("FLOAT out of range")
		}
		return math.Float64frombits(binary.LittleEndian.Uint64(row[pos:])), pos + 8, nil
	case "DOUBLE", "DOUBLE PRECISION", "BINARY_DOUBLE":
		if pos+8 > len(row) {
			return nil, pos, fmt.Errorf("%s out of range", dataType)
		}
		return math.Float64frombits(binary.LittleEndian.Uint64(row[pos:])), pos + 8, nil
	case "BINARY":
		if size <= 0 || pos+size > len(row) {
			return nil, pos, fmt.Errorf("BINARY out of range")
		}
		return dmBinary(append([]byte(nil), row[pos:pos+size]...)), pos + size, nil
	case "CHAR", "CHARACTER":
		if size != 1 || pos+size > len(row) {
			return nil, pos, fmt.Errorf("%s out of range", dataType)
		}
		return string(row[pos : pos+size]), pos + size, nil
	case "DATE":
		if pos+3 > len(row) {
			return nil, pos, fmt.Errorf("DATE out of range")
		}
		value, err := decodeDMDate(row[pos : pos+3])
		if err != nil {
			return nil, pos, err
		}
		return value, pos + 3, nil
	case "TIME":
		if pos+5 > len(row) {
			return nil, pos, fmt.Errorf("TIME out of range")
		}
		value, err := decodeDMTimeWithPrecision(row[pos:pos+5], timeFractionalPrecision(col.Scale))
		if err != nil {
			return nil, pos, err
		}
		return value, pos + 5, nil
	case "TIME WITH TIME ZONE":
		if pos+7 > len(row) {
			return nil, pos, fmt.Errorf("TIME WITH TIME ZONE out of range")
		}
		value, err := decodeDMTimeWithPrecision(row[pos:pos+5], timeFractionalPrecision(col.Scale))
		if err != nil {
			return nil, pos, err
		}
		return value + " " + decodeDMTimezone(row[pos+5:pos+7]), pos + 7, nil
	case "DATETIME", "TIMESTAMP", "TIMESTAMP WITH LOCAL TIME ZONE":
		if size <= 0 || pos+size > len(row) {
			return nil, pos, fmt.Errorf("%s out of range", dataType)
		}
		value, err := decodeDMDateTimeWithPrecision(row[pos:pos+size], timeFractionalPrecision(col.Scale))
		if err != nil {
			return nil, pos, err
		}
		return value, pos + size, nil
	case "DATETIME WITH TIME ZONE", "TIMESTAMP WITH TIME ZONE":
		if size <= 2 || pos+size > len(row) {
			return nil, pos, fmt.Errorf("%s out of range", dataType)
		}
		value, err := decodeDMDateTimeWithPrecision(row[pos:pos+size-2], timeFractionalPrecision(col.Scale))
		if err != nil {
			return nil, pos, err
		}
		return value + " " + decodeDMTimezone(row[pos+size-2:pos+size]), pos + size, nil
	case "INTERVAL DAY", "INTERVAL HOUR", "INTERVAL MINUTE", "INTERVAL SECOND",
		"INTERVAL DAY TO HOUR", "INTERVAL DAY TO MINUTE", "INTERVAL DAY TO SECOND",
		"INTERVAL HOUR TO MINUTE", "INTERVAL HOUR TO SECOND", "INTERVAL MINUTE TO SECOND":
		if pos+24 > len(row) {
			return nil, pos, fmt.Errorf("%s out of range", dataType)
		}
		return decodeDMIntervalDayToSecond(row[pos:pos+24], dataType), pos + 24, nil
	case "INTERVAL YEAR TO MONTH", "INTERVAL YEAR", "INTERVAL MONTH":
		if pos+12 > len(row) {
			return nil, pos, fmt.Errorf("%s out of range", dataType)
		}
		return decodeDMIntervalYearToMonth(row[pos:pos+12], dataType), pos + 12, nil
	case "ROWID":
		if pos+12 > len(row) {
			return nil, pos, fmt.Errorf("ROWID out of range")
		}
		return dmRowID(decodeDMRowID(row[pos : pos+12])), pos + 12, nil
	default:
		return nil, pos, fmt.Errorf("unsupported fixed type: %s", dataType)
	}
}

func isFixedNullSentinel(dataType string, raw []byte) bool {
	switch dataType {
	case "CHAR", "CHARACTER":
		return len(raw) == 1 && (raw[0] == 0 || raw[0] == 0xFF)
	case "DATE":
		if len(raw) != 3 {
			return false
		}
		return raw[0] == 0 && raw[1] == 0 && raw[2] != 0
	case "TIME":
		if len(raw) != 5 {
			return false
		}
		if isAllBytes(raw, 0x00) {
			return true
		}
		if raw[0] == 0 && raw[1] == 0 {
			return true
		}
		return raw[0] == 0xFF && raw[1] == 0xFF && raw[4] == 0x7F
	case "TIME WITH TIME ZONE":
		if len(raw) != 7 {
			return false
		}
		return isFixedNullSentinel("TIME", raw[:5])
	case "DATETIME", "TIMESTAMP", "TIMESTAMP WITH LOCAL TIME ZONE":
		if len(raw) != 8 && len(raw) != 9 {
			return false
		}
		if isAllBytes(raw, 0x00) {
			return true
		}
		if raw[0] == 0 && raw[1] == 0 {
			return true
		}
		return raw[0] == 0xFF && raw[1] == 0xFF && raw[4] == 0x7F
	case "DATETIME WITH TIME ZONE", "TIMESTAMP WITH TIME ZONE":
		if len(raw) != 10 && len(raw) != 11 {
			return false
		}
		return isFixedNullSentinel("DATETIME", raw[:len(raw)-2])
	default:
		return false
	}
}

func isAllBytes(raw []byte, value byte) bool {
	if len(raw) == 0 {
		return false
	}
	for _, b := range raw {
		if b != value {
			return false
		}
	}
	return true
}

func readVariableDataValue(col columnDef, row []byte, pos int, decoder textDecoder, lobReader *dmLOBReader) (any, int, error) {
	if isJSONDataType(col.DataType) {
		return readJSONDataValue(col, row, pos, decoder, lobReader)
	}
	if isNumberDataType(col.DataType) {
		value, next, err := readDMNumber(row, pos)
		if err != nil {
			return nil, pos, fmt.Errorf("%s: %w", col.Name, err)
		}
		return value, next, nil
	}
	if isBinaryDataType(col.DataType) {
		value, next, err := readShortDataBytes(row, pos)
		if err != nil {
			return nil, pos, fmt.Errorf("%s: %w", col.Name, err)
		}
		if payload, ok := unwrapInlineLOBPayload(value); ok {
			value = payload
		} else if locator, ok := parseDMLOBLocatorRaw(value); ok {
			if lobReader == nil {
				return nil, pos, fmt.Errorf("%s: out-of-line binary LOB locator cannot be resolved without data files", col.Name)
			}
			lazy, lazyErr := lobReader.lazyLOBValue(locator, dmPageKindLOBData, false, decoder)
			if lazyErr != nil {
				return nil, pos, fmt.Errorf("%s: %w", col.Name, lazyErr)
			}
			return lazy, next, nil
		}
		return dmBinary(value), next, nil
	}
	if isCharacterLOBDataType(col.DataType) {
		value, next, err := readInlineTextLOB(row, pos, decoder, lobReader)
		if err != nil {
			return nil, pos, fmt.Errorf("%s: %w", col.Name, err)
		}
		return value, next, nil
	}
	raw, next, marker, err := readShortDataBytesWithMarker(row, pos)
	if err != nil {
		return nil, pos, fmt.Errorf("%s: %w", col.Name, err)
	}
	if locator, ok := parseDMLOBLocatorRaw(raw); ok {
		if lobReader == nil {
			return nil, pos, fmt.Errorf("%s: out-of-line long row locator cannot be resolved without data files", col.Name)
		}
		// An overflow VARCHAR/CHAR column can live in either a long-row (0x22)
		// page or a regular LOB (0x20) page: DM keeps sub-page overflow in
		// long-row pages but spills columns at/over ~page/2 into LOB pages
		// (observed at the 4000-byte boundary on 8K pages). Try both.
		raw, err = lobReader.readTextLOBOrLongRowPayload(locator)
		if err != nil {
			return nil, pos, fmt.Errorf("%s: %w", col.Name, err)
		}
	}
	value, ok := decoder.decode(raw)
	if !ok {
		return nil, pos, fmt.Errorf("%s: cannot decode varchar marker=0x%02X raw=%s", col.Name, marker, strings.ToUpper(hex.EncodeToString(raw)))
	}
	if strings.ContainsRune(value, '\uFFFD') || containsBadControl(value) {
		return nil, pos, fmt.Errorf("%s: decoded varchar contains invalid characters marker=0x%02X raw=%s", col.Name, marker, strings.ToUpper(hex.EncodeToString(raw)))
	}
	return value, next, nil
}

func readJSONDataValue(col columnDef, row []byte, pos int, decoder textDecoder, lobReader *dmLOBReader) (any, int, error) {
	raw, next, err := readShortDataBytes(row, pos)
	if err != nil {
		return nil, pos, fmt.Errorf("%s: %w", col.Name, err)
	}
	var value any = dmBinary(raw)
	if payload, ok := unwrapInlineLOBPayload(raw); ok {
		value = dmBinary(payload)
	} else if locator, ok := parseDMLOBLocatorRaw(raw); ok {
		if lobReader == nil {
			return nil, pos, fmt.Errorf("%s: out-of-line JSON locator cannot be resolved without data files", col.Name)
		}
		lazy, lazyErr := lobReader.lazyLOBValue(locator, dmPageKindLOBData, false, decoder)
		if lazyErr != nil {
			return nil, pos, fmt.Errorf("%s: %w", col.Name, lazyErr)
		}
		value = lazy
	}
	return dmJSONValue{value: value, binary: normalizeDataType(col.DataType) == "JSONB", decoder: decoder}, next, nil
}

func readInlineTextLOB(row []byte, pos int, decoder textDecoder, lobReader *dmLOBReader) (any, int, error) {
	raw, next, marker, err := readShortDataBytesWithMarker(row, pos)
	if err != nil {
		return nil, pos, fmt.Errorf("%s", strings.Replace(err.Error(), "raw value", "text LOB", 1))
	}
	if payload, ok := unwrapInlineLOBPayload(raw); ok {
		raw = payload
	} else if locator, ok := parseDMLOBLocatorRaw(raw); ok {
		if lobReader == nil {
			return nil, pos, fmt.Errorf("out-of-line text LOB locator cannot be resolved without data files")
		}
		if value, lazyErr := lobReader.lazyLOBValue(locator, dmPageKindLOBData, true, decoder); lazyErr == nil {
			return value, next, nil
		}
		raw, err = lobReader.readLongRowPayload(locator)
		if err != nil {
			return nil, pos, err
		}
	}
	value, ok := decoder.decode(raw)
	if !ok {
		return nil, pos, fmt.Errorf("cannot decode text LOB marker=0x%02X pos=%d raw=%s", marker, pos, strings.ToUpper(hex.EncodeToString(raw)))
	}
	if strings.ContainsRune(value, '\uFFFD') || containsBadControl(value) {
		return nil, pos, fmt.Errorf("decoded text LOB contains invalid characters marker=0x%02X pos=%d raw=%s", marker, pos, strings.ToUpper(hex.EncodeToString(raw)))
	}
	return value, next, nil
}

func decodeLOBTextValue(column string, raw []byte, decoder textDecoder) (string, error) {
	value, ok := decoder.decode(raw)
	if !ok || strings.ContainsRune(value, '\uFFFD') || containsBadControl(value) {
		return "", fmt.Errorf("%s: cannot decode out-of-line text LOB", column)
	}
	return value, nil
}

func readShortDataVarchar(row []byte, pos int, decoder textDecoder) (string, int, error) {
	raw, next, marker, err := readShortDataBytesWithMarker(row, pos)
	if err != nil {
		return "", pos, fmt.Errorf("%s", strings.Replace(err.Error(), "raw value", "varchar", 1))
	}
	value, ok := decoder.decode(raw)
	if !ok {
		return "", pos, fmt.Errorf("cannot decode varchar marker=0x%02X pos=%d raw=%s", marker, pos, strings.ToUpper(hex.EncodeToString(raw)))
	}
	if strings.ContainsRune(value, '\uFFFD') || containsBadControl(value) {
		return "", pos, fmt.Errorf("decoded varchar contains invalid characters marker=0x%02X pos=%d raw=%s", marker, pos, strings.ToUpper(hex.EncodeToString(raw)))
	}
	return value, next, nil
}

func readShortDataBytes(row []byte, pos int) ([]byte, int, error) {
	raw, next, _, err := readShortDataBytesWithMarker(row, pos)
	return raw, next, err
}

func readShortDataBytesWithMarker(row []byte, pos int) ([]byte, int, byte, error) {
	if pos >= len(row) {
		return nil, pos, 0, fmt.Errorf("raw value marker out of range")
	}
	marker := row[pos]
	if marker == 0x80 {
		return []byte{}, pos + 1, marker, nil
	}
	if marker < 0x80 {
		if pos+2 > len(row) {
			return nil, pos, marker, fmt.Errorf("raw value extended length out of range")
		}
		n := int(binary.BigEndian.Uint16(row[pos:]))
		if n <= 0 {
			return nil, pos, marker, fmt.Errorf("unsupported raw value marker 0x%02X at %d", marker, pos)
		}
		start := pos + 2
		end := start + n
		if end > len(row) {
			return nil, pos, marker, fmt.Errorf("raw value content out of range")
		}
		return append([]byte(nil), row[start:end]...), end, marker, nil
	}
	if marker < 0x81 || marker > 0xFE {
		return nil, pos, marker, fmt.Errorf("unsupported raw value marker 0x%02X at %d", marker, pos)
	}
	n := int(marker - 0x80)
	start := pos + 1
	end := start + n
	if end > len(row) {
		return nil, pos, marker, fmt.Errorf("raw value content out of range")
	}
	return append([]byte(nil), row[start:end]...), end, marker, nil
}

func unwrapInlineLOBPayload(raw []byte) ([]byte, bool) {
	if len(raw) < 13 {
		return nil, false
	}
	if raw[0] != 0x01 || raw[2] != 0x04 {
		return nil, false
	}
	payloadLen := int(binary.LittleEndian.Uint32(raw[9:13]))
	if payloadLen < 0 || payloadLen != len(raw)-13 {
		return nil, false
	}
	return append([]byte(nil), raw[13:]...), true
}

func readDMLOBLocator(row []byte, pos int) (dmLOBLocator, int, error) {
	if pos < 0 || pos+dmLOBLocatorSize > len(row) {
		return dmLOBLocator{}, pos, fmt.Errorf("LOB locator out of range")
	}
	raw := append([]byte(nil), row[pos:pos+dmLOBLocatorSize]...)
	locator, ok := parseDMLOBLocatorRaw(raw)
	if !ok {
		return dmLOBLocator{}, pos, fmt.Errorf("invalid LOB locator %s", strings.ToUpper(hex.EncodeToString(raw)))
	}
	return locator, pos + dmLOBLocatorSize, nil
}

func parseDMLOBLocatorRaw(raw []byte) (dmLOBLocator, bool) {
	if len(raw) != dmLOBLocatorSize || raw[0] != 0x02 {
		return dmLOBLocator{}, false
	}
	locator := dmLOBLocator{
		raw:       append([]byte(nil), raw...),
		lobID:     binary.LittleEndian.Uint32(raw[1:5]),
		byteLen:   binary.LittleEndian.Uint32(raw[9:13]),
		groupID:   binary.LittleEndian.Uint32(raw[13:17]),
		firstPage: binary.LittleEndian.Uint32(raw[17:21]),
	}
	if locator.lobID == 0 || locator.groupID == 0 || locator.firstPage == 0 {
		return dmLOBLocator{}, false
	}
	return locator, true
}

func (r *dmLOBReader) lazyLOBValue(locator dmLOBLocator, kind uint32, text bool, decoder textDecoder) (dmLOBValue, error) {
	if r == nil || r.cache == nil {
		return dmLOBValue{}, fmt.Errorf("LOB reader is not available")
	}
	if _, ok := r.findFirstLOBPage(locator, kind); !ok {
		return dmLOBValue{}, fmt.Errorf("LOB page not found: lob_id=%d group=%d page=%d kind=0x%X", locator.lobID, locator.groupID, locator.firstPage, kind)
	}
	return dmLOBValue{reader: r, locator: locator, kind: kind, text: text, decoder: decoder}, nil
}

func (v dmLOBValue) open() (io.Reader, error) {
	if v.reader == nil {
		return nil, fmt.Errorf("LOB reader is not available")
	}
	return v.reader.openLOBPayload(v.locator, v.kind)
}

func (v dmLOBValue) readAll() ([]byte, error) {
	reader, err := v.open()
	if err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	return raw, nil
}

func (r *dmLOBReader) openLOBPayload(locator dmLOBLocator, kind uint32) (io.Reader, error) {
	if r == nil || r.cache == nil {
		return nil, fmt.Errorf("LOB reader is not available")
	}
	start, ok := r.findFirstLOBPage(locator, kind)
	if !ok {
		return nil, fmt.Errorf("LOB page not found: lob_id=%d group=%d page=%d kind=0x%X", locator.lobID, locator.groupID, locator.firstPage, kind)
	}
	maxSteps := r.cache.totalPageCount() * maxLeafChainWalkMultiplier
	if maxSteps <= 0 {
		maxSteps = 1
	}
	return &dmLOBChainReader{
		owner: r, locator: locator, kind: kind, current: start, hasCurrent: true,
		remaining: uint64(locator.byteLen), seen: make(map[dataPageRef]bool), maxSteps: maxSteps,
	}, nil
}

func (r *dmLOBChainReader) Read(dst []byte) (int, error) {
	if len(dst) == 0 {
		return 0, nil
	}
	if r.terminalErr != nil {
		return 0, r.terminalErr
	}
	if r.remaining == 0 {
		return 0, io.EOF
	}
	written := 0
	for written < len(dst) && r.remaining > 0 {
		if r.payloadPos >= len(r.payload) {
			if err := r.loadNextPayload(); err != nil {
				if written > 0 {
					r.terminalErr = err
					return written, nil
				}
				return 0, err
			}
		}
		available := len(r.payload) - r.payloadPos
		if available <= 0 {
			continue
		}
		length := len(dst) - written
		if length > available {
			length = available
		}
		if uint64(length) > r.remaining {
			length = int(r.remaining)
		}
		copy(dst[written:written+length], r.payload[r.payloadPos:r.payloadPos+length])
		r.payloadPos += length
		r.remaining -= uint64(length)
		written += length
	}
	if written > 0 {
		return written, nil
	}
	return 0, io.EOF
}

func (r *dmLOBChainReader) loadNextPayload() error {
	if !r.hasCurrent || r.steps >= r.maxSteps {
		return fmt.Errorf("LOB payload incomplete: remaining=%d want=%d", r.remaining, r.locator.byteLen)
	}
	if r.seen[r.current] {
		return fmt.Errorf("LOB page chain cycle at group=%d file=%d page=%d", r.current.key.groupID, r.current.key.fileID, r.current.pageNo)
	}
	r.seen[r.current] = true
	r.steps++
	page, ok := r.owner.cache.readPage(r.current)
	if !ok || !pageHeaderMatchesRef(page, r.current) || dataPageKind(page) != r.kind || lobPageID(page) != r.locator.lobID {
		return fmt.Errorf("invalid LOB page at group=%d file=%d page=%d", r.current.key.groupID, r.current.key.fileID, r.current.pageNo)
	}
	payloadLen := int(lobPagePayloadLen(page))
	if payloadLen < 0 || dmLOBPagePayloadOff+payloadLen > len(page) {
		return fmt.Errorf("bad LOB payload length %d at page %d", payloadLen, r.current.pageNo)
	}
	if uint64(payloadLen) > r.remaining {
		payloadLen = int(r.remaining)
	}
	r.payload = page[dmLOBPagePayloadOff : dmLOBPagePayloadOff+payloadLen]
	r.payloadPos = 0
	nextFileID, nextPageNo, hasNext := readDMPageRef(page, dmPageNextRefOff)
	if hasNext {
		r.current = dataPageRef{
			key: dataFileKey{groupID: r.locator.groupID, fileID: nextFileID}, pageNo: nextPageNo,
		}
	} else {
		r.hasCurrent = false
	}
	if payloadLen == 0 && !r.hasCurrent && r.remaining > 0 {
		return fmt.Errorf("LOB payload incomplete: remaining=%d want=%d", r.remaining, r.locator.byteLen)
	}
	return nil
}

func (r *dmLOBReader) readLOBPayload(locator dmLOBLocator, kind uint32) ([]byte, error) {
	reader, err := r.openLOBPayload(locator, kind)
	if err != nil {
		return nil, err
	}
	out, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	if uint64(len(out)) != uint64(locator.byteLen) {
		return nil, fmt.Errorf("LOB payload incomplete: got=%d want=%d", len(out), locator.byteLen)
	}
	return out, nil
}

func (r *dmLOBReader) readTextLOBOrLongRowPayload(locator dmLOBLocator) ([]byte, error) {
	payload, err := r.readLOBPayload(locator, dmPageKindLOBData)
	if err == nil {
		return payload, nil
	}
	longPayload, longErr := r.readLongRowPayload(locator)
	if longErr == nil {
		return longPayload, nil
	}
	return nil, err
}

func (r *dmLOBReader) readLongRowPayload(locator dmLOBLocator) ([]byte, error) {
	if r == nil || r.cache == nil {
		return nil, fmt.Errorf("LOB reader is not available")
	}
	start, ok := r.findFirstLOBPage(locator, dmPageKindLongRowData)
	if !ok {
		return nil, fmt.Errorf("long-row page not found: lob_id=%d group=%d page=%d", locator.lobID, locator.groupID, locator.firstPage)
	}
	current := start
	seen := make(map[dataPageRef]bool)
	maxSteps := r.cache.totalPageCount() * maxLeafChainWalkMultiplier
	if maxSteps <= 0 {
		maxSteps = 1
	}
	for steps := 0; steps < maxSteps; steps++ {
		if seen[current] {
			break
		}
		seen[current] = true
		page, ok := r.cache.readPage(current)
		if !ok || !pageHeaderMatchesRef(page, current) || dataPageKind(page) != dmPageKindLongRowData {
			break
		}
		if payload, ok := longRowPayloadFromPage(page, locator); ok {
			return payload, nil
		}
		nextFileID, nextPageNo, ok := readDMPageRef(page, dmPageNextRefOff)
		if !ok {
			break
		}
		current = dataPageRef{
			key: dataFileKey{
				groupID: locator.groupID,
				fileID:  nextFileID,
			},
			pageNo: nextPageNo,
		}
	}
	return nil, fmt.Errorf("long-row payload not found: lob_id=%d", locator.lobID)
}

func (r *dmLOBReader) findFirstLOBPage(locator dmLOBLocator, kind uint32) (dataPageRef, bool) {
	if r == nil || r.cache == nil {
		return dataPageRef{}, false
	}
	for key := range r.cache.refs {
		if key.groupID != locator.groupID {
			continue
		}
		ref := dataPageRef{key: key, pageNo: locator.firstPage}
		page, ok := r.cache.readPage(ref)
		if !ok || !pageHeaderMatchesRef(page, ref) || dataPageKind(page) != kind {
			continue
		}
		if kind == dmPageKindLOBData && lobPageID(page) != locator.lobID {
			continue
		}
		if kind == dmPageKindLongRowData {
			if _, ok := longRowPayloadFromPage(page, locator); !ok {
				continue
			}
		}
		return ref, true
	}
	return dataPageRef{}, false
}

func lobPageID(page []byte) uint32 {
	if len(page) < dmLOBPageIDOff+4 {
		return 0
	}
	return binary.LittleEndian.Uint32(page[dmLOBPageIDOff:])
}

func lobPagePayloadLen(page []byte) uint16 {
	if len(page) < dmLOBPagePayloadLenOff+2 {
		return 0
	}
	return binary.LittleEndian.Uint16(page[dmLOBPagePayloadLenOff:])
}

func longRowPayloadFromPage(page []byte, locator dmLOBLocator) ([]byte, bool) {
	for off := dmLOBPagePayloadOff; off+0x0E <= len(page); off++ {
		recordLen := int(binary.BigEndian.Uint16(page[off:]))
		if recordLen < 0x0E || off+recordLen > len(page) {
			continue
		}
		if binary.LittleEndian.Uint32(page[off+0x02:off+0x06]) != locator.lobID {
			continue
		}
		payloadLen1 := int(binary.LittleEndian.Uint16(page[off+0x0A:]))
		payloadLen2 := int(binary.LittleEndian.Uint16(page[off+0x0C:]))
		payloadLen := payloadLen1
		if payloadLen2 > 0 && payloadLen2 < payloadLen {
			payloadLen = payloadLen2
		}
		if payloadLen <= 0 || payloadLen > int(locator.byteLen) || off+0x0E+payloadLen > off+recordLen {
			continue
		}
		return append([]byte(nil), page[off+0x0E:off+0x0E+payloadLen]...), true
	}
	return nil, false
}

func readDMNumber(row []byte, pos int) (any, int, error) {
	if pos >= len(row) {
		return nil, pos, fmt.Errorf("NUMBER marker out of range")
	}
	marker := row[pos]
	if marker == 0x80 {
		return nil, pos + 1, nil
	}
	if marker < 0x81 || marker > 0xFE {
		return nil, pos, fmt.Errorf("unsupported NUMBER marker 0x%02X at %d", marker, pos)
	}
	n := int(marker - 0x80)
	start := pos + 1
	end := start + n
	if end > len(row) {
		return nil, pos, fmt.Errorf("NUMBER content out of range")
	}
	value, ok := decodeDMNumber(row[start:end])
	if !ok {
		return nil, pos, fmt.Errorf("cannot decode NUMBER")
	}
	return dmNumber(value), end, nil
}

func decodeDMNumber(raw []byte) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	if len(raw) == 1 && raw[0] == 0x80 {
		return "0", true
	}
	if raw[0] >= 0x80 {
		exp := int(raw[0]) - 0xC1
		digits := make([]int, 0, len(raw)-1)
		for _, b := range raw[1:] {
			digit := int(b) - 1
			if digit < 0 || digit > 99 {
				return "", false
			}
			digits = append(digits, digit)
		}
		return formatBase100Number(false, exp, digits), true
	}

	exp := 0x3E - int(raw[0])
	digits := make([]int, 0, len(raw)-1)
	for _, b := range raw[1:] {
		if b == 0x66 {
			break
		}
		digit := 101 - int(b)
		if digit < 0 || digit > 99 {
			return "", false
		}
		digits = append(digits, digit)
	}
	return formatBase100Number(true, exp, digits), true
}

func decodeDMDate(raw []byte) (string, error) {
	if len(raw) != 3 {
		return "", fmt.Errorf("date needs 3 bytes")
	}
	v := uint32(raw[0]) | uint32(raw[1])<<8 | uint32(raw[2])<<16
	year := int(v & ((1 << 15) - 1))
	month := int((v >> 15) & 0xF)
	day := int((v >> 19) & 0x1F)
	if year < 1 || year > 9999 || month < 1 || month > 12 || day < 1 || day > daysInMonth(year, month) {
		return "", fmt.Errorf("invalid date bits: %04d-%02d-%02d", year, month, day)
	}
	return fmt.Sprintf("%04d-%02d-%02d", year, month, day), nil
}

func formatBase100Number(negative bool, exp int, digits []int) string {
	if len(digits) == 0 {
		return "0"
	}
	beforeGroups := exp + 1
	var out strings.Builder
	if negative {
		out.WriteByte('-')
	}
	switch {
	case beforeGroups <= 0:
		out.WriteString("0.")
		for i := 0; i < -beforeGroups; i++ {
			out.WriteString("00")
		}
		for _, digit := range digits {
			out.WriteString(fmt.Sprintf("%02d", digit))
		}
	case beforeGroups >= len(digits):
		out.WriteString(fmt.Sprintf("%d", digits[0]))
		for _, digit := range digits[1:] {
			out.WriteString(fmt.Sprintf("%02d", digit))
		}
		for i := len(digits); i < beforeGroups; i++ {
			out.WriteString("00")
		}
	default:
		out.WriteString(fmt.Sprintf("%d", digits[0]))
		for i := 1; i < beforeGroups; i++ {
			out.WriteString(fmt.Sprintf("%02d", digits[i]))
		}
		out.WriteByte('.')
		for i := beforeGroups; i < len(digits); i++ {
			out.WriteString(fmt.Sprintf("%02d", digits[i]))
		}
	}
	value := out.String()
	if strings.Contains(value, ".") {
		value = strings.TrimRight(value, "0")
		value = strings.TrimRight(value, ".")
	}
	if value == "" || value == "-" {
		return "0"
	}
	return value
}

func decodeDMDateTime(raw []byte) (string, error) {
	precision := 6
	if len(raw) == 9 {
		precision = 9
	}
	return decodeDMDateTimeWithPrecision(raw, precision)
}

func decodeDMDateTimeWithPrecision(raw []byte, precision int) (string, error) {
	if len(raw) != 8 && len(raw) != 9 {
		return "", fmt.Errorf("datetime needs 8 or 9 bytes")
	}
	date, err := decodeDMDate(raw[:3])
	if err != nil {
		return "", fmt.Errorf("%s", strings.Replace(err.Error(), "date", "datetime date", 1))
	}
	timeValue, err := decodeDMTimeWithPrecision(raw[3:], precision)
	if err != nil {
		return "", fmt.Errorf("%s", strings.Replace(err.Error(), "time", "datetime time", 1))
	}
	return date + " " + timeValue, nil
}

func decodeDMTime(raw []byte) (string, error) {
	precision := 6
	if len(raw) == 6 {
		precision = 9
	}
	return decodeDMTimeWithPrecision(raw, precision)
}

func decodeDMTimeWithPrecision(raw []byte, precision int) (string, error) {
	if len(raw) != 5 && len(raw) != 6 {
		return "", fmt.Errorf("time needs 5 or 6 bytes")
	}
	v := uint64(raw[0]) | uint64(raw[1])<<8 | uint64(raw[2])<<16 | uint64(raw[3])<<24 | uint64(raw[4])<<32
	maxPrecision := 6
	fractionMask := uint64((1 << 23) - 1)
	if len(raw) == 6 {
		v |= uint64(raw[5]) << 40
		maxPrecision = 9
		fractionMask = (1 << 31) - 1
	}
	hour := int(v & 0x1F)
	minute := int((v >> 5) & 0x3F)
	second := int((v >> 11) & 0x3F)
	fraction := int((v >> 17) & fractionMask)
	maxFraction := 999999
	if maxPrecision == 9 {
		maxFraction = 999999999
	}
	if hour > 23 || minute > 59 || second > 59 || fraction > maxFraction {
		return "", fmt.Errorf("invalid datetime time bits: %02d:%02d:%02d fraction=%d", hour, minute, second, fraction)
	}
	if precision <= 0 || precision > maxPrecision {
		precision = maxPrecision
	}
	fractionText := fmt.Sprintf("%0*d", maxPrecision, fraction)
	return fmt.Sprintf("%02d:%02d:%02d.%s", hour, minute, second, fractionText[:precision]), nil
}

func decodeDMTimezone(raw []byte) string {
	if len(raw) != 2 {
		return "+00:00"
	}
	minutes := int(int16(binary.LittleEndian.Uint16(raw)))
	sign := "+"
	if minutes < 0 {
		sign = "-"
		minutes = -minutes
	}
	return fmt.Sprintf("%s%02d:%02d", sign, minutes/60, minutes%60)
}

func decodeDMIntervalDayToSecond(raw []byte, dataType string) string {
	if len(raw) < 24 {
		return ""
	}
	values := []int64{
		int64(int32(binary.LittleEndian.Uint32(raw[0:4]))),
		int64(int32(binary.LittleEndian.Uint32(raw[4:8]))),
		int64(int32(binary.LittleEndian.Uint32(raw[8:12]))),
		int64(int32(binary.LittleEndian.Uint32(raw[12:16]))),
		int64(int32(binary.LittleEndian.Uint32(raw[16:20]))),
	}
	negative := false
	for i := range values {
		if values[i] < 0 {
			negative = true
			values[i] = -values[i]
		}
	}
	sign := ""
	if negative {
		sign = "-"
	}
	_, fractional := intervalPrecisions(int16(binary.LittleEndian.Uint16(raw[20:22])))
	if fractional < 0 || fractional > 6 {
		fractional = 6
	}
	fractionText := fmt.Sprintf("%06d", values[4])
	if fractional < len(fractionText) {
		fractionText = fractionText[:fractional]
	}
	seconds := fmt.Sprintf("%02d", values[3])
	if fractional > 0 {
		seconds += "." + fractionText
	}
	switch normalizeDataType(dataType) {
	case "INTERVAL DAY":
		return fmt.Sprintf("%s%d", sign, values[0])
	case "INTERVAL HOUR":
		return fmt.Sprintf("%s%d", sign, values[1])
	case "INTERVAL MINUTE":
		return fmt.Sprintf("%s%d", sign, values[2])
	case "INTERVAL SECOND":
		seconds = fmt.Sprintf("%d", values[3])
		if fractional > 0 {
			seconds += "." + fractionText
		}
		return sign + seconds
	case "INTERVAL DAY TO HOUR":
		return fmt.Sprintf("%s%d %02d", sign, values[0], values[1])
	case "INTERVAL DAY TO MINUTE":
		return fmt.Sprintf("%s%d %02d:%02d", sign, values[0], values[1], values[2])
	case "INTERVAL DAY TO SECOND":
		return fmt.Sprintf("%s%d %02d:%02d:%s", sign, values[0], values[1], values[2], seconds)
	case "INTERVAL HOUR TO MINUTE":
		return fmt.Sprintf("%s%d:%02d", sign, values[1], values[2])
	case "INTERVAL HOUR TO SECOND":
		return fmt.Sprintf("%s%d:%02d:%s", sign, values[1], values[2], seconds)
	case "INTERVAL MINUTE TO SECOND":
		return fmt.Sprintf("%s%d:%s", sign, values[2], seconds)
	default:
		return ""
	}
}

func decodeDMIntervalYearToMonth(raw []byte, dataType string) string {
	if len(raw) < 12 {
		return ""
	}
	year := int64(int32(binary.LittleEndian.Uint32(raw[0:4])))
	month := int64(int32(binary.LittleEndian.Uint32(raw[4:8])))
	negative := year < 0 || month < 0
	if year < 0 {
		year = -year
	}
	if month < 0 {
		month = -month
	}
	sign := ""
	if negative {
		sign = "-"
	}
	switch normalizeDataType(dataType) {
	case "INTERVAL YEAR":
		return fmt.Sprintf("%s%d", sign, year)
	case "INTERVAL MONTH":
		return fmt.Sprintf("%s%d", sign, month)
	default:
		return fmt.Sprintf("%s%d-%02d", sign, year, month)
	}
}

func decodeDMRowID(raw []byte) string {
	if len(raw) != 12 {
		return ""
	}
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	appendFixed := func(out []byte, value uint64, width int) []byte {
		start := len(out)
		out = append(out, make([]byte, width)...)
		for i := width - 1; i >= 0; i-- {
			out[start+i] = alphabet[value&0x3F]
			value >>= 6
		}
		return out
	}
	epno := uint64(binary.BigEndian.Uint16(raw[0:2]))
	partno := uint64(binary.BigEndian.Uint16(raw[4:6]))
	realRowID := uint64(0)
	for _, value := range raw[6:12] {
		realRowID = realRowID<<8 | uint64(value)
	}
	out := make([]byte, 0, 18)
	out = appendFixed(out, epno, 4)
	out = appendFixed(out, partno, 6)
	out = appendFixed(out, realRowID, 8)
	return string(out)
}

type dmJSONBDescriptor struct {
	kind    byte
	length  int
	special bool
}

func decodeDMJSONB(raw []byte, decoder textDecoder) (dmJSON, error) {
	if len(raw) < 20 {
		return "", fmt.Errorf("JSONB payload is too short: %d", len(raw))
	}
	value, err := decodeDMJSONBContainer(raw[16:], decoder)
	if err != nil {
		return "", err
	}
	if !json.Valid([]byte(value)) {
		return "", fmt.Errorf("decoded JSONB value is not valid JSON")
	}
	return dmJSON(value), nil
}

func decodeDMJSONBContainer(raw []byte, decoder textDecoder) (string, error) {
	if len(raw) < 4 {
		return "", fmt.Errorf("JSONB container header is truncated")
	}
	header := binary.LittleEndian.Uint32(raw[:4])
	kind := byte(header >> 28)
	count := int(header & 0x0FFFFFFF)
	if count < 0 || count > 1_000_000 {
		return "", fmt.Errorf("invalid JSONB item count %d", count)
	}
	descriptorCount := 0
	switch kind {
	case 1:
		descriptorCount = count
	case 2:
		descriptorCount = count * 2
	case 4:
		descriptorCount = count
	default:
		return "", fmt.Errorf("unsupported JSONB container kind 0x%X", kind)
	}
	descriptorEnd := 4 + descriptorCount*4
	if descriptorEnd > len(raw) {
		return "", fmt.Errorf("JSONB descriptor array is truncated")
	}
	if descriptorCount == 0 {
		if kind == 2 {
			return "{}", nil
		}
		if kind == 4 {
			return "[]", nil
		}
		return "null", nil
	}

	descriptors := make([]dmJSONBDescriptor, descriptorCount)
	knownPayload := 0
	specialIndex := -1
	for i := 0; i < descriptorCount; i++ {
		desc := binary.LittleEndian.Uint32(raw[4+i*4:])
		descKind := byte(desc >> 28)
		special := descKind >= 8
		if special {
			descKind -= 8
			if specialIndex >= 0 {
				return "", fmt.Errorf("multiple JSONB special descriptors")
			}
			specialIndex = i
		}
		length := int(desc & 0x0FFFFFFF)
		if descKind >= 4 && descKind <= 6 {
			length = 0
		}
		descriptors[i] = dmJSONBDescriptor{kind: descKind, length: length, special: special}
		if !special {
			knownPayload += length
		}
	}
	payloadLength := len(raw) - descriptorEnd
	if specialIndex >= 0 {
		length := payloadLength - knownPayload
		if length < 0 {
			return "", fmt.Errorf("invalid JSONB special descriptor length")
		}
		descriptors[specialIndex].length = length
	} else if knownPayload != payloadLength {
		return "", fmt.Errorf("JSONB payload length mismatch: descriptors=%d payload=%d", knownPayload, payloadLength)
	}

	values := make([]string, descriptorCount)
	pos := descriptorEnd
	for i, desc := range descriptors {
		if desc.length < 0 || pos+desc.length > len(raw) {
			return "", fmt.Errorf("JSONB item %d is out of range", i)
		}
		text, err := decodeDMJSONBItem(desc.kind, raw[pos:pos+desc.length], decoder)
		if err != nil {
			return "", fmt.Errorf("JSONB item %d: %w", i, err)
		}
		values[i] = text
		pos += desc.length
	}
	if pos != len(raw) {
		return "", fmt.Errorf("JSONB payload has %d trailing bytes", len(raw)-pos)
	}

	switch kind {
	case 1:
		if len(values) != 1 {
			return "", fmt.Errorf("JSONB scalar wrapper contains %d values", len(values))
		}
		return values[0], nil
	case 2:
		pairs := make([]string, 0, count)
		for i := 0; i < len(values); i += 2 {
			pairs = append(pairs, values[i]+":"+values[i+1])
		}
		return "{" + strings.Join(pairs, ",") + "}", nil
	case 4:
		return "[" + strings.Join(values, ",") + "]", nil
	default:
		return "", fmt.Errorf("unsupported JSONB container kind 0x%X", kind)
	}
}

func decodeDMJSONBItem(kind byte, raw []byte, decoder textDecoder) (string, error) {
	switch kind {
	case 0:
		return decodeDMJSONBContainer(raw, decoder)
	case 1:
		var value int64
		switch len(raw) {
		case 1:
			value = int64(int8(raw[0]))
		case 2:
			value = int64(int16(binary.LittleEndian.Uint16(raw)))
		case 4:
			value = int64(int32(binary.LittleEndian.Uint32(raw)))
		case 8:
			value = int64(binary.LittleEndian.Uint64(raw))
		default:
			return "", fmt.Errorf("unsupported integer width %d", len(raw))
		}
		return fmt.Sprintf("%d", value), nil
	case 2:
		value := string(raw)
		if !json.Valid([]byte(value)) {
			return "", fmt.Errorf("invalid numeric text %q", value)
		}
		return value, nil
	case 3:
		value, ok := decoder.decode(raw)
		if !ok || strings.ContainsRune(value, '\uFFFD') || containsBadControl(value) {
			return "", fmt.Errorf("cannot decode JSONB string")
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return "", err
		}
		return string(encoded), nil
	case 4:
		return "false", nil
	case 5:
		return "true", nil
	case 6:
		return "null", nil
	default:
		return "", fmt.Errorf("unsupported JSONB item kind 0x%X", kind)
	}
}

func sqlValueForDataColumn(col columnDef, value any) (string, error) {
	var err error
	value, err = materializeDataValue(value)
	if err != nil {
		return "", fmt.Errorf("%s: %w", col.Name, err)
	}
	if value == nil {
		return "NULL", nil
	}
	typ := normalizeDataType(col.DataType)
	if isYearMonthIntervalDataType(typ) || isDayTimeIntervalDataType(typ) {
		text := fmt.Sprintf("%v", value)
		sign := ""
		if strings.HasPrefix(text, "-") {
			sign = "-"
			text = strings.TrimPrefix(text, "-")
		}
		return "INTERVAL " + sign + sqlLiteral(text) + " " + intervalQualifier(typ), nil
	}
	switch typ {
	case "BIT", "BOOL", "BOOLEAN", "BYTE", "TINYINT", "SMALLINT", "INT", "INTEGER", "PLS_INTEGER", "BIGINT":
		return fmt.Sprintf("%v", value), nil
	case "REAL", "BINARY_FLOAT", "FLOAT", "DOUBLE", "DOUBLE PRECISION", "BINARY_DOUBLE":
		return fmt.Sprintf("%v", value), nil
	case "NUMBER", "NUMERIC", "DEC", "DECIMAL":
		return fmt.Sprintf("%v", value), nil
	case "DATETIME", "TIMESTAMP", "DATETIME WITH TIME ZONE", "TIMESTAMP WITH TIME ZONE", "TIMESTAMP WITH LOCAL TIME ZONE":
		prefix := "DATETIME "
		if strings.HasPrefix(typ, "TIMESTAMP") {
			prefix = "TIMESTAMP "
		}
		return prefix + sqlLiteral(fmt.Sprintf("%v", value)), nil
	case "DATE":
		text := fmt.Sprintf("%v", value)
		if len(text) >= 10 {
			text = text[:10]
		}
		return "DATE " + sqlLiteral(text), nil
	case "TIME", "TIME WITH TIME ZONE":
		return "TIME " + sqlLiteral(fmt.Sprintf("%v", value)), nil
	case "JSON", "JSONB":
		return "CAST(" + sqlLiteral(fmt.Sprintf("%v", value)) + " AS " + typ + ")", nil
	case "BFILE":
		directory, filename, ok := strings.Cut(fmt.Sprintf("%v", value), ":")
		if !ok {
			return "", fmt.Errorf("invalid BFILE value %q", value)
		}
		return "BFILENAME(" + sqlLiteral(directory) + "," + sqlLiteral(filename) + ")", nil
	case "ROWID":
		return sqlLiteral(fmt.Sprintf("%v", value)), nil
	default:
		if raw, ok := value.(dmBinary); ok {
			return "HEXTORAW('" + strings.ToUpper(hex.EncodeToString(raw)) + "')", nil
		}
		text := fmt.Sprintf("%v", value)
		if strings.ContainsRune(text, '\uFFFD') || containsBadControl(text) {
			return "", fmt.Errorf("invalid text value for %s", col.Name)
		}
		return sqlLiteral(text), nil
	}
}

func materializeDataValue(value any) (any, error) {
	if jsonValue, ok := value.(dmJSONValue); ok {
		materialized, err := materializeDataValue(jsonValue.value)
		if err != nil {
			return nil, err
		}
		var raw []byte
		switch value := materialized.(type) {
		case dmBinary:
			raw = []byte(value)
		case []byte:
			raw = value
		case string:
			raw = []byte(value)
		default:
			return nil, fmt.Errorf("unsupported JSON storage value %T", materialized)
		}
		if jsonValue.binary {
			return decodeDMJSONB(raw, jsonValue.decoder)
		}
		text, ok := jsonValue.decoder.decode(raw)
		if !ok || strings.ContainsRune(text, '\uFFFD') || containsBadControl(text) {
			return nil, fmt.Errorf("cannot decode JSON text")
		}
		return dmJSON(text), nil
	}
	lob, ok := value.(dmLOBValue)
	if !ok {
		return value, nil
	}
	raw, err := lob.readAll()
	if err != nil {
		return nil, err
	}
	if !lob.text {
		return dmBinary(raw), nil
	}
	text, ok := lob.decoder.decode(raw)
	if !ok || strings.ContainsRune(text, '\uFFFD') || containsBadControl(text) {
		return nil, fmt.Errorf("cannot decode out-of-line text LOB")
	}
	return text, nil
}
