package dm

import (
	"encoding/binary"
	"encoding/csv"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func storageRescueFixture(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	raw := make([]byte, 24*8192)
	binary.LittleEndian.PutUint32(raw[0x80:], 16)
	binary.LittleEndian.PutUint32(raw[0x84:], 8192)
	for p := uint32(16); p < 19; p++ {
		page := raw[p*8192 : (p+1)*8192]
		putTestIntDataPage(page, 4, 0, p, 45678, int32(p))
		if p == 18 {
			binary.BigEndian.PutUint16(page[dataRowAreaStart:], 7|dataRowDeletedMask)
			binary.LittleEndian.PutUint16(page[dataPageRecordCountOff:], 0)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "MAIN.DBF"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	cols := filepath.Join(dir, "manual.tsv")
	if err := os.WriteFile(cols, []byte("col_id\tname\tdata_type\tlength\tscale\tnullable\n0\tID\tINT\t4\t0\tN\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return dir, cols
}

func TestStorageScanWithoutSystemOrDictionary(t *testing.T) {
	dir, _ := storageRescueFixture(t)
	// Misleading sidecars must not influence the raw physical scan.
	if err := os.WriteFile(filepath.Join(dir, "dm.ctl"), []byte("not a control file"), 0600); err != nil {
		t.Fatal(err)
	}
	r, err := ScanStorages(StorageScanOptions{DataDir: dir, OutputDir: filepath.Join(dir, "out")})
	if err != nil {
		t.Fatal(err)
	}
	if r.Files != 1 || r.Storages != 1 || r.Samples != 3 {
		t.Fatalf("%+v", r)
	}
	f, err := os.Open(r.SummaryPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	reader := csv.NewReader(f)
	reader.Comma = '\t'
	recs, err := reader.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 || recs[1][4] != "45678" || recs[1][9] != "2" || recs[1][11] != "UNATTRIBUTED" {
		t.Fatalf("%v", recs)
	}
}

func TestStorageRecoveryManualColumnsAndResidual(t *testing.T) {
	for _, format := range []string{"sql", "fldr", "dmp"} {
		for _, residual := range []bool{false, true} {
			t.Run(format+map[bool]string{true: "-residual", false: "-slots"}[residual], func(t *testing.T) {
				dir, cols := storageRescueFixture(t)
				ext := map[string]string{"sql": "sql", "fldr": "txt", "dmp": "dmp"}[format]
				opts := StorageRecoveryOptions{DataDir: dir, OutputPath: filepath.Join(dir, "out", "recovered."+ext), ColumnsPath: cols, Owner: "RECOVERED", Table: "MANUAL", Charset: "utf-8", OutputFormat: format, GroupID: 4, StorageID: 45678, IncludeResidual: residual}
				caseSensitive := true
				opts.CaseSensitive = &caseSensitive
				r, err := RecoverStorage(opts)
				if err != nil {
					t.Fatal(err)
				}
				want := 2
				if residual {
					want = 3
				}
				if r.RowsExported != want || r.RowsFailed != 0 {
					t.Fatalf("%+v", r)
				}
				text, err := os.ReadFile(r.EvidencePath)
				if err != nil || !strings.Contains(string(text), "OPERATOR_SUPPLIED") {
					t.Fatalf("evidence: %q %v", text, err)
				}
				if format == "sql" {
					text, _ := os.ReadFile(opts.OutputPath)
					if !strings.Contains(string(text), "VALUES (16)") {
						t.Fatalf("%s", text)
					}
				}
				if _, err := RecoverStorage(opts); err == nil {
					t.Fatal("overwrote existing recovery")
				}
			})
		}
	}
}

func TestStorageRescueRejectsAmbiguousInputs(t *testing.T) {
	dir, cols := storageRescueFixture(t)
	opts := StorageRecoveryOptions{DataDir: dir, OutputPath: filepath.Join(dir, "out.sql"), ColumnsPath: cols, Owner: "R", Table: "T", GroupID: 4, StorageID: 45678, Charset: "auto"}
	if _, err := RecoverStorage(opts); err == nil {
		t.Fatal("guessed charset")
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "MAIN.DBF"))
	if err := os.WriteFile(filepath.Join(dir, "DUP.DBF"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ScanStorages(StorageScanOptions{DataDir: dir, OutputDir: filepath.Join(dir, "out")}); err == nil {
		t.Fatal("duplicate identity accepted")
	}
}

func TestStorageColumnsRejectOversizeAndMalformed(t *testing.T) {
	for _, raw := range []string{"wrong\theader\n", "col_id\tname\tdata_type\tlength\tscale\tnullable\n1\tID\tINT\t4\t0\tN\n", strings.Repeat("a", (1<<20)+1)} {
		path := filepath.Join(t.TempDir(), "manual.tsv")
		if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := readStorageColumns(path); err == nil {
			t.Fatal("malformed columns accepted")
		}
	}
}

func TestStorageRecoveryDoesNotPublishPartialOutput(t *testing.T) {
	dir, cols := storageRescueFixture(t)
	path := filepath.Join(dir, "MAIN.DBF")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	binary.LittleEndian.PutUint16(raw[18*8192+dataPageSlotCountOff:], 4096)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out", "rescue.sql")
	_, err = RecoverStorage(StorageRecoveryOptions{DataDir: dir, OutputPath: out, ColumnsPath: cols, Owner: "R", Table: "T", GroupID: 4, StorageID: 45678, Charset: "utf-8", OutputFormat: "sql"})
	if err == nil {
		t.Fatal("corrupt page accepted")
	}
	for _, name := range []string{out, out + ".evidence.tsv"} {
		if _, err := os.Stat(name); !os.IsNotExist(err) {
			t.Fatalf("partial output published: %s %v", name, err)
		}
	}
	entries, err := os.ReadDir(filepath.Dir(out))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("staging artifacts leaked: %v", entries)
	}
}

func TestStorageRecoveryRejectsTruncatedTail(t *testing.T) {
	dir, cols := storageRescueFixture(t)
	path := filepath.Join(dir, "MAIN.DBF")
	if err := os.Truncate(path, 24*8192-1); err != nil {
		t.Fatal(err)
	}
	_, err := RecoverStorage(StorageRecoveryOptions{DataDir: dir, OutputPath: filepath.Join(dir, "out.sql"), ColumnsPath: cols, Owner: "R", Table: "T", GroupID: 4, StorageID: 45678, Charset: "utf-8", OutputFormat: "sql"})
	if err == nil || !strings.Contains(err.Error(), "truncated") {
		t.Fatalf("%v", err)
	}
}
