package cli

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dmdul/internal/dm"
	"dmdul/internal/version"
)

func TestRunHelp(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	if err := Run([]string{"help"}, &stdout, &stderr); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if !strings.Contains(stdout.String(), "dmdul") {
		t.Fatalf("help output should mention dmdul, got %q", stdout.String())
	}
	for _, want := range []string{
		"bootstrap;", "list datafile;", "list asmfile;", "list schema [owner];",
		"describe <owner.table_name>;", "unload object <owner|all>;",
		"unload schema <schema>[,<schema>...];", "unload database;",
		"recover table", "check pages", "set asm_disk",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("help output should contain %q, got %q", want, stdout.String())
		}
	}
	for _, removed := range []string{"inspect-ctl", "scan-system", "scan-partitions", "export-ddl", "export-data"} {
		if strings.Contains(stdout.String(), removed) {
			t.Fatalf("help output should not advertise removed command %q", removed)
		}
	}
	if strings.Contains(stdout.String(), "show dmp") {
		t.Fatal("help output should not advertise the experimental show dmp command")
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr should be empty, got %q", stderr.String())
	}
}

func TestRunInteractiveHelpAndExit(t *testing.T) {
	previousDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd failed: %v", err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatalf("Chdir failed: %v", err)
	}
	defer func() {
		if err := os.Chdir(previousDir); err != nil {
			t.Fatalf("restore Chdir failed: %v", err)
		}
	}()

	var stdout bytes.Buffer
	var stderr bytes.Buffer

	if err := RunInteractive(strings.NewReader("help;\nexit;\n"), &stdout, &stderr); err != nil {
		t.Fatalf("RunInteractive returned error: %v", err)
	}
	output := stdout.String()
	for _, want := range []string{"dmdul: Release " + version.Version, "Dameng Database Offline Recovery & Data Unloader", "Copyright (c) 2026 greatfinish", "https://github.com/greatfinish/dmdul", "DMDUL>", "bootstrap;", "list asmfile;", "set asm_disk", "list user;", "unload table", "unload object", "unload database", "recover table", "bye"} {
		if !strings.Contains(output, want) {
			t.Fatalf("interactive output should contain %q, got %q", want, output)
		}
	}
}

func TestASMDisksPersistInInitDUL(t *testing.T) {
	session := newInteractiveSession()
	session.asmDisks = splitASMDisks("/dev/asmdisk/data01, /dev/asmdisk/data02,/dev/asmdisk/data01")
	if len(session.asmDisks) != 2 {
		t.Fatalf("asm disks = %#v", session.asmDisks)
	}
	content := session.initDULContent()
	if !strings.Contains(content, "asm_disk=/dev/asmdisk/data01,/dev/asmdisk/data02") {
		t.Fatalf("init.dul does not persist ASM disks:\n%s", content)
	}
	path := filepath.Join(t.TempDir(), "init.dul")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	loaded := newInteractiveSession()
	if err := loaded.loadInitDUL(path); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(loaded.asmDisks, ","); got != "/dev/asmdisk/data01,/dev/asmdisk/data02" {
		t.Fatalf("reloaded asm_disk = %q", got)
	}
}

func TestInteractiveOutputDirDefaultsToDedicatedSubdirectory(t *testing.T) {
	session := newInteractiveSession()
	currentDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(currentDir, "output", "HR_TEST_data.sql")
	if got := session.outputPath("HR_TEST_data.sql"); got != want {
		t.Fatalf("default outputPath = %q", got)
	}
	session.dataDir = `D:\temp\oldpro`
	session.dataDirSet = true
	if got := session.outputPath("HR_TEST_data.sql"); got != want {
		t.Fatalf("data_dir must not change default outputPath, got %q", got)
	}
	if got := session.effectiveControlDULPath(); got != `D:\temp\oldpro\control.dul` {
		t.Fatalf("control.dul path = %q", got)
	}
	if got := session.effectiveDictionaryDir(); got != `D:\temp\oldpro\dmdul_dict` {
		t.Fatalf("dictionary path = %q", got)
	}
	if got := session.effectiveLogPath(); got != `D:\temp\oldpro\dul.log` {
		t.Fatalf("log path = %q", got)
	}
	session.outputDir = `D:\out`
	session.outputDirSet = true
	if got := session.outputPath("HR_TEST_data.sql"); got != `D:\out\HR_TEST_data.sql` {
		t.Fatalf("explicit output_dir outputPath = %q", got)
	}
	if got := session.effectiveDictionaryDir(); got != `D:\temp\oldpro\dmdul_dict` {
		t.Fatalf("output_dir must not move dictionary path, got %q", got)
	}
}

func TestInteractiveEnsureOutputDirCreatesDedicatedSubdirectory(t *testing.T) {
	currentDir := t.TempDir()
	dataDir := t.TempDir()
	runInDir(t, currentDir, func() {
		session := newInteractiveSession()
		session.dataDir = dataDir
		session.dataDirSet = true
		if err := session.ensureOutputDir(); err != nil {
			t.Fatalf("ensureOutputDir failed: %v", err)
		}
		info, err := os.Stat(filepath.Join(currentDir, defaultOutputDirName))
		if err != nil {
			t.Fatalf("launch-directory output was not created: %v", err)
		}
		if !info.IsDir() {
			t.Fatalf("output path is not a directory")
		}
		if _, err := os.Stat(filepath.Join(dataDir, defaultOutputDirName)); !os.IsNotExist(err) {
			t.Fatalf("default output must not follow data_dir, stat err=%v", err)
		}
	})
}

func TestInteractiveWritesInitDULToCurrentDirThenDataDir(t *testing.T) {
	previousDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd failed: %v", err)
	}
	currentDir := t.TempDir()
	dataDir := t.TempDir()
	if err := os.Chdir(currentDir); err != nil {
		t.Fatalf("Chdir failed: %v", err)
	}
	defer func() {
		if err := os.Chdir(previousDir); err != nil {
			t.Fatalf("restore Chdir failed: %v", err)
		}
	}()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	input := "show parameter;\nset data_dir " + dataDir + ";\nshow parameter;\nexit;\n"
	if err := RunInteractive(strings.NewReader(input), &stdout, &stderr); err != nil {
		t.Fatalf("RunInteractive returned error: %v", err)
	}
	currentINI := currentDir + string(os.PathSeparator) + defaultInitDULPath
	dataINI := dataDir + string(os.PathSeparator) + defaultInitDULPath
	if _, err := os.Stat(currentINI); err != nil {
		t.Fatalf("current init.dul was not generated: %v", err)
	}
	content, err := os.ReadFile(dataINI)
	if err != nil {
		t.Fatalf("data_dir init.dul was not generated: %v", err)
	}
	text := string(content)
	for _, want := range []string{"data_dir=" + dataDir, "data_format=sql", "charset=auto"} {
		if !strings.Contains(text, want) {
			t.Fatalf("init.dul should contain %q, got %q", want, text)
		}
	}
	if !strings.Contains(stdout.String(), "init_dul") {
		t.Fatalf("show parameter should print init_dul path, got %q", stdout.String())
	}
}

func TestInteractiveLoadInitDULCommand(t *testing.T) {
	previousDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd failed: %v", err)
	}
	dir := t.TempDir()
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Chdir failed: %v", err)
	}
	defer func() {
		if err := os.Chdir(previousDir); err != nil {
			t.Fatalf("restore Chdir failed: %v", err)
		}
	}()

	content := "\ufeffsystem=D:\\manual\\SYSTEM.DBF\ncharset=gb18030\ndata_format=csv\n"
	if err := os.WriteFile(defaultInitDULPath, []byte(content), 0644); err != nil {
		t.Fatalf("WriteFile init.dul failed: %v", err)
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := RunInteractive(strings.NewReader("load init;\nshow parameter;\nexit;\n"), &stdout, &stderr); err != nil {
		t.Fatalf("RunInteractive returned error: %v", err)
	}
	output := stdout.String()
	for _, want := range []string{"init loaded: init.dul", "system     = D:\\manual\\SYSTEM.DBF", "data_format= csv", "charset    = gb18030", "init_load  = init.dul"} {
		if !strings.Contains(output, want) {
			t.Fatalf("interactive output should contain %q, got %q", want, output)
		}
	}
}

func TestInteractiveLoadParameterRestoresBootstrapMetadata(t *testing.T) {
	previousDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd failed: %v", err)
	}
	dir := t.TempDir()
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Chdir failed: %v", err)
	}
	defer func() {
		if err := os.Chdir(previousDir); err != nil {
			t.Fatalf("restore Chdir failed: %v", err)
		}
	}()

	content := strings.Join([]string{
		"system=D:\\manual\\SYSTEM.DBF",
		"db_name=DMTEST",
		"db_name_source=dm.ctl",
		"instance_name=DMTEST01",
		"instance_name_source=dm.ini",
		"extent_size=64",
		"extent_size_source=u32 @ 0x80",
		"page_size=32768",
		"page_size_source=u32 @ 0x84",
		"page_count=100",
		"page_count_source=u32 @ 0x8C",
		"database_charset=EUC-KR (UNICODE_FLAG=2)",
		"unicode_flag=2",
		"charset_source=SYSTEM.DBF page 4 + 0x2D",
		"case_sensitive=auto",
		"case_sensitive_value=0",
		"case_sensitive_source=SYSTEM.DBF page 4 + 0x2C",
	}, "\n") + "\n"
	if err := os.WriteFile(defaultInitDULPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := RunInteractive(strings.NewReader("load parameter;\nshow parameter;\nexit;\n"), &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"parameters loaded: init.dul",
		"db_name    = DMTEST (dm.ctl)",
		"instance_name= DMTEST01 (dm.ini)",
		"extent_size= 64 pages (u32 @ 0x80)",
		"page_size  = 32768 bytes (u32 @ 0x84)",
		"unicode_flag= 2 (SYSTEM.DBF page 4 + 0x2D)",
		"case_effective= 0 (SYSTEM.DBF page 4 + 0x2C)",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("output missing %q: %s", want, stdout.String())
		}
	}
}

func TestInteractiveLoadDictionaryUpdatesAutoCharset(t *testing.T) {
	previousDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd failed: %v", err)
	}
	dir := t.TempDir()
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Chdir failed: %v", err)
	}
	defer func() {
		if err := os.Chdir(previousDir); err != nil {
			t.Fatalf("restore Chdir failed: %v", err)
		}
	}()

	if _, err := dm.WriteDictionaryFiles(filepath.Join(dir, dm.DefaultDictionaryDirName), &dm.DictionaryInfo{
		SystemPath:          `D:\manual\SYSTEM.DBF`,
		Source:              "SYSTEM.DBF",
		Charset:             "GB18030 (UNICODE_FLAG=0)",
		CharsetSource:       "test",
		CaseSensitive:       false,
		CaseSensitiveSource: "SYSTEM.DBF page 4 + 0x2C",
		HasCaseSensitive:    true,
		Users:               []dm.DictionaryUser{{ID: 1, Name: "HR_TEST"}},
	}); err != nil {
		t.Fatalf("WriteDictionaryFiles failed: %v", err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := RunInteractive(strings.NewReader("load dictionary;\nshow parameter;\nexit;\n"), &stdout, &stderr); err != nil {
		t.Fatalf("RunInteractive returned error: %v", err)
	}
	if !strings.Contains(stdout.String(), "charset    = gb18030") {
		t.Fatalf("load dictionary should update charset, got %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "case_effective= 0 (SYSTEM.DBF page 4 + 0x2C)") {
		t.Fatalf("load dictionary should resolve case_sensitive=auto, got %q", stdout.String())
	}
}

func TestSetDataDirClearsLoadedDictionary(t *testing.T) {
	session := newInteractiveSession()
	session.dictionary = &dm.DictionaryInfo{Source: "old dictionary"}
	if err := session.executeSet([]string{"data_dir", t.TempDir()}, io.Discard); err != nil {
		t.Fatalf("set data_dir returned error: %v", err)
	}
	if session.dictionary != nil {
		t.Fatal("set data_dir retained a dictionary from the previous work directory")
	}
}

func TestFailedBootstrapClearsLoadedDictionary(t *testing.T) {
	session := newInteractiveSession()
	session.systemPath = filepath.Join(t.TempDir(), "missing-SYSTEM.DBF")
	session.dictionary = &dm.DictionaryInfo{Source: "old dictionary"}
	if err := session.bootstrap(io.Discard); err == nil {
		t.Fatal("bootstrap unexpectedly succeeded with a missing SYSTEM.DBF")
	}
	if session.dictionary != nil {
		t.Fatal("failed bootstrap retained the old in-memory dictionary")
	}
}

func TestImplicitDictionaryLoadRejectsDifferentCurrentSystem(t *testing.T) {
	dir := t.TempDir()
	currentSystem := filepath.Join(dir, "CURRENT_SYSTEM.DBF")
	writeMinimalSystemDBF(t, currentSystem)
	oldSystem := filepath.Join(dir, "OLD_SYSTEM.DBF")
	if _, err := dm.WriteDictionaryFiles(filepath.Join(dir, dm.DefaultDictionaryDirName), &dm.DictionaryInfo{
		SystemPath: oldSystem,
		Source:     "dictionary files",
		PageSize:   8192,
		PageCount:  5,
		Users:      []dm.DictionaryUser{{ID: 1, Name: "OLD_USER"}},
	}); err != nil {
		t.Fatalf("write stale dictionary: %v", err)
	}

	session := newInteractiveSession()
	session.systemPath = currentSystem
	session.dataDir = dir
	session.dataDirSet = true
	err := session.ensureDictionaryLoaded()
	if err == nil || !strings.Contains(err.Error(), "belongs to SYSTEM.DBF") {
		t.Fatalf("implicit stale dictionary load error = %v", err)
	}
	if session.dictionary != nil || session.systemPath != currentSystem {
		t.Fatalf("stale dictionary changed the session: dictionary=%+v system=%q", session.dictionary, session.systemPath)
	}

	if err := session.loadDictionaryFiles(io.Discard); err != nil {
		t.Fatalf("explicit dictionary load should remain available: %v", err)
	}
	if session.dictionary == nil || session.systemPath != oldSystem {
		t.Fatalf("explicit dictionary load did not select its recorded source: dictionary=%+v system=%q", session.dictionary, session.systemPath)
	}
}

func TestInteractiveListUserShowsObjectCounts(t *testing.T) {
	previousDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd failed: %v", err)
	}
	dir := t.TempDir()
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Chdir failed: %v", err)
	}
	defer func() {
		if err := os.Chdir(previousDir); err != nil {
			t.Fatalf("restore Chdir failed: %v", err)
		}
	}()

	if _, err := dm.WriteDictionaryFiles(filepath.Join(dir, dm.DefaultDictionaryDirName), &dm.DictionaryInfo{
		Source: "SYSTEM.DBF",
		Users:  []dm.DictionaryUser{{ID: 1, Name: "APP"}},
		Tables: []dm.DictionaryTable{
			{ID: 1001, Owner: "APP", Name: "T1"},
		},
		Views: []dm.DictionaryView{
			{ID: 2001, Owner: "APP", Name: "V1"},
		},
		Sequences: []dm.DictionarySequence{
			{ID: 3001, Owner: "APP", Name: "S1"},
		},
		Routines: []dm.DictionaryRoutine{
			{ID: 4001, Owner: "APP", Name: "F1", ObjectType: "FUNCTION"},
			{ID: 4002, Owner: "APP", Name: "P1", ObjectType: "PROCEDURE"},
			{ID: 4003, Owner: "APP", Name: "PKG1", ObjectType: "PACKAGE"},
			{ID: 4003, Owner: "APP", Name: "PKG1", ObjectType: "PACKAGE BODY"},
		},
		Triggers: []dm.DictionaryTrigger{
			{ID: 5001, Owner: "APP", Name: "TRG1"},
		},
		Synonyms: []dm.DictionarySynonym{
			{ID: 6001, Owner: "APP", Name: "SYN1"},
		},
	}); err != nil {
		t.Fatalf("WriteDictionaryFiles failed: %v", err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := RunInteractive(strings.NewReader("load dictionary;\nlist user;\nexit;\n"), &stdout, &stderr); err != nil {
		t.Fatalf("RunInteractive returned error: %v", err)
	}
	output := stdout.String()
	// Headings are upper-cased and underlined with a rule, disql/sqlplus style.
	for _, want := range []string{
		"USER", "SCHEMAS", "TABLES", "VIEWS", "SYNONYMS", "SEQUENCES", "TRIGGERS", "FUNCTIONS", "PROCEDURES", "PACKAGES",
		"---- -------- -------- -------- ---------- ---------- --------- ---------- ----------- ---------",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("list user output should contain %q, got %q", want, output)
		}
	}
	if !strings.Contains(output, "APP         1        1        1          1          1         1          1           1         1") {
		t.Fatalf("list user should show per-owner object counts, got %q", output)
	}
}

func TestInteractiveUnloadDatabaseSQLAutoLoadsDictionary(t *testing.T) {
	cwd, dataDir, outDir := setupUnloadDatabaseFixture(t)
	runInDir(t, cwd, func() {
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		input := "set output_dir " + outDir + ";\nunload database;\nexit;\n"
		if err := RunInteractive(strings.NewReader(input), &stdout, &stderr); err != nil {
			t.Fatalf("RunInteractive returned error: %v", err)
		}
		output := stdout.String()
		for _, want := range []string{
			"ddl output:",
			"DATABASE_ddl.sql",
			"data output:",
			"DATABASE_data.sql",
			"users exported: 2",
			"tables exported: 2",
			"views exported: 1",
			"sequences exported: 1",
			"routines exported: 1",
			"triggers exported: 1",
			"synonyms exported: 2",
			"tab privileges exported: 3",
			"rows exported: 1",
			"rows failed: 0",
			"planned pages: 0",
			"direct pages read: 0",
			"fallback pages scanned: 1",
			"fallback reason:",
		} {
			if !strings.Contains(output, want) {
				t.Fatalf("interactive output should contain %q, got %q", want, output)
			}
		}
		ddl := readTestFile(t, filepath.Join(outDir, "DATABASE_ddl.sql"))
		for _, want := range []string{
			"CREATE USER APP",
			"CREATE USER NO_TABLE",
			"CREATE TABLE APP.WITH_ROWS",
			"CREATE TABLE APP.EMPTY_TABLE",
			"CREATE SEQUENCE APP.SEQ_WITH_ROWS",
			"START WITH 1",
			"INCREMENT BY 1",
			"MAXVALUE 999999999999",
			"CACHE 20",
			"CREATE OR REPLACE FUNCTION APP.F_WITH_ROWS RETURN INT AS BEGIN RETURN 1; END;",
			"CREATE OR REPLACE VIEW APP.V_WITH_ROWS AS SELECT ID FROM APP.WITH_ROWS;",
			"CREATE OR REPLACE TRIGGER APP.TRG_WITH_ROWS BEFORE INSERT ON APP.WITH_ROWS BEGIN NULL; END;",
			"CREATE OR REPLACE SYNONYM NO_TABLE.SYN_WITH_ROWS FOR APP.WITH_ROWS;",
			"CREATE OR REPLACE SYNONYM NO_TABLE.SYN_SEQ_WITH_ROWS FOR APP.SEQ_WITH_ROWS;",
			"GRANT SELECT ON APP.WITH_ROWS TO NO_TABLE;",
			"GRANT SELECT ON APP.V_WITH_ROWS TO NO_TABLE;",
			"GRANT SELECT ON APP.SEQ_WITH_ROWS TO NO_TABLE;",
			"STORAGE(ON \"MAIN\", CLUSTERBTR)",
		} {
			if !strings.Contains(ddl, want) {
				t.Fatalf("DATABASE_ddl.sql should contain %q, got %q", want, ddl)
			}
		}
		data := readTestFile(t, filepath.Join(outDir, "DATABASE_data.sql"))
		if !strings.Contains(data, "INSERT INTO APP.WITH_ROWS (ID) VALUES (100);") {
			t.Fatalf("DATABASE_data.sql should contain exported row, got %q", data)
		}
		logText := readTestFile(t, filepath.Join(dataDir, "dul.log"))
		for _, want := range []string{"[UNLOAD] planned_pages=0 direct_pages_read=0 fallback_pages_scanned=1", "[UNLOAD] fallback_reason="} {
			if !strings.Contains(logText, want) {
				t.Fatalf("dul.log should contain %q, got %q", want, logText)
			}
		}
	})
}

func TestInteractiveUnloadUserUsesDefaultOutputSubdirectory(t *testing.T) {
	cwd, dataDir, _ := setupUnloadDatabaseFixture(t)
	runInDir(t, cwd, func() {
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		input := "unload user APP;\nexit;\n"
		if err := RunInteractive(strings.NewReader(input), &stdout, &stderr); err != nil {
			t.Fatalf("RunInteractive returned error: %v", err)
		}
		outputDir := filepath.Join(cwd, defaultOutputDirName)
		for _, name := range []string{"APP_WITH_ROWS_ddl.sql", "APP_WITH_ROWS_data.sql"} {
			if _, err := os.Stat(filepath.Join(outputDir, name)); err != nil {
				t.Fatalf("default output %s should exist: %v", name, err)
			}
			if _, err := os.Stat(filepath.Join(dataDir, name)); !os.IsNotExist(err) {
				t.Fatalf("default output %s must not clutter data_dir, stat err=%v", name, err)
			}
		}
		if !strings.Contains(stdout.String(), "output dir: "+outputDir) {
			t.Fatalf("interactive output should report dedicated directory, got %q", stdout.String())
		}
	})
}

func TestInteractiveUnloadObjectExportsOwnerDictionary(t *testing.T) {
	cwd, _, outDir := setupUnloadDatabaseFixture(t)
	runInDir(t, cwd, func() {
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		input := "set output_dir " + outDir + ";\nunload object APP;\nexit;\n"
		if err := RunInteractive(strings.NewReader(input), &stdout, &stderr); err != nil {
			t.Fatalf("RunInteractive returned error: %v", err)
		}
		output := stdout.String()
		for _, want := range []string{
			"object ddl output:",
			"APP_objects.sql",
			"users exported: 1",
			"tables exported: 2",
			"views exported: 1",
			"sequences exported: 1",
			"routines exported: 1",
			"triggers exported: 1",
		} {
			if !strings.Contains(output, want) {
				t.Fatalf("interactive output should contain %q, got %q", want, output)
			}
		}
		ddl := readTestFile(t, filepath.Join(outDir, "APP_objects.sql"))
		for _, want := range []string{
			"CREATE USER APP",
			"CREATE TABLE APP.WITH_ROWS",
			"CREATE TABLE APP.EMPTY_TABLE",
			"CREATE SEQUENCE APP.SEQ_WITH_ROWS",
			"CREATE OR REPLACE FUNCTION APP.F_WITH_ROWS",
			"CREATE OR REPLACE VIEW APP.V_WITH_ROWS",
			"CREATE OR REPLACE TRIGGER APP.TRG_WITH_ROWS",
		} {
			if !strings.Contains(ddl, want) {
				t.Fatalf("APP_objects.sql should contain %q, got %q", want, ddl)
			}
		}
		if _, err := os.Stat(filepath.Join(outDir, "APP_data.sql")); !os.IsNotExist(err) {
			t.Fatalf("unload object must not create data output, stat err=%v", err)
		}
	})
}

func TestInteractiveUnloadObjectKeepsUserWithoutTables(t *testing.T) {
	cwd, _, outDir := setupUnloadDatabaseFixture(t)
	runInDir(t, cwd, func() {
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		input := "set output_dir " + outDir + ";\nunload object NO_TABLE;\nexit;\n"
		if err := RunInteractive(strings.NewReader(input), &stdout, &stderr); err != nil {
			t.Fatalf("RunInteractive returned error: %v", err)
		}
		output := stdout.String()
		for _, want := range []string{"NO_TABLE_objects.sql", "users exported: 1", "tables exported: 0"} {
			if !strings.Contains(output, want) {
				t.Fatalf("interactive output should contain %q, got %q", want, output)
			}
		}
		ddl := readTestFile(t, filepath.Join(outDir, "NO_TABLE_objects.sql"))
		if !strings.Contains(ddl, "CREATE USER NO_TABLE") {
			t.Fatalf("owner without tables must still be created, got %q", ddl)
		}
		if strings.Contains(ddl, "CREATE TABLE") {
			t.Fatalf("owner without tables must not acquire another owner's table DDL, got %q", ddl)
		}
	})
}

func TestInteractiveUnloadUserSQLExportsPerTableFiles(t *testing.T) {
	cwd, _, outDir := setupUnloadDatabaseFixture(t)
	runInDir(t, cwd, func() {
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		input := "set output_dir " + outDir + ";\nunload user APP;\nexit;\n"
		if err := RunInteractive(strings.NewReader(input), &stdout, &stderr); err != nil {
			t.Fatalf("RunInteractive returned error: %v", err)
		}
		output := stdout.String()
		for _, want := range []string{
			// Per-table progress lines report what each table yielded.
			". unloading table APP.WITH_ROWS",
			"1 rows unloaded",
			"APP_WITH_ROWS_ddl.sql",
			"APP_WITH_ROWS_data.sql",
			". unloading table APP.EMPTY_TABLE",
			"0 rows unloaded",
			"APP_EMPTY_TABLE_ddl.sql",
			"APP_EMPTY_TABLE_data.sql",
			"tables exported: 2",
			"ddl files exported: 2",
			"data files exported: 2",
			"rows exported: 1",
			"rows failed: 0",
		} {
			if !strings.Contains(output, want) {
				t.Fatalf("interactive output should contain %q, got %q", want, output)
			}
		}

		withRowsDDL := readTestFile(t, filepath.Join(outDir, "APP_WITH_ROWS_ddl.sql"))
		for _, want := range []string{
			"CREATE TABLE APP.WITH_ROWS",
			"CREATE OR REPLACE TRIGGER APP.TRG_WITH_ROWS",
			"GRANT SELECT ON APP.WITH_ROWS TO NO_TABLE;",
		} {
			if !strings.Contains(withRowsDDL, want) {
				t.Fatalf("per-table DDL should contain %q, got %q", want, withRowsDDL)
			}
		}
		for _, unwanted := range []string{
			"CREATE USER APP",
			"CREATE OR REPLACE VIEW APP.V_WITH_ROWS",
			"CREATE SEQUENCE APP.SEQ_WITH_ROWS",
			"CREATE OR REPLACE FUNCTION APP.F_WITH_ROWS",
			"CREATE OR REPLACE SYNONYM",
		} {
			if strings.Contains(withRowsDDL, unwanted) {
				t.Fatalf("per-table DDL must not contain %q, got %q", unwanted, withRowsDDL)
			}
		}
		data := readTestFile(t, filepath.Join(outDir, "APP_WITH_ROWS_data.sql"))
		if !strings.Contains(data, "INSERT INTO APP.WITH_ROWS (ID) VALUES (100);") {
			t.Fatalf("per-table data SQL should contain exported row, got %q", data)
		}
		emptyData := readTestFile(t, filepath.Join(outDir, "APP_EMPTY_TABLE_data.sql"))
		if strings.Contains(emptyData, "INSERT INTO") {
			t.Fatalf("empty table data SQL must not contain INSERT, got %q", emptyData)
		}
		for _, oldName := range []string{"APP_ddl.sql", "APP_data.sql"} {
			if _, err := os.Stat(filepath.Join(outDir, oldName)); !os.IsNotExist(err) {
				t.Fatalf("legacy owner-level output %s must not be created, stat err=%v", oldName, err)
			}
		}
	})
}

func TestInteractiveUnloadUserCSVExportsPerTableAndSkipsEmptyData(t *testing.T) {
	cwd, _, outDir := setupUnloadDatabaseFixture(t)
	runInDir(t, cwd, func() {
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		input := "set output_dir " + outDir + ";\nset data_format csv;\nunload user APP;\nexit;\n"
		if err := RunInteractive(strings.NewReader(input), &stdout, &stderr); err != nil {
			t.Fatalf("RunInteractive returned error: %v", err)
		}
		output := stdout.String()
		for _, want := range []string{
			"APP_WITH_ROWS_ddl.sql",
			"APP_WITH_ROWS_data.txt",
			"APP_EMPTY_TABLE_ddl.sql",
			"data output: skipped (no rows)",
			"ddl files exported: 2",
			"data files exported: 1",
			"rows exported: 1",
		} {
			if !strings.Contains(output, want) {
				t.Fatalf("interactive output should contain %q, got %q", want, output)
			}
		}
		csvText := readTestFile(t, filepath.Join(outDir, "APP_WITH_ROWS_data.txt"))
		// fldr data files carry rows only; column order lives in the .ctl.
		if strings.TrimSpace(csvText) != "100" {
			t.Fatalf("unexpected fldr output %q", csvText)
		}
		if _, err := os.Stat(filepath.Join(outDir, "APP_EMPTY_TABLE_data.txt")); !os.IsNotExist(err) {
			t.Fatalf("empty table CSV should not exist, stat err=%v", err)
		}
	})
}

func TestInteractiveUnloadTableDMPExportsLoadableTableFile(t *testing.T) {
	cwd, _, outDir := setupUnloadDatabaseFixture(t)
	runInDir(t, cwd, func() {
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		input := "set output_dir " + outDir + ";\nset data_format dmp;\nset case_sensitive 0;\nunload table APP.WITH_ROWS;\nexit;\n"
		if err := RunInteractive(strings.NewReader(input), &stdout, &stderr); err != nil {
			t.Fatalf("RunInteractive returned error: %v", err)
		}
		output := stdout.String()
		for _, want := range []string{"APP_WITH_ROWS_ddl.sql", "APP_WITH_ROWS.dmp", "dmp mode: TABLES", "objects exported:", "rows exported: 1"} {
			if !strings.Contains(output, want) {
				t.Fatalf("interactive output should contain %q, got %q", want, output)
			}
		}
		info, err := dm.InspectDMP(filepath.Join(outDir, "APP_WITH_ROWS.dmp"))
		if err != nil {
			t.Fatalf("InspectDMP failed: %v", err)
		}
		if info.Mode != dm.DMPModeTables || info.ObjectCount == 0 || info.CaseSensitive || info.Charset != "UTF-8" || info.PageSize != 8192 || info.ExtentSize != 16 || len(info.Tables) != 1 || info.Tables[0].ObjectID != 1001 || info.Tables[0].RowCount != 1 {
			t.Fatalf("unexpected table dmp info %+v", info)
		}
	})
}

func TestInteractiveUnloadUserDMPExportsOneOwnerFileIncludingEmptyTableMetadata(t *testing.T) {
	cwd, _, outDir := setupUnloadDatabaseFixture(t)
	runInDir(t, cwd, func() {
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		input := "set output_dir " + outDir + ";\nset data_format dmp;\nunload user APP;\nexit;\n"
		if err := RunInteractive(strings.NewReader(input), &stdout, &stderr); err != nil {
			t.Fatalf("RunInteractive returned error: %v", err)
		}
		output := stdout.String()
		for _, want := range []string{"APP.dmp", "dmp mode: OWNER", "tables exported: 2", "rows exported: 1"} {
			if !strings.Contains(output, want) {
				t.Fatalf("interactive output should contain %q, got %q", want, output)
			}
		}
		info, err := dm.InspectDMP(filepath.Join(outDir, "APP.dmp"))
		if err != nil {
			t.Fatalf("InspectDMP failed: %v", err)
		}
		if info.Mode != dm.DMPModeOwner || info.ObjectCount == 0 || len(info.Schemas) != 2 || info.Schemas[0].Name != "APP" || info.Schemas[1].Name != "APP_EXTRA" || len(info.Tables) != 2 || info.Tables[0].Name != "EMPTY_TABLE" || info.Tables[0].RowCount != 0 || info.Tables[1].Name != "WITH_ROWS" || info.Tables[1].RowCount != 1 {
			t.Fatalf("unexpected user dmp info %+v", info.Tables)
		}
		if _, err := os.Stat(filepath.Join(outDir, "APP_WITH_ROWS_data.dmp")); !os.IsNotExist(err) {
			t.Fatalf("owner export must not leave per-table DMP files, stat err=%v", err)
		}
	})
}

func TestInteractiveUnloadSchemasDMPExportsSelectedSchemasInOneFile(t *testing.T) {
	cwd, _, outDir := setupUnloadDatabaseFixture(t)
	runInDir(t, cwd, func() {
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		input := "set output_dir " + outDir + ";\nset data_format dmp;\nunload schema APP,APP_EXTRA to schema_bundle;\nexit;\n"
		if err := RunInteractive(strings.NewReader(input), &stdout, &stderr); err != nil {
			t.Fatalf("RunInteractive returned error: %v", err)
		}
		for _, want := range []string{"schema_bundle_ddl.sql", "schema_bundle.dmp", "dmp mode: SCHEMAS", "schemas exported: 2", "tables exported: 2", "rows exported: 1"} {
			if !strings.Contains(stdout.String(), want) {
				t.Fatalf("interactive output should contain %q, got %q", want, stdout.String())
			}
		}
		info, err := dm.InspectDMP(filepath.Join(outDir, "schema_bundle.dmp"))
		if err != nil {
			t.Fatalf("InspectDMP failed: %v", err)
		}
		if info.Mode != dm.DMPModeSchemas || len(info.Schemas) != 2 || info.Schemas[0].Name != "APP" || info.Schemas[1].Name != "APP_EXTRA" || len(info.Tables) != 2 {
			t.Fatalf("unexpected schemas dmp info %+v", info)
		}
	})
}

func TestInteractiveUnloadUserAllDirectsToDatabaseCommand(t *testing.T) {
	cwd, _, outDir := setupUnloadDatabaseFixture(t)
	runInDir(t, cwd, func() {
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		input := "set output_dir " + outDir + ";\nunload user all;\nexit;\n"
		if err := RunInteractive(strings.NewReader(input), &stdout, &stderr); err != nil {
			t.Fatalf("RunInteractive returned error: %v", err)
		}
		if !strings.Contains(stdout.String(), "unload user all has been removed; use unload database") {
			t.Fatalf("unexpected output %q", stdout.String())
		}
		if _, err := os.Stat(filepath.Join(outDir, "DATABASE_ddl.sql")); !os.IsNotExist(err) {
			t.Fatalf("unload user all must not start a database export, stat err=%v", err)
		}
	})
}

func TestInteractiveUnloadUserCustomPrefixAppliesPerTable(t *testing.T) {
	cwd, _, outDir := setupUnloadDatabaseFixture(t)
	runInDir(t, cwd, func() {
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		input := "set output_dir " + outDir + ";\nunload user APP to rescue_app;\nexit;\n"
		if err := RunInteractive(strings.NewReader(input), &stdout, &stderr); err != nil {
			t.Fatalf("RunInteractive returned error: %v", err)
		}
		for _, name := range []string{
			"rescue_app_WITH_ROWS_ddl.sql",
			"rescue_app_WITH_ROWS_data.sql",
			"rescue_app_EMPTY_TABLE_ddl.sql",
			"rescue_app_EMPTY_TABLE_data.sql",
		} {
			if _, err := os.Stat(filepath.Join(outDir, name)); err != nil {
				t.Fatalf("custom-prefix output %s should exist: %v", name, err)
			}
		}
		if _, err := os.Stat(filepath.Join(outDir, "APP_WITH_ROWS_ddl.sql")); !os.IsNotExist(err) {
			t.Fatalf("default prefix must not be used, stat err=%v", err)
		}
	})
}

func TestInteractiveUnloadDatabaseCSVExportsPerTableAndSkipsEmptyTables(t *testing.T) {
	cwd, _, outDir := setupUnloadDatabaseFixture(t)
	runInDir(t, cwd, func() {
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		input := "set output_dir " + outDir + ";\nset data_format csv;\nunload database;\nexit;\n"
		if err := RunInteractive(strings.NewReader(input), &stdout, &stderr); err != nil {
			t.Fatalf("RunInteractive returned error: %v", err)
		}
		output := stdout.String()
		for _, want := range []string{
			"DATABASE_ddl.sql",
			"fldr output:",
			"DATABASE_APP_WITH_ROWS_data.txt",
			"fldr skipped: APP.EMPTY_TABLE (no rows)",
			"fldr files exported: 1",
			"rows exported: 1",
			"rows failed: 0",
		} {
			if !strings.Contains(output, want) {
				t.Fatalf("interactive output should contain %q, got %q", want, output)
			}
		}
		csvPath := filepath.Join(outDir, "DATABASE_APP_WITH_ROWS_data.txt")
		csvText := readTestFile(t, csvPath)
		// fldr data files carry rows only; column order lives in the .ctl.
		if strings.TrimSpace(csvText) != "100" {
			t.Fatalf("unexpected fldr output %q", csvText)
		}
		if _, err := os.Stat(filepath.Join(outDir, "DATABASE_APP_EMPTY_TABLE_data.txt")); !os.IsNotExist(err) {
			t.Fatalf("empty table csv should not exist, stat err=%v", err)
		}
	})
}

func TestInteractiveUnloadDatabaseDMPExportsOneFullFileIncludingEmptyTables(t *testing.T) {
	cwd, _, outDir := setupUnloadDatabaseFixture(t)
	runInDir(t, cwd, func() {
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		input := "set output_dir " + outDir + ";\nset data_format dmp;\nunload database;\nexit;\n"
		if err := RunInteractive(strings.NewReader(input), &stdout, &stderr); err != nil {
			t.Fatalf("RunInteractive returned error: %v", err)
		}
		output := stdout.String()
		for _, want := range []string{
			"DATABASE_ddl.sql",
			"dmp output:", "DATABASE.dmp",
			"dmp mode: FULL",
			"tables exported: 2",
			"rows exported: 1",
		} {
			if !strings.Contains(output, want) {
				t.Fatalf("interactive output should contain %q, got %q", want, output)
			}
		}
		info, err := dm.InspectDMP(filepath.Join(outDir, "DATABASE.dmp"))
		if err != nil {
			t.Fatalf("InspectDMP failed: %v", err)
		}
		if info.Mode != dm.DMPModeFull || info.ObjectCount == 0 || len(info.Tables) != 2 || info.Tables[0].RowCount != 0 || info.Tables[1].RowCount != 1 || !info.PayloadMD5Valid || !info.HeaderChecksumValid {
			t.Fatalf("unexpected database dmp info %+v", info)
		}
		if _, err := os.Stat(filepath.Join(outDir, "DATABASE_APP_WITH_ROWS_data.dmp")); !os.IsNotExist(err) {
			t.Fatalf("full export must not leave per-table DMP files, stat err=%v", err)
		}
	})
}

func TestInteractiveUnloadDatabaseCustomPrefix(t *testing.T) {
	cwd, _, outDir := setupUnloadDatabaseFixture(t)
	runInDir(t, cwd, func() {
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		input := "set output_dir " + outDir + ";\nunload database to rescue_all;\nexit;\n"
		if err := RunInteractive(strings.NewReader(input), &stdout, &stderr); err != nil {
			t.Fatalf("RunInteractive returned error: %v", err)
		}
		output := stdout.String()
		for _, want := range []string{"rescue_all_ddl.sql", "rescue_all_data.sql", "rows exported: 1"} {
			if !strings.Contains(output, want) {
				t.Fatalf("interactive output should contain %q, got %q", want, output)
			}
		}
		for _, name := range []string{"rescue_all_ddl.sql", "rescue_all_data.sql"} {
			if _, err := os.Stat(filepath.Join(outDir, name)); err != nil {
				t.Fatalf("%s should exist: %v", name, err)
			}
		}
		if _, err := os.Stat(filepath.Join(outDir, "DATABASE_ddl.sql")); !os.IsNotExist(err) {
			t.Fatalf("default ddl should not be created when custom prefix is used, stat err=%v", err)
		}
	})
}

func TestCharsetParameterFromDictionary(t *testing.T) {
	tests := []struct {
		input string
		want  string
		ok    bool
	}{
		{input: "GB18030 (UNICODE_FLAG=0)", want: "gb18030", ok: true},
		{input: "UTF-8 (UNICODE_FLAG=1)", want: "utf-8", ok: true},
		{input: "EUC-KR (UNICODE_FLAG=2)", want: "euc-kr", ok: true},
		{input: "unknown (UNICODE_FLAG=9)", ok: false},
	}
	for _, tt := range tests {
		got, ok := charsetParameterFromDictionary(tt.input)
		if ok != tt.ok || got != tt.want {
			t.Fatalf("charsetParameterFromDictionary(%q) = %q,%v want %q,%v", tt.input, got, ok, tt.want, tt.ok)
		}
	}
}

func TestBootstrapCharsetReplacesStaleInitValueButKeepsExplicitOverride(t *testing.T) {
	systemPath := filepath.Join(t.TempDir(), "SYSTEM.DBF")
	writeMinimalSystemDBF(t, systemPath)

	session := newInteractiveSession()
	session.charset = "gb18030"
	if got := session.bootstrapCharset(systemPath, ""); got != "utf-8" {
		t.Fatalf("bootstrap charset with stale init value = %q, want utf-8", got)
	}

	session.charsetExplicit = true
	if got := session.bootstrapCharset(systemPath, ""); got != "gb18030" {
		t.Fatalf("bootstrap charset with explicit override = %q, want gb18030", got)
	}
}

func TestInteractiveBootstrapPersistsStructuredDiagnostics(t *testing.T) {
	dir := t.TempDir()
	systemPath := filepath.Join(dir, "SYSTEM.DBF")
	writeMinimalSystemDBF(t, systemPath)
	runInDir(t, dir, func() {
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		input := strings.Join([]string{
			"set system " + systemPath + ";",
			"set data_dir " + dir + ";",
			"bootstrap;",
			"exit;",
		}, "\n")
		if err := RunInteractive(strings.NewReader(input), &stdout, &stderr); err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"[bootstrap] phase=start", "phase=metadata", "phase=complete", "mode=stream-scan-fallback"} {
			if !strings.Contains(stdout.String(), want) {
				t.Fatalf("stdout missing %q: %s", want, stdout.String())
			}
		}
		logText := readTestFile(t, filepath.Join(dir, "dul.log"))
		for _, want := range []string{"[bootstrap] phase=start", "stage=1 phase=anchor", "phase=complete"} {
			if !strings.Contains(logText, want) {
				t.Fatalf("dul.log missing %q: %s", want, logText)
			}
		}
		initText := readTestFile(t, filepath.Join(dir, defaultInitDULPath))
		for _, want := range []string{
			"db_name=DAMENG",
			"instance_name=DMSERVER",
			"extent_size=16",
			"page_size=8192",
			"unicode_flag=1",
			"case_sensitive_value=0",
		} {
			if !strings.Contains(initText, want) {
				t.Fatalf("init.dul missing %q: %s", want, initText)
			}
		}
	})
}

func TestBootstrapFileDiagnosticIgnoresRecreatedTempHeader(t *testing.T) {
	path := filepath.Join(t.TempDir(), "TEMP.DBF")
	if err := os.WriteFile(path, make([]byte, 8192), 0600); err != nil {
		t.Fatal(err)
	}
	line, warning := formatBootstrapFileDiagnostic(dm.OfflineDataFile{
		GroupID: 3, FileID: 0, Tablespace: "TEMP", Path: path,
	}, 8192)
	if warning || !strings.Contains(line, "status=IGNORED_TEMP") {
		t.Fatalf("line=%q warning=%v", line, warning)
	}
	line, warning = formatBootstrapFileDiagnostic(dm.OfflineDataFile{
		GroupID: 4, FileID: 0, Tablespace: "MAIN", Path: path,
	}, 8192)
	if !warning || !strings.Contains(line, "status=HEADER_MISMATCH") {
		t.Fatalf("line=%q warning=%v", line, warning)
	}
}

func TestMissingDictionaryDataFiles(t *testing.T) {
	dictionary := &dm.DictionaryInfo{Tables: []dm.DictionaryTable{
		{ID: 1001, Owner: "APP", Name: "T_MAIN", GroupID: 4, RootFile: 0, Tablespace: "MAIN"},
		{ID: 1002, Owner: "APP", Name: "T_DATA", GroupID: 5, RootFile: 0, Tablespace: "TBS_DATA"},
		{ID: 1003, Owner: "APP", Name: "T_DATA_2", GroupID: 5, RootFile: 0, Tablespace: "TBS_DATA"},
		{ID: 1004, Owner: "APP", Name: "T_HIGH", GroupID: 9, RootFile: 1, Tablespace: "TBS_HIGH"},
		{ID: 1006, Owner: "APP", Name: "T_MAIN_SECOND", GroupID: 4, RootFile: 1, Tablespace: "MAIN"},
		{ID: 1005, Owner: "APP", Name: "T_TEMP", GroupID: 3, Temporary: true},
	}}
	missing := missingDictionaryDataFiles(dictionary, []dm.OfflineDataFile{{GroupID: 4, FileID: 0, Tablespace: "MAIN"}})
	if got := formatMissingDictionaryDataFiles(missing); got != "4/1(MAIN,tables=1),5/0(TBS_DATA,tables=2),9/1(TBS_HIGH,tables=1)" {
		t.Fatalf("unexpected missing group summary %q", got)
	}
}

func setupUnloadDatabaseFixture(t *testing.T) (string, string, string) {
	t.Helper()
	cwd := t.TempDir()
	dataDir := t.TempDir()
	outDir := t.TempDir()
	systemPath := filepath.Join(dataDir, "SYSTEM.DBF")
	mainPath := filepath.Join(dataDir, "MAIN.DBF")
	writeMinimalSystemDBF(t, systemPath)
	writeMinimalMainDBFWithOneIntRow(t, mainPath, 1001, 100)
	_, err := dm.WriteDictionaryFiles(filepath.Join(dataDir, dm.DefaultDictionaryDirName), &dm.DictionaryInfo{
		SystemPath:       systemPath,
		Source:           "SYSTEM.DBF",
		ExtentSize:       16,
		ExtentSizeSource: "u32 @ 0x80",
		PageSize:         8192,
		PageCount:        5,
		Charset:          "UTF-8 (UNICODE_FLAG=1)",
		CharsetSource:    "SYSTEM.DBF page 4 + 0x2D",
		Users: []dm.DictionaryUser{
			{ID: 10, Name: "APP"},
			{ID: 11, Name: "NO_TABLE"},
		},
		Schemas: []dm.DictionarySchema{
			{ID: 20, Name: "APP", OwnerID: 10, Owner: "APP"},
			{ID: 21, Name: "APP_EXTRA", OwnerID: 10, Owner: "APP"},
			{ID: 22, Name: "NO_TABLE", OwnerID: 11, Owner: "NO_TABLE"},
		},
		Tables: []dm.DictionaryTable{
			{ID: 1001, Owner: "APP", Name: "WITH_ROWS", ColumnCount: 1, Tablespace: "MAIN", GroupID: 4, Storage: "CLUSTERBTR"},
			{ID: 1002, Owner: "APP", Name: "EMPTY_TABLE", ColumnCount: 1, Tablespace: "MAIN", GroupID: 4, Storage: "CLUSTERBTR"},
		},
		Columns: []dm.DictionaryColumn{
			{TableID: 1001, TableOwner: "APP", TableName: "WITH_ROWS", ColID: 1, Name: "ID", DataType: "INT", Nullable: "N"},
			{TableID: 1002, TableOwner: "APP", TableName: "EMPTY_TABLE", ColID: 1, Name: "ID", DataType: "INT", Nullable: "N"},
		},
		Views: []dm.DictionaryView{
			{ID: 2001, Owner: "APP", Name: "V_WITH_ROWS", Valid: "Y", SQL: "CREATE OR REPLACE VIEW APP.V_WITH_ROWS AS SELECT ID FROM APP.WITH_ROWS"},
		},
		Sequences: []dm.DictionarySequence{
			{ID: 2101, Owner: "APP", Name: "SEQ_WITH_ROWS", Valid: "Y", StartWith: 1, MinValue: 1, MaxValue: 999999999999, IncrementBy: 1, CycleFlag: "N", OrderFlag: "N", CacheSize: 20},
		},
		Routines: []dm.DictionaryRoutine{
			{ID: 2151, Owner: "APP", Name: "F_WITH_ROWS", ObjectType: "FUNCTION", SeqNo: 0, Valid: "Y", SQL: "CREATE OR REPLACE FUNCTION APP.F_WITH_ROWS RETURN INT AS BEGIN RETURN 1; END;"},
		},
		Triggers: []dm.DictionaryTrigger{
			{ID: 2201, Owner: "APP", Name: "TRG_WITH_ROWS", TableOwner: "APP", TableName: "WITH_ROWS", Valid: "Y", SQL: "CREATE OR REPLACE TRIGGER APP.TRG_WITH_ROWS BEFORE INSERT ON APP.WITH_ROWS BEGIN NULL; END;"},
		},
		Synonyms: []dm.DictionarySynonym{
			{ID: 3001, Owner: "NO_TABLE", Name: "SYN_WITH_ROWS", TableOwner: "APP", TableName: "WITH_ROWS"},
			{ID: 3002, Owner: "NO_TABLE", Name: "SYN_SEQ_WITH_ROWS", TableOwner: "APP", TableName: "SEQ_WITH_ROWS"},
		},
		TabPrivileges: []dm.DictionaryTabPrivilege{
			{Grantee: "NO_TABLE", Owner: "APP", ObjectName: "WITH_ROWS", ObjectType: "TABLE", Privilege: "SELECT", Grantable: "N"},
			{Grantee: "NO_TABLE", Owner: "APP", ObjectName: "V_WITH_ROWS", ObjectType: "VIEW", Privilege: "SELECT", Grantable: "N"},
			{Grantee: "NO_TABLE", Owner: "APP", ObjectName: "SEQ_WITH_ROWS", ObjectType: "SEQUENCE", Privilege: "SELECT", Grantable: "N"},
		},
	})
	if err != nil {
		t.Fatalf("WriteDictionaryFiles failed: %v", err)
	}
	initContent := fmt.Sprintf("system=%s\ndata_dir=%s\ncharset=utf-8\n", systemPath, dataDir)
	if err := os.WriteFile(filepath.Join(cwd, defaultInitDULPath), []byte(initContent), 0644); err != nil {
		t.Fatalf("WriteFile init.dul failed: %v", err)
	}
	return cwd, dataDir, outDir
}

func writeMinimalSystemDBF(t *testing.T, path string) {
	t.Helper()
	const pageSize = 8192
	pageCount := 5
	raw := make([]byte, pageSize*pageCount)
	binary.LittleEndian.PutUint32(raw[0x80:], 16)
	binary.LittleEndian.PutUint32(raw[0x84:], pageSize)
	binary.LittleEndian.PutUint32(raw[0x8C:], uint32(pageCount))
	raw[pageSize*4+0x2D] = 1
	if err := os.WriteFile(path, raw, 0644); err != nil {
		t.Fatalf("WriteFile SYSTEM.DBF failed: %v", err)
	}
}

func writeMinimalMainDBFWithOneIntRow(t *testing.T, path string, tableID uint32, idValue int32) {
	t.Helper()
	const (
		pageSize               = 8192
		pageSlotTrailerLen     = 8
		dataRowAreaStart       = 0x62
		dataPageSlotCountOff   = 0x24
		dataPageFreeEndOff     = 0x26
		dataPageRecordCountOff = 0x2C
		dataPageTreeLevelOff   = 0x38
		dataPageAssistIndexOff = 0x3A
		dataFilePageGroupIDOff = 0
		dataFilePageFileIDOff  = 2
		dataFilePagePageNoOff  = 4
		dmPageKindOff          = 0x14
		dmPageKindRowData      = 0x14
	)
	page := make([]byte, pageSize)
	binary.LittleEndian.PutUint16(page[dataFilePageGroupIDOff:], 4)
	binary.LittleEndian.PutUint16(page[dataFilePageFileIDOff:], 0)
	binary.LittleEndian.PutUint32(page[dataFilePagePageNoOff:], 0)
	binary.LittleEndian.PutUint32(page[dmPageKindOff:], dmPageKindRowData)
	binary.LittleEndian.PutUint16(page[dataPageSlotCountOff:], 1)
	binary.LittleEndian.PutUint16(page[dataPageFreeEndOff:], dataRowAreaStart+7)
	binary.LittleEndian.PutUint16(page[dataPageRecordCountOff:], 1)
	binary.LittleEndian.PutUint16(page[dataPageTreeLevelOff:], 0)
	binary.LittleEndian.PutUint32(page[dataPageAssistIndexOff:], 0x02000000|(tableID&0x00FFFFFF))
	binary.BigEndian.PutUint16(page[dataRowAreaStart:], 7)
	page[dataRowAreaStart+2] = 0
	binary.LittleEndian.PutUint32(page[dataRowAreaStart+3:], uint32(idValue))
	slotArrayStart := pageSize - pageSlotTrailerLen - 2
	binary.LittleEndian.PutUint16(page[slotArrayStart:], dataRowAreaStart)
	if err := os.WriteFile(path, page, 0644); err != nil {
		t.Fatalf("WriteFile MAIN.DBF failed: %v", err)
	}
}

func runInDir(t *testing.T, dir string, fn func()) {
	t.Helper()
	previousDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd failed: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Chdir failed: %v", err)
	}
	defer func() {
		if err := os.Chdir(previousDir); err != nil {
			t.Fatalf("restore Chdir failed: %v", err)
		}
	}()
	fn()
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile %s failed: %v", path, err)
	}
	return string(raw)
}

func TestDataExportDiagnosticsRecordsRecoverySourceEvidence(t *testing.T) {
	dir := t.TempDir()
	session := interactiveSession{
		dataDir:    dir,
		dataDirSet: true,
		logPath:    "dul.log",
		logPathSet: true,
	}
	var stdout bytes.Buffer
	session.printDataExportDiagnostics(&stdout, &dm.DataExportResult{
		FallbackReasons: []string{"orphan storage recovery is heuristic"},
		RecoverySources: []dm.DataRecoverySource{{
			Owner: "APP", Name: "T_RECOVER", GroupID: 4, FileID: 0, StorageID: 44556678,
			FirstPage: 10, LastPage: 14, Pages: 3, RowsLocated: 8, RowsExported: 7, RowsFailed: 1, Heuristic: true,
		}},
	})
	session.closeLog()

	for _, want := range []string{
		"recovery source: target=APP.T_RECOVER",
		"group=4 file=0 storage_id=44556678",
		"pages=3 page_range=10-14",
		"attribution=heuristic-orphan",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("diagnostics should contain %q, got %q", want, stdout.String())
		}
	}
	logRaw, err := os.ReadFile(filepath.Join(dir, "dul.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logRaw), "[RECOVERY] recovery source: target=APP.T_RECOVER") {
		t.Fatalf("dul.log is missing recovery source evidence: %q", logRaw)
	}
}

func TestTimestampedLogLine(t *testing.T) {
	at := time.Date(2026, 6, 28, 10, 23, 45, 0, time.Local)
	got := timestampedLogLine("DMDUL> list user", at)
	want := "2026-06-28 10:23:45 DMDUL> list user"
	if got != want {
		t.Fatalf("timestampedLogLine = %q, want %q", got, want)
	}
}

func TestRemovedFunctionalCommandsAreRejected(t *testing.T) {
	commands := []string{"inspect", "inspect-ctl", "scan-system", "scan-partitions", "export-ddl", "export-data"}
	for _, command := range commands {
		t.Run(command, func(t *testing.T) {
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			err := Run([]string{command}, &stdout, &stderr)
			if err == nil {
				t.Fatalf("removed command %s should fail", command)
			}
			for _, want := range []string{"has been removed", "interactive shell"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error should contain %q, got %v", want, err)
				}
			}
		})
	}
}

func TestStartupHintsWhenNoSystemFile(t *testing.T) {
	previousDir, _ := os.Getwd()
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatalf("Chdir failed: %v", err)
	}
	defer os.Chdir(previousDir)

	var stdout, stderr bytes.Buffer
	if err := RunInteractive(strings.NewReader("exit;\n"), &stdout, &stderr); err != nil {
		t.Fatalf("RunInteractive returned error: %v", err)
	}
	out := stdout.String()
	if strings.Contains(out, "detected:") {
		t.Fatalf("no SYSTEM.DBF present, must not report a detected database, got:\n%s", out)
	}
	if !strings.Contains(out, "no SYSTEM.DBF found") || !strings.Contains(out, "set system") {
		t.Fatalf("startup should hint to set paths manually when no SYSTEM.DBF, got:\n%s", out)
	}
}

func TestSetSystemReportsUnreadableFile(t *testing.T) {
	previousDir, _ := os.Getwd()
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatalf("Chdir failed: %v", err)
	}
	defer os.Chdir(previousDir)

	missing := filepath.Join(t.TempDir(), "NOPE", "SYSTEM.DBF")
	var stdout, stderr bytes.Buffer
	if err := RunInteractive(strings.NewReader("set system "+missing+";\nexit;\n"), &stdout, &stderr); err != nil {
		t.Fatalf("RunInteractive returned error: %v", err)
	}
	out := stdout.String()
	if !strings.Contains(out, "detected: SYSTEM.DBF not readable") {
		t.Fatalf("set system with a missing file should report it, got:\n%s", out)
	}
}

func TestListDataFileReportsStatusAndPages(t *testing.T) {
	dir := t.TempDir()
	// One page-aligned file (2 pages) and one truncated file (not a multiple).
	good := make([]byte, 2*8192)
	// minimal DM page header so page-header scan recognizes it: group/file/page.
	binary.LittleEndian.PutUint16(good[0:], 4)       // group
	binary.LittleEndian.PutUint16(good[2:], 0)       // file
	binary.LittleEndian.PutUint32(good[0x14:], 0x14) // kind row-data
	if err := os.WriteFile(filepath.Join(dir, "MAIN.DBF"), good, 0644); err != nil {
		t.Fatal(err)
	}
	session := newInteractiveSession()
	session.dataDir = dir
	session.dataDirSet = true
	session.metadata.PageSize = 8192

	var stdout bytes.Buffer
	if err := session.printDataFiles(&stdout); err != nil {
		t.Fatalf("printDataFiles failed: %v", err)
	}
	out := stdout.String()
	if !strings.Contains(out, "MAIN.DBF") || !strings.Contains(out, "OK") {
		t.Fatalf("expected MAIN.DBF listed OK, got:\n%s", out)
	}
	if !strings.Contains(out, "data files:") || !strings.Contains(out, "page_size=8192") {
		t.Fatalf("expected summary line, got:\n%s", out)
	}
}

func TestDescribeTableShowsPhysicalLocationAndColumns(t *testing.T) {
	session := newInteractiveSession()
	session.dictionary = &dm.DictionaryInfo{
		Tables: []dm.DictionaryTable{{
			ID: 1268, Owner: "MOCK", Name: "T_CUSTOMER_MOCK", ColumnCount: 2,
			Tablespace: "TBS_MOCK", GroupID: 10, StorageID: 33555838,
			RootFile: 0, RootPage: 16, HeaderFile: 0, HeaderBlock: 16,
			Blocks: 269600, Extents: 16850, Bytes: 2208563200,
			Storage: "CLUSTERBTR", AssistIDs: []uint32{33555838, 33555700},
		}},
		Columns: []dm.DictionaryColumn{
			{TableID: 1268, ColID: 0, Name: "ID", DataType: "BIGINT", Length: 8, Nullable: "N"},
			{TableID: 1268, ColID: 1, Name: "AMOUNT", DataType: "DEC", Length: 12, Scale: 2, Default: "0"},
		},
	}
	var stdout bytes.Buffer
	if err := session.executeDescribe([]string{"MOCK.T_CUSTOMER_MOCK"}, &stdout); err != nil {
		t.Fatalf("executeDescribe failed: %v", err)
	}
	out := stdout.String()
	for _, want := range []string{
		"Table MOCK.T_CUSTOMER_MOCK",
		"table_id= 1268",
		"tablespace= TBS_MOCK",
		"storage_id= 33555838",
		"root= file#0/page#16",
		"blocks= 269600",
		"storage= CLUSTERBTR",
		"assist_ids= [33555838 33555700]",
		"Column information:",
		"ID", "BIGINT len=8", "NOT NULL",
		"AMOUNT", "DEC len=12 scale=2", "DEFAULT 0",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("describe output should contain %q, got:\n%s", want, out)
		}
	}
}

func TestDescribeRejectsBadNameAndMissingTable(t *testing.T) {
	session := newInteractiveSession()
	session.dictionary = &dm.DictionaryInfo{}
	var stdout bytes.Buffer
	if err := session.executeDescribe([]string{"NOT_QUALIFIED"}, &stdout); err == nil {
		t.Fatal("describe must reject a name without owner.table form")
	}
	if err := session.executeDescribe([]string{"APP.GHOST"}, &stdout); err == nil {
		t.Fatal("describe must report a table missing from the dictionary")
	}
}
