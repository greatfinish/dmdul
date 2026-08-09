package dm

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	sysHPartBaseTableIDOffset = 0x05
	sysHPartPartTableIDOffset = 0x09
	sysHPartTypeOffset        = 0x1A
)

type PartitionScanOptions struct {
	SystemPath  string
	ControlPath string
	OwnerFilter string
	Charset     string
}

type PartitionScanResult struct {
	SystemPath     string
	ExtentSize     uint32
	PageSize       uint32
	PageCount      uint32
	ObjectCount    int
	TableCount     int
	PartitionCount int
	Tables         []PartitionedTable
}

type PartitionedTable struct {
	Owner      string
	Name       string
	TableID    uint32
	Partitions []PartitionInfo
}

type PartitionInfo struct {
	BaseTableID      uint32
	PartTableID      uint32
	Type             string
	Name             string
	HighValue        []byte
	HighValuePreview string
	HighValueHex     string
	PageNo           uint32
	SlotNo           uint16
	SlotOffset       uint16
	RowOffset        uint64
}

type partitionDef struct {
	BaseTableID uint32
	PartTableID uint32
	Type        string
	Name        string
	HighValue   []byte
	Location    ddlLocation
}

func ScanPartitions(opts PartitionScanOptions) (*PartitionScanResult, error) {
	if opts.SystemPath == "" {
		return nil, fmt.Errorf("scan-partitions requires SYSTEM.DBF path")
	}

	stream, err := openSystemPageStream(opts.SystemPath)
	if err != nil {
		return nil, err
	}
	defer stream.close()

	pageSize, pageCount, extentSize := stream.pageSize, stream.pageCount, stream.extentSize
	preferredCharset := strings.ToLower(strings.TrimSpace(opts.Charset))
	if preferredCharset == "" || preferredCharset == "auto" {
		if charset, ok := stream.charset(); ok && charset.DecoderName != "" {
			preferredCharset = charset.DecoderName
		}
	}
	decoder := textDecoder{preferred: preferredCharset}
	matcher := newOwnerMatcher(opts.OwnerFilter)

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
	for _, obj := range objects {
		if obj.Type == "SCHOBJ" && obj.Subtype == "UTAB" {
			tables[obj.ID] = obj
		}
	}

	partitionsByTable, err := stream.partitionsByTable(decoder, tables, matcher)
	if err != nil {
		return nil, err
	}

	result := &PartitionScanResult{
		SystemPath:     opts.SystemPath,
		ExtentSize:     extentSize,
		PageSize:       pageSize,
		PageCount:      pageCount,
		ObjectCount:    len(objects),
		TableCount:     len(partitionsByTable),
		PartitionCount: countPartitions(partitionsByTable),
		Tables:         partitionedTablesFromMap(tables, partitionsByTable),
	}
	return result, nil
}

func partitionedTablesFromMap(tables map[uint32]dictionaryObject, partitionsByTable map[uint32][]PartitionInfo) []PartitionedTable {
	var result []PartitionedTable
	for tableID, parts := range partitionsByTable {
		table := tables[tableID]
		result = append(result, PartitionedTable{
			Owner:      table.Owner,
			Name:       table.Name,
			TableID:    table.ID,
			Partitions: parts,
		})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Owner == result[j].Owner {
			if result[i].Name == result[j].Name {
				return result[i].TableID < result[j].TableID
			}
			return result[i].Name < result[j].Name
		}
		return result[i].Owner < result[j].Owner
	})
	return result
}

func countPartitions(partitionsByTable map[uint32][]PartitionInfo) int {
	count := 0
	for _, parts := range partitionsByTable {
		count += len(parts)
	}
	return count
}

func countPartitionKeys(keysByTable map[uint32][]uint16) int {
	count := 0
	for _, keys := range keysByTable {
		count += len(keys)
	}
	return count
}

func sysObjInfosColumns() []columnDef {
	return []columnDef{
		{ColID: 0, Name: "ID", DataType: "INT", Nullable: "N"},
		{ColID: 1, Name: "TYPE$", DataType: "VARCHAR", Nullable: "N"},
		{ColID: 2, Name: "INT_VALUE", DataType: "INT", Nullable: "Y"},
		{ColID: 3, Name: "STR_VALUE", DataType: "VARCHAR", Nullable: "Y"},
		{ColID: 4, Name: "BIN_VALUE", DataType: "VARBINARY", Nullable: "Y"},
	}
}

func parseTabPartInfoRow(page []byte, rowOff int, pageSize uint32, decoder textDecoder) (uint32, []uint16, bool) {
	if rowOff < 0 || rowOff+3 > len(page) || uint32(rowOff)+3 > pageSize {
		return 0, nil, false
	}
	header := binary.BigEndian.Uint16(page[rowOff:])
	if header&dataRowDeletedMask != 0 {
		return 0, nil, false
	}
	rowLen := int(header &^ dataRowDeletedMask)
	if rowLen < 12 || rowOff+rowLen > len(page) || uint32(rowOff+rowLen) > pageSize {
		return 0, nil, false
	}
	values, _, _, err := parseDataRowValues(page[rowOff:rowOff+rowLen], sysObjInfosColumns(), decoder, false, nil)
	if err != nil {
		return 0, nil, false
	}
	idValue, ok := values[0]
	if !ok {
		return 0, nil, false
	}
	var tableID uint32
	switch value := idValue.value.(type) {
	case int32:
		if value <= 0 {
			return 0, nil, false
		}
		tableID = uint32(value)
	case int64:
		if value <= 0 || value > int64(^uint32(0)) {
			return 0, nil, false
		}
		tableID = uint32(value)
	default:
		return 0, nil, false
	}
	typeValue, ok := values[1]
	if !ok || !strings.EqualFold(strings.TrimSpace(fmt.Sprint(typeValue.value)), "TABPART") {
		return 0, nil, false
	}
	binValue, ok := values[4].value.(dmBinary)
	if !ok {
		return 0, nil, false
	}
	colIDs, ok := decodeTabPartBinValue(binValue)
	if !ok {
		return 0, nil, false
	}
	return tableID, colIDs, true
}

func decodeTabPartBinValue(binValue []byte) ([]uint16, bool) {
	if len(binValue) < 4 {
		return nil, false
	}
	keyCount := int(binary.LittleEndian.Uint16(binValue[0:]))
	if keyCount <= 0 || keyCount > 64 {
		return nil, false
	}
	colIDs := make([]uint16, 0, keyCount)
	for i := 0; i < keyCount; i++ {
		off := 2 + i*4
		if off+2 > len(binValue) {
			return nil, false
		}
		colIDs = append(colIDs, binary.LittleEndian.Uint16(binValue[off:]))
	}
	return colIDs, true
}

func iterDictionarySlotRangesInPage(page []byte, pageSize uint32, pageNo uint32, visit func(page []byte, pageNo uint32, slotNo uint16, slotOff uint16, nextOff uint16)) {
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

	slots := make([]dictionarySlotRange, 0, slotCount)
	for slotNo := uint16(0); slotNo < slotCount; slotNo++ {
		pos := slotArrayStart + int(slotNo)*2
		slotOff := binary.LittleEndian.Uint16(page[pos:])
		if slotOff == 0 || int(slotOff) >= slotArrayStart {
			continue
		}
		slots = append(slots, dictionarySlotRange{slotNo: slotNo, slotOff: slotOff})
	}
	sort.Slice(slots, func(i, j int) bool {
		if slots[i].slotOff == slots[j].slotOff {
			return slots[i].slotNo < slots[j].slotNo
		}
		return slots[i].slotOff < slots[j].slotOff
	})
	for i, slot := range slots {
		nextOff := uint16(slotArrayStart)
		if i+1 < len(slots) {
			nextOff = slots[i+1].slotOff
		}
		if nextOff <= slot.slotOff {
			continue
		}
		visit(page, pageNo, slot.slotNo, slot.slotOff, nextOff)
	}
}

type dictionarySlotRange struct {
	slotNo  uint16
	slotOff uint16
}

func parseDDLPartitionRow(page []byte, rowOff int, pageNo uint32, slotNo uint16, slotOff uint16, pageSize uint32, decoder textDecoder) (partitionDef, bool) {
	for delta := 0; delta < 8; delta++ {
		part, ok := parseDDLPartitionRowAt(page, rowOff+delta, pageNo, slotNo, slotOff, pageSize, decoder)
		if ok {
			return part, true
		}
	}
	return partitionDef{}, false
}

func parseDDLPartitionRowAt(page []byte, rowOff int, pageNo uint32, slotNo uint16, slotOff uint16, pageSize uint32, decoder textDecoder) (partitionDef, bool) {
	if rowOff+sysHPartTypeOffset+2 >= int(pageSize) {
		return partitionDef{}, false
	}
	rowLen := int(binary.LittleEndian.Uint16(page[rowOff+0x01:]))
	if rowLen < 0x24 || rowLen > 0x2400 || rowOff+rowLen > len(page) {
		return partitionDef{}, false
	}

	partType, next, ok := readDDLShortString(page, rowOff+sysHPartTypeOffset, decoder, false)
	if !ok {
		return partitionDef{}, false
	}
	partType = strings.ToUpper(strings.TrimSpace(partType))
	switch partType {
	case "RANGE", "LIST", "HASH":
	default:
		return partitionDef{}, false
	}

	partName, next, ok := readDDLShortString(page, next, decoder, false)
	if !ok || !isSafeShortText(partName) {
		return partitionDef{}, false
	}

	baseTableID := binary.LittleEndian.Uint32(page[rowOff+sysHPartBaseTableIDOffset:])
	partTableID := binary.LittleEndian.Uint32(page[rowOff+sysHPartPartTableIDOffset:])
	if baseTableID == 0 || partTableID == 0 || baseTableID == partTableID {
		return partitionDef{}, false
	}

	rowAbs := uint64(pageNo)*uint64(pageSize) + uint64(rowOff)
	highValueEnd := rowOff + rowLen
	highValue := normalizePartitionHighValue(page[next:highValueEnd])
	return partitionDef{
		BaseTableID: baseTableID,
		PartTableID: partTableID,
		Type:        partType,
		Name:        partName,
		HighValue:   highValue,
		Location:    ddlLocation{PageNo: pageNo, SlotNo: slotNo, SlotOffset: slotOff, RowOffset: rowAbs},
	}, true
}

func normalizePartitionHighValue(raw []byte) []byte {
	if len(raw) == 0 {
		return nil
	}
	if value, next, err := readShortDataBytes(raw, 0); err == nil && next <= len(raw) {
		return append([]byte(nil), value...)
	}
	// A decoded SYSHPARTTABLEINFO payload starts with its descriptor, not a
	// length marker. Its trailing zero bytes are part of the binary value and
	// must survive dictionary persistence unchanged.
	if len(raw) >= 26 && raw[0] == 0x01 && raw[4] == 0x03 {
		return append([]byte(nil), raw...)
	}
	raw = trimPartitionHighValue(raw)
	return append([]byte(nil), raw...)
}

func trimPartitionHighValue(raw []byte) []byte {
	end := len(raw)
	for end > 0 && raw[end-1] == 0 {
		end--
	}
	return raw[:end]
}

func partitionHighValueHex(raw []byte, maxBytes int) string {
	if len(raw) == 0 {
		return ""
	}
	if maxBytes > 0 && len(raw) > maxBytes {
		return strings.ToUpper(hex.EncodeToString(raw[:maxBytes])) + "..."
	}
	return strings.ToUpper(hex.EncodeToString(raw))
}

func partitionHighValuePreview(raw []byte) string {
	tokens := partitionHighValueTokens(raw)
	if len(tokens) == 0 {
		return ""
	}
	return strings.Join(tokens, ",")
}

func partitionHighValueTokens(raw []byte) []string {
	if len(raw) == 0 {
		return nil
	}
	var tokens []string
	var current strings.Builder
	flush := func() {
		value := strings.TrimSpace(current.String())
		current.Reset()
		if len([]rune(value)) < 2 || !hasPartitionPreviewSignal(value) {
			return
		}
		for _, existing := range tokens {
			if existing == value {
				return
			}
		}
		tokens = append(tokens, value)
	}

	for i := 0; i < len(raw); {
		r, size := utf8.DecodeRune(raw[i:])
		if r == utf8.RuneError && size == 1 {
			flush()
			i++
			continue
		}
		if isPartitionPreviewRune(r) {
			current.WriteRune(r)
		} else {
			flush()
		}
		i += size
	}
	flush()
	if len(tokens) == 0 {
		return nil
	}
	if len(tokens) > 8 {
		tokens = tokens[:8]
	}
	return tokens
}

func dictionaryPartitionsFromMap(tables map[uint32]dictionaryObject, partitionsByTable map[uint32][]PartitionInfo) []DictionaryPartition {
	var result []DictionaryPartition
	for tableID, parts := range partitionsByTable {
		table, ok := tables[tableID]
		if !ok {
			continue
		}
		for i, part := range parts {
			result = append(result, DictionaryPartition{
				BaseTableID: part.BaseTableID,
				Owner:       table.Owner,
				TableName:   table.Name,
				Position:    uint32(i + 1),
				Type:        part.Type,
				Name:        part.Name,
				PartTableID: part.PartTableID,
				HighValue:   normalizePartitionHighValue(part.HighValue),
				PageNo:      part.PageNo,
				SlotNo:      part.SlotNo,
				SlotOffset:  part.SlotOffset,
				RowOffset:   part.RowOffset,
			})
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Owner != result[j].Owner {
			return result[i].Owner < result[j].Owner
		}
		if result[i].TableName != result[j].TableName {
			return result[i].TableName < result[j].TableName
		}
		if result[i].Position != result[j].Position {
			return result[i].Position < result[j].Position
		}
		return result[i].PartTableID < result[j].PartTableID
	})
	return result
}

func dictionaryPartitionKeysFromMap(tables map[uint32]dictionaryObject, keysByTable map[uint32][]uint16, columns []DictionaryColumn) []DictionaryPartitionKey {
	columnNames := make(map[tableColKey]string)
	for _, column := range columns {
		columnNames[tableColKey{tableID: column.TableID, colID: column.ColID}] = column.Name
	}
	var result []DictionaryPartitionKey
	for tableID, colIDs := range keysByTable {
		table, ok := tables[tableID]
		if !ok {
			continue
		}
		for i, colID := range colIDs {
			result = append(result, DictionaryPartitionKey{
				TableID:    tableID,
				Owner:      table.Owner,
				TableName:  table.Name,
				Position:   uint32(i + 1),
				ColID:      colID,
				ColumnName: columnNames[tableColKey{tableID: tableID, colID: colID}],
			})
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Owner != result[j].Owner {
			return result[i].Owner < result[j].Owner
		}
		if result[i].TableName != result[j].TableName {
			return result[i].TableName < result[j].TableName
		}
		return result[i].Position < result[j].Position
	})
	return result
}

func isPartitionPreviewRune(r rune) bool {
	if r == ' ' || r == '_' || r == '-' || r == '.' || r == ':' || r == '/' || r == '\'' {
		return true
	}
	return unicode.IsLetter(r) || unicode.IsNumber(r)
}

func hasPartitionPreviewSignal(value string) bool {
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			return true
		}
	}
	return false
}
