package dm

import (
	"bytes"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestRawASMFileReadsFragmentedExtentChain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dmasm.raw")
	file := createRawASMTestDisk(t, path)

	writeRawASMAUHeader(t, file, 0, rawASMAUTypeXDesc)
	writeRawASMAUHeader(t, file, 1, rawASMAUTypeXDesc)
	writeRawASMAUHeader(t, file, 4, rawASMAUTypeInode)

	first := rawASMDAddr{diskID: 0, auNo: 0, offset: 0x4a0}
	second := rawASMDAddr{diskID: 0, auNo: 1, offset: 0x420}
	writeRawASMXDesc(t, file, first, rawASMDAddr{}, second)
	writeRawASMXDesc(t, file, second, first, rawASMDAddr{})
	writeRawASMInode(t, file, 4, 0x400, 0x82000009, "+DMDATA/DMDB/SYSTEM.DBF", rawASMExtentSize+16, 2, first)

	writeRawASMBytes(t, file, rawASMReservedSize+20*rawASMAUSize, make([]byte, 8))
	writeRawASMBytes(t, file, rawASMReservedSize+20*rawASMAUSize+8, []byte("SYSTEM-HEAD"))
	writeRawASMBytes(t, file, rawASMReservedSize+20*rawASMAUSize+rawASMExtentSize-4, []byte("ABCD"))
	writeRawASMBytes(t, file, rawASMReservedSize+5*rawASMAUSize, []byte("EFGHIJKLMNOPQRST"))
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	group, err := OpenRawASMGroup(path)
	if err != nil {
		t.Fatal(err)
	}
	defer group.Close()
	if group.GroupID() != 2 {
		t.Fatalf("group id = %d, want 2", group.GroupID())
	}
	files := group.Files()
	if len(files) != 1 || files[0].Path != "+DMDATA/DMDB/SYSTEM.DBF" {
		t.Fatalf("unexpected files: %#v", files)
	}
	sources, err := group.DataFiles("+DMDATA/DMDB/SYSTEM.DBF")
	if err != nil {
		t.Fatalf("DataFiles(%#v): %v", files, err)
	}
	if len(sources) != 1 || sources[0].GroupID != 0 || sources[0].FileID != 0 || sources[0].Reader.Size() != rawASMExtentSize+16 {
		t.Fatalf("unexpected DBF sources: %#v", sources)
	}

	system, err := group.Open("+dmdata\\dmdb\\system.dbf")
	if err != nil {
		t.Fatal(err)
	}
	head := make([]byte, 11)
	if _, err := system.ReadAt(head, 8); err != nil {
		t.Fatal(err)
	}
	if string(head) != "SYSTEM-HEAD" {
		t.Fatalf("head = %q", head)
	}
	cross := make([]byte, 12)
	if _, err := system.ReadAt(cross, rawASMExtentSize-4); err != nil {
		t.Fatal(err)
	}
	if string(cross) != "ABCDEFGHIJKL" {
		t.Fatalf("cross-extent read = %q", cross)
	}
	tail := make([]byte, 32)
	n, err := system.ReadAt(tail, rawASMExtentSize)
	if err != io.EOF || n != 16 {
		t.Fatalf("tail read = %d, %v; want 16, EOF", n, err)
	}
}

func TestRawASMRejectsBrokenXDescChain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broken.raw")
	file := createRawASMTestDisk(t, path)
	writeRawASMAUHeader(t, file, 0, rawASMAUTypeXDesc)
	writeRawASMAUHeader(t, file, 4, rawASMAUTypeInode)
	first := rawASMDAddr{diskID: 0, auNo: 0, offset: 0x4a0}
	bad := rawASMDAddr{diskID: 0, auNo: 0, offset: 0x4a1}
	writeRawASMXDesc(t, file, first, rawASMDAddr{}, bad)
	writeRawASMInode(t, file, 4, 0x400, 0x82000009, "+DMDATA/DMDB/SYSTEM.DBF", 2*rawASMExtentSize, 2, first)
	file.Close()

	group, err := OpenRawASMGroup(path)
	if err != nil {
		t.Fatal(err)
	}
	defer group.Close()
	if _, err := group.Open("+DMDATA/DMDB/SYSTEM.DBF"); err == nil {
		t.Fatal("expected invalid XDESC offset error")
	}
}

func TestRawASMMirrorAUSizeValidation(t *testing.T) {
	for _, test := range []struct {
		value uint16
		valid bool
	}{
		{value: 0, valid: false},
		{value: 1, valid: true},
		{value: 3, valid: false},
		{value: 4, valid: true},
		{value: 32, valid: true},
		{value: 64, valid: true},
		{value: 128, valid: false},
	} {
		if got := validRawASMMirrorAUMiB(test.value); got != test.valid {
			t.Fatalf("validRawASMMirrorAUMiB(%d) = %v, want %v", test.value, got, test.valid)
		}
	}
}

func TestRawASMInodeTieBreakIsDeterministic(t *testing.T) {
	base := RawASMFileInfo{ID: 7, Path: "+DATA/DB/T.DBF", Size: 8192, Extents: 1, InodeDisk: 2, InodeAU: 9, InodeOff: 0x600}
	preferred := base
	preferred.InodeDisk = 1
	preferred.InodeAU = 12
	if !sameRawASMInodeMetadata(base, preferred) || !lessRawASMInodeLocation(preferred, base) {
		t.Fatalf("expected the lower physical INODE location to win: base=%+v preferred=%+v", base, preferred)
	}
	conflict := base
	conflict.Size++
	if sameRawASMInodeMetadata(base, conflict) {
		t.Fatal("conflicting INODE sizes must not be treated as duplicate metadata")
	}
}

func TestRawASMRealDisk(t *testing.T) {
	diskList := strings.TrimSpace(os.Getenv("DMDUL_TEST_DMASM_DISKS"))
	filePath := strings.TrimSpace(os.Getenv("DMDUL_TEST_DMASM_FILE"))
	if diskList == "" || filePath == "" {
		t.Skip("set DMDUL_TEST_DMASM_DISKS and DMDUL_TEST_DMASM_FILE for the read-only integration test")
	}
	group, err := OpenRawASMGroup(strings.Split(diskList, ",")...)
	if err != nil {
		t.Fatal(err)
	}
	defer group.Close()
	asmFile, err := group.Open(filePath)
	if err != nil {
		t.Fatal(err)
	}
	header := make([]byte, systemHeaderReadSize)
	if _, err := asmFile.ReadAt(header, 0); err != nil {
		t.Fatal(err)
	}
	pageSize, _ := detectSystemPageSize(header, asmFile.Size())
	if pageSize == 0 {
		t.Fatalf("cannot detect SYSTEM.DBF page size from %s", filePath)
	}
	pageCount, _ := detectSystemPageCount(header, asmFile.Size(), pageSize)
	if int64(pageCount)*int64(pageSize) != asmFile.Size() {
		t.Fatalf("page geometry %d x %d does not match inode size %d", pageCount, pageSize, asmFile.Size())
	}
	t.Logf("group=%d files=%d system=%s size=%d page_size=%d page_count=%d extents=%d",
		group.GroupID(), len(group.Files()), asmFile.Info().Path, asmFile.Size(), pageSize, pageCount, asmFile.Info().Extents)
	dictionary, err := LoadDictionary(DictionaryOptions{
		SystemPath:   asmFile.Info().Path,
		SystemReader: asmFile,
		SystemSize:   asmFile.Size(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if dictionary.ObjectCount == 0 || dictionary.UserCount == 0 {
		t.Fatalf("empty dictionary: objects=%d users=%d", dictionary.ObjectCount, dictionary.UserCount)
	}
	t.Logf("dictionary objects=%d users=%d tables=%d columns=%d mode=%s",
		dictionary.ObjectCount, dictionary.UserCount, dictionary.TableCount, dictionary.ColumnCount, dictionary.BootstrapMode)
	for _, table := range dictionary.Tables {
		t.Logf("table=%s.%s id=%d columns=%d storage=%d root=%d/%d",
			table.Owner, table.Name, table.ID, table.ColumnCount, table.StorageID, table.RootFile, table.RootPage)
	}
	if target := strings.TrimSpace(os.Getenv("DMDUL_TEST_DMASM_TABLE")); target != "" {
		diagnoseRawASMTablePage(t, group, dictionary, target)
	}
	if rawID := strings.TrimSpace(os.Getenv("DMDUL_TEST_DMASM_TEXT_ID")); rawID != "" {
		id, err := strconv.ParseUint(rawID, 0, 32)
		if err != nil {
			t.Fatalf("DMDUL_TEST_DMASM_TEXT_ID: %v", err)
		}
		diagnoseRawASMTextRows(t, asmFile, dictionary, uint32(id))
	}
}

func diagnoseRawASMTextRows(t *testing.T, system *RawASMFile, dictionary *DictionaryInfo, id uint32) {
	t.Helper()
	needle := make([]byte, 4)
	binary.LittleEndian.PutUint32(needle, id)
	page := make([]byte, dictionary.PageSize)
	matched := 0
	for pageNo := uint32(0); pageNo < dictionary.PageCount; pageNo++ {
		if _, err := system.ReadAt(page, int64(pageNo)*int64(dictionary.PageSize)); err != nil {
			t.Fatal(err)
		}
		if !isProbableDMDataPage(page, dictionary.PageSize) {
			continue
		}
		for _, row := range locateRowsInDataPage(page, dictionary.PageSize, int(binary.LittleEndian.Uint16(page[dataPageRecordCountOff:]))) {
			start, end := int(row.offset), int(row.offset)+int(row.length)
			if start < 0 || end > len(page) || !bytes.Contains(page[start:end], needle) {
				continue
			}
			parsed, ok := parseDDLTextRow(page, start, pageNo, row.slotNo, row.offset, dictionary.PageSize, textDecoder{preferred: "utf-8"})
			t.Logf("text id=%d page=%d slot=%d off=0x%X len=%d parsed=%t value=%+v hex=%X", id, pageNo, row.slotNo, row.offset, row.length, ok, parsed, page[start:end])
			matched++
		}
	}
	if matched == 0 {
		t.Fatalf("text id %d was not found", id)
	}
}

func diagnoseRawASMTablePage(t *testing.T, group *RawASMGroup, dictionary *DictionaryInfo, target string) {
	t.Helper()
	owner, name, ok := strings.Cut(target, ".")
	if !ok {
		t.Fatalf("DMDUL_TEST_DMASM_TABLE must be owner.table, got %q", target)
	}
	var table DictionaryTable
	for _, candidate := range dictionary.Tables {
		if strings.EqualFold(candidate.Owner, owner) && strings.EqualFold(candidate.Name, name) {
			table = candidate
			break
		}
	}
	if table.ID == 0 {
		t.Fatalf("diagnostic table %s not found", target)
	}
	var source OfflineDataSource
	for _, candidate := range mustRawASMDataFiles(t, group, dictionary.SystemPath) {
		if candidate.GroupID == table.GroupID && candidate.FileID == table.RootFile {
			source = candidate
			break
		}
	}
	if source.Reader == nil {
		t.Fatalf("data source group=%d file=%d not found", table.GroupID, table.RootFile)
	}
	page := make([]byte, dictionary.PageSize)
	if _, err := source.Reader.ReadAt(page, int64(table.RootPage)*int64(dictionary.PageSize)); err != nil {
		t.Fatal(err)
	}
	nSlot := binary.LittleEndian.Uint16(page[dataPageSlotCountOff:])
	nRec := binary.LittleEndian.Uint16(page[dataPageRecordCountOff:])
	freeEnd := binary.LittleEndian.Uint16(page[dataPageFreeEndOff:])
	rows := locateRowsInDataPage(page, dictionary.PageSize, int(nRec))
	t.Logf("diagnostic %s root=%d/%d kind=0x%X storage=%d slots=%d records=%d free_end=0x%X trailer=%d rows=%d",
		target, table.RootFile, table.RootPage, dataPageKind(page), dataPageStorageID(page), nSlot, nRec, freeEnd, pageSlotTrailerLenForPage(page), len(rows))
	var columns []columnDef
	for _, column := range dictionary.Columns {
		if column.TableID == table.ID {
			columns = append(columns, columnDef{ColID: column.ColID, Name: column.Name, DataType: column.DataType, Length: column.Length, Scale: column.Scale, Nullable: column.Nullable, Default: column.Default})
		}
	}
	for _, row := range rows {
		start, end := int(row.offset), int(row.offset)+int(row.length)
		_, _, _, err := parseDataRowValues(page[start:end], columns, textDecoder{preferred: "utf-8"}, false, nil)
		t.Logf("row slot=%d off=0x%X len=%d deleted=%t parse=%v", row.slotNo, row.offset, row.length, row.deleted, err)
		if row == rows[0] {
			t.Logf("row hex=%X", page[start:end])
		}
	}
}

func mustRawASMDataFiles(t *testing.T, group *RawASMGroup, systemPath string) []OfflineDataSource {
	t.Helper()
	sources, err := group.DataFiles(systemPath)
	if err != nil {
		t.Fatal(err)
	}
	return sources
}

func createRawASMTestDisk(t *testing.T, path string) *os.File {
	t.Helper()
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0600)
	if err != nil {
		t.Fatal(err)
	}
	const lastAU = uint32(31)
	if err := file.Truncate(rawASMReservedSize + int64(lastAU+1)*rawASMAUSize); err != nil {
		t.Fatal(err)
	}
	header := make([]byte, 128)
	binary.LittleEndian.PutUint16(header[0:2], 2)
	binary.LittleEndian.PutUint16(header[2:4], 0)
	binary.LittleEndian.PutUint32(header[8:12], rawASMNonMirrorFormat)
	binary.LittleEndian.PutUint32(header[16:20], rawASMAUTypeXDesc)
	binary.LittleEndian.PutUint32(header[20:24], lastAU)
	copy(header[24:], "DMASMtestdisk")
	writeRawASMBytes(t, file, 0, header)
	return file
}

func writeRawASMAUHeader(t *testing.T, file *os.File, auNo, auType uint32) {
	t.Helper()
	header := make([]byte, 24)
	binary.LittleEndian.PutUint16(header[0:2], 2)
	binary.LittleEndian.PutUint16(header[2:4], 0)
	binary.LittleEndian.PutUint32(header[4:8], auNo)
	binary.LittleEndian.PutUint32(header[8:12], rawASMNonMirrorFormat)
	binary.LittleEndian.PutUint32(header[16:20], auType)
	writeRawASMBytes(t, file, rawASMReservedSize+int64(auNo)*rawASMAUSize, header)
}

func writeRawASMInode(t *testing.T, file *os.File, auNo, offset, id uint32, path string, size int64, extents uint32, first rawASMDAddr) {
	t.Helper()
	entry := make([]byte, rawASMInodeSize)
	binary.LittleEndian.PutUint32(entry[0:4], id)
	copy(entry[4:4+rawASMMaxPathSize], path)
	binary.LittleEndian.PutUint64(entry[0x104:0x10c], uint64(size))
	binary.LittleEndian.PutUint32(entry[rawASMInodeExtentCount:rawASMInodeExtentCount+4], extents)
	putRawASMDAddr(entry[rawASMInodeFirstXDesc:rawASMInodeFirstXDesc+10], first)
	writeRawASMBytes(t, file, rawASMReservedSize+int64(auNo)*rawASMAUSize+int64(offset), entry)
}

func writeRawASMXDesc(t *testing.T, file *os.File, at, previous, next rawASMDAddr) {
	t.Helper()
	desc := make([]byte, rawASMXDescSize)
	putRawASMDAddr(desc[0:10], previous)
	putRawASMDAddr(desc[10:20], next)
	writeRawASMBytes(t, file, rawASMReservedSize+int64(at.auNo)*rawASMAUSize+int64(at.offset), desc)
}

func putRawASMDAddr(raw []byte, address rawASMDAddr) {
	binary.LittleEndian.PutUint16(raw[0:2], address.diskID)
	binary.LittleEndian.PutUint32(raw[2:6], address.auNo)
	binary.LittleEndian.PutUint32(raw[6:10], address.offset)
}

func writeRawASMBytes(t *testing.T, file *os.File, offset int64, data []byte) {
	t.Helper()
	if _, err := file.WriteAt(data, offset); err != nil {
		t.Fatal(err)
	}
}
