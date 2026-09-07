package dm

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSystemPrivilegeRawRowAndUnknownID(t *testing.T) {
	page := make([]byte, 256)
	start := 64
	binary.LittleEndian.PutUint16(page[start+1:], 44)
	for off, v := range map[int]uint32{4: 67108964, 8: ^uint32(0), 12: ^uint32(0), 16: 4110, 20: ^uint32(0)} {
		binary.LittleEndian.PutUint32(page[start+off:], v)
	}
	page[start+24] = 'Y'
	names := map[uint32]string{67108964: "PROBE_R"}
	got, ok := parseSystemPrivilegeRow(page, start, 256, names)
	if !ok || got.Privilege != "CREATE VIEW" || got.AdminOption != "Y" {
		t.Fatalf("%+v %t", got, ok)
	}
	sql, ok := systemPrivilegeSQL(got)
	if !ok || sql != "GRANT CREATE VIEW TO PROBE_R WITH ADMIN OPTION;" {
		t.Fatalf("%s %t", sql, ok)
	}
	page[start] = 0x80
	if _, ok := parseSystemPrivilegeRow(page, start, 256, names); ok {
		t.Fatal("deleted grant was recovered as active")
	}
	page[start] = 0
	binary.LittleEndian.PutUint32(page[start+16:], 4351)
	got, ok = parseSystemPrivilegeRow(page, start, 256, names)
	if !ok || got.PrivilegeID != 4351 || got.Privilege != "" {
		t.Fatal("unknown ID lost")
	}
	var out strings.Builder
	renderSystemPrivileges(&out, []DictionarySystemPrivilege{got})
	if !strings.Contains(out.String(), "WARNING") || strings.Contains(out.String(), "CREATE SESSION") {
		t.Fatal(out.String())
	}
	binary.LittleEndian.PutUint32(page[start+8:], 1001)
	if _, ok := parseSystemPrivilegeRow(page, start, 256, names); ok {
		t.Fatal("table grant mistaken for system privilege")
	}
}

func TestSystemPrivilegePersistenceAndScope(t *testing.T) {
	dir := t.TempDir()
	items := []DictionarySystemPrivilege{{Grantee: "APP", PrivilegeID: 4109, Privilege: "CREATE TABLE", AdminOption: "N"}, {Grantee: "ORPHAN_R", PrivilegeID: 4110, Privilege: "CREATE VIEW", AdminOption: "Y"}}
	dict := &DictionaryInfo{PageSize: 8192, Users: []DictionaryUser{{ID: 1, Name: "APP"}}, SystemPrivileges: items}
	if _, err := WriteDictionaryFiles(dir, dict); err != nil {
		t.Fatal(err)
	}
	got, _, err := LoadDictionaryFiles(dir)
	if err != nil || !reflect.DeepEqual(got.SystemPrivileges, items) {
		t.Fatalf("roundtrip=%+v %v", got, err)
	}
	users := map[uint32]dictionaryObject{1: {ID: 1, Name: "APP"}}
	roles := map[uint32]dictionaryObject{2: {ID: 2, Name: "ORPHAN_R"}}
	if selected := selectSystemPrivileges(items, users, roles, nil, newOwnerMatcher("all")); len(selected) != 2 {
		t.Fatalf("full omitted orphan role: %+v", selected)
	}
	if selected := selectSystemPrivileges(items, users, roles, nil, newOwnerMatcher("APP")); len(selected) != 1 || selected[0].Grantee != "APP" {
		t.Fatalf("owner scope leaked: %+v", selected)
	}
	if err := os.Remove(filepath.Join(dir, "sys_privs.tsv")); err != nil {
		t.Fatal(err)
	}
	got, _, err = LoadDictionaryFiles(dir)
	if err != nil || got.SystemPrivileges != nil {
		t.Fatalf("legacy dictionary: %+v %v", got, err)
	}
	bad := items[0]
	bad.Privilege = "CREATE TABLE; GRANT DBA TO APP"
	if _, ok := systemPrivilegeSQL(bad); ok {
		t.Fatal("accepted altered name for known privilege ID")
	}
}

func TestExportDDLSystemPrivilegesUseDictionaryAndScope(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "SYSTEM.DBF")
	writeDataExportTestSystem(t, path)
	dict := testDataExportDictionary(path, 123, 1, 1, 1)
	dict.SystemPrivileges = []DictionarySystemPrivilege{{Grantee: "APP", PrivilegeID: 4110, Privilege: "CREATE VIEW", AdminOption: "Y"}}
	opts := DDLExportOptions{SystemPath: path, OutputPath: filepath.Join(dir, "out.sql"), OwnerFilter: "APP", Dictionary: dict, DMPMode: DMPModeOwner}
	result, err := ExportDDL(opts)
	if err != nil {
		t.Fatal(err)
	}
	text, err := os.ReadFile(opts.OutputPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(text), "GRANT CREATE VIEW TO APP WITH ADMIN OPTION;") || strings.Contains(string(text), "GRANT CREATE SESSION") {
		t.Fatal(string(text))
	}
	if result.DMPMetadata.Counts().SystemPrivileges != 1 {
		t.Fatal("DMP omitted recovered privilege")
	}
	for _, rec := range result.DMPMetadata.GlobalRecords {
		if rec.RecordType == dmpRecordSystemPrivilege && rec.SQL != "GRANT CREATE VIEW TO APP WITH ADMIN OPTION;" {
			t.Fatalf("invented system privilege: %+v", rec)
		}
	}
	opts.TableFilter = "APP.T_PLAN"
	opts.DMPMode = DMPModeTables
	result, err = ExportDDL(opts)
	if err != nil {
		t.Fatal(err)
	}
	if result.DMPMetadata.Counts().SystemPrivileges != 0 {
		t.Fatal("table DMP leaked user privileges")
	}
	opts.TableFilter = "all"
	opts.DMPMode = DMPModeOwner
	dict.SystemPrivileges[0].PrivilegeID = 4351
	dict.SystemPrivileges[0].Privilege = ""
	if _, err := ExportDDL(opts); err == nil || !strings.Contains(err.Error(), "unresolved system privilege") {
		t.Fatalf("unknown DMP privilege=%v", err)
	}
}
