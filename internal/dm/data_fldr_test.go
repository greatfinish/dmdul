package dm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The expectations below are pinned to what dmfldr V8 actually accepted on a
// live DM8 instance; see the comment block at the top of data_fldr.go.

func TestFldrDialectFollowsColumnTypes(t *testing.T) {
	numeric := []columnDef{
		{Name: "ID", DataType: "INT"},
		{Name: "AMT", DataType: "DEC(20,4)"},
		{Name: "TS", DataType: "TIMESTAMP(6)"},
		{Name: "RAW", DataType: "VARBINARY(100)"},
	}
	if got := fldrDialectForColumns(numeric); got.fieldSep != "|" {
		t.Fatalf("delimiter-safe columns should keep the readable pipe dialect, got %q", got.fieldSep)
	}
	withText := append(append([]columnDef(nil), numeric...), columnDef{Name: "NOTE", DataType: "VARCHAR(200)"})
	if got := fldrDialectForColumns(withText); got.fieldSep != "\x01" {
		t.Fatalf("text columns must force the control-byte dialect, got %q", got.fieldSep)
	}
	unknown := []columnDef{{Name: "X", DataType: "SOME_FUTURE_TYPE"}}
	if got := fldrDialectForColumns(unknown); got.fieldSep != "\x01" {
		t.Fatalf("unknown types must fail safe to the control-byte dialect, got %q", got.fieldSep)
	}
}

func TestFldrValueRendering(t *testing.T) {
	text := columnDef{Name: "NOTE", DataType: "VARCHAR(200)"}
	ts := columnDef{Name: "TS", DataType: "TIMESTAMP(9)"}
	num := columnDef{Name: "AMT", DataType: "DEC(38,18)"}
	bin := columnDef{Name: "RAW", DataType: "VARBINARY(10)"}

	for _, tc := range []struct {
		name    string
		col     columnDef
		value   any
		dialect fldrDialect
		want    string
		wantErr bool
	}{
		{name: "null becomes the sentinel", col: text, value: nil, dialect: fldrControlDialect, want: `\N`},
		{name: "empty string stays empty", col: text, value: "", dialect: fldrControlDialect, want: ""},
		{name: "pipes survive the control dialect", col: text, value: "a|b", dialect: fldrControlDialect, want: "a|b"},
		{name: "newlines survive the control dialect", col: text, value: "l1\nl2", dialect: fldrControlDialect, want: "l1\nl2"},
		{name: "binary is upper hex", col: bin, value: dmBinary{0xde, 0xad}, dialect: fldrPipeDialect, want: "DEAD"},
		{name: "subsecond tail is capped at six digits", col: ts, value: "2024-03-15 12:34:56.123456000", dialect: fldrPipeDialect, want: "2024-03-15 12:34:56.123456"},
		{name: "number mantissas are left alone", col: num, value: "1234567890.123456789", dialect: fldrPipeDialect, want: "1234567890.123456789"},
		{name: "a delimiter in a pipe-dialect value is refused", col: text, value: "a|b", dialect: fldrPipeDialect, wantErr: true},
		{name: "a value equal to the NULL marker is refused", col: text, value: `\N`, dialect: fldrControlDialect, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := fldrValueForDataColumn(tc.col, tc.value, tc.dialect)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestWriteFldrControlFile(t *testing.T) {
	dir := t.TempDir()
	dataPath := filepath.Join(dir, "APP_ORDERS_data.txt")
	controlPath := fldrControlFilePath(dataPath)
	columns := []columnDef{
		{Name: "ID", DataType: "INT"},
		{Name: "NOTE", DataType: "VARCHAR(200)"},
		{Name: "TS", DataType: "TIMESTAMP(6)"},
	}
	if err := WriteFldrControlFile(controlPath, dataPath, "APP", "ORDERS", columns); err != nil {
		t.Fatalf("write control file: %v", err)
	}
	raw, err := os.ReadFile(controlPath)
	if err != nil {
		t.Fatalf("read control file: %v", err)
	}
	body := string(raw)
	for _, want := range []string{
		"OPTIONS",
		"\tBLOB_TYPE = 'HEX_CHAR'\n", // plain 'HEX' stores the hex digits verbatim
		"\tNULL_MODE = TRUE\n",
		"\tNULL_STR = '\\\\N'\n",
		"\tCHARACTER_CODE = 'UTF-8'\n",
		"INFILE 'APP_ORDERS_data.txt' STR X '020A'\n", // NOTE forces the control-byte dialect
		"BADFILE 'APP_ORDERS_data.bad'\n",
		"INTO TABLE APP.ORDERS\n",
		"FIELDS X '01'\n",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("control file should contain %q, got:\n%s", want, body)
		}
	}
	// dmfldr has no ENCLOSED BY and rejects a column FORMAT it cannot match,
	// so neither may ever be emitted again.
	for _, forbidden := range []string{"ENCLOSED", "FORMAT"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("control file must not contain %q, got:\n%s", forbidden, body)
		}
	}
}

func TestWriteFldrControlFileKeepsPipeForDelimiterSafeTables(t *testing.T) {
	dir := t.TempDir()
	dataPath := filepath.Join(dir, "APP_FACTS_data.txt")
	controlPath := fldrControlFilePath(dataPath)
	columns := []columnDef{{Name: "ID", DataType: "BIGINT"}, {Name: "D", DataType: "DATE"}}
	if err := WriteFldrControlFile(controlPath, dataPath, "APP", "FACTS", columns); err != nil {
		t.Fatalf("write control file: %v", err)
	}
	raw, err := os.ReadFile(controlPath)
	if err != nil {
		t.Fatalf("read control file: %v", err)
	}
	body := string(raw)
	if !strings.Contains(body, "FIELDS '|'\n") || !strings.Contains(body, "STR X '0A'\n") {
		t.Fatalf("delimiter-safe table should use the pipe dialect, got:\n%s", body)
	}
	if !strings.Contains(body, "\tCHARACTER_CODE = 'UTF-8'\n") {
		t.Fatalf("control file should describe dmdul's UTF-8 data stream, got:\n%s", body)
	}
}
