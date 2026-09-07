package cli

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dmdul/internal/dm"
)

func TestExecuteCheckWritesImpactReports(t *testing.T) {
	const pageSize = 8192
	dataDir := t.TempDir()
	outputDir := t.TempDir()
	raw := make([]byte, 2*pageSize)
	for pageNo := 0; pageNo < 2; pageNo++ {
		page := raw[pageNo*pageSize : (pageNo+1)*pageSize]
		binary.LittleEndian.PutUint16(page[0:], 4)
		binary.LittleEndian.PutUint16(page[2:], 0)
		binary.LittleEndian.PutUint32(page[4:], uint32(pageNo))
		binary.LittleEndian.PutUint32(page[0x14:], 0x14)
		binary.LittleEndian.PutUint16(page[0x26:], 0x62)
		binary.LittleEndian.PutUint32(page[0x3A:], 1042)
	}
	// Keep page 0 healthy so header discovery identifies group/file; corrupt
	// page 1's self-id to produce one deterministic HEADER_INVALID result.
	binary.LittleEndian.PutUint32(raw[0x84:], pageSize)
	binary.LittleEndian.PutUint32(raw[pageSize+4:], 999)
	if err := os.WriteFile(filepath.Join(dataDir, "MAIN.DBF"), raw, 0644); err != nil {
		t.Fatal(err)
	}

	session := newInteractiveSession()
	session.dataDir = dataDir
	session.dataDirSet = true
	session.outputDir = outputDir
	session.outputDirSet = true
	session.metadata.PageSize = pageSize
	defer session.closeLog()
	session.dictionary = &dm.DictionaryInfo{Tables: []dm.DictionaryTable{{
		ID: 88, Owner: "HR_TEST", Name: "EMP_INFO", Tablespace: "MAIN", GroupID: 4,
		HeaderFile: 0, HeaderBlock: 0, Blocks: 2, Bytes: 2 * pageSize,
		StorageID: 1042, RootFile: 0, RootPage: 0,
	}}}
	var stdout bytes.Buffer
	if err := session.executeCheck([]string{"pages", "MAIN.DBF"}, &stdout); err != nil {
		t.Fatalf("executeCheck failed: %v", err)
	}
	for _, want := range []string{
		"bad pages total: 1", "attributed bad pages: 1", "affected tables: 1 / 1",
		"summary report: " + filepath.Join(outputDir, pageCheckSummaryName),
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("check output missing %q:\n%s", want, stdout.String())
		}
	}
	for _, name := range []string{pageCheckSummaryName, pageCheckBadPagesName, pageCheckAffectedObjectsName} {
		if _, err := os.Stat(filepath.Join(outputDir, name)); err != nil {
			t.Fatalf("missing %s: %v", name, err)
		}
	}
}

func TestBootstrapSystemPrecheckRunsBeforeDictionaryRecovery(t *testing.T) {
	const pageSize = 8192
	dir := t.TempDir()
	systemPath := filepath.Join(dir, "SYSTEM.DBF")
	raw := make([]byte, 2*pageSize)
	for pageNo := 0; pageNo < 2; pageNo++ {
		page := raw[pageNo*pageSize : (pageNo+1)*pageSize]
		binary.LittleEndian.PutUint16(page[0:], 0)
		binary.LittleEndian.PutUint16(page[2:], 0)
		binary.LittleEndian.PutUint32(page[4:], uint32(pageNo))
		binary.LittleEndian.PutUint32(page[0x14:], 0x1A1A001A)
		binary.LittleEndian.PutUint32(page[0x3A:], 500)
	}
	if err := os.WriteFile(systemPath, raw, 0644); err != nil {
		t.Fatal(err)
	}
	session := newInteractiveSession()
	session.dataDir = dir
	session.dataDirSet = true
	defer session.closeLog()
	files := []dm.OfflineDataFile{{GroupID: 0, FileID: 0, Tablespace: "SYSTEM", Path: systemPath}}
	var stdout bytes.Buffer
	if warning := session.runBootstrapSystemPrecheck(&stdout, systemPath, files, pageSize); warning {
		t.Fatalf("clean SYSTEM precheck returned warning:\n%s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "phase=precheck name=SYSTEM.DBF status=OK") ||
		!strings.Contains(stdout.String(), "pages=2") || !strings.Contains(stdout.String(), "bad=0") {
		t.Fatalf("clean precheck output incomplete:\n%s", stdout.String())
	}

	binary.LittleEndian.PutUint32(raw[pageSize+4:], 999)
	if err := os.WriteFile(systemPath, raw, 0644); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if warning := session.runBootstrapSystemPrecheck(&stdout, systemPath, files, pageSize); !warning {
		t.Fatalf("corrupt SYSTEM precheck did not return warning:\n%s", stdout.String())
	}
	for _, want := range []string{
		"phase=precheck name=SYSTEM.DBF status=WARNING", "bad=1", "header=1",
		"name=SYSTEM.DBF-page", "coordinate=\"page(0,0,1)\"",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("corrupt precheck output missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestExecuteCheckDoesNotAutoLoadLeftoverDictionary(t *testing.T) {
	const pageSize = 8192
	dataDir := t.TempDir()
	outputDir := t.TempDir()
	raw := make([]byte, 2*pageSize)
	for pageNo := 0; pageNo < 2; pageNo++ {
		page := raw[pageNo*pageSize : (pageNo+1)*pageSize]
		binary.LittleEndian.PutUint16(page[0:], 4)
		binary.LittleEndian.PutUint16(page[2:], 0)
		binary.LittleEndian.PutUint32(page[4:], uint32(pageNo))
		binary.LittleEndian.PutUint32(page[0x14:], 0x14)
		binary.LittleEndian.PutUint16(page[0x26:], 0x62)
		binary.LittleEndian.PutUint32(page[0x3A:], 1042)
	}
	binary.LittleEndian.PutUint32(raw[0x84:], pageSize)
	binary.LittleEndian.PutUint32(raw[pageSize+4:], 999)
	if err := os.WriteFile(filepath.Join(dataDir, "MAIN.DBF"), raw, 0644); err != nil {
		t.Fatal(err)
	}
	_, err := dm.WriteDictionaryFiles(filepath.Join(dataDir, dm.DefaultDictionaryDirName), &dm.DictionaryInfo{
		SystemPath: filepath.Join(dataDir, "OLD_SYSTEM.DBF"), PageSize: pageSize,
		Users: []dm.DictionaryUser{{ID: 1, Name: "STALE"}},
		Tables: []dm.DictionaryTable{{
			ID: 88, Owner: "STALE", Name: "WRONG_TABLE", GroupID: 4,
			HeaderFile: 0, HeaderBlock: 0, Blocks: 2, StorageID: 1042,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	session := newInteractiveSession()
	session.dataDir = dataDir
	session.dataDirSet = true
	session.outputDir = outputDir
	session.outputDirSet = true
	session.metadata.PageSize = pageSize
	defer session.closeLog()
	var stdout bytes.Buffer
	if err := session.executeCheck([]string{"pages", "MAIN.DBF"}, &stdout); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "dictionary: not loaded (physical scan only") {
		t.Fatalf("missing physical-only warning:\n%s", stdout.String())
	}
	badTSV, err := os.ReadFile(filepath.Join(outputDir, pageCheckBadPagesName))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(badTSV), "WRONG_TABLE") || !strings.Contains(string(badTSV), "UNATTRIBUTED") {
		t.Fatalf("leftover dictionary affected physical check:\n%s", badTSV)
	}
}

func TestPageCheckReportsContainFullCoordinatesAndImpactSummary(t *testing.T) {
	dir := t.TempDir()
	writeReports := func(pageNo uint32) pageCheckReportPaths {
		writer, err := newPageCheckReportWriter(dir, 8192)
		if err != nil {
			t.Fatal(err)
		}
		defer writer.abort()
		bad := dm.BadPage{
			Path: "D:/snapshot/MAIN.DBF", Tablespace: "MAIN", GroupID: 4, FileID: 0,
			PageNo: pageNo, StorageID: 1042, Owner: "HR_TEST", Table: "EMP_INFO", TableID: 88,
			ObjectType: dm.PageObjectTableAssist, ObjectStorageID: 1042,
			Attribution: dm.PageAttributionAssistStorageID, AttributionConfidence: dm.PageAttributionHigh,
			Kind: dm.PageCorruptionChecksum, Detail: "PAGE_CHECK=3 checksum mismatch",
		}
		if err := writer.writeBadPage(bad); err != nil {
			t.Fatal(err)
		}
		result := &dm.PageCheckResult{
			PageSize: 8192, PageCheckMode: 3, PageHashName: "SHA256", DictionaryUsed: true,
			FilesChecked: 1, PagesChecked: 100, PagesEmpty: 10, BadPagesTotal: 1,
			Corruption:         map[dm.PageCorruptionKind]int{dm.PageCorruptionChecksum: 1},
			AttributedBadPages: 1, AffectedTables: 1, TotalTables: 10,
			AffectedTableBytes: 8192 * 16, TotalTableBytes: 8192 * 160,
			AffectedObjects: []dm.PageAffectedObject{{
				Owner: "HR_TEST", Table: "EMP_INFO", TableID: 88,
				ObjectType: dm.PageObjectTableAssist, StorageID: 1042, Tablespace: "MAIN",
				HeaderFile: -1, Attribution: "assist_storage_id", AttributionConfidence: dm.PageAttributionHigh,
				BadPages: 1, ChecksumFail: 1,
			}},
			Files: []dm.PageCheckFileResult{{
				Path: "D:/snapshot/MAIN.DBF", GroupID: 4, FileID: 0, Tablespace: "MAIN",
				PagesChecked: 100, PagesEmpty: 10, BadPages: 1,
			}},
		}
		paths, err := writer.finalize(result, pageCheckReportContext{
			SystemPath: "D:/snapshot/SYSTEM.DBF", DataDir: "D:/snapshot",
			GeneratedAt: time.Date(2026, 8, 17, 12, 0, 0, 0, time.FixedZone("CST", 8*3600)),
		})
		if err != nil {
			t.Fatal(err)
		}
		return paths
	}

	paths := writeReports(1)
	// Generate the same deterministic report names again to cover Windows
	// replacement semantics when reports from an earlier run already exist.
	paths = writeReports(2)
	for _, path := range []string{paths.Summary, paths.BadPages, paths.AffectedObjects} {
		if filepath.Dir(path) != dir {
			t.Fatalf("report escaped output directory: %s", path)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("report missing %s: %v", path, err)
		}
	}
	badTSV, err := os.ReadFile(paths.BadPages)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"byte_offset_hex", "\t2\t16384\t0x4000\t1042\tCHECKSUM_FAIL\tTABLE_ASSIST", "HR_TEST\tEMP_INFO"} {
		if !strings.Contains(string(badTSV), want) {
			t.Fatalf("bad-page TSV missing %q:\n%s", want, badTSV)
		}
	}
	summary, err := os.ReadFile(paths.Summary)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"DMDUL 离线坏页检查报告", "坏页占扫描页比例", "TABLE_ASSIST", "当前持久化字典不足以继续区分 INDEX、LOB"} {
		if !strings.Contains(string(summary), want) {
			t.Fatalf("summary missing %q:\n%s", want, summary)
		}
	}
	affected, err := os.ReadFile(paths.AffectedObjects)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(affected), "HR_TEST\tEMP_INFO\t88\tTABLE_ASSIST\t1042") {
		t.Fatalf("affected-object TSV missing object row:\n%s", affected)
	}
}
