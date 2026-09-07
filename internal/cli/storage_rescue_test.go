package cli

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStorageCommandsDoNotRequireDictionary(t *testing.T) {
	dir := t.TempDir()
	raw := make([]byte, 24*8192)
	binary.LittleEndian.PutUint32(raw[0x84:], 8192)
	for p := uint32(16); p < 19; p++ {
		page := raw[p*8192 : (p+1)*8192]
		binary.LittleEndian.PutUint16(page, 4)
		binary.LittleEndian.PutUint32(page[4:], p)
		binary.LittleEndian.PutUint32(page[0x14:], 0x14)
		binary.LittleEndian.PutUint16(page[0x26:], 0x62)
		binary.LittleEndian.PutUint32(page[0x3a:], 1234)
	}
	if err := os.WriteFile(filepath.Join(dir, "MAIN.DBF"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	cols := filepath.Join(dir, "columns.tsv")
	if err := os.WriteFile(cols, []byte("col_id\tname\tdata_type\tlength\tscale\tnullable\n0\tID\tINT\t4\t0\tN\n"), 0600); err != nil {
		t.Fatal(err)
	}
	s := newInteractiveSession()
	s.dataDir, s.dataDirSet = dir, true
	s.outputDir, s.outputDirSet = filepath.Join(dir, "out"), true
	s.charset = "utf-8"
	defer s.closeLog()
	var out bytes.Buffer
	if _, err := s.execute("scan storage;", &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "UNATTRIBUTED") {
		t.Fatal(out.String())
	}
	if _, err := s.execute("recover storage 4.1234 using \""+cols+"\" as RESCUE.T;", &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "OPERATOR_SUPPLIED") || s.dictionary != nil {
		t.Fatal(out.String())
	}
	for _, command := range []string{"scan", "scan unknown", "storage_scan extra", "recover storage", "recover storage 4.1 using x as T", "recover storage 4.1 using x as A.T typo"} {
		if _, err := s.execute(command, &out); err == nil {
			t.Errorf("accepted malformed command: %s", command)
		}
	}
}
