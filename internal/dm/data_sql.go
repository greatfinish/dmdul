package dm

import (
	"encoding/hex"
	"fmt"
	"strings"
)

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
	case "VECTOR":
		return sqlLiteral(fmt.Sprintf("%v", value)), nil
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
