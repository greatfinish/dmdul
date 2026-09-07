package dm

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestColumnGrantDoesNotBecomeTableGrant(t *testing.T) {
	page := make([]byte, 256)
	base := 64
	binary.LittleEndian.PutUint16(page[base+1:], 44)
	for off, value := range map[int]uint32{4: 67108964, 8: 1018, 12: 1, 16: 8195, 20: 50331649} {
		binary.LittleEndian.PutUint32(page[base+off:], value)
	}
	page[base+24] = 'Y'
	grant, ok := parseDDLObjectPrivilegeRow(page, base, 1, 1, uint16(base), 256)
	if !ok || grant.ColumnID != 1 {
		t.Fatalf("grant=%+v %t", grant, ok)
	}
	page[base] = 0x80
	if _, ok := parseDDLObjectPrivilegeRow(page, base, 1, 1, uint16(base), 256); ok {
		t.Fatal("deleted column grant was recovered as active")
	}
	page[base] = 0
	item := DictionaryTabPrivilege{Owner: "SYSDBA", ObjectName: "GRANT_PROBE", Grantee: "GRANT_PROBE_R", Privilege: grant.Privilege, Grantable: grant.Grantable}
	enrichColumnPrivilege(&item, grant, map[uint32][]columnDef{1018: {{ColID: 1, Name: "Mixed Column"}}}, map[uint32]string{50331649: "SYSDBA"})
	var out strings.Builder
	renderTabPrivileges(&out, []DictionaryTabPrivilege{item})
	if !strings.Contains(out.String(), `GRANT UPDATE ("Mixed Column") ON SYSDBA.GRANT_PROBE TO GRANT_PROBE_R WITH GRANT OPTION;`) {
		t.Fatal(out.String())
	}
	item.ColumnName = ""
	out.Reset()
	renderTabPrivileges(&out, []DictionaryTabPrivilege{item})
	if strings.Contains(out.String(), "GRANT UPDATE ON") || !strings.Contains(out.String(), "WARNING") {
		t.Fatal("unresolved column escalated to table grant: " + out.String())
	}
}

func TestColumnPrivilegesTSVCompatibility(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tab_privs.tsv")
	id := uint16(0)
	want := []DictionaryTabPrivilege{{Owner: "SYSDBA", ObjectName: "P", Grantee: "R", ObjectType: "TABLE", Privilege: "REFERENCES", Grantable: "N", ColumnID: &id, ColumnName: "ID", Grantor: "SYSDBA"}, {Owner: "SYSDBA", ObjectName: "P", Grantee: "R", ObjectType: "TABLE", Privilege: "SELECT", Grantable: "N"}}
	if err := writeDictionaryTabPrivileges(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := readDictionaryTabPrivileges(path)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("got=%+v err=%v", got, err)
	}
	if err := writeTSV(path, []string{"grantee", "owner", "object_name", "object_type", "privilege", "grantable"}, [][]string{{"R", "SYSDBA", "P", "TABLE", "SELECT", "N"}}); err != nil {
		t.Fatal(err)
	}
	got, err = readDictionaryTabPrivileges(path)
	if err != nil || len(got) != 1 || got[0].ColumnID != nil || got[0].ColumnName != "" {
		t.Fatalf("legacy=%+v %v", got, err)
	}
	first := want[0]
	second := first
	id2 := uint16(1)
	second.ColumnID = &id2
	if dictionaryPrivilegeKey(first) == dictionaryPrivilegeKey(second) {
		t.Fatal("different column IDs collapsed")
	}
}

func TestDMPColumnGrantMatchesOfficialRecord(t *testing.T) {
	// dexp V8 03134284336-20250117-257733-20132, record at 0x10EA.
	want, _ := hex.DecodeString("1100FFFF060000005359534442410D0000004752414E545F50524F42455F520A0000005245464552454E434553060000005359534442410B0000004752414E545F50524F4245020000004944010000004E")
	id := uint16(0)
	record := dmpObjectGrantRecord(DictionaryTabPrivilege{Owner: "SYSDBA", ObjectName: "GRANT_PROBE", Grantee: "GRANT_PROBE_R", Grantor: "SYSDBA", Privilege: "REFERENCES", Grantable: "N", ColumnID: &id, ColumnName: "ID"})
	f, err := os.CreateTemp(t.TempDir(), "grant")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	charset, err := dmpCharsetFromName("utf-8")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeDMPMetadataRecord(f, charset, record); err != nil {
		t.Fatal(err)
	}
	f.Close()
	got, err := os.ReadFile(f.Name())
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("record=%X want=%X err=%v", got, want, err)
	}
}
