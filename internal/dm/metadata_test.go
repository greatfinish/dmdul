package dm

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

type metadataReadTracker struct {
	reader  *bytes.Reader
	offsets []int64
}

func (r *metadataReadTracker) ReadAt(p []byte, off int64) (int, error) {
	r.offsets = append(r.offsets, off)
	return r.reader.ReadAt(p, off)
}

func TestDefaultDatabaseMetadataUsesDminitDefaults(t *testing.T) {
	meta := DefaultDatabaseMetadata()
	if meta.DatabaseName != "DAMENG" || meta.InstanceName != "DMSERVER" {
		t.Fatalf("default names = %s/%s", meta.DatabaseName, meta.InstanceName)
	}
	if meta.ExtentSize != 16 || meta.PageSize != 8192 || meta.CharsetFlag != 0 || !meta.HasCharsetFlag {
		t.Fatalf("default storage metadata = %+v", meta)
	}
	if !meta.CaseSensitive || !meta.HasCaseSensitive {
		t.Fatalf("default case-sensitive metadata = %+v", meta)
	}
}

func TestInspectDatabaseMetadataCombinesSystemControlAndIni(t *testing.T) {
	dir := t.TempDir()
	const pageSize = uint32(8192)
	const pageCount = uint32(5)
	systemPath := filepath.Join(dir, "SYSTEM.DBF")
	controlPath := filepath.Join(dir, "dm.ctl")
	iniPath := filepath.Join(dir, "dm.ini")

	raw := make([]byte, int(pageSize*pageCount))
	binary.LittleEndian.PutUint32(raw[systemExtentSizeOffset:], 64)
	binary.LittleEndian.PutUint32(raw[systemPageSizeOffset:], pageSize)
	binary.LittleEndian.PutUint32(raw[systemPageCountOffset:], pageCount)
	raw[int(pageSize)*systemControlPage4No+systemCaseSensitiveFlagOffset] = 0
	raw[int(pageSize)*systemControlPage4No+systemUnicodeFlagOffset] = 2
	if err := os.WriteFile(systemPath, raw, 0644); err != nil {
		t.Fatal(err)
	}
	control := make([]byte, 0x100)
	binary.LittleEndian.PutUint32(control, 4096)
	copy(control[4:], []byte("TESTDB\x00"))
	if err := os.WriteFile(controlPath, control, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(iniPath, []byte("INSTANCE_NAME = TESTSERVER\n"), 0644); err != nil {
		t.Fatal(err)
	}

	meta := InspectDatabaseMetadata(systemPath, controlPath, "", "auto")
	if meta.DatabaseName != "TESTDB" || meta.DatabaseNameSrc != "dm.ctl" {
		t.Fatalf("database metadata = %+v", meta)
	}
	if meta.InstanceName != "TESTSERVER" || meta.InstanceNameSrc != "dm.ini" {
		t.Fatalf("instance metadata = %+v", meta)
	}
	if meta.ExtentSize != 64 || meta.PageSize != pageSize || meta.PageCount != pageCount {
		t.Fatalf("storage metadata = %+v", meta)
	}
	if meta.CharsetFlag != 2 || meta.Charset != "EUC-KR (UNICODE_FLAG=2)" || !meta.HasCharsetFlag {
		t.Fatalf("charset metadata = %+v", meta)
	}
	if meta.CaseSensitive || !meta.HasCaseSensitive {
		t.Fatalf("case-sensitive metadata = %+v", meta)
	}
}

func TestInspectDatabaseHeaderMetadataFromReaderUsesOnlyHeaderAndControlFlags(t *testing.T) {
	const pageSize = uint32(8192)
	const pageCount = uint32(7)
	raw := make([]byte, int(pageSize*pageCount))
	binary.LittleEndian.PutUint32(raw[systemExtentSizeOffset:], 32)
	binary.LittleEndian.PutUint32(raw[systemPageSizeOffset:], pageSize)
	binary.LittleEndian.PutUint32(raw[systemPageCountOffset:], pageCount)
	raw[int(pageSize)*systemControlPage4No+systemCaseSensitiveFlagOffset] = 0
	raw[int(pageSize)*systemControlPage4No+systemUnicodeFlagOffset] = 1
	reader := &metadataReadTracker{reader: bytes.NewReader(raw)}

	meta, err := InspectDatabaseHeaderMetadataFromReader(
		"+DATA/DB1/SYSTEM.DBF", reader, int64(len(raw)), "auto",
	)
	if err != nil {
		t.Fatal(err)
	}
	if meta.PageSize != pageSize || meta.PageCount != pageCount || meta.ExtentSize != 32 {
		t.Fatalf("storage metadata = %+v", meta)
	}
	if meta.Charset != "UTF-8 (UNICODE_FLAG=1)" || meta.CaseSensitive {
		t.Fatalf("persistent flags = %+v", meta)
	}
	wantOffsets := []int64{
		0,
		int64(pageSize)*systemControlPage4No + systemUnicodeFlagOffset,
		int64(pageSize)*systemControlPage4No + systemCaseSensitiveFlagOffset,
	}
	if len(reader.offsets) != len(wantOffsets) {
		t.Fatalf("ReadAt offsets = %v, want %v", reader.offsets, wantOffsets)
	}
	for index := range wantOffsets {
		if reader.offsets[index] != wantOffsets[index] {
			t.Fatalf("ReadAt offsets = %v, want %v", reader.offsets, wantOffsets)
		}
	}
}

func TestInspectControlDatabaseNameFromReader(t *testing.T) {
	raw := make([]byte, 0x84)
	binary.LittleEndian.PutUint32(raw, 4096)
	copy(raw[4:], []byte("ASM_DB\x00"))
	name, err := InspectControlDatabaseNameFromReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatal(err)
	}
	if name != "ASM_DB" {
		t.Fatalf("database name = %q", name)
	}
	if _, err := InspectControlDatabaseNameFromReader(bytes.NewReader(raw[:7]), 7); err == nil {
		t.Fatal("short dm.ctl must be rejected")
	}
}

func TestInspectControlFileFromReaderRecoversASMDataFilePaths(t *testing.T) {
	raw := make([]byte, 0x200)
	binary.LittleEndian.PutUint32(raw, 4096)
	copy(raw[4:], []byte("ASM_DB\x00"))
	const nameOffset = 0x106
	binary.LittleEndian.PutUint32(raw[nameOffset-6:], 5)
	copy(raw[nameOffset:], []byte("TBS_APP\x00"))
	copy(raw[0x120:], []byte("+APP/data/ASM_DB/TBS_APP01.DBF\x00"))

	info, err := InspectControlFileFromReader(bytes.NewReader(raw), int64(len(raw)), "+DATA/data/ASM_DB/dm.ctl")
	if err != nil {
		t.Fatal(err)
	}
	if info.DatabaseName != "ASM_DB" || info.Path != "+DATA/data/ASM_DB/dm.ctl" || info.Size != int64(len(raw)) {
		t.Fatalf("control info = %+v", info)
	}
	if len(info.Entries) != 1 || info.Entries[0].ID != 5 || info.Entries[0].Name != "TBS_APP" || len(info.Entries[0].Paths) != 1 {
		t.Fatalf("control entries = %+v", info.Entries)
	}
	if info.Entries[0].Paths[0].Value != "+APP/data/ASM_DB/TBS_APP01.DBF" {
		t.Fatalf("control path = %+v", info.Entries[0].Paths)
	}
}

func TestDetectSystemCharsetFromDataUsesControlPage4UnicodeFlag(t *testing.T) {
	pageSize := uint32(128)
	data := make([]byte, int(pageSize)*5)
	data[int(pageSize)*systemControlPage4No+systemUnicodeFlagOffset] = 1

	charset, ok := detectSystemCharsetFromData(data, pageSize)
	if !ok {
		t.Fatalf("detectSystemCharsetFromData() did not detect charset")
	}
	if charset.DecoderName != "utf-8" || charset.Flag != 1 {
		t.Fatalf("charset = %+v, want UTF-8 flag 1", charset)
	}
}

func TestDetectSystemCaseSensitiveFromDataUsesAdjacentControlFlag(t *testing.T) {
	pageSize := uint32(128)
	data := make([]byte, int(pageSize)*5)
	offset := int(pageSize)*systemControlPage4No + systemCaseSensitiveFlagOffset

	data[offset] = 0
	caseSensitive, ok := detectSystemCaseSensitiveFromData(data, pageSize)
	if !ok || caseSensitive {
		t.Fatalf("case_sensitive = %v, ok=%v, want 0", caseSensitive, ok)
	}

	data[offset] = 1
	caseSensitive, ok = detectSystemCaseSensitiveFromData(data, pageSize)
	if !ok || !caseSensitive {
		t.Fatalf("case_sensitive = %v, ok=%v, want 1", caseSensitive, ok)
	}

	data[offset] = 2
	if _, ok := detectSystemCaseSensitiveFromData(data, pageSize); ok {
		t.Fatal("invalid case_sensitive flag must not be accepted")
	}
}

func TestCharsetComparisonMatchesIniNumericValue(t *testing.T) {
	meta := DatabaseMetadata{
		Charset:        "EUC-KR (UNICODE_FLAG=2)",
		CharsetSource:  "SYSTEM.DBF page 4 + 0x2D",
		CharsetFlag:    2,
		HasCharsetFlag: true,
		IniCharset:     "2",
	}

	if got := meta.CharsetComparison(); got != "match" {
		t.Fatalf("CharsetComparison() = %q, want match", got)
	}
}

func TestRestorePageProtectionBytesRestoresFourKBoundary(t *testing.T) {
	page := make([]byte, 8192)
	copy(page[0x0FFC:0x1000], []byte{0xAA, 0xBB, 0xCC, 0xDD})
	copy(page[0x1FF0:0x1FF4], []byte("cess"))

	restorePageProtectionBytes(page, 8192)

	if got := string(page[0x0FFC:0x1000]); got != "cess" {
		t.Fatalf("restored boundary = %q, want cess", got)
	}
	if got := pageTailReservedLen(8192); got != 16 {
		t.Fatalf("pageTailReservedLen(8192) = %d, want 16", got)
	}
	if got := pageTailReservedLen(32768); got != 40 {
		t.Fatalf("pageTailReservedLen(32768) = %d, want 40", got)
	}
}

func TestRestorePageProtectionBytesKeepsPlausibleBoundary(t *testing.T) {
	page := make([]byte, 32768)
	copy(page[0x4FFC:0x5000], []byte("RREN"))
	copy(page[0x7FE8:0x7FEC], []byte{0xCB, 0x09, 0x91, 0x09})

	restorePageProtectionBytes(page, 32768)

	if got := string(page[0x4FFC:0x5000]); got != "RREN" {
		t.Fatalf("boundary = %q, want RREN", got)
	}
}

// nearlyFullDataPage builds an 8 KiB row page whose slot directory grows into
// the tail reserved area, mirroring the layout that previously made
// restorePageProtectionBytes copy slot entries over live row bytes.
func nearlyFullDataPage() []byte {
	page := make([]byte, 8192)
	binary.LittleEndian.PutUint32(page[dmPageChecksumOffset:], 0x5265B2DF)
	const nSlot = 117
	binary.LittleEndian.PutUint16(page[dataPageSlotCountOff:], nSlot)
	freeEnd := uint16(0x1EEF)
	binary.LittleEndian.PutUint16(page[dataPageFreeEndOff:], freeEnd)
	copy(page[0x0FFC:0x1000], []byte{0x08, 0x00, 0x22, 0x01})
	slotArrayStart := 8192 - pageSlotTrailerLen - nSlot*2
	offset := uint16(dataRowAreaStart)
	for slot := nSlot - 1; slot >= 0; slot-- {
		binary.LittleEndian.PutUint16(page[slotArrayStart+slot*2:], offset)
		offset += 0x44
	}
	return page
}

func TestRestorePageProtectionBytesSkipsSlotEntriesInTail(t *testing.T) {
	page := nearlyFullDataPage()
	if !tailBytesAreSlotEntries(page, 8192, 0x1FF0) {
		t.Fatal("tail slot entries not detected")
	}
	want := append([]byte(nil), page...)

	restorePageProtectionBytes(page, 8192)

	if !equalBytes(page, want) {
		t.Fatalf("nearly full page was modified: boundary=% X tail=% X", page[0x0FF8:0x1004], page[0x1FE8:0x1FF8])
	}
}

func TestRestorePageProtectionBytesAcceptsFreeSlotSentinelInTail(t *testing.T) {
	page := nearlyFullDataPage()
	binary.LittleEndian.PutUint16(page[0x1FF0:], 0xFFFF)
	if !tailBytesAreSlotEntries(page, 8192, 0x1FF0) {
		t.Fatal("tail slot entries with a 0xFFFF free/deleted sentinel were not detected")
	}
	want := append([]byte(nil), page...)

	restorePageProtectionBytes(page, 8192)

	if !equalBytes(page, want) {
		t.Fatalf("page with a free tail slot was modified: boundary=% X tail=% X", page[0x0FF8:0x1004], page[0x1FE8:0x1FF8])
	}
}

func TestRestorePageProtectionBytesStillRestoresDictionaryBackups(t *testing.T) {
	// Dictionary pages carrying real sector backups have tail bytes that do
	// not decode to plausible slot offsets, so restoration must still run
	// even when the header advertises a slot count that reaches the tail.
	page := make([]byte, 8192)
	binary.LittleEndian.PutUint32(page[dmPageChecksumOffset:], 0x11223344)
	binary.LittleEndian.PutUint16(page[dataPageSlotCountOff:], 60)
	binary.LittleEndian.PutUint16(page[dataPageFreeEndOff:], 0x0800)
	copy(page[0x0FFC:0x1000], []byte{0xAA, 0xBB, 0xCC, 0xDD})
	copy(page[0x1FF0:0x1FF4], []byte("cess"))

	if tailBytesAreSlotEntries(page, 8192, 0x1FF0) {
		t.Fatal("ASCII backup bytes misclassified as slot entries")
	}

	restorePageProtectionBytes(page, 8192)

	if got := string(page[0x0FFC:0x1000]); got != "cess" {
		t.Fatalf("restored boundary = %q, want cess", got)
	}
}

func TestDataFilePageReaderReadsUserPagesVerbatim(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "TBS_TEST01.DBF")
	page := make([]byte, 8192)
	// Boundary bytes score worse than the tail bytes, so the old reader-side
	// restoration would have rewritten them.
	copy(page[0x0FFC:0x1000], []byte{0xAA, 0xBB, 0xCC, 0xDD})
	copy(page[0x1FF0:0x1FF4], []byte("data"))
	if err := os.WriteFile(path, page, 0o644); err != nil {
		t.Fatal(err)
	}

	key := dataFileKey{groupID: 4, fileID: 0}
	reader := newDataFilePageReader([]dataFileRef{{key: key, path: path}}, 8192)
	defer func() { _ = reader.close() }()
	got, err := reader.readPage(dataPageRef{key: key, pageNo: 0})
	if err != nil {
		t.Fatal(err)
	}
	if !equalBytes(got, page) {
		t.Fatalf("reader altered page bytes: boundary=% X", got[0x0FFC:0x1000])
	}

	cache := newDataFilePageCache([]dataFileRef{{key: key, path: path}}, 8192)
	cached, ok := cache.readPage(dataPageRef{key: key, pageNo: 0})
	if !ok {
		t.Fatal("cache read failed")
	}
	if !equalBytes(cached, page) {
		t.Fatalf("cache altered page bytes: boundary=% X", cached[0x0FFC:0x1000])
	}
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
