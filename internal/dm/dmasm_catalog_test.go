package dm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteAndLoadASMCatalogFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), DefaultDictionaryDirName)
	catalog := &ASMCatalog{
		Databases: []ASMCatalogDatabase{
			{
				CandidateNo: 2, DatabaseName: "DB2", DatabaseNameSrc: "DMASM path",
				SystemPath: "+DATA2/data/DB2/SYSTEM.DBF", Charset: "EUC-KR (UNICODE_FLAG=2)",
				CharsetFlag: 2, HasCharsetFlag: true, PageSize: 32768, PageCount: 512,
				ExtentSize: 64, CaseSensitive: false, HasCaseSensitive: true,
				DataFileCount: 1, Status: "OK", ASMMembers: "disk1,disk2",
			},
			{
				CandidateNo: 1, Selected: true, DatabaseName: "DB1", DatabaseNameSrc: "DMASM dm.ctl",
				SystemPath: "+DATA1/data/DB1/SYSTEM.DBF", ControlPath: "+DATA1/data/DB1/dm.ctl",
				Charset: "UTF-8 (UNICODE_FLAG=1)", CharsetFlag: 1, HasCharsetFlag: true,
				PageSize: 8192, PageCount: 1024, ExtentSize: 16,
				CaseSensitive: true, HasCaseSensitive: true, DataFileCount: 2, Status: "OK",
			},
		},
		DataFiles: []ASMCatalogDataFile{
			{CandidateNo: 1, DatabaseName: "DB1", SystemPath: "+DATA1/data/DB1/SYSTEM.DBF", GroupID: 4, FileID: 0, Tablespace: "MAIN", Pages: 2048, SizeBytes: 16777216, Status: "OK", Path: "+DATA1/data/DB1/MAIN.DBF"},
			{CandidateNo: 1, DatabaseName: "DB1", SystemPath: "+DATA1/data/DB1/SYSTEM.DBF", GroupID: 0, FileID: 0, Tablespace: "SYSTEM", Pages: 1024, SizeBytes: 8388608, Status: "OK", Path: "+DATA1/data/DB1/SYSTEM.DBF"},
			{CandidateNo: 2, DatabaseName: "DB2", SystemPath: "+DATA2/data/DB2/SYSTEM.DBF", GroupID: 0, FileID: 0, Tablespace: "SYSTEM", Pages: 512, SizeBytes: 16777216, Status: "OK", Path: "+DATA2/data/DB2/SYSTEM.DBF"},
		},
	}

	result, err := WriteASMCatalogFiles(dir, catalog)
	if err != nil {
		t.Fatal(err)
	}
	if result.DatabaseCount != 2 || result.DataFileCount != 3 {
		t.Fatalf("write result = %+v", result)
	}
	loaded, loadedFiles, err := LoadASMCatalogFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	if loadedFiles.DatabaseCount != 2 || loadedFiles.DataFileCount != 3 {
		t.Fatalf("load result = %+v", loadedFiles)
	}
	if len(loaded.Databases) != 2 || loaded.Databases[0].DatabaseName != "DB1" || !loaded.Databases[0].Selected || loaded.Databases[1].Selected {
		t.Fatalf("loaded databases = %+v", loaded.Databases)
	}
	if loaded.Databases[0].ControlPath != "+DATA1/data/DB1/dm.ctl" || loaded.Databases[1].ASMMembers != "disk1,disk2" {
		t.Fatalf("catalog evidence not preserved: %+v", loaded.Databases)
	}
	if len(loaded.DataFiles) != 3 || loaded.DataFiles[0].Path != "+DATA1/data/DB1/SYSTEM.DBF" || loaded.DataFiles[1].Path != "+DATA1/data/DB1/MAIN.DBF" {
		t.Fatalf("loaded data files = %+v", loaded.DataFiles)
	}
	for _, name := range []string{ASMDatabaseCatalogFileName, ASMDataFileCatalogFileName} {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(string(raw), "\n") {
			t.Fatalf("%s is not a complete TSV file", name)
		}
	}
}

func TestWriteASMCatalogFilesRewritesSelectedDatabase(t *testing.T) {
	dir := filepath.Join(t.TempDir(), DefaultDictionaryDirName)
	first := &ASMCatalog{Databases: []ASMCatalogDatabase{
		{CandidateNo: 1, Selected: true, DatabaseName: "DB1", SystemPath: "+DATA/DB1/SYSTEM.DBF"},
		{CandidateNo: 2, DatabaseName: "DB2", SystemPath: "+DATA/DB2/SYSTEM.DBF"},
	}}
	if _, err := WriteASMCatalogFiles(dir, first); err != nil {
		t.Fatal(err)
	}
	second := &ASMCatalog{Databases: []ASMCatalogDatabase{
		{CandidateNo: 1, DatabaseName: "DB1", SystemPath: "+DATA/DB1/SYSTEM.DBF"},
		{CandidateNo: 2, Selected: true, DatabaseName: "DB2", SystemPath: "+DATA/DB2/SYSTEM.DBF"},
	}}
	if _, err := WriteASMCatalogFiles(dir, second); err != nil {
		t.Fatal(err)
	}
	loaded, _, err := LoadASMCatalogFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Databases[0].Selected || !loaded.Databases[1].Selected {
		t.Fatalf("selected database was not rewritten: %+v", loaded.Databases)
	}
}
