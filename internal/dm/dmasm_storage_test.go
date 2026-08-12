package dm

import (
	"reflect"
	"testing"
)

func TestRawASMStorageSystemFiles(t *testing.T) {
	storage := &RawASMStorage{groups: map[uint16]*RawASMGroup{
		1: {
			groupID: 1,
			name:    "DATA1",
			files: map[string]RawASMFileInfo{
				"+DATA1/DB1/SYSTEM.DBF": {Path: "+DATA1/DB1/SYSTEM.DBF"},
				"+DATA1/DB1/MAIN.DBF":   {Path: "+DATA1/DB1/MAIN.DBF"},
				"+DATA1/DB1/OLD":        {Path: "+DATA1/DB1/OLD/SYSTEM.DBF", IsDir: true},
			},
		},
		2: {
			groupID: 2,
			name:    "DATA2",
			files: map[string]RawASMFileInfo{
				"+DATA2/DB2/system.dbf": {Path: "+DATA2/DB2/system.dbf"},
			},
		},
	}}

	files := storage.SystemFiles()
	if len(files) != 2 {
		t.Fatalf("SYSTEM.DBF candidates = %#v", files)
	}
	if files[0].Path != "+DATA1/DB1/SYSTEM.DBF" || files[0].GroupName != "DATA1" {
		t.Fatalf("first SYSTEM.DBF = %+v", files[0])
	}
	if files[1].Path != "+DATA2/DB2/system.dbf" || files[1].GroupName != "DATA2" {
		t.Fatalf("second SYSTEM.DBF = %+v", files[1])
	}
}

func TestSelectASMDatabaseDataFilesSeparatesSameDirectoryAcrossDatabases(t *testing.T) {
	db1System := "+DATA1/data/DAMENG/SYSTEM.DBF"
	db2System := "+DATA2/data/DAMENG/SYSTEM.DBF"
	files := []RawASMFileInfo{
		{Path: db1System, GroupID: 1},
		{Path: "+DATA1/data/DAMENG/MAIN.DBF", GroupID: 1},
		{Path: "+DATA1/data/DAMENG/TEMP0.DBF", GroupID: 1},
		{Path: db2System, GroupID: 2},
		{Path: "+DATA2/data/DAMENG/MAIN.DBF", GroupID: 2},
		{Path: "+DATA2/data/DAMENG/TEMP0.DBF", GroupID: 2},
		{Path: "+APP1/data/DAMENG/TBS_APP01.DBF", GroupID: 3},
		{Path: "+APP2/data/DAMENG/TBS_APP02.DBF", GroupID: 4},
	}
	systems := []RawASMFileInfo{{Path: db1System}, {Path: db2System}}
	control := &ControlInfo{Entries: []ControlEntry{
		{ID: 0, Name: "SYSTEM", Paths: []ControlPath{{Value: db1System}}},
		{ID: 4, Name: "MAIN", Paths: []ControlPath{{Value: "+DATA1/data/DAMENG/MAIN.DBF"}}},
		{ID: 5, Name: "TBS_APP", Paths: []ControlPath{{Value: "+APP1/data/DAMENG/TBS_APP01.DBF"}}},
	}}

	selected, tablespaces := selectASMDatabaseDataFiles(db1System, files, systems, control)
	paths := make([]string, 0, len(selected))
	for _, file := range selected {
		paths = append(paths, file.Path)
	}
	want := []string{
		"+DATA1/data/DAMENG/MAIN.DBF",
		"+DATA1/data/DAMENG/SYSTEM.DBF",
		"+DATA1/data/DAMENG/TEMP0.DBF",
		"+APP1/data/DAMENG/TBS_APP01.DBF",
	}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("selected paths = %#v, want %#v", paths, want)
	}
	if tablespaces[normalizeASMPath("+APP1/data/DAMENG/TBS_APP01.DBF")] != "TBS_APP" {
		t.Fatalf("control tablespace names = %#v", tablespaces)
	}
	for _, path := range paths {
		if normalizeASMPath(path) == normalizeASMPath(db2System) || normalizeASMPath(path) == normalizeASMPath("+APP2/data/DAMENG/TBS_APP02.DBF") {
			t.Fatalf("second database file leaked into first candidate: %s", path)
		}
	}
}

func TestSelectASMDatabaseDataFilesAllowsUniqueDirectoryAcrossGroups(t *testing.T) {
	system := "+DATA/data/UNIQUE/SYSTEM.DBF"
	files := []RawASMFileInfo{
		{Path: system, GroupID: 1},
		{Path: "+APP/data/UNIQUE/TBS_APP01.DBF", GroupID: 2},
	}
	selected, _ := selectASMDatabaseDataFiles(system, files, []RawASMFileInfo{{Path: system}}, nil)
	if len(selected) != 2 {
		t.Fatalf("unique database directory must span groups: %#v", selected)
	}
}
