package dm

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const (
	hugeHFSFileHeaderSize    = int64(4096)
	hugeHFSSectionHeaderSize = int64(128)
	maxHugeDeltaUpdates      = 2_000_000
	maxHugeDeltaUpdateBytes  = int64(256 * 1024 * 1024)
	maxHugeOffsetTableBytes  = uint64(256 * 1024 * 1024)
	hugeColumnReadBufferSize = 64 * 1024
)

var hugeHFSSectionMagic = []byte{0x09, 0xEC, 0x1D, 0x02}

type hugeDataExportContext struct {
	dataDir        string
	tables         map[uint32]dictionaryObject
	columnsByTable map[uint32][]columnDef
	storageByTable map[uint32]indexDef
	dataFiles      []dataFileRef
	pageCache      *dataFilePageCache
	pageSize       uint32
	decoder        textDecoder
	outputFormat   string
	dmpCharset     dmpCharsetHeader
	maxRows        int
}

type hugeDataExportStats struct {
	sectionsRead int
	filesRead    int
}

type hugeAuxRow struct {
	values       map[uint16]dataValue
	clusterRowID uint64
	page         dataPageRef
	slot         uint16
}

type hugeColumnSection struct {
	colID      uint16
	section    uint32
	fileID     int32
	offset     int64
	count      uint32
	nlen       uint32
	nulls      uint32
	nullsKnown bool
	cprFlag    string
	encFlag    string
}

type hugeDeleteRange struct {
	start uint64
	end   uint64
}

type hugeUpdateKey struct {
	rowID uint64
	colID uint16
}

type hugeUpdateSet map[hugeUpdateKey]dataValue

type hugeColumnSectionReader struct {
	column        columnDef
	meta          hugeColumnSection
	decoder       textDecoder
	file          *os.File
	fixedWidth    int
	fixedReader   *bufio.Reader
	offsets       []uint32
	variable      *bufio.Reader
	variablePos   uint32
	row           uint32
	nextOffsetPos uint32
	presentBits   []byte
}

func selectedHugeAuxTableIDs(tables map[uint32]dictionaryObject, ownerMatcher ownerMatcher, tableMatcher tableNameMatcher, excludeMatcher tableNameMatcher) map[uint32]bool {
	result := make(map[uint32]bool)
	for _, table := range tables {
		if !table.isHugeTable() || !ownerMatcher.allowed(table.Owner) || !tableMatcher.allowed(table.Owner, table.Name) || excludeMatcher.allowed(table.Owner, table.Name) {
			continue
		}
		for _, id := range []uint32{table.HugeAuxID, table.HugeRAuxID, table.HugeDAuxID, table.HugeUAuxID} {
			if id != 0 {
				result[id] = true
			}
		}
	}
	return result
}

func ensureHugeAuxColumnDefinitions(tables map[uint32]dictionaryObject, columnsByTable map[uint32][]columnDef) {
	repairHugeMainColumnsFromRAux(tables, columnsByTable)
	for _, table := range tables {
		if !table.isHugeTable() {
			continue
		}
		if table.HugeAuxID != 0 {
			columnsByTable[table.HugeAuxID] = hugeAuxColumns(table.HugeAuxID)
		}
		if table.HugeRAuxID != 0 {
			columnsByTable[table.HugeRAuxID] = cloneHugeRAuxColumns(table.HugeRAuxID, columnsByTable[table.ID])
		}
		if table.HugeDAuxID != 0 {
			columnsByTable[table.HugeDAuxID] = []columnDef{
				{TableID: table.HugeDAuxID, ColID: 0, Name: "START_ID", DataType: "BIGINT", Length: 8, Nullable: "N"},
				{TableID: table.HugeDAuxID, ColID: 1, Name: "COUNT", DataType: "INT", Length: 4, Nullable: "Y"},
				{TableID: table.HugeDAuxID, ColID: 2, Name: "INFO", DataType: "VARBINARY", Length: 8188, Nullable: "Y"},
			}
		}
		if table.HugeUAuxID != 0 {
			columnsByTable[table.HugeUAuxID] = []columnDef{
				{TableID: table.HugeUAuxID, ColID: 0, Name: "COLID", DataType: "SMALLINT", Length: 2, Nullable: "N"},
				{TableID: table.HugeUAuxID, ColID: 1, Name: "DTA_ROWID", DataType: "BIGINT", Length: 8, Nullable: "N"},
				{TableID: table.HugeUAuxID, ColID: 2, Name: "VALUE", DataType: "VARBINARY", Length: 8188, Nullable: "Y"},
			}
		}
	}
}

func ensureHugeAuxStorageMappings(tables map[uint32]dictionaryObject, indexes map[uint32]indexDef, assistsByTable map[uint32][]indexDef, storageByTable map[uint32]indexDef) {
	for _, table := range tables {
		if !table.isHugeTable() {
			continue
		}
		for _, tableID := range []uint32{table.HugeAuxID, table.HugeRAuxID, table.HugeDAuxID, table.HugeUAuxID} {
			if tableID == 0 {
				continue
			}
			if assists := assistsByTable[tableID]; len(assists) > 0 {
				// ALTER/recreate history can leave older roots attached to the
				// same internal object. The current HUGE auxiliary storage is the
				// newest catalog id; choosing the first (oldest) one replays stale
				// metadata or RAUX rows.
				storageByTable[tableID] = assists[len(assists)-1]
				continue
			}
			if storage, ok := storageByTable[tableID]; ok && storage.ID != 0 {
				continue
			}
			if storage, ok := indexes[tableDataAssistID(tableID)]; ok {
				storageByTable[tableID] = storage
			}
		}
		// DM creates the four transaction auxiliaries together and their
		// SYSINDEXES ids are consecutive in $AUX/$RAUX/$DAUX/$UAUX order.
		// Use the catalog-backed $AUX storage as the anchor when a protected
		// SYSOBJECTS/SYSINDEXES row for one sibling could not be decoded.
		if auxStorage, ok := storageByTable[table.HugeAuxID]; ok && auxStorage.ID != 0 {
			for offset, tableID := range []uint32{table.HugeAuxID, table.HugeRAuxID, table.HugeDAuxID, table.HugeUAuxID} {
				if tableID == 0 || storageByTable[tableID].ID != 0 {
					continue
				}
				storageID := auxStorage.ID + uint32(offset)
				if storage, exists := indexes[storageID]; exists {
					storageByTable[tableID] = storage
					continue
				}
				storageByTable[tableID] = indexDef{
					ID: storageID, GroupID: uint16(table.Info2 & 0xFFFF), RootFile: -1, RootPage: -1, Flag: 1,
				}
			}
		}
	}
}

func repairHugeMainColumnsFromRAux(tables map[uint32]dictionaryObject, columnsByTable map[uint32][]columnDef) {
	for tableID, table := range tables {
		if !table.isHugeTable() || table.HugeRAuxID == 0 {
			continue
		}
		rauxByID := make(map[uint16]columnDef)
		for _, column := range columnsByTable[table.HugeRAuxID] {
			rauxByID[column.ColID] = column
		}
		columns := columnsByTable[tableID]
		for i := range columns {
			if isPlausibleCatalogDataType(columns[i].DataType) {
				continue
			}
			if recovered, ok := rauxByID[columns[i].ColID]; ok && isPlausibleCatalogDataType(recovered.DataType) {
				columns[i].DataType = recovered.DataType
				columns[i].Length = recovered.Length
				columns[i].Scale = recovered.Scale
			}
		}
		columnsByTable[tableID] = columns
	}
}

func isPlausibleCatalogDataType(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	for _, ch := range value {
		if ch > 0x7F || !(ch == ' ' || ch == '_' || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9')) {
			return false
		}
	}
	return true
}

func cloneHugeRAuxColumns(tableID uint32, source []columnDef) []columnDef {
	result := make([]columnDef, len(source))
	for i, column := range source {
		column.TableID = tableID
		column.Location = ddlLocation{}
		result[i] = column
	}
	return result
}

func hugeAuxColumns(tableID uint32) []columnDef {
	return []columnDef{
		{TableID: tableID, ColID: 0, Name: "COLID", DataType: "SMALLINT", Length: 2, Nullable: "N"},
		{TableID: tableID, ColID: 1, Name: "SEC_ID", DataType: "INT", Length: 4, Nullable: "N"},
		{TableID: tableID, ColID: 2, Name: "FILE_ID", DataType: "INT", Length: 4, Nullable: "Y"},
		{TableID: tableID, ColID: 3, Name: "OFFSET", DataType: "BIGINT", Length: 8, Nullable: "Y"},
		{TableID: tableID, ColID: 4, Name: "COUNT", DataType: "INT", Length: 4, Nullable: "Y"},
		{TableID: tableID, ColID: 5, Name: "ACOUNT", DataType: "INT", Length: 4, Nullable: "Y"},
		{TableID: tableID, ColID: 6, Name: "N_LEN", DataType: "INT", Length: 4, Nullable: "Y"},
		{TableID: tableID, ColID: 7, Name: "N_NULL", DataType: "INT", Length: 4, Nullable: "Y"},
		{TableID: tableID, ColID: 8, Name: "N_DIST", DataType: "INT", Length: 4, Nullable: "Y"},
		{TableID: tableID, ColID: 9, Name: "CPR_FLAG", DataType: "CHAR", Length: 1, Nullable: "Y"},
		{TableID: tableID, ColID: 10, Name: "ENC_FLAG", DataType: "CHAR", Length: 1, Nullable: "Y"},
		{TableID: tableID, ColID: 11, Name: "CHKSUM", DataType: "INT", Length: 4, Nullable: "Y"},
		{TableID: tableID, ColID: 12, Name: "MAX_VAL", DataType: "VARBINARY", Length: 8188, Nullable: "Y"},
		{TableID: tableID, ColID: 13, Name: "MIN_VAL", DataType: "VARBINARY", Length: 8188, Nullable: "Y"},
		{TableID: tableID, ColID: 14, Name: "SUM_VAL", DataType: "VARBINARY", Length: 8188, Nullable: "Y"},
	}
}

func exportHugeTableData(ctx hugeDataExportContext, info dataTableInfo, output *dataOutputRouter, rowStats *DataTableRowCount, result *DataExportResult) (hugeDataExportStats, error) {
	stats := hugeDataExportStats{}
	if !info.table.isHugeTable() {
		return stats, nil
	}
	if info.table.HugeAuxID == 0 {
		return stats, fmt.Errorf("HFS section metadata table %s$AUX is missing", info.table.Name)
	}
	if len(ctx.dataFiles) == 0 {
		return stats, fmt.Errorf("ordinary DBF data files are unavailable; HUGE transaction auxiliaries cannot be read")
	}

	sections, err := loadHugeColumnSections(ctx, info.table.HugeAuxID)
	if err != nil {
		return stats, err
	}
	if len(sections) == 0 {
		// An empty HUGE table has no materialized HFS section yet.
		return stats, nil
	}
	hasHFSSection := false
	for _, section := range sections {
		if section.fileID >= 0 && section.count > 0 {
			hasHFSSection = true
			break
		}
	}
	tableDir := ""
	if hasHFSSection {
		tableDir, err = findHugeTableDir(ctx.dataDir, info.table.SchemaID, info.table.ID)
		if err != nil {
			return stats, err
		}
	}

	deletes, err := loadHugeDeleteRanges(ctx, info.table.HugeDAuxID)
	if err != nil {
		return stats, err
	}
	updates, err := loadHugeUpdates(ctx, info, info.table.HugeUAuxID)
	if err != nil {
		return stats, err
	}

	bySection := make(map[uint32]map[uint16]hugeColumnSection)
	sectionIDs := make([]uint32, 0)
	for _, section := range sections {
		if section.fileID < 0 || section.count == 0 {
			continue
		}
		if bySection[section.section] == nil {
			bySection[section.section] = make(map[uint16]hugeColumnSection)
			sectionIDs = append(sectionIDs, section.section)
		}
		bySection[section.section][section.colID] = section
	}
	sort.Slice(sectionIDs, func(i, j int) bool { return sectionIDs[i] < sectionIDs[j] })
	for _, sectionID := range sectionIDs {
		columnSections := bySection[sectionID]
		sectionCount := uint32(0)
		var sectionIndexBytes uint64
		for _, column := range info.columns {
			section, ok := columnSections[column.ColID]
			if !ok {
				return stats, fmt.Errorf("section %d has no metadata for column %s (colid=%d)", sectionID, column.Name, column.ColID)
			}
			if sectionCount == 0 {
				sectionCount = section.count
			} else if section.count != sectionCount {
				return stats, fmt.Errorf("section %d column counts disagree: %d and %d", sectionID, sectionCount, section.count)
			}
			_, variable, err := hugeColumnSectionLayout(column, section)
			if err != nil {
				return stats, err
			}
			if variable {
				sectionIndexBytes += (uint64(section.count) + 1) * 4
			} else if isNullableColumn(column) {
				sectionIndexBytes += (uint64(section.count) + 7) / 8
			}
			if sectionIndexBytes > maxHugeOffsetTableBytes {
				return stats, fmt.Errorf("section %d offset tables and NULL bitmaps exceed safe in-memory limit %d bytes", sectionID, maxHugeOffsetTableBytes)
			}
		}
	}

	filesSeen := make(map[string]bool)
	var fullRowCount uint64
	for _, sectionID := range sectionIDs {
		if ctx.maxRows > 0 && result.RowsLocated >= ctx.maxRows {
			break
		}
		columnSections := bySection[sectionID]
		readers := make([]*hugeColumnSectionReader, 0, len(info.columns))
		sectionCount := uint32(0)
		for _, column := range info.columns {
			section, ok := columnSections[column.ColID]
			if !ok {
				closeHugeColumnReaders(readers)
				return stats, fmt.Errorf("section %d has no metadata for column %s (colid=%d)", sectionID, column.Name, column.ColID)
			}
			if sectionCount == 0 {
				sectionCount = section.count
			} else if section.count != sectionCount {
				closeHugeColumnReaders(readers)
				return stats, fmt.Errorf("section %d column counts disagree: %d and %d", sectionID, sectionCount, section.count)
			}
			reader, path, openErr := openHugeColumnSection(tableDir, column, section, ctx.decoder)
			if openErr != nil {
				closeHugeColumnReaders(readers)
				return stats, openErr
			}
			filesSeen[path] = true
			readers = append(readers, reader)
		}

		for row := uint32(0); row < sectionCount; row++ {
			logicalRowID := uint64(sectionID)*uint64(info.table.hugeSectionRows()) + uint64(row) + 1
			values := make(map[uint16]dataValue, len(readers))
			for _, reader := range readers {
				value, readErr := reader.next()
				if readErr != nil {
					closeHugeColumnReaders(readers)
					return stats, fmt.Errorf("section %d row %d column %s: %w", sectionID, row+1, reader.column.Name, readErr)
				}
				values[reader.column.ColID] = dataValue{value: value}
			}
			if hugeRowDeleted(deletes, logicalRowID) {
				continue
			}
			applyHugeRowUpdates(values, updates, logicalRowID, info.columns)
			if err := writeHugeLogicalRow(ctx, info, values, output, rowStats, result); err != nil {
				closeHugeColumnReaders(readers)
				return stats, err
			}
			if ctx.maxRows > 0 && result.RowsLocated >= ctx.maxRows {
				break
			}
		}
		closeHugeColumnReaders(readers)
		stats.sectionsRead++
		end := uint64(sectionID)*uint64(info.table.hugeSectionRows()) + uint64(sectionCount)
		if end > fullRowCount {
			fullRowCount = end
		}
	}

	if info.table.HugeRAuxID != 0 && (ctx.maxRows == 0 || result.RowsLocated < ctx.maxRows) {
		sequence := uint64(0)
		err = walkHugeAuxRows(ctx, info.table.HugeRAuxID, func(row hugeAuxRow) error {
			sequence++
			rowOffset := row.clusterRowID
			if rowOffset == 0 {
				rowOffset = sequence
			}
			logicalRowID := rowOffset
			// RAUX cluster rowids are normally the table-wide DTA_ROWID
			// (1025 for the first row after a 1024-row HFS section). Older
			// layouts may number the auxiliary segment locally from one.
			if logicalRowID <= fullRowCount {
				logicalRowID = fullRowCount + rowOffset
			}
			if hugeRowDeleted(deletes, logicalRowID) {
				return nil
			}
			applyHugeRowUpdates(row.values, updates, logicalRowID, info.columns)
			if err := writeHugeLogicalRow(ctx, info, row.values, output, rowStats, result); err != nil {
				return err
			}
			if ctx.maxRows > 0 && result.RowsLocated >= ctx.maxRows {
				return errStopPageScan
			}
			return nil
		})
		if err != nil && err != errStopPageScan {
			return stats, fmt.Errorf("read %s$RAUX: %w", info.table.Name, err)
		}
	}

	stats.filesRead = len(filesSeen)
	return stats, nil
}

func writeHugeLogicalRow(ctx hugeDataExportContext, info dataTableInfo, values map[uint16]dataValue, output *dataOutputRouter, rowStats *DataTableRowCount, result *DataExportResult) error {
	result.RowsLocated++
	if rowStats != nil {
		rowStats.RowsLocated++
	}
	line, record, fields, fractionLoss, err := renderHugeValuesForExport(ctx.outputFormat, info, values, ctx.dmpCharset)
	if err != nil {
		result.RowsFailed++
		if rowStats != nil {
			rowStats.RowsFailed++
		}
		return fmt.Errorf("render HUGE row for %s.%s: %w", info.table.Owner, info.table.Name, err)
	}
	if fractionLoss {
		result.TimeFractionLoss++
	}
	if err := output.writeRow(info, line, record, fields); err != nil {
		return err
	}
	result.RowsExported++
	if rowStats != nil {
		rowStats.RowsExported++
	}
	return nil
}

func renderHugeValuesForExport(outputFormat string, info dataTableInfo, values map[uint16]dataValue, charset dmpCharsetHeader) (string, []string, []DMPField, bool, error) {
	switch outputFormat {
	case "fldr":
		dialect := info.fldrDialect.resolved()
		record := make([]string, 0, len(info.columns))
		for _, column := range info.columns {
			value := values[column.ColID].value
			text, err := fldrValueForDataColumn(column, value, dialect)
			if err != nil {
				return "", nil, nil, false, err
			}
			record = append(record, text)
		}
		return "", record, nil, false, nil
	case "dmp":
		fields := make([]DMPField, 0, len(info.columns))
		fractionLoss := false
		for _, column := range info.columns {
			value := values[column.ColID].value
			if value == nil {
				fields = append(fields, DMPNullField())
				continue
			}
			field, losesFraction, err := dmpFieldForDataColumn(column, value, charset)
			if err != nil {
				return "", nil, nil, false, err
			}
			fields = append(fields, field)
			fractionLoss = fractionLoss || losesFraction
		}
		return "", nil, fields, fractionLoss, nil
	default:
		prefix := info.sqlInsertPrefix
		if prefix == "" {
			prefix = sqlInsertPrefixForTable(info.table, info.columns)
		}
		var out strings.Builder
		out.WriteString(prefix)
		for i, column := range info.columns {
			if i > 0 {
				out.WriteString(", ")
			}
			sqlValue, err := sqlValueForDataColumn(column, values[column.ColID].value)
			if err != nil {
				return "", nil, nil, false, err
			}
			out.WriteString(sqlValue)
		}
		out.WriteString(");")
		return out.String(), nil, nil, false, nil
	}
}

func loadHugeColumnSections(ctx hugeDataExportContext, tableID uint32) ([]hugeColumnSection, error) {
	columns := ctx.columnsByTable[tableID]
	if len(columns) == 0 {
		return nil, fmt.Errorf("HUGE $AUX columns are unavailable")
	}
	var sections []hugeColumnSection
	err := walkHugeAuxRows(ctx, tableID, func(row hugeAuxRow) error {
		colID, err := hugeIntValue(row.values, columns, "COLID")
		if err != nil {
			return err
		}
		sectionID, err := hugeIntValue(row.values, columns, "SEC_ID")
		if err != nil {
			return err
		}
		fileID, err := hugeIntValue(row.values, columns, "FILE_ID")
		if err != nil {
			return err
		}
		offset, err := hugeIntValue(row.values, columns, "OFFSET")
		if err != nil {
			return err
		}
		count, err := hugeIntValue(row.values, columns, "COUNT")
		if err != nil {
			return err
		}
		nlen, err := hugeIntValue(row.values, columns, "N_LEN")
		if err != nil {
			return err
		}
		nulls, err := hugeIntValue(row.values, columns, "N_NULL")
		if err != nil {
			return err
		}
		cpr, _ := hugeStringValue(row.values, columns, "CPR_FLAG")
		enc, _ := hugeStringValue(row.values, columns, "ENC_FLAG")
		if colID < 0 || colID > 65535 || sectionID < 0 || sectionID > int64(^uint32(0)) || fileID < -1 || fileID > int64(^uint32(0)>>1) || offset < 0 || count < 0 || count > int64(^uint32(0)) || nlen < 0 || nlen > int64(^uint32(0)) || nulls < 0 || nulls > count {
			return fmt.Errorf("invalid $AUX row colid=%d section=%d file=%d offset=%d count=%d n_len=%d", colID, sectionID, fileID, offset, count, nlen)
		}
		sections = append(sections, hugeColumnSection{
			colID: uint16(colID), section: uint32(sectionID), fileID: int32(fileID),
			offset: offset, count: uint32(count), nlen: uint32(nlen),
			nulls: uint32(nulls), nullsKnown: true,
			cprFlag: strings.ToUpper(strings.TrimSpace(cpr)), encFlag: strings.ToUpper(strings.TrimSpace(enc)),
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read HUGE $AUX: %w", err)
	}
	return sections, nil
}

func loadHugeDeleteRanges(ctx hugeDataExportContext, tableID uint32) ([]hugeDeleteRange, error) {
	if tableID == 0 {
		return nil, nil
	}
	columns := ctx.columnsByTable[tableID]
	var ranges []hugeDeleteRange
	err := walkHugeAuxRows(ctx, tableID, func(row hugeAuxRow) error {
		start, err := hugeIntValue(row.values, columns, "START_ID")
		if err != nil {
			return err
		}
		count, err := hugeIntValue(row.values, columns, "COUNT")
		if err != nil {
			return err
		}
		if start <= 0 || count <= 0 {
			return nil
		}
		ranges = append(ranges, hugeDeleteRange{start: uint64(start), end: uint64(start) + uint64(count)})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(ranges, func(i, j int) bool { return ranges[i].start < ranges[j].start })
	merged := ranges[:0]
	for _, item := range ranges {
		if len(merged) == 0 || item.start > merged[len(merged)-1].end {
			merged = append(merged, item)
			continue
		}
		if item.end > merged[len(merged)-1].end {
			merged[len(merged)-1].end = item.end
		}
	}
	return merged, nil
}

func loadHugeUpdates(ctx hugeDataExportContext, info dataTableInfo, tableID uint32) (hugeUpdateSet, error) {
	updates := make(hugeUpdateSet)
	if tableID == 0 {
		return updates, nil
	}
	columns := ctx.columnsByTable[tableID]
	mainColumns := make(map[uint16]columnDef, len(info.columns))
	for _, column := range info.columns {
		mainColumns[column.ColID] = column
	}
	count := 0
	var valueBytes int64
	err := walkHugeAuxRows(ctx, tableID, func(row hugeAuxRow) error {
		colIDValue, err := hugeIntValue(row.values, columns, "COLID")
		if err != nil {
			return err
		}
		rowIDValue, err := hugeIntValue(row.values, columns, "DTA_ROWID")
		if err != nil {
			return err
		}
		if colIDValue < 0 || colIDValue > 65535 || rowIDValue <= 0 {
			return fmt.Errorf("invalid $UAUX row colid=%d rowid=%d", colIDValue, rowIDValue)
		}
		column, ok := mainColumns[uint16(colIDValue)]
		if !ok {
			return fmt.Errorf("$UAUX references unknown colid=%d", colIDValue)
		}
		rawValue, _ := hugeNamedValue(row.values, columns, "VALUE")
		decoded, err := decodeHugeDeltaValue(column, rawValue, ctx.decoder)
		if err != nil {
			return fmt.Errorf("decode $UAUX rowid=%d column=%s: %w", rowIDValue, column.Name, err)
		}
		key := hugeUpdateKey{rowID: uint64(rowIDValue), colID: column.ColID}
		if previous, exists := updates[key]; exists {
			valueBytes -= hugeDeltaValueSize(previous.value)
		} else {
			count++
		}
		updates[key] = dataValue{value: decoded}
		valueBytes += hugeDeltaValueSize(decoded)
		if count > maxHugeDeltaUpdates {
			return fmt.Errorf("HUGE delta update count exceeds safe in-memory limit %d", maxHugeDeltaUpdates)
		}
		if valueBytes > maxHugeDeltaUpdateBytes {
			return fmt.Errorf("HUGE delta update values exceed safe in-memory limit %d bytes", maxHugeDeltaUpdateBytes)
		}
		return nil
	})
	return updates, err
}

func applyHugeRowUpdates(values map[uint16]dataValue, updates hugeUpdateSet, rowID uint64, columns []columnDef) {
	for _, column := range columns {
		if value, ok := updates[hugeUpdateKey{rowID: rowID, colID: column.ColID}]; ok {
			values[column.ColID] = value
		}
	}
}

func hugeDeltaValueSize(value any) int64 {
	switch typed := value.(type) {
	case string:
		return int64(len(typed))
	case dmBinary:
		return int64(len(typed))
	case []byte:
		return int64(len(typed))
	case dmNumber:
		return int64(len(typed))
	case nil:
		return 1
	default:
		return 16
	}
}

func hugeRowDeleted(ranges []hugeDeleteRange, rowID uint64) bool {
	idx := sort.Search(len(ranges), func(i int) bool { return ranges[i].end > rowID })
	return idx < len(ranges) && ranges[idx].start <= rowID
}

func decodeHugeDeltaValue(column columnDef, value any, decoder textDecoder) (any, error) {
	if value == nil {
		return nil, nil
	}
	var raw []byte
	switch typed := value.(type) {
	case dmBinary:
		raw = []byte(typed)
	case []byte:
		raw = typed
	case string:
		raw = []byte(typed)
	default:
		return nil, fmt.Errorf("unsupported delta value type %T", value)
	}
	if isCharacterDataType(column.DataType) {
		text, ok := decoder.decode(raw)
		if !ok {
			return nil, fmt.Errorf("cannot decode text")
		}
		return text, nil
	}
	if isBinaryDataType(column.DataType) {
		return dmBinary(append([]byte(nil), raw...)), nil
	}
	if isNumberDataType(column.DataType) {
		value, ok := decodeDMNumber(raw)
		if !ok {
			return nil, fmt.Errorf("cannot decode NUMBER")
		}
		return dmNumber(value), nil
	}
	decoded, end, err := parseFixedDataValuePresent(column, raw, 0)
	if err != nil {
		return nil, err
	}
	if end != len(raw) {
		return nil, fmt.Errorf("fixed value consumed %d/%d bytes", end, len(raw))
	}
	return decoded, nil
}

func walkHugeAuxRows(ctx hugeDataExportContext, tableID uint32, visit func(hugeAuxRow) error) error {
	if tableID == 0 {
		return nil
	}
	columns := ctx.columnsByTable[tableID]
	if len(columns) == 0 {
		return fmt.Errorf("columns for internal HUGE table id=%d are unavailable", tableID)
	}
	storage, ok := ctx.storageByTable[tableID]
	if !ok || storage.ID == 0 {
		return fmt.Errorf("storage root for internal HUGE table id=%d is unavailable", tableID)
	}
	seen := make(map[dataPageRef]bool)
	visitPage := func(ref dataPageRef, page []byte) error {
		if seen[ref] {
			return nil
		}
		seen[ref] = true
		if !pageHeaderMatchesRef(page, ref) || dataPageKind(page) != dmPageKindRowData || dataPageStorageID(page) != storage.ID {
			return fmt.Errorf("internal HUGE page validation failed at group=%d file=%d page=%d", ref.key.groupID, ref.key.fileID, ref.pageNo)
		}
		nRec := int(binary.LittleEndian.Uint16(page[dataPageRecordCountOff:]))
		for _, located := range locateRowsInDataPage(page, ctx.pageSize, nRec) {
			start := int(located.offset)
			end := start + int(located.length)
			rowBytes := page[start:end]
			values, _, _, err := parseDataRowValues(rowBytes, columns, ctx.decoder, false, nil)
			if err != nil {
				return fmt.Errorf("parse internal HUGE row page=%d slot=%d: %w", ref.pageNo, located.slotNo, err)
			}
			var clusterRowID uint64
			if tail, ok := decodeDataRowControlTail(rowBytes); ok {
				clusterRowID = tail.clusterRowID
			}
			if err := visit(hugeAuxRow{values: values, clusterRowID: clusterRowID, page: ref, slot: located.slotNo}); err != nil {
				return err
			}
		}
		return nil
	}

	plan, reason := buildStoragePagePlanDetailed(storage, ctx.pageCache)
	if len(plan) > 0 {
		for _, ref := range sortedDataPageRefs(plan) {
			page, ok := ctx.pageCache.readPage(ref)
			if !ok {
				return fmt.Errorf("read planned internal HUGE page group=%d file=%d page=%d", ref.key.groupID, ref.key.fileID, ref.pageNo)
			}
			if err := visitPage(ref, page); err != nil {
				return err
			}
		}
		return nil
	}

	groupID := uint32(storage.GroupID)
	matched := false
	for _, file := range ctx.dataFiles {
		if file.key.groupID != groupID {
			continue
		}
		_, err := forEachDataFileRefPage(file, ctx.pageSize, func(page []byte, pageNo uint32) error {
			if dataPageKind(page) != dmPageKindRowData || dataPageStorageID(page) != storage.ID {
				return nil
			}
			matched = true
			return visitPage(dataPageRef{key: file.key, pageNo: pageNo}, page)
		})
		if err != nil {
			return err
		}
	}
	if !matched {
		return fmt.Errorf("no pages found for internal HUGE storage_id=%d: %s", storage.ID, reason)
	}
	return nil
}

func hugeNamedValue(values map[uint16]dataValue, columns []columnDef, name string) (any, bool) {
	for _, column := range columns {
		if strings.EqualFold(column.Name, name) {
			value, ok := values[column.ColID]
			if !ok {
				return nil, false
			}
			return value.value, true
		}
	}
	return nil, false
}

func hugeIntValue(values map[uint16]dataValue, columns []columnDef, name string) (int64, error) {
	value, ok := hugeNamedValue(values, columns, name)
	if !ok || value == nil {
		return 0, fmt.Errorf("%s is NULL or missing", name)
	}
	switch typed := value.(type) {
	case int8:
		return int64(typed), nil
	case int16:
		return int64(typed), nil
	case int32:
		return int64(typed), nil
	case int64:
		return typed, nil
	case dmNumber:
		return strconv.ParseInt(string(typed), 10, 64)
	case string:
		return strconv.ParseInt(strings.TrimSpace(typed), 10, 64)
	default:
		return 0, fmt.Errorf("%s has unsupported integer type %T", name, value)
	}
}

func hugeStringValue(values map[uint16]dataValue, columns []columnDef, name string) (string, bool) {
	value, ok := hugeNamedValue(values, columns, name)
	if !ok || value == nil {
		return "", false
	}
	switch typed := value.(type) {
	case string:
		return typed, true
	case dmBinary:
		return string(typed), true
	default:
		return fmt.Sprintf("%v", value), true
	}
}

func findHugeTableDir(dataDir string, schemaID uint32, tableID uint32) (string, error) {
	dataDir = filepath.Clean(dataDir)
	schemaDir := fmt.Sprintf("SCH%09d", schemaID)
	tableDir := fmt.Sprintf("TAB%04d", tableID)

	var matches []string
	err := filepath.WalkDir(dataDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if !entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dataDir, path)
		if err != nil {
			return nil
		}
		depth := 0
		if rel != "." {
			depth = len(strings.Split(rel, string(filepath.Separator)))
		}
		if depth > 8 {
			return filepath.SkipDir
		}
		if !strings.EqualFold(entry.Name(), schemaDir) {
			if strings.HasPrefix(strings.ToUpper(entry.Name()), "SCH") {
				return filepath.SkipDir
			}
			return nil
		}
		candidate := filepath.Join(path, tableDir)
		if info, statErr := os.Stat(candidate); statErr == nil && info.IsDir() {
			matches = append(matches, candidate)
		}
		return filepath.SkipDir
	})
	if err != nil {
		return "", fmt.Errorf("scan HFS root: %w", err)
	}
	sort.Strings(matches)
	if len(matches) == 0 {
		return "", fmt.Errorf("HFS directory %s/%s was not found under %s", schemaDir, tableDir, dataDir)
	}
	if len(matches) > 1 {
		return "", fmt.Errorf("multiple HFS directories match %s/%s: %s", schemaDir, tableDir, strings.Join(matches, ", "))
	}
	return matches[0], nil
}

func openHugeColumnSection(tableDir string, column columnDef, meta hugeColumnSection, decoder textDecoder) (*hugeColumnSectionReader, string, error) {
	fixedWidth, variable, err := hugeColumnSectionLayout(column, meta)
	if err != nil {
		return nil, "", err
	}
	path, err := findHugeColumnFile(tableDir, meta.colID, meta.fileID)
	if err != nil {
		return nil, "", err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, "", fmt.Errorf("open HFS column file %s: %w", path, err)
	}
	fileInfo, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, "", fmt.Errorf("stat HFS column file %s: %w", path, err)
	}
	reader := &hugeColumnSectionReader{column: column, meta: meta, decoder: decoder, file: file}
	header := make([]byte, hugeHFSSectionHeaderSize)
	if _, err := file.ReadAt(header, meta.offset); err != nil {
		file.Close()
		return nil, "", fmt.Errorf("read HFS section header %s@%d: %w", path, meta.offset, err)
	}
	if !bytes.Equal(header[:len(hugeHFSSectionMagic)], hugeHFSSectionMagic) {
		file.Close()
		return nil, "", fmt.Errorf("invalid HFS section magic in %s@%d", path, meta.offset)
	}
	headerLength := binary.LittleEndian.Uint32(header[4:8])
	if meta.nlen != 0 && headerLength != meta.nlen {
		file.Close()
		return nil, "", fmt.Errorf("HFS section length mismatch in %s@%d: header=%d AUX=%d", path, meta.offset, headerLength, meta.nlen)
	}
	if meta.nlen == 0 {
		meta.nlen = headerLength
		reader.meta.nlen = headerLength
	}
	if typeID := binary.LittleEndian.Uint16(header[24:]); typeID != 0 && typeID != hugeFixedTypeID(column) && !variable {
		file.Close()
		return nil, "", fmt.Errorf("HFS column %s type mismatch: header=%d dictionary=%s", column.Name, typeID, column.DataType)
	}
	if headerLength < uint32(hugeHFSSectionHeaderSize) || meta.offset+int64(headerLength) > fileInfo.Size() {
		file.Close()
		return nil, "", fmt.Errorf("HFS section exceeds file bounds in %s@%d: length=%d file_size=%d", path, meta.offset, headerLength, fileInfo.Size())
	}

	if variable {
		if err := reader.initVariable(); err != nil {
			file.Close()
			return nil, "", fmt.Errorf("initialize HFS variable column %s: %w", column.Name, err)
		}
		return reader, path, nil
	}
	reader.fixedWidth = fixedWidth
	dataLength := int64(meta.count) * int64(reader.fixedWidth)
	bitmapLength := int64(0)
	if isNullableColumn(column) {
		bitmapLength = (int64(meta.count) + 7) / 8
	}
	if dataLength > int64(meta.nlen)-hugeHFSSectionHeaderSize-bitmapLength {
		file.Close()
		return nil, "", fmt.Errorf("fixed HUGE column %s exceeds section length: data=%d section=%d", column.Name, dataLength, meta.nlen)
	}
	if err := reader.initFixedNulls(bitmapLength); err != nil {
		file.Close()
		return nil, "", err
	}
	reader.fixedReader = bufio.NewReaderSize(io.NewSectionReader(file, meta.offset+hugeHFSSectionHeaderSize, dataLength), hugeColumnReadBufferSize)
	return reader, path, nil
}

func hugeColumnSectionLayout(column columnDef, meta hugeColumnSection) (fixedWidth int, variable bool, err error) {
	if meta.cprFlag != "" && meta.cprFlag != "N" {
		return 0, false, fmt.Errorf("column %s section %d uses unsupported HUGE compression CPR_FLAG=%s", column.Name, meta.section, meta.cprFlag)
	}
	if meta.encFlag != "" && meta.encFlag != "N" {
		return 0, false, fmt.Errorf("column %s section %d uses unsupported HUGE encoding ENC_FLAG=%s", column.Name, meta.section, meta.encFlag)
	}
	if meta.offset < hugeHFSFileHeaderSize || meta.offset%4096 != 0 {
		return 0, false, fmt.Errorf("column %s section %d has invalid HFS offset %d", column.Name, meta.section, meta.offset)
	}
	typeName := normalizeDataType(column.DataType)
	switch typeName {
	case "CHAR", "CHARACTER", "VARCHAR", "VARCHAR2":
		return 0, true, nil
	case "INT", "INTEGER", "PLS_INTEGER":
		fixedWidth = 4
	case "SMALLINT":
		fixedWidth = 4
	case "BIGINT", "DOUBLE", "DOUBLE PRECISION":
		fixedWidth = 8
	case "DATE":
		fixedWidth = 13
	default:
		return 0, false, fmt.Errorf("column %s type %s has no verified HUGE HFS decoder", column.Name, column.DataType)
	}
	return fixedWidth, false, nil
}

func (reader *hugeColumnSectionReader) initVariable() error {
	count := uint64(reader.meta.count) + 1
	if count > uint64(^uint(0)>>1)/4 || count*4 > maxHugeOffsetTableBytes {
		return fmt.Errorf("offset table is too large")
	}
	if uint64(reader.meta.nlen) < uint64(hugeHFSSectionHeaderSize)+count*4 {
		return fmt.Errorf("offset table exceeds section length")
	}
	raw := make([]byte, int(count)*4)
	if _, err := reader.file.ReadAt(raw, reader.meta.offset+hugeHFSSectionHeaderSize); err != nil {
		return err
	}
	reader.offsets = make([]uint32, count)
	for i := range reader.offsets {
		reader.offsets[i] = binary.LittleEndian.Uint32(raw[i*4:])
	}
	first := reader.offsets[len(reader.offsets)-1]
	for _, offset := range reader.offsets[:len(reader.offsets)-1] {
		if offset != ^uint32(0) {
			first = offset
			break
		}
	}
	end := reader.offsets[len(reader.offsets)-1]
	if first > end || uint64(end) > uint64(reader.meta.nlen) {
		return fmt.Errorf("invalid variable payload offsets first=%d end=%d n_len=%d", first, end, reader.meta.nlen)
	}
	reader.variablePos = first
	reader.nextOffsetPos = 1
	reader.variable = bufio.NewReaderSize(io.NewSectionReader(reader.file, reader.meta.offset+int64(first), int64(end-first)), hugeColumnReadBufferSize)
	return nil
}

func (reader *hugeColumnSectionReader) next() (any, error) {
	if reader.row >= reader.meta.count {
		return nil, io.EOF
	}
	if reader.fixedWidth > 0 {
		raw := make([]byte, reader.fixedWidth)
		if _, err := io.ReadFull(reader.fixedReader, raw); err != nil {
			return nil, err
		}
		index := reader.row
		reader.row++
		if len(reader.presentBits) > 0 && reader.presentBits[index/8]&(0x80>>(index%8)) == 0 {
			return nil, nil
		}
		return decodeHugeFixedValue(reader.column, raw)
	}

	index := reader.row
	reader.row++
	start := reader.offsets[index]
	if start == ^uint32(0) {
		return nil, nil
	}
	if reader.nextOffsetPos <= index {
		reader.nextOffsetPos = index + 1
	}
	for reader.nextOffsetPos < uint32(len(reader.offsets)) && reader.offsets[reader.nextOffsetPos] == ^uint32(0) {
		reader.nextOffsetPos++
	}
	if reader.nextOffsetPos >= uint32(len(reader.offsets)) {
		return nil, fmt.Errorf("value at row %d has no end offset", index+1)
	}
	end := reader.offsets[reader.nextOffsetPos]
	if start < reader.variablePos || end < start || uint64(end) > uint64(reader.meta.nlen) {
		return nil, fmt.Errorf("invalid value offsets row=%d start=%d end=%d current=%d", index+1, start, end, reader.variablePos)
	}
	if skip := int64(start - reader.variablePos); skip > 0 {
		if _, err := io.CopyN(io.Discard, reader.variable, skip); err != nil {
			return nil, err
		}
	}
	raw := make([]byte, int(end-start))
	if _, err := io.ReadFull(reader.variable, raw); err != nil {
		return nil, err
	}
	reader.variablePos = end
	typeName := normalizeDataType(reader.column.DataType)
	if isCharacterDataType(typeName) {
		text, ok := reader.decoder.decode(raw)
		if !ok {
			return nil, fmt.Errorf("cannot decode text")
		}
		return text, nil
	}
	if isVariableBinaryDataType(typeName) {
		return dmBinary(raw), nil
	}
	if isNumberDataType(typeName) {
		value, ok := decodeDMNumber(raw)
		if !ok {
			return nil, fmt.Errorf("cannot decode NUMBER")
		}
		return dmNumber(value), nil
	}
	return nil, fmt.Errorf("unsupported variable HUGE type %s", reader.column.DataType)
}

func closeHugeColumnReaders(readers []*hugeColumnSectionReader) {
	for _, reader := range readers {
		if reader != nil && reader.file != nil {
			_ = reader.file.Close()
		}
	}
}

func findHugeColumnFile(tableDir string, colID uint16, fileID int32) (string, error) {
	want := fmt.Sprintf("COL%04d_%010d.dta", colID, fileID)
	direct := filepath.Join(tableDir, want)
	if info, err := os.Stat(direct); err == nil && !info.IsDir() {
		return direct, nil
	}
	entries, err := os.ReadDir(tableDir)
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		if !entry.IsDir() && strings.EqualFold(entry.Name(), want) {
			return filepath.Join(tableDir, entry.Name()), nil
		}
	}
	return "", fmt.Errorf("HFS column file %s is missing", direct)
}
