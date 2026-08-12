package dm

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteAndLoadDictionaryFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), DefaultDictionaryDirName)
	dict := &DictionaryInfo{
		SystemPath:          `D:\temp\oldpro\SYSTEM.DBF`,
		ControlPath:         `D:\temp\oldpro\dm.ctl`,
		Source:              "SYSTEM.DBF",
		ExtentSize:          16,
		ExtentSizeSource:    "u32 @ 0x80",
		PageSize:            8192,
		PageCount:           9472,
		Charset:             "UTF-8",
		CharsetSource:       "UNICODE_FLAG=1",
		CaseSensitive:       false,
		CaseSensitiveSource: "SYSTEM.DBF page 4 + 0x2C",
		HasCaseSensitive:    true,
		ObjectCount:         781,
		BootstrapMode:       "standard-two-stage",
		BootstrapFallback:   true,
		Users: []DictionaryUser{
			{ID: 1, Name: "HR_TEST"},
		},
		Schemas: []DictionarySchema{
			{ID: 10, Name: "HR_TEST", OwnerID: 1, Owner: "HR_TEST"},
			{ID: 11, Name: "HR_ARCHIVE", OwnerID: 1, Owner: "HR_TEST"},
		},
		Tables: []DictionaryTable{
			{ID: 1035, Owner: "HR_TEST", Name: "EMP_INFO", ColumnCount: 2, Tablespace: "MAIN", GroupID: 4, HeaderFile: 0, HeaderBlock: 16, Bytes: 131072, Blocks: 16, Extents: 1, Storage: "CLUSTERBTR", Partitioned: true, StorageID: 33555530, RootFile: 0, RootPage: 16, AssistIDs: []uint32{33555530, 33555531}},
		},
		Columns: []DictionaryColumn{
			{TableID: 1035, TableOwner: "HR_TEST", TableName: "EMP_INFO", ColID: 1, Name: "EMP_ID", DataType: "INT", Nullable: "N"},
			{TableID: 1035, TableOwner: "HR_TEST", TableName: "EMP_INFO", ColID: 2, Name: "EMP_NAME", DataType: "VARCHAR", Length: 50, Nullable: "Y", Default: "'匿名'"},
		},
		Views: []DictionaryView{
			{ID: 2001, Owner: "SYSDBA", Name: "V_EMP", Valid: "Y", SQL: "CREATE OR REPLACE VIEW SYSDBA.V_EMP AS SELECT 1 AS ID"},
		},
		Sequences: []DictionarySequence{
			{ID: 2101, Owner: "SYSDBA", Name: "SEQ_EMP", Valid: "Y", StartWith: 1, HasStartWith: true, MinValue: 1, HasMinValue: true, MaxValue: 999999999999, HasMaxValue: true, IncrementBy: 1, CycleFlag: "N", OrderFlag: "N", CacheSize: 20, LastNumber: 21, HasLastNumber: true, RuntimeFile: 0, RuntimePage: 2, RuntimeSlot: 2, RuntimeState: 0x01, HasRuntimeLocator: true},
		},
		Routines: []DictionaryRoutine{
			{ID: 2151, Owner: "SYSDBA", Name: "F_EMP", ObjectType: "FUNCTION", SeqNo: 0, Valid: "Y", SQL: "CREATE OR REPLACE FUNCTION SYSDBA.F_EMP RETURN INT AS BEGIN RETURN 1; END;"},
		},
		Triggers: []DictionaryTrigger{
			{ID: 2201, Owner: "SYSDBA", Name: "TRG_EMP", TableOwner: "HR_TEST", TableName: "EMP_INFO", Valid: "Y", SQL: "CREATE OR REPLACE TRIGGER SYSDBA.TRG_EMP BEFORE INSERT ON HR_TEST.EMP_INFO BEGIN NULL; END;"},
		},
		Synonyms: []DictionarySynonym{
			{ID: 3001, Owner: "HR_TEST", Name: "SYN_V_EMP", TableOwner: "SYSDBA", TableName: "V_EMP"},
		},
		TabPrivileges: []DictionaryTabPrivilege{
			{Grantee: "HR_TEST", Owner: "SYSDBA", ObjectName: "V_EMP", ObjectType: "VIEW", Privilege: "SELECT", Grantable: "N"},
			{Grantee: "HR_TEST", Owner: "SYSDBA", ObjectName: "SEQ_EMP", ObjectType: "SEQUENCE", Privilege: "SELECT", Grantable: "N"},
		},
		Partitions: []DictionaryPartition{
			{BaseTableID: 1035, Owner: "HR_TEST", TableName: "EMP_INFO", Position: 1, Type: "RANGE", Name: "P1000", PartTableID: 1036, HighValue: []byte{0x01, 0x02, 0x03}, PageNo: 240, SlotNo: 4, SlotOffset: 90, RowOffset: 0x1E005A},
		},
		PartitionKeys: []DictionaryPartitionKey{
			{TableID: 1035, Owner: "HR_TEST", TableName: "EMP_INFO", Position: 1, ColID: 1, ColumnName: "EMP_ID"},
		},
	}

	written, err := WriteDictionaryFiles(dir, dict)
	if err != nil {
		t.Fatalf("WriteDictionaryFiles returned error: %v", err)
	}
	if written.UserCount != 1 || written.SchemaCount != 2 || written.TableCount != 1 || written.ColumnCount != 2 || written.ViewCount != 1 || written.SequenceCount != 1 || written.RoutineCount != 1 || written.TriggerCount != 1 || written.SynonymCount != 1 || written.TabPrivilegeCount != 2 || written.PartitionCount != 1 || written.PartitionKeyCount != 1 {
		t.Fatalf("unexpected written counts: %+v", written)
	}

	loaded, files, err := LoadDictionaryFiles(dir)
	if err != nil {
		t.Fatalf("LoadDictionaryFiles returned error: %v", err)
	}
	if files.Dir != dir || files.SchemaCount != 2 || files.ColumnCount != 2 || files.ViewCount != 1 || files.SequenceCount != 1 || files.RoutineCount != 1 || files.TriggerCount != 1 || files.SynonymCount != 1 || files.TabPrivilegeCount != 2 || files.PartitionCount != 1 || files.PartitionKeyCount != 1 {
		t.Fatalf("unexpected loaded files result: %+v", files)
	}
	if loaded.Source != "dictionary files" || loaded.DictionaryDir != dir {
		t.Fatalf("unexpected loaded source: source=%q dir=%q", loaded.Source, loaded.DictionaryDir)
	}
	if loaded.PageSize != 8192 || loaded.TableCount != 1 || loaded.ColumnCount != 2 {
		t.Fatalf("unexpected loaded dictionary counts: %+v", loaded)
	}
	if len(loaded.Schemas) != 2 || loaded.Schemas[0].Name != "HR_ARCHIVE" || loaded.Schemas[0].Owner != "HR_TEST" || loaded.Schemas[1].Name != "HR_TEST" {
		t.Fatalf("schema ownership was not preserved: %+v", loaded.Schemas)
	}
	if loaded.BootstrapMode != "standard-two-stage" || !loaded.BootstrapFallback {
		t.Fatalf("bootstrap metadata was not preserved: mode=%q fallback=%v", loaded.BootstrapMode, loaded.BootstrapFallback)
	}
	if !loaded.HasCaseSensitive || loaded.CaseSensitive || loaded.CaseSensitiveSource != "SYSTEM.DBF page 4 + 0x2C" {
		t.Fatalf("case-sensitive metadata was not preserved: %+v", loaded)
	}
	if loaded.Tables[0].HeaderBlock != 16 || loaded.Tables[0].Bytes != 131072 || loaded.Tables[0].Blocks != 16 || loaded.Tables[0].Extents != 1 {
		t.Fatalf("segment fields were not preserved: %+v", loaded.Tables[0])
	}
	if loaded.Tables[0].StorageID != 33555530 || loaded.Tables[0].RootFile != 0 || loaded.Tables[0].RootPage != 16 || len(loaded.Tables[0].AssistIDs) != 2 || loaded.Tables[0].AssistIDs[1] != 33555531 {
		t.Fatalf("storage recovery fields were not preserved: %+v", loaded.Tables[0])
	}
	if loaded.Columns[1].Default != "'匿名'" {
		t.Fatalf("default value was not preserved: %+v", loaded.Columns[1])
	}
	if len(loaded.Views) != 1 || loaded.Views[0].Name != "V_EMP" || loaded.Views[0].SQL == "" {
		t.Fatalf("view was not preserved: %+v", loaded.Views)
	}
	if len(loaded.Sequences) != 1 || loaded.Sequences[0].Name != "SEQ_EMP" || loaded.Sequences[0].IncrementBy != 1 || loaded.Sequences[0].MaxValue != 999999999999 || loaded.Sequences[0].CacheSize != 20 || !loaded.Sequences[0].HasLastNumber || loaded.Sequences[0].LastNumber != 21 || !loaded.Sequences[0].HasRuntimeLocator || loaded.Sequences[0].RuntimePage != 2 || loaded.Sequences[0].RuntimeSlot != 2 || loaded.Sequences[0].RuntimeState != 0x01 {
		t.Fatalf("sequence was not preserved: %+v", loaded.Sequences)
	}
	if len(loaded.Routines) != 1 || loaded.Routines[0].Name != "F_EMP" || loaded.Routines[0].ObjectType != "FUNCTION" || loaded.Routines[0].SQL == "" {
		t.Fatalf("routine was not preserved: %+v", loaded.Routines)
	}
	if len(loaded.Triggers) != 1 || loaded.Triggers[0].Name != "TRG_EMP" || loaded.Triggers[0].SQL == "" {
		t.Fatalf("trigger was not preserved: %+v", loaded.Triggers)
	}
	if len(loaded.Synonyms) != 1 || loaded.Synonyms[0].Name != "SYN_V_EMP" {
		t.Fatalf("synonym was not preserved: %+v", loaded.Synonyms)
	}
	if len(loaded.TabPrivileges) != 2 || loaded.TabPrivileges[0].Privilege != "SELECT" {
		t.Fatalf("tab privilege was not preserved: %+v", loaded.TabPrivileges)
	}
	if len(loaded.Partitions) != 1 || loaded.Partitions[0].PartTableID != 1036 || loaded.Partitions[0].HighValue[2] != 0x03 {
		t.Fatalf("partition was not preserved: %+v", loaded.Partitions)
	}
	if len(loaded.PartitionKeys) != 1 || loaded.PartitionKeys[0].ColID != 1 || loaded.PartitionKeys[0].ColumnName != "EMP_ID" {
		t.Fatalf("partition key was not preserved: %+v", loaded.PartitionKeys)
	}
}

func TestRebuildDictionaryFilesArchivesPreviousDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), DefaultDictionaryDirName)
	oldDict := &DictionaryInfo{
		Source:      "SYSTEM.DBF",
		ObjectCount: 1,
		Users:       []DictionaryUser{{ID: 1, Name: "OLD_USER"}},
	}
	if _, err := WriteDictionaryFiles(dir, oldDict); err != nil {
		t.Fatalf("write old dictionary: %v", err)
	}
	stalePath := filepath.Join(dir, "stale-manual-file.tsv")
	if err := os.WriteFile(stalePath, []byte("old-only\n"), 0644); err != nil {
		t.Fatalf("write stale dictionary file: %v", err)
	}

	newDict := &DictionaryInfo{
		Source:      "SYSTEM.DBF",
		ObjectCount: 2,
		Users:       []DictionaryUser{{ID: 2, Name: "NEW_USER"}},
	}
	result, backupDir, err := RebuildDictionaryFiles(dir, newDict)
	if err != nil {
		t.Fatalf("RebuildDictionaryFiles returned error: %v", err)
	}
	if result.Dir != dir || backupDir == "" {
		t.Fatalf("unexpected rebuild result: result=%+v backup=%q", result, backupDir)
	}
	if _, err := os.Stat(filepath.Join(dir, "stale-manual-file.tsv")); !os.IsNotExist(err) {
		t.Fatalf("stale file leaked into rebuilt dictionary: %v", err)
	}
	if raw, err := os.ReadFile(filepath.Join(backupDir, "stale-manual-file.tsv")); err != nil || string(raw) != "old-only\n" {
		t.Fatalf("previous dictionary was not archived: raw=%q err=%v", raw, err)
	}
	loaded, _, err := LoadDictionaryFiles(dir)
	if err != nil {
		t.Fatalf("load rebuilt dictionary: %v", err)
	}
	if len(loaded.Users) != 1 || loaded.Users[0].Name != "NEW_USER" || loaded.ObjectCount != 2 {
		t.Fatalf("rebuilt dictionary is not the new generation: %+v", loaded)
	}
}

func TestRebuildDictionaryFilesReplacesASMCatalogOnlyDirectoryWithoutBackup(t *testing.T) {
	dir := filepath.Join(t.TempDir(), DefaultDictionaryDirName)
	catalog := &ASMCatalog{
		Databases: []ASMCatalogDatabase{{
			CandidateNo: 1, Selected: true, DatabaseName: "ASM_DB",
			SystemPath: "+DATA/ASM_DB/SYSTEM.DBF", PageSize: 8192, Status: "OK",
		}},
	}
	if _, err := WriteASMCatalogFiles(dir, catalog); err != nil {
		t.Fatalf("write ASM-only catalog: %v", err)
	}
	result, backupDir, err := RebuildDictionaryFiles(dir, &DictionaryInfo{
		Source: "SYSTEM.DBF", SystemPath: "+DATA/ASM_DB/SYSTEM.DBF",
		ObjectCount: 1, Users: []DictionaryUser{{ID: 1, Name: "APP"}},
	})
	if err != nil {
		t.Fatalf("rebuild dictionary over ASM-only catalog: %v", err)
	}
	if result.Dir != dir || backupDir != "" {
		t.Fatalf("result=%+v backup=%q, want no backup for catalog-only directory", result, backupDir)
	}
	if _, err := os.Stat(filepath.Join(dir, "meta.tsv")); err != nil {
		t.Fatalf("rebuilt dictionary missing meta.tsv: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ASMDatabaseCatalogFileName)); !os.IsNotExist(err) {
		t.Fatalf("ASM catalog should be refreshed explicitly after rebuild: %v", err)
	}
}

func TestLoadDictionaryFilesAcceptsUTF8BOMHeaders(t *testing.T) {
	dir := filepath.Join(t.TempDir(), DefaultDictionaryDirName)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	files := dictionaryFilesResultForDir(dir)
	if err := os.WriteFile(files.MetaPath, []byte("\ufeffkey\tvalue\nformat_version\t1\npage_size\t8192\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(files.UsersPath, []byte("\ufeffuser_id\tuser_name\n1\tHR_TEST\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(files.TablesPath, []byte("\ufefftable_id\towner\ttable_name\tcolumn_count\ttablespace\tgroup_id\ttemporary\tstorage\tpartitioned\n1035\tHR_TEST\tEMP_INFO\t1\tMAIN\t4\tNO\tCLUSTERBTR\tNO\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(files.ColumnsPath, []byte("\ufefftable_id\towner\ttable_name\tcol_id\tcolumn_name\tdata_type\tlength\tscale\tnullable\tdefault\n1035\tHR_TEST\tEMP_INFO\t1\tEMP_ID\tINT\t\t0\tN\t\n"), 0644); err != nil {
		t.Fatal(err)
	}

	loaded, result, err := LoadDictionaryFiles(dir)
	if err != nil {
		t.Fatalf("LoadDictionaryFiles returned error: %v", err)
	}
	if result.UserCount != 1 || result.SchemaCount != 1 || result.TableCount != 1 || result.ColumnCount != 1 {
		t.Fatalf("unexpected result counts: %+v", result)
	}
	if len(loaded.Schemas) != 1 || loaded.Schemas[0].Name != "HR_TEST" || loaded.Schemas[0].Owner != "HR_TEST" {
		t.Fatalf("legacy dictionary should synthesize the default schema: %+v", loaded.Schemas)
	}
	if loaded.PageSize != 8192 || loaded.Users[0].Name != "HR_TEST" || loaded.Tables[0].Name != "EMP_INFO" || loaded.Columns[0].Name != "EMP_ID" {
		t.Fatalf("unexpected loaded dictionary: %+v", loaded)
	}
}
