package dm

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

const (
	systemHeaderReadSize          = 0x100
	systemExtentSizeOffset        = 0x80
	systemPageSizeOffset          = 0x84
	systemPageCountOffset         = 0x8C
	systemControlPage4No          = 4
	systemCaseSensitiveFlagOffset = 0x2C
	systemUnicodeFlagOffset       = 0x2D
	sysObjectsSlotCountOff        = 0x24
	systemSectorSize              = 4096
	pageSlotTrailerLen            = 8
)

var ImportantSystemObjectNames = []string{
	"SYSOBJECTS",
	"SYSCOLUMNS",
	"SYSTEXTS",
	"SYSGRANTS",
	"SYSHPARTTABLEINFO",
	"SYSDISTABLEINFO",
	"SYSPWDCHGS",
	"SYSCONTEXTINDEXES",
	"SYSTABLECOMMENTS",
	"SYSCOLUMNCOMMENTS",
	"SYSUSERS",
	"SYSOBJINFOS",
	"SYSCOLINFOS",
	"SYSUSERINI$",
	"SYSDEPENDENCIES",
}

type SystemInfo struct {
	Path             string
	Size             int64
	ExtentSize       uint32
	ExtentSizeSource string
	PageSize         uint32
	PageSizeSource   string
	PageCount        uint32
	PageCountSource  string
	Objects          []ObjectLocation
	Missing          []string
}

type ObjectLocation struct {
	Name       string
	Owner      string
	SchemaID   uint32
	ObjectID   uint32
	ParentID   int32
	Valid      string
	Type       string
	Subtype    string
	PageNo     uint32
	SlotNo     uint16
	SlotOffset uint16
	RowOffset  uint64
	NameOffset uint64
}

func ScanSystem(path string, targetNames []string) (*SystemInfo, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open SYSTEM.DBF: %w", err)
	}
	defer file.Close()

	stat, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat SYSTEM.DBF: %w", err)
	}

	header := make([]byte, systemHeaderReadSize)
	if _, err := io.ReadFull(file, header); err != nil {
		return nil, fmt.Errorf("read SYSTEM.DBF header: %w", err)
	}

	info := &SystemInfo{
		Path: path,
		Size: stat.Size(),
	}
	info.ExtentSize, info.ExtentSizeSource = detectSystemExtentSize(header)
	info.PageSize, info.PageSizeSource, err = detectPageSizeFromReader(file, stat.Size(), header)
	if err != nil {
		return nil, err
	}
	info.PageCount, info.PageCountSource = detectSystemPageCount(header, stat.Size(), info.PageSize)

	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("seek SYSTEM.DBF: %w", err)
	}

	targetSet := make(map[string]bool, len(targetNames))
	for _, name := range targetNames {
		targetSet[strings.ToUpper(name)] = true
	}

	var allObjects []ObjectLocation
	page := make([]byte, info.PageSize)
	for pageNo := uint32(0); ; pageNo++ {
		n, err := io.ReadFull(file, page)
		if err == io.EOF {
			break
		}
		if err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read SYSTEM.DBF page %d: %w", pageNo, err)
		}
		if uint32(n) != info.PageSize {
			break
		}
		restorePageProtectionBytes(page, info.PageSize)

		objects := scanSysObjectRowsInPage(page, pageNo, info.PageSize, nil)
		allObjects = append(allObjects, objects...)
	}

	schemaNames := schemaNamesFromObjectLocations(allObjects)
	for _, obj := range allObjects {
		obj.Owner = resolveSchemaName(obj.SchemaID, schemaNames)
		if targetSet[obj.Name] {
			info.Objects = append(info.Objects, obj)
		}
	}

	sort.Slice(info.Objects, func(i, j int) bool {
		if info.Objects[i].RowOffset == info.Objects[j].RowOffset {
			return info.Objects[i].Name < info.Objects[j].Name
		}
		return info.Objects[i].RowOffset < info.Objects[j].RowOffset
	})

	found := make(map[string]bool, len(info.Objects))
	for _, obj := range info.Objects {
		found[obj.Name] = true
	}
	for _, name := range targetNames {
		if !found[name] {
			info.Missing = append(info.Missing, name)
		}
	}

	return info, nil
}

func detectSystemExtentSize(header []byte) (uint32, string) {
	if len(header) >= systemExtentSizeOffset+4 {
		value := binary.LittleEndian.Uint32(header[systemExtentSizeOffset:])
		if value > 0 && value <= 65536 {
			return value, "u32 @ 0x80"
		}
	}
	return 0, "unknown"
}

func detectSystemPageSize(header []byte, fileSize int64) (uint32, string) {
	if len(header) >= systemPageSizeOffset+4 {
		value := binary.LittleEndian.Uint32(header[systemPageSizeOffset:])
		if validPageSize(value) && fileSize >= int64(value) {
			return value, "u32 @ 0x84"
		}
	}

	return 0, "unknown: page header unavailable; multi-page probe required"
}

func detectSystemPageCount(header []byte, fileSize int64, pageSize uint32) (uint32, string) {
	if pageSize == 0 || fileSize < 0 {
		return 0, "unknown"
	}
	if len(header) >= systemPageCountOffset+4 {
		value := binary.LittleEndian.Uint32(header[systemPageCountOffset:])
		if value > 0 && uint64(value)*uint64(pageSize) == uint64(fileSize) {
			return value, "u32 @ 0x8C"
		}
	}
	return uint32(fileSize / int64(pageSize)), "computed from file size"
}

func validPageSize(value uint32) bool {
	switch value {
	case 4096, 8192, 16384, 32768:
		return true
	default:
		return false
	}
}

func restorePageProtectionBytes(page []byte, pageSize uint32) {
	if len(page) < int(pageSize) {
		return
	}
	if layout, ok := detectDMPageHashLayout(page, true); ok {
		restoreDMHashSectorBytes(page, layout)
		return
	}
	if _, ok := inferDMPageHashSizeFromSlots(page); ok {
		return
	}
	sectors := int(pageSize) / systemSectorSize
	if sectors <= 1 || int(pageSize)%systemSectorSize != 0 {
		return
	}
	tailStart := int(pageSize) - pageTailReservedLen(pageSize)
	if tailStart < 0 || tailStart >= int(pageSize) {
		return
	}
	// DM stores the original four bytes before each 4 KiB sector boundary
	// in the page tail, replacing the in-place bytes with page protection data.
	for sector := 1; sector < sectors; sector++ {
		src := tailStart + (sector-1)*4
		dst := sector*systemSectorSize - 4
		if src+4 > int(pageSize) || dst+4 > int(pageSize) {
			return
		}
		if tailBytesAreSlotEntries(page, pageSize, src) {
			continue
		}
		if shouldRestoreProtectionBytes(page[dst:dst+4], page[src:src+4]) {
			copy(page[dst:dst+4], page[src:src+4])
		}
	}
}

// restoreUserDataPageProtectionBytes restores only pages whose slot layout
// proves that a sector-protection backup area is present. This keeps ordinary
// fixed-trailer pages byte-for-byte unchanged while repairing row bytes that
// cross a 4 KiB sector boundary.
func restoreUserDataPageProtectionBytes(page []byte, pageSize uint32) {
	if len(page) < int(pageSize) || int(pageSize)%systemSectorSize != 0 {
		return
	}
	if layout, ok := detectDMPageHashLayout(page, true); ok {
		restoreDMHashSectorBytes(page, layout)
		return
	}
	// LOB and Long Row pages have no ordinary slot directory from which to
	// infer the protection trailer. Their page kind is authoritative and their
	// final reserved area contains one four-byte backup for every 4 KiB sector
	// boundary. Without restoring it, large CLOB/BLOB/VARCHAR values acquire
	// protection words every 4096 bytes and fail text/binary round trips.
	if kind := dataPageKind(page); kind == dmPageKindLOBData || kind == dmPageKindLongRowData {
		restorePayloadPageProtectionBytes(page, pageSize)
		return
	}
	trailerLen, ok := inferDMPageProtectionTrailerLen(page)
	if !ok || trailerLen != pageTailReservedLen(pageSize) {
		return
	}
	sectors := int(pageSize) / systemSectorSize
	tailStart := int(pageSize) - trailerLen
	for sector := 1; sector < sectors; sector++ {
		src := tailStart + (sector-1)*4
		dst := sector*systemSectorSize - 4
		if src+4 > int(pageSize) || dst+4 > int(pageSize) {
			return
		}
		copy(page[dst:dst+4], page[src:src+4])
	}
}

func restorePayloadPageProtectionBytes(page []byte, pageSize uint32) {
	sectors := int(pageSize) / systemSectorSize
	if sectors <= 1 || len(page) < int(pageSize) {
		return
	}
	tailStart := int(pageSize) - pageTailReservedLen(pageSize)
	if tailStart < 0 {
		return
	}
	for sector := 1; sector < sectors; sector++ {
		src := tailStart + (sector-1)*4
		dst := sector*systemSectorSize - 4
		if src+4 > len(page) || dst+4 > len(page) {
			return
		}
		copy(page[dst:dst+4], page[src:src+4])
	}
}

// tailBytesAreSlotEntries reports whether the four tail bytes at src belong to
// the page's slot directory rather than sector-boundary backups. Nearly full
// pages grow the slot array into the tail reserved area; copying slot entries
// over the 4 KiB boundary silently corrupts row data. Real backups fail the
// row-offset plausibility check (e.g. ASCII text decodes to offsets beyond the
// free-space end), so pages that genuinely carry backups are still restored.
func tailBytesAreSlotEntries(page []byte, pageSize uint32, src int) bool {
	if len(page) < dataPageFreeEndOff+2 || src < 0 || src+4 > len(page) {
		return false
	}
	nSlot := int(binary.LittleEndian.Uint16(page[dataPageSlotCountOff:]))
	if nSlot <= 0 || nSlot >= 2048 {
		return false
	}
	slotArrayEnd := int(pageSize) - pageSlotTrailerLen
	slotArrayStart := slotArrayEnd - nSlot*2
	if slotArrayStart < 0x40 || src < slotArrayStart || src+4 > slotArrayEnd {
		return false
	}
	freeEnd := int(binary.LittleEndian.Uint16(page[dataPageFreeEndOff:]))
	if freeEnd < 0x40 || freeEnd > int(pageSize) {
		return false
	}
	for _, pos := range []int{src, src + 2} {
		off := int(binary.LittleEndian.Uint16(page[pos:]))
		if off != 0 && off != 0xFFFF && (off < 0x40 || off >= freeEnd) {
			return false
		}
	}
	return true
}

func shouldRestoreProtectionBytes(current []byte, backup []byte) bool {
	return protectionBytesScore(backup) > protectionBytesScore(current)
}

func protectionBytesScore(raw []byte) int {
	score := 0
	for _, b := range raw {
		switch {
		case b >= 'A' && b <= 'Z':
			score += 4
		case b >= 'a' && b <= 'z':
			score += 4
		case b >= '0' && b <= '9':
			score += 3
		case b == '_' || b == '$' || b == '#' || b == ' ':
			score += 2
		case b >= 0x20 && b <= 0x7E:
			score += 1
		case b >= 0x80 && b != 0xFF:
			score += 0
		default:
			score -= 4
		}
	}
	return score
}

func pageTailReservedLen(pageSize uint32) int {
	sectors := int(pageSize) / systemSectorSize
	if sectors <= 1 || int(pageSize)%systemSectorSize != 0 {
		return 8
	}
	return (sectors-1)*4 + 12
}

func scanSysObjectRowsInPage(page []byte, pageNo uint32, pageSize uint32, targetSet map[string]bool) []ObjectLocation {
	if len(page) < int(pageSize) || len(page) < sysObjectsSlotCountOff+2 {
		return nil
	}

	slotCount := binary.LittleEndian.Uint16(page[sysObjectsSlotCountOff:])
	if slotCount == 0 || slotCount >= 2048 {
		return nil
	}

	slotArrayStart := int(pageSize) - pageSlotTrailerLenForPage(page) - int(slotCount)*2
	if slotArrayStart < 0x40 || slotArrayStart >= int(pageSize) {
		return nil
	}

	var result []ObjectLocation
	for slotNo := uint16(0); slotNo < slotCount; slotNo++ {
		pos := slotArrayStart + int(slotNo)*2
		slotOffset := binary.LittleEndian.Uint16(page[pos:])
		if slotOffset == 0 || int(slotOffset) >= int(pageSize) {
			continue
		}

		obj, ok := parseSysObjectRow(page, int(slotOffset), pageNo, slotNo, slotOffset, pageSize)
		if !ok {
			continue
		}
		if targetSet == nil || targetSet[obj.Name] {
			result = append(result, obj)
		}
	}
	return result
}

func parseSysObjectRow(page []byte, rowOff int, pageNo uint32, slotNo uint16, slotOffset uint16, pageSize uint32) (ObjectLocation, bool) {
	if rowOff+0x50 >= int(pageSize) {
		return ObjectLocation{}, false
	}

	validByte := page[rowOff+0x3F]
	valid := ""
	if validByte == 'Y' || validByte == 'N' {
		valid = string([]byte{validByte})
	}

	name, next, ok := readShortString(page, rowOff+0x40, int(pageSize))
	if !ok {
		return ObjectLocation{}, false
	}
	objType, next, ok := readShortString(page, next, int(pageSize))
	if !ok {
		return ObjectLocation{}, false
	}
	subtype, ok := readOptionalObjectSubtype(page, next, int(pageSize), objType)
	if !ok {
		return ObjectLocation{}, false
	}

	rowAbs := uint64(pageNo)*uint64(pageSize) + uint64(rowOff)
	return ObjectLocation{
		Name:       name,
		Owner:      schemaName(binary.LittleEndian.Uint32(page[rowOff+0x0B:])),
		SchemaID:   binary.LittleEndian.Uint32(page[rowOff+0x0B:]),
		ObjectID:   binary.LittleEndian.Uint32(page[rowOff+0x07:]),
		ParentID:   int32(binary.LittleEndian.Uint32(page[rowOff+0x0F:])),
		Valid:      valid,
		Type:       objType,
		Subtype:    subtype,
		PageNo:     pageNo,
		SlotNo:     slotNo,
		SlotOffset: slotOffset,
		RowOffset:  rowAbs,
		NameOffset: rowAbs + 0x41,
	}, true
}

func readShortString(page []byte, markerOff int, pageSize int) (string, int, bool) {
	if markerOff >= pageSize {
		return "", markerOff, false
	}
	marker := page[markerOff]
	if marker == 0x80 {
		return "", markerOff + 1, true
	}
	if marker < 0x81 || marker > 0xBF {
		return "", markerOff, false
	}

	n := int(marker - 0x80)
	start := markerOff + 1
	end := start + n
	if end > pageSize {
		return "", markerOff, false
	}

	value := string(page[start:end])
	for _, ch := range value {
		if ch < 32 || ch == 127 {
			return "", markerOff, false
		}
	}
	return value, end, true
}

func readOptionalObjectSubtype(page []byte, markerOff int, pageSize int, objType string) (string, bool) {
	if markerOff < pageSize {
		marker := page[markerOff]
		if marker >= 0x80 && marker <= 0xBF {
			value, _, ok := readShortString(page, markerOff, pageSize)
			if ok {
				return value, true
			}
		}
	}
	switch objType {
	case "SCH", "DIR", "PROFILE":
		return "", true
	default:
		return "", false
	}
}

func schemaName(id uint32) string {
	switch id {
	case 150994944:
		return "SYS"
	case 150994945:
		return "SYSDBA"
	case 150994946:
		return "SYSAUDITOR"
	case 150994947:
		return "SYSSSO"
	case 150994948:
		return "CTISYS"
	default:
		return fmt.Sprintf("SCHID_%d", id)
	}
}

func schemaNamesFromObjectLocations(objects []ObjectLocation) map[uint32]string {
	result := make(map[uint32]string)
	for _, obj := range objects {
		if obj.Type != "SCH" || obj.Valid == "N" || !isSafeShortText(obj.Name) {
			continue
		}
		result[obj.ObjectID] = obj.Name
	}
	return result
}

func schemaNamesFromDictionaryObjects(objects map[uint32]dictionaryObject) map[uint32]string {
	result := make(map[uint32]string)
	for _, obj := range objects {
		if obj.Type != "SCH" || obj.Valid == "N" || !isSafeShortText(obj.Name) {
			continue
		}
		result[obj.ID] = obj.Name
	}
	return result
}

func resolveSchemaName(id uint32, schemaNames map[uint32]string) string {
	if schemaNames != nil {
		if name := schemaNames[id]; name != "" {
			return name
		}
	}
	return schemaName(id)
}
