package dm

import (
	"encoding/binary"
	"fmt"
	"math"
	"strings"
)

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
		isJSONDataType(dataType) || isVectorDataType(dataType) || normalizeDataType(dataType) == "BFILE"
}

func isVectorDataType(dataType string) bool {
	return normalizeDataType(dataType) == "VECTOR"
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
