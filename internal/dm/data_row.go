package dm

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
)

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
	if !validPageSize(pageSize) || uint64(len(page)) < uint64(pageSize) {
		return nil
	}
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
	if uint32(rowOff)+uint32(rowLen) > uint32(freeEnd) || uint32(rowOff)+uint32(rowLen) > pageSize || int(rowOff)+int(rowLen) > len(page) {
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
		if err == nil {
			return dmJSONValue{value: value, binary: normalizeDataType(col.DataType) == "JSONB", decoder: decoder}, next, nil
		}
		payload, longErr := lobReader.readLongRowPayload(locator)
		if longErr != nil {
			return nil, pos, fmt.Errorf("%s: %w", col.Name, err)
		}
		return dmJSONValue{value: dmBinary(payload), binary: normalizeDataType(col.DataType) == "JSONB", decoder: decoder}, next, nil
	}
	if isBinaryDataType(col.DataType) {
		value, err := lobReader.lazyLOBValue(locator, dmPageKindLOBData, false, decoder)
		if err == nil {
			return value, next, nil
		}
		payload, longErr := lobReader.readLongRowPayload(locator)
		if longErr != nil {
			return nil, pos, fmt.Errorf("%s: %w", col.Name, err)
		}
		return dmBinary(payload), next, nil
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
