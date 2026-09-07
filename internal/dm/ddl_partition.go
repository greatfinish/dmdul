package dm

import (
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"
)

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
