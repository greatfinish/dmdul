package dm

// dmfldr (DM Fast Loader) output: a delimited .txt data file plus a complete
// .ctl control file per table, so recovered data can be bulk-loaded back into a
// Dameng instance instead of only being eyeballed.
//
// The format below is what dmfldr V8 actually accepts — every rule was probed
// against /dm8/bin/dmfldr on a live DM8 instance, because the shipped grammar
// is narrower than the documentation suggests:
//
//   - There is NO working enclosure. `FIELDS '|' ENCLOSED BY '"'` is a syntax
//     error, and in DB2_MODE a quoted field containing the separator still
//     splits at the separator. Quoting a value therefore cannot protect it; the
//     separator itself has to be chosen so it cannot occur in the data.
//   - `ESCAPED BY` parses but does not unescape: `a\|b` loads as `a\`. So
//     escaping cannot protect a value either.
//   - Command-line parameter values containing '.' or '-' must be quoted:
//     CONTROL='x.ctl', not CONTROL=x.ctl.
//   - OPTIONS values that are bare words must be quoted too: BLOB_TYPE = 'HEX'.
//   - NULL_MODE = TRUE + NULL_STR makes an empty field an empty string and the
//     sentinel a NULL; without it every empty field is NULL and empty strings
//     cannot be expressed.
//   - Every recovered type loads correctly with no FORMAT clause at all, so
//     none is emitted; a wrong-precision FORMAT only creates failures.
//
// Given no enclosure and no escape, framing safety comes from the delimiter
// choice. Tables whose columns can never render '|', CR or LF get the readable
// pipe dialect the operator expects. Tables with text columns get delimiters
// built from C0 control bytes, which dmdul already refuses to emit inside a
// value (see containsBadControl), making a collision impossible rather than
// unlikely. Either way the .ctl declares exactly what the .txt contains.

import (
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// fldrNullString is the NULL sentinel written into the data file and declared
// as NULL_STR. It is the same marker mysqldump-style loaders use.
const fldrNullString = `\N`

// fldrDialect is the delimiter pair a table's data file and control file agree
// on. fieldSep/rowTerm are the literal bytes written; fieldsClause/rowTermHex
// are how they are spelled in the control file.
type fldrDialect struct {
	fieldSep     string
	rowTerm      string
	fieldsClause string
	rowTermHex   string
}

// fldrPipeDialect is the readable default: '|' between fields, LF between rows.
// Only used where no column can render one of those bytes.
var fldrPipeDialect = fldrDialect{
	fieldSep:     "|",
	rowTerm:      "\n",
	fieldsClause: `'|'`,
	rowTermHex:   "0A",
}

// fldrControlDialect is the collision-proof fallback for tables with text
// columns: SOH between fields, STX+LF between rows. dmdul never writes a C0
// control byte inside a value, so neither delimiter can appear in the data,
// and the trailing LF keeps the file line-oriented for tools like wc and split.
var fldrControlDialect = fldrDialect{
	fieldSep:     "\x01",
	rowTerm:      "\x02\n",
	fieldsClause: `X '01'`,
	rowTermHex:   "020A",
}

func (d fldrDialect) resolved() fldrDialect {
	if d.fieldSep == "" {
		return fldrPipeDialect
	}
	return d
}

// fldrDialectForColumns picks the delimiters for a table. Any column that can
// render free text forces the control-byte dialect, because '|' and newlines
// are ordinary characters in addresses, notes and JSON.
func fldrDialectForColumns(columns []columnDef) fldrDialect {
	for _, col := range columns {
		if fldrColumnRendersText(col) {
			return fldrControlDialect
		}
	}
	return fldrPipeDialect
}

// fldrColumnRendersText reports whether a column's exported text can contain
// arbitrary characters. Numbers, dates, intervals and hex-encoded binaries
// cannot; anything character-shaped can. Unknown types are treated as text so
// a type dmdul has not seen before fails safe.
func fldrColumnRendersText(col columnDef) bool {
	switch dataType := normalizeDataType(col.DataType); dataType {
	case "BIT", "BOOL", "BOOLEAN", "BYTE", "TINYINT", "SMALLINT", "INT", "INTEGER", "PLS_INTEGER", "BIGINT",
		"DEC", "DECIMAL", "NUMERIC", "NUMBER", "FLOAT", "DOUBLE", "DOUBLE PRECISION", "REAL",
		"BINARY", "VARBINARY", "LONGVARBINARY", "RAW", "BLOB", "IMAGE", "BFILE", "ROWID":
		return false
	default:
		if strings.HasPrefix(dataType, "DATE") || strings.HasPrefix(dataType, "TIME") ||
			strings.HasPrefix(dataType, "TIMESTAMP") || strings.HasPrefix(dataType, "INTERVAL") {
			return false
		}
		return true
	}
}

// fldrValueForDataColumn renders one recovered value as a dmfldr text field.
func fldrValueForDataColumn(col columnDef, value any, dialect fldrDialect) (string, error) {
	materialized, err := materializeDataValue(value)
	if err != nil {
		return "", fmt.Errorf("%s: %w", col.Name, err)
	}
	if materialized == nil {
		return fldrNullString, nil
	}
	if raw, ok := materialized.(dmBinary); ok {
		return strings.ToUpper(hex.EncodeToString(raw)), nil
	}
	text := fmt.Sprintf("%v", materialized)
	if strings.ContainsRune(text, '�') || containsBadControl(text) {
		return "", fmt.Errorf("invalid text value for %s", col.Name)
	}
	if fldrColumnHasSubsecond(col) {
		text = trimFldrFraction(text)
	}
	// Nothing in this format can quote or escape a delimiter, so a value that
	// contains one has to fail loudly. fldrDialectForColumns is chosen so this
	// is unreachable for well-typed columns; it is the backstop for a type
	// misclassified as delimiter-safe.
	if strings.Contains(text, dialect.fieldSep) || strings.ContainsAny(text, "\r\n") && dialect.rowTerm == "\n" {
		return "", fmt.Errorf("%s contains the dmfldr delimiter and cannot be framed", col.Name)
	}
	if text == fldrNullString {
		return "", fmt.Errorf("%s equals the dmfldr NULL marker %s and cannot be distinguished from NULL", col.Name, fldrNullString)
	}
	return text, nil
}

// fldrColumnHasSubsecond reports whether a column's text form ends in a
// fractional-seconds tail. Only these get trimFldrFraction: applying it to a
// number would truncate the mantissa instead.
func fldrColumnHasSubsecond(col columnDef) bool {
	dataType := normalizeDataType(col.DataType)
	return strings.HasPrefix(dataType, "TIME") || strings.HasPrefix(dataType, "TIMESTAMP") ||
		strings.HasPrefix(dataType, "DATETIME")
}

// trimFldrFraction caps a fractional-seconds tail at six digits. dmdul renders
// as many digits as the column's declared scale, but DM stores at most six and
// dmfldr rejects a longer fraction outright, so a TIMESTAMP(9) value would fail
// to load with the trailing zeros dmdul pads it to.
func trimFldrFraction(text string) string {
	dot := strings.LastIndexByte(text, '.')
	if dot < 0 {
		return text
	}
	end := dot + 1
	for end < len(text) && text[end] >= '0' && text[end] <= '9' {
		end++
	}
	if end-dot-1 <= 6 {
		return text
	}
	return text[:dot+7] + text[end:]
}

// renderFldrForDataRowWithMeta decodes one row into dmfldr text fields. It
// mirrors the SQL renderer's structure so the row pipeline (including the
// parallel decode workers) is unchanged; only the value encoding differs.
func renderFldrForDataRowWithMeta(info dataTableInfo, row []byte, decoder textDecoder) ([]string, int, int, dataRowRenderMeta, error) {
	values, dataStart, dataEnd, err := parseDataRowValues(row, info.columns, decoder, info.historicalRows, info.lobReader)
	if err != nil {
		return nil, 0, 0, dataRowRenderMeta{}, err
	}
	dialect := info.fldrDialect.resolved()
	record := make([]string, 0, len(info.columns))
	for _, col := range info.columns {
		value, ok := values[col.ColID]
		if !ok {
			record = append(record, fldrNullString)
			continue
		}
		text, err := fldrValueForDataColumn(col, value.value, dialect)
		if err != nil {
			return nil, 0, 0, dataRowRenderMeta{}, err
		}
		record = append(record, text)
	}
	return record, dataStart, dataEnd, dataRowRenderMetaForValues(info.columns, values, info.coverage.active()), nil
}

// fldrControlFilePath maps a data file path to its control file sibling:
// OWNER_TABLE_data.txt -> OWNER_TABLE_data.ctl
func fldrControlFilePath(dataPath string) string {
	ext := filepath.Ext(dataPath)
	return strings.TrimSuffix(dataPath, ext) + ".ctl"
}

// fldrBadFilePath is where dmfldr records rows it rejects.
func fldrBadFilePath(dataPath string) string {
	ext := filepath.Ext(dataPath)
	return strings.TrimSuffix(dataPath, ext) + ".bad"
}

// WriteFldrControlFile emits the dmfldr control file that loads dataPath back
// into owner.table.
func WriteFldrControlFile(controlPath string, dataPath string, owner string, table string, columns []columnDef, charset string) error {
	if strings.TrimSpace(controlPath) == "" {
		return nil
	}
	if dir := filepath.Dir(controlPath); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("create control file directory: %w", err)
		}
	}
	dialect := fldrDialectForColumns(columns)
	var out strings.Builder
	out.WriteString("-- Generated by dmdul. Create the table first (companion _ddl.sql), then load with:\n")
	out.WriteString(fmt.Sprintf("--   dmfldr USERID=SYSDBA/password@127.0.0.1:5236 CONTROL=\"'%s'\"\n", filepath.Base(controlPath)))
	out.WriteString("-- The single quotes must reach dmfldr itself, which is why they are wrapped in\n")
	out.WriteString("-- double quotes: dmfldr rejects an unquoted value containing '.' (it reports\n")
	out.WriteString("-- \"parameters parse error\" naming the part before the dot), and a POSIX shell\n")
	out.WriteString("-- strips bare single quotes before dmfldr ever sees them. Conversely, do not\n")
	out.WriteString("-- quote values without a dot -- DIRECT='TRUE' is itself a parse error.\n")
	if dialect.fieldSep == fldrControlDialect.fieldSep {
		out.WriteString("-- Fields are separated by SOH (0x01) and rows by STX+LF (0x02 0x0A): this table has\n")
		out.WriteString("-- text columns, and dmfldr supports neither enclosure nor escaping, so a printable\n")
		out.WriteString("-- separator could not be told apart from column content.\n")
	}
	out.WriteString("OPTIONS\n(\n")
	out.WriteString("\tSKIP = 0\n")
	out.WriteString("\tROWS = 50000\n")
	out.WriteString("\tERRORS = 100\n")
	// DIRECT=FALSE keeps BLOB_TYPE usable; hex is how dmdul writes binary.
	// HEX_CHAR, not HEX: 'HEX' stores the hex digits themselves, doubling the
	// length of every BLOB, while 'HEX_CHAR' decodes them back to bytes.
	out.WriteString("\tDIRECT = FALSE\n")
	out.WriteString("\tBLOB_TYPE = 'HEX_CHAR'\n")
	// Without NULL_MODE every empty field is NULL and an empty string cannot
	// be represented; with it, NULL is the explicit sentinel instead.
	out.WriteString("\tNULL_MODE = TRUE\n")
	out.WriteString(fmt.Sprintf("\tNULL_STR = '%s'\n", strings.ReplaceAll(fldrNullString, `\`, `\\`)))
	if code := fldrCharacterCode(charset); code != "" {
		out.WriteString(fmt.Sprintf("\tCHARACTER_CODE = '%s'\n", code))
	}
	out.WriteString(")\n")
	out.WriteString("LOAD DATA\n")
	out.WriteString(fmt.Sprintf("INFILE '%s' STR X '%s'\n", filepath.Base(dataPath), dialect.rowTermHex))
	out.WriteString(fmt.Sprintf("BADFILE '%s'\n", filepath.Base(fldrBadFilePath(dataPath))))
	out.WriteString(fmt.Sprintf("INTO TABLE %s.%s\n", quoteIdent(owner), quoteIdent(table)))
	out.WriteString(fmt.Sprintf("FIELDS %s\n", dialect.fieldsClause))
	out.WriteString("(\n")
	clauses := make([]string, 0, len(columns))
	for _, col := range columns {
		// No FORMAT clause: every recovered type round-trips through dmfldr's
		// default parsing, and a fixed FORMAT breaks the moment a column's
		// fractional-seconds scale differs from the one guessed here.
		clauses = append(clauses, "\t"+quoteIdent(col.Name))
	}
	out.WriteString(strings.Join(clauses, ",\n"))
	out.WriteString("\n)\n")
	return os.WriteFile(controlPath, []byte(out.String()), 0644)
}

// fldrCharacterCode maps a recovered charset name to dmfldr's CHARACTER_CODE.
func fldrCharacterCode(charset string) string {
	switch strings.ToUpper(strings.TrimSpace(charset)) {
	case "UTF-8", "UTF8":
		return "UTF-8"
	case "GB18030", "GBK", "GB2312":
		return "GBK"
	case "EUC-KR", "EUCKR":
		return "EUC-KR"
	default:
		return ""
	}
}
