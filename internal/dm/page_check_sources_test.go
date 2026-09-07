package dm

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckPagesIncludesSystemAndVerifiesRawBytes(t *testing.T) {
	dir := t.TempDir()
	raw := make([]byte, 8192*2)
	putTestDMPageHeader(raw[:8192], 4, 0, 0, 0x13, 0)
	page, _ := sectorHashTestPage(8192, "SM3")
	copy(raw[8192:], page)
	if err := os.WriteFile(filepath.Join(dir, "MAIN.DBF"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	system := make([]byte, 8192*2)
	putTestDMPageHeader(system[:8192], 0, 0, 0, 0x13, 0)
	putTestDMPageHeader(system[8192:], 0, 0, 1, 0xFFFF00FF, 0)
	if err := os.WriteFile(filepath.Join(dir, "SYSTEM.DBF"), system, 0600); err != nil {
		t.Fatal(err)
	}
	opts := PageCheckOptions{DataDir: dir, PageSize: 8192, PageCheckMode: 2, PageHashName: "SM3"}
	result, err := CheckPages(opts)
	if err != nil {
		t.Fatal(err)
	}
	if result.FilesChecked != 2 || result.PagesChecked != 4 || result.BadPagesTotal != 0 || result.ChecksumNotApplicable != 3 {
		t.Fatalf("result=%+v", result)
	}
	opts.FileFilter = []string{"SYSTEM.DBF"}
	result, err = CheckPages(opts)
	if err != nil || result.FilesChecked != 1 || result.PagesChecked != 2 {
		t.Fatalf("SYSTEM scan=%+v err=%v", result, err)
	}
	opts.FileFilter = []string{"MISSING.DBF"}
	if _, err = CheckPages(opts); err == nil {
		t.Fatal("zero files incorrectly reported a clean scan")
	}
	got, err := os.ReadFile(filepath.Join(dir, "MAIN.DBF"))
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatal("physical input was modified")
	}
}

func TestPhysicalPrecheckRejectsInvalidAlgorithm(t *testing.T) {
	src := OfflineDataSource{Reader: bytes.NewReader(make([]byte, 8192))}
	for _, opts := range []PageCheckOptions{{PageSize: 8192, PageCheckMode: 4}, {PageSize: 8192, PageCheckMode: 2, PageHashName: "BAD_HASH"}} {
		if _, err := CheckPhysicalPageSource(src, opts); err == nil {
			t.Fatal("invalid checksum option was silently ignored")
		}
		if _, err := CheckPages(opts); err == nil || strings.Contains(err.Error(), "data_dir") {
			t.Fatalf("options not validated first: %v", err)
		}
	}
}

func TestSystemMetadataChecksumExemptionStillChecksIdentity(t *testing.T) {
	page := make([]byte, 8192)
	putTestDMPageHeader(page, 0, 0, 4, 0x64, 0)
	binary.LittleEndian.PutUint32(page[4:], 5)
	kind, _, ok := classifyPageCorruption(page, dataFileKey{}, 4, 8192, PageCheckOptions{PageCheckMode: 2, PageHashName: "SM3"})
	if ok || kind != PageCorruptionHeader {
		t.Fatalf("bad metadata identity bypassed check: %s %t", kind, ok)
	}
}
