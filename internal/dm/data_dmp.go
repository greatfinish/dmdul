package dm

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
)

func dmpCharsetForDataExport(value string) (dmpCharsetHeader, error) {
	normalized := normalizeCharsetToken(value)
	switch normalized {
	case "", "AUTO":
		normalized = "UTF-8"
	case "GBK", "CP936":
		normalized = "GB18030"
	}
	return dmpCharsetFromName(normalized)
}

func detectDMPCaseSensitiveFromINI(systemPath string, dataDir string) *bool {
	paths := []string{filepath.Join(dataDir, "dm.ini"), DefaultIniPathForSystem(systemPath)}
	seen := make(map[string]bool)
	for _, path := range paths {
		path = filepath.Clean(path)
		if path == "." || seen[strings.ToUpper(path)] {
			continue
		}
		seen[strings.ToUpper(path)] = true
		values, ok := loadDMIni(path)
		if !ok {
			continue
		}
		value := strings.ToLower(strings.TrimSpace(values["CASE_SENSITIVE"]))
		switch value {
		case "1", "y", "yes", "true":
			enabled := true
			return &enabled
		case "0", "n", "no", "false":
			enabled := false
			return &enabled
		}
	}
	return nil
}

func detectDMPCaseSensitive(systemPath string, dataDir string, pageSize uint32) *bool {
	if enabled, ok := detectSystemCaseSensitiveFromFile(systemPath, pageSize); ok {
		return &enabled
	}
	return detectDMPCaseSensitiveFromINI(systemPath, dataDir)
}

func renderDMPForDataRowWithMeta(info dataTableInfo, row []byte, decoder textDecoder, charset dmpCharsetHeader) ([]DMPField, int, int, dataRowRenderMeta, bool, error) {
	values, dataStart, dataEnd, err := parseDataRowValues(row, info.columns, decoder, info.historicalRows, info.lobReader)
	if err != nil {
		return nil, 0, 0, dataRowRenderMeta{}, false, err
	}
	fields := make([]DMPField, 0, len(info.columns))
	timeFractionLoss := false
	for _, col := range info.columns {
		value, ok := values[col.ColID]
		if !ok || value.value == nil {
			fields = append(fields, DMPNullField())
			continue
		}
		field, losesFraction, err := dmpFieldForDataColumn(col, value.value, charset)
		if err != nil {
			return nil, 0, 0, dataRowRenderMeta{}, false, fmt.Errorf("%s: %w", col.Name, err)
		}
		fields = append(fields, field)
		timeFractionLoss = timeFractionLoss || losesFraction
	}
	return fields, dataStart, dataEnd, dataRowRenderMetaForValues(info.columns, values, info.coverage.active()), timeFractionLoss, nil
}

func dmpFieldForDataColumn(col columnDef, value any, charset dmpCharsetHeader) (DMPField, bool, error) {
	typ := normalizeDataType(col.DataType)
	if isVectorDataType(typ) {
		vector, ok := value.(dmVectorValue)
		if !ok {
			return DMPField{}, false, fmt.Errorf("VECTOR value has type %T", value)
		}
		raw, err := encodeDMPText(vector.text, charset)
		if err != nil {
			return DMPField{}, false, err
		}
		if len(raw) > int(^uint16(0))-1 {
			return DMPLongField(raw), false, nil
		}
		return DMPShortField(raw), false, nil
	}
	if isJSONDataType(typ) {
		materialized, err := materializeDataValue(value)
		if err != nil {
			return DMPField{}, false, err
		}
		raw, err := encodeDMPText(fmt.Sprintf("%v", materialized), charset)
		if err != nil {
			return DMPField{}, false, err
		}
		return DMPLongField(raw), false, nil
	}
	if lob, ok := value.(dmLOBValue); ok {
		return dmpFieldForLOBValue(col, lob, charset)
	}
	switch typ {
	case "BIT", "BOOL", "BOOLEAN", "BYTE", "TINYINT", "SMALLINT", "INT", "INTEGER", "PLS_INTEGER", "BIGINT",
		"REAL", "BINARY_FLOAT", "FLOAT", "DOUBLE", "DOUBLE PRECISION", "BINARY_DOUBLE", "NUMBER", "NUMERIC", "DEC", "DECIMAL":
		return DMPShortField([]byte(fmt.Sprintf("%v", value))), false, nil
	case "DATE":
		year, month, day, err := parseDMPDate(fmt.Sprintf("%v", value))
		if err != nil {
			return DMPField{}, false, err
		}
		raw := make([]byte, 6)
		binary.LittleEndian.PutUint16(raw[0:2], uint16(year))
		binary.LittleEndian.PutUint16(raw[2:4], uint16(month))
		binary.LittleEndian.PutUint16(raw[4:6], uint16(day))
		return DMPShortField(raw), false, nil
	case "TIME":
		hour, minute, second, microsecond, _, err := parseDMPTime(fmt.Sprintf("%v", value))
		if err != nil {
			return DMPField{}, false, err
		}
		raw := make([]byte, 6)
		binary.LittleEndian.PutUint16(raw[0:2], uint16(hour))
		binary.LittleEndian.PutUint16(raw[2:4], uint16(minute))
		binary.LittleEndian.PutUint16(raw[4:6], uint16(second))
		return DMPShortField(raw), microsecond != 0, nil
	case "TIME WITH TIME ZONE", "DATETIME WITH TIME ZONE", "TIMESTAMP WITH TIME ZONE":
		return DMPShortField([]byte(fmt.Sprintf("%v", value))), false, nil
	case "DATETIME", "TIMESTAMP", "TIMESTAMP WITH LOCAL TIME ZONE":
		raw, err := encodeDMPTimestamp(fmt.Sprintf("%v", value))
		if err != nil {
			return DMPField{}, false, err
		}
		return DMPShortField(raw), false, nil
	case "INTERVAL YEAR", "INTERVAL MONTH", "INTERVAL YEAR TO MONTH",
		"INTERVAL DAY", "INTERVAL HOUR", "INTERVAL MINUTE", "INTERVAL SECOND",
		"INTERVAL DAY TO HOUR", "INTERVAL DAY TO MINUTE", "INTERVAL DAY TO SECOND",
		"INTERVAL HOUR TO MINUTE", "INTERVAL HOUR TO SECOND", "INTERVAL MINUTE TO SECOND":
		return DMPShortField([]byte(formatDMPInterval(fmt.Sprintf("%v", value), strings.TrimPrefix(typ, "INTERVAL ")))), false, nil
	case "BFILE":
		raw, err := encodeDMPText(fmt.Sprintf("%v", value), charset)
		if err != nil {
			return DMPField{}, false, err
		}
		return DMPShortField(raw), false, nil
	case "ROWID":
		return DMPShortField([]byte(fmt.Sprintf("%v", value))), false, nil
	}

	if isBinaryDataType(typ) {
		raw, ok := value.(dmBinary)
		if !ok {
			return DMPField{}, false, fmt.Errorf("binary value has type %T", value)
		}
		if isBinaryLOBDataType(typ) {
			return DMPLongField([]byte(raw)), false, nil
		}
		encoded := make([]byte, hex.EncodedLen(len(raw)))
		hex.Encode(encoded, raw)
		return DMPShortField(encoded), false, nil
	}
	if isCharacterDataType(typ) {
		raw, err := encodeDMPText(fmt.Sprintf("%v", value), charset)
		if err != nil {
			return DMPField{}, false, err
		}
		if isCharacterLOBDataType(typ) {
			return DMPLongField(raw), false, nil
		}
		return DMPShortField(raw), false, nil
	}
	return DMPField{}, false, fmt.Errorf("unsupported dmp data type %s", col.DataType)
}

func dmpFieldForLOBValue(col columnDef, lob dmLOBValue, charset dmpCharsetHeader) (DMPField, bool, error) {
	if lob.text && !sameDMPCharset(lob.decoder.preferred, charset.Name) {
		value, err := materializeDataValue(lob)
		if err != nil {
			return DMPField{}, false, err
		}
		raw, err := encodeDMPText(fmt.Sprintf("%v", value), charset)
		if err != nil {
			return DMPField{}, false, err
		}
		return DMPLongField(raw), false, nil
	}
	reader, err := lob.open()
	if err != nil {
		return DMPField{}, false, err
	}
	if !lob.text && !isBinaryLOBDataType(normalizeDataType(col.DataType)) {
		return DMPField{}, false, fmt.Errorf("streamed binary value is only supported for BLOB/IMAGE/LONGVARBINARY")
	}
	return DMPLongReaderField(reader, uint64(lob.locator.byteLen)), false, nil
}

func sameDMPCharset(left string, right string) bool {
	normalize := func(value string) string {
		value = normalizeCharsetToken(value)
		if value == "GBK" || value == "CP936" {
			return "GB18030"
		}
		return value
	}
	return normalize(left) == normalize(right)
}

func isBinaryLOBDataType(dataType string) bool {
	switch normalizeDataType(dataType) {
	case "BLOB", "IMAGE", "LONGVARBINARY", "LONG RAW":
		return true
	default:
		return false
	}
}

func parseDMPDate(value string) (int, int, int, error) {
	value = strings.TrimSpace(value)
	if len(value) < 10 {
		return 0, 0, 0, fmt.Errorf("invalid DATE value %q", value)
	}
	parts := strings.Split(value[:10], "-")
	if len(parts) != 3 {
		return 0, 0, 0, fmt.Errorf("invalid DATE value %q", value)
	}
	year, err1 := strconv.Atoi(parts[0])
	month, err2 := strconv.Atoi(parts[1])
	day, err3 := strconv.Atoi(parts[2])
	if err1 != nil || err2 != nil || err3 != nil || year < 1 || year > 0xFFFF || month < 1 || month > 12 || day < 1 || day > 31 {
		return 0, 0, 0, fmt.Errorf("invalid DATE value %q", value)
	}
	return year, month, day, nil
}

func parseDMPTime(value string) (int, int, int, int, string, error) {
	value = strings.TrimSpace(value)
	timezone := ""
	if len(value) >= 6 {
		candidate := value[len(value)-6:]
		if (candidate[0] == '+' || candidate[0] == '-') && candidate[3] == ':' {
			timezone = candidate
			value = strings.TrimSpace(value[:len(value)-6])
		}
	}
	mainAndFraction := strings.SplitN(value, ".", 2)
	parts := strings.Split(mainAndFraction[0], ":")
	if len(parts) != 3 {
		return 0, 0, 0, 0, "", fmt.Errorf("invalid TIME value %q", value)
	}
	hour, err1 := strconv.Atoi(parts[0])
	minute, err2 := strconv.Atoi(parts[1])
	second, err3 := strconv.Atoi(parts[2])
	nanosecond := 0
	if len(mainAndFraction) == 2 {
		fraction := mainAndFraction[1]
		if len(fraction) > 9 {
			fraction = fraction[:9]
		}
		fraction += strings.Repeat("0", 9-len(fraction))
		nanosecond, _ = strconv.Atoi(fraction)
	}
	if err1 != nil || err2 != nil || err3 != nil || hour < 0 || hour > 23 || minute < 0 || minute > 59 || second < 0 || second > 59 {
		return 0, 0, 0, 0, "", fmt.Errorf("invalid TIME value %q", value)
	}
	return hour, minute, second, nanosecond, timezone, nil
}

func encodeDMPTimestamp(value string) ([]byte, error) {
	parts := strings.SplitN(strings.TrimSpace(value), " ", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid TIMESTAMP value %q", value)
	}
	year, month, day, err := parseDMPDate(parts[0])
	if err != nil {
		return nil, err
	}
	hour, minute, second, nanosecond, _, err := parseDMPTime(parts[1])
	if err != nil {
		return nil, err
	}
	raw := make([]byte, 16)
	for i, component := range []int{year, month, day, hour, minute, second} {
		binary.LittleEndian.PutUint16(raw[i*2:i*2+2], uint16(component))
	}
	binary.LittleEndian.PutUint32(raw[12:16], uint32(nanosecond))
	return raw, nil
}

func formatDMPInterval(value string, qualifier string) string {
	value = strings.TrimSpace(value)
	sign := ""
	if strings.HasPrefix(value, "-") {
		sign = "-"
		value = strings.TrimPrefix(value, "-")
	}
	return fmt.Sprintf("INTERVAL %s'%s' %s", sign, value, qualifier)
}
