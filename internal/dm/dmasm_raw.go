package dm

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	pathpkg "path"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	rawASMNonMirrorFormat  = uint32(0x1004)
	rawASMMirrorFormat     = uint32(0x3001)
	rawASMMirrorSignature  = 0x21352811
	rawASMReservedSize     = int64(32 * 1024 * 1024)
	rawASMAUSize           = int64(1024 * 1024)
	rawASMExtentAUs        = int64(4)
	rawASMExtentSize       = rawASMAUSize * rawASMExtentAUs
	rawASMAUTypeInode      = uint32(0x11)
	rawASMAUTypeXDesc      = uint32(0x13)
	rawASMInodeStart       = int64(0x400)
	rawASMInodeSize        = int64(512)
	rawASMXDescStart       = uint32(0x400)
	rawASMXDescSize        = uint32(32)
	rawASMMaxPathSize      = 256
	rawASMInodeExtentCount = 0x124
	rawASMInodeFirstXDesc  = 0x128
	rawASMMinMirrorAUMiB   = uint16(1)
	rawASMMaxMirrorAUMiB   = uint16(64)
)

type rawASMLayout uint8

const (
	rawASMLayoutNonMirror rawASMLayout = iota + 1
	rawASMLayoutMirror
)

// RawASMFileInfo is the file identity recovered from a DMASM INODE entry.
type RawASMFileInfo struct {
	ID         uint32
	Path       string
	Size       int64
	CreateTime time.Time
	ModifyTime time.Time
	Extents    uint32
	IsDir      bool
	InodeDisk  uint16
	InodeAU    uint32
	InodeOff   uint32
	GroupID    uint16
	GroupName  string
	Copies     uint32
	StripingKB uint32
	AUGroup    uint8
}

type rawASMDAddr struct {
	diskID uint16
	auNo   uint32
	offset uint32
}

type rawASMExtent struct {
	copies []rawASMCopy
}

type rawASMCopy struct {
	diskID uint16
	auNo   uint32
}

type rawASMDisk struct {
	path       string
	file       *os.File
	layout     rawASMLayout
	groupID    uint16
	diskID     uint16
	name       string
	groupGen   uint32
	lastAU     uint32
	auSize     int64
	dataOffset int64
	usableSize int64
}

// RawASMGroup provides read-only access to files stored in one offline DMASM
// disk group. It never opens a disk with write permissions.
type RawASMGroup struct {
	layout     rawASMLayout
	groupID    uint16
	name       string
	generation uint32
	auSize     int64
	disks      map[uint16]*rawASMDisk
	files      map[string]RawASMFileInfo
	maps       map[uint16]map[uint32][]rawASMCopy
}

// RawASMFile implements io.ReaderAt over a logical file reconstructed from
// its XDESC chain.
type RawASMFile struct {
	group   *RawASMGroup
	info    RawASMFileInfo
	extents []rawASMExtent
}

// OpenRawASMGroup opens one or more raw DMASM member disks read-only and
// recovers their INODE catalog.
func OpenRawASMGroup(paths ...string) (*RawASMGroup, error) {
	if len(paths) == 0 {
		return nil, fmt.Errorf("at least one DMASM disk path is required")
	}
	group := &RawASMGroup{
		disks: make(map[uint16]*rawASMDisk),
		files: make(map[string]RawASMFileInfo),
	}
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		disk, err := openRawASMDisk(path)
		if err != nil {
			group.Close()
			return nil, err
		}
		if len(group.disks) == 0 {
			group.layout = disk.layout
			group.groupID = disk.groupID
			group.auSize = disk.auSize
		} else if disk.groupID != group.groupID {
			disk.file.Close()
			group.Close()
			return nil, fmt.Errorf("DMASM disk %s belongs to group %d, expected %d", path, disk.groupID, group.groupID)
		} else if disk.layout != group.layout || disk.auSize != group.auSize {
			disk.file.Close()
			group.Close()
			return nil, fmt.Errorf("DMASM disk %s layout/AU size does not match group %d", path, group.groupID)
		}
		if previous := group.disks[disk.diskID]; previous != nil {
			disk.file.Close()
			group.Close()
			return nil, fmt.Errorf("duplicate DMASM disk id %d: %s and %s", disk.diskID, previous.path, path)
		}
		group.disks[disk.diskID] = disk
	}
	if len(group.disks) == 0 {
		return nil, fmt.Errorf("at least one non-empty DMASM disk path is required")
	}
	if group.layout == rawASMLayoutMirror {
		if err := group.loadMirrorGroupHeader(); err != nil {
			group.Close()
			return nil, err
		}
		if err := group.loadMirrorAllocationMaps(); err != nil {
			group.Close()
			return nil, err
		}
	}
	if err := group.loadInodes(); err != nil {
		group.Close()
		return nil, err
	}
	return group, nil
}

func openRawASMDisk(path string) (*rawASMDisk, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open DMASM disk %s read-only: %w", path, err)
	}
	header := make([]byte, 128)
	if _, err := file.ReadAt(header, 0); err != nil {
		file.Close()
		return nil, fmt.Errorf("read DMASM disk header %s: %w", path, err)
	}
	version := binary.LittleEndian.Uint32(header[8:12])
	var disk *rawASMDisk
	switch version {
	case rawASMNonMirrorFormat:
		if binary.LittleEndian.Uint32(header[16:20]) != rawASMAUTypeXDesc {
			file.Close()
			return nil, fmt.Errorf("invalid DMASM disk header type on %s", path)
		}
		name := cString(header[24:])
		if !strings.HasPrefix(name, "DMASM") {
			file.Close()
			return nil, fmt.Errorf("invalid DMASM disk signature on %s", path)
		}
		disk = &rawASMDisk{
			path: path, file: file, layout: rawASMLayoutNonMirror,
			groupID: binary.LittleEndian.Uint16(header[0:2]),
			diskID:  binary.LittleEndian.Uint16(header[2:4]),
			name:    name,
			lastAU:  binary.LittleEndian.Uint32(header[20:24]),
			auSize:  rawASMAUSize, dataOffset: rawASMReservedSize,
		}
	case rawASMMirrorFormat:
		if binary.LittleEndian.Uint32(header[12:16]) != rawASMMirrorSignature {
			file.Close()
			return nil, fmt.Errorf("invalid DMASM mirror disk signature on %s", path)
		}
		auMiB := binary.LittleEndian.Uint16(header[0:2])
		diskAUs := binary.LittleEndian.Uint32(header[24:28])
		name := cString(header[28:60])
		if !validRawASMMirrorAUMiB(auMiB) || diskAUs == 0 || !strings.HasPrefix(name, "DMASM") {
			file.Close()
			return nil, fmt.Errorf("invalid DMASM mirror disk header on %s", path)
		}
		disk = &rawASMDisk{
			path: path, file: file, layout: rawASMLayoutMirror,
			groupID: binary.LittleEndian.Uint16(header[16:18]),
			diskID:  binary.LittleEndian.Uint16(header[2:4]),
			name:    name, lastAU: diskAUs - 1,
			auSize: int64(auMiB) * 1024 * 1024,
		}
	default:
		file.Close()
		return nil, fmt.Errorf("unsupported DMASM disk version 0x%x on %s", version, path)
	}
	auCount := uint64(disk.lastAU) + 1
	maxInt64 := uint64(^uint64(0) >> 1)
	if disk.auSize <= 0 || disk.dataOffset < 0 || uint64(disk.dataOffset) > maxInt64 || auCount > (maxInt64-uint64(disk.dataOffset))/uint64(disk.auSize) {
		file.Close()
		return nil, fmt.Errorf("DMASM disk geometry overflows address space on %s", path)
	}
	disk.usableSize = disk.dataOffset + int64(auCount)*disk.auSize
	if stat, statErr := file.Stat(); statErr == nil && stat.Size() > 0 {
		if stat.Size() < disk.usableSize {
			file.Close()
			return nil, fmt.Errorf("DMASM disk %s is truncated: size=%d expected-at-least=%d", path, stat.Size(), disk.usableSize)
		}
		disk.usableSize = stat.Size()
	}
	return disk, nil
}

func validRawASMMirrorAUMiB(value uint16) bool {
	return value >= rawASMMinMirrorAUMiB && value <= rawASMMaxMirrorAUMiB && value&(value-1) == 0
}

// Close releases all member disk handles.
func (g *RawASMGroup) Close() error {
	if g == nil {
		return nil
	}
	var result error
	for _, disk := range g.disks {
		if err := disk.file.Close(); err != nil {
			result = errors.Join(result, err)
		}
	}
	g.disks = nil
	return result
}

// GroupID returns the on-disk DMASM group id.
func (g *RawASMGroup) GroupID() uint16 {
	if g == nil {
		return 0
	}
	return g.groupID
}

// GroupName returns the ASM disk-group name recovered from mirror metadata.
// Early DMASM layouts may not carry a separately decoded group name.
func (g *RawASMGroup) GroupName() string {
	if g == nil {
		return ""
	}
	return g.name
}

// Files returns the recovered INODE catalog in path order.
func (g *RawASMGroup) Files() []RawASMFileInfo {
	if g == nil {
		return nil
	}
	files := make([]RawASMFileInfo, 0, len(g.files))
	for _, file := range g.files {
		files = append(files, file)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files
}

// Open opens a logical DMASM file by its full ASM path. Path matching is
// case-insensitive and accepts either slash direction.
func (g *RawASMGroup) Open(path string) (*RawASMFile, error) {
	if g == nil {
		return nil, fmt.Errorf("DMASM group is not open")
	}
	info, ok := g.files[normalizeASMPath(path)]
	if !ok {
		return nil, fmt.Errorf("DMASM file not found: %s", path)
	}
	if info.IsDir {
		return nil, fmt.Errorf("DMASM path is a directory: %s", info.Path)
	}
	var extents []rawASMExtent
	var err error
	if g.layout == rawASMLayoutMirror {
		extents, err = g.loadMirrorExtentMap(info)
	} else {
		extents, err = g.loadExtentChain(info)
	}
	if err != nil {
		return nil, fmt.Errorf("open DMASM file %s: %w", info.Path, err)
	}
	return &RawASMFile{group: g, info: info, extents: extents}, nil
}

func (g *RawASMGroup) loadInodes() error {
	if g.layout == rawASMLayoutMirror {
		return g.loadMirrorInodes()
	}
	entry := make([]byte, rawASMInodeSize)
	header := make([]byte, 24)
	for _, disk := range g.disks {
		for rawAUNo := uint64(0); rawAUNo <= uint64(disk.lastAU); rawAUNo++ {
			auNo := uint32(rawAUNo)
			base := rawASMReservedSize + int64(auNo)*rawASMAUSize
			if _, err := disk.file.ReadAt(header, base); err != nil {
				return fmt.Errorf("scan DMASM AU %d on disk %d: %w", auNo, disk.diskID, err)
			}
			if !validRawASMAUHeader(header, disk.groupID, disk.diskID, auNo, rawASMAUTypeInode) {
				continue
			}
			for off := rawASMInodeStart; off+rawASMInodeSize <= rawASMAUSize; off += rawASMInodeSize {
				if _, err := disk.file.ReadAt(entry, base+off); err != nil {
					return fmt.Errorf("read DMASM INODE disk=%d au=%d off=0x%x: %w", disk.diskID, auNo, off, err)
				}
				info, ok := parseRawASMInode(entry, disk.diskID, auNo, uint32(off))
				if !ok {
					continue
				}
				info.GroupID = g.groupID
				info.GroupName = g.name
				info.Copies = 1
				key := normalizeASMPath(info.Path)
				old, exists := g.files[key]
				if !exists {
					g.files[key] = info
					continue
				}
				if !sameRawASMInodeMetadata(old, info) {
					return fmt.Errorf("conflicting DMASM INODE metadata for %s at disk=%d au=%d off=0x%x and disk=%d au=%d off=0x%x", info.Path, old.InodeDisk, old.InodeAU, old.InodeOff, info.InodeDisk, info.InodeAU, info.InodeOff)
				}
				if lessRawASMInodeLocation(info, old) {
					g.files[key] = info
				}
			}
		}
	}
	if len(g.files) == 0 {
		return fmt.Errorf("no DMASM INODE entries found in group %d", g.groupID)
	}
	return nil
}

func sameRawASMInodeMetadata(left RawASMFileInfo, right RawASMFileInfo) bool {
	return left.ID == right.ID && left.Path == right.Path && left.Size == right.Size && left.Extents == right.Extents && left.IsDir == right.IsDir
}

func lessRawASMInodeLocation(left RawASMFileInfo, right RawASMFileInfo) bool {
	if left.InodeDisk != right.InodeDisk {
		return left.InodeDisk < right.InodeDisk
	}
	if left.InodeAU != right.InodeAU {
		return left.InodeAU < right.InodeAU
	}
	return left.InodeOff < right.InodeOff
}

func validRawASMAUHeader(header []byte, groupID, diskID uint16, auNo, auType uint32) bool {
	return len(header) >= 20 &&
		binary.LittleEndian.Uint16(header[0:2]) == groupID &&
		binary.LittleEndian.Uint16(header[2:4]) == diskID &&
		binary.LittleEndian.Uint32(header[4:8]) == auNo &&
		binary.LittleEndian.Uint32(header[8:12]) == rawASMNonMirrorFormat &&
		binary.LittleEndian.Uint32(header[16:20]) == auType
}

func parseRawASMInode(entry []byte, diskID uint16, auNo, off uint32) (RawASMFileInfo, bool) {
	if len(entry) < int(rawASMInodeSize) || entry[4] != '+' {
		return RawASMFileInfo{}, false
	}
	path := cString(entry[4 : 4+rawASMMaxPathSize])
	if path == "" || !strings.HasPrefix(path, "+") {
		return RawASMFileInfo{}, false
	}
	size := binary.LittleEndian.Uint64(entry[0x104:0x10c])
	if size > uint64(^uint64(0)>>1) {
		return RawASMFileInfo{}, false
	}
	return RawASMFileInfo{
		ID:         binary.LittleEndian.Uint32(entry[0:4]),
		Path:       strings.ReplaceAll(path, "\\", "/"),
		Size:       int64(size),
		CreateTime: rawASMTime(entry[0x10c:0x118]),
		ModifyTime: rawASMTime(entry[0x118:0x124]),
		Extents:    binary.LittleEndian.Uint32(entry[rawASMInodeExtentCount : rawASMInodeExtentCount+4]),
		IsDir:      entry[0x140] != 0,
		InodeDisk:  diskID,
		InodeAU:    auNo,
		InodeOff:   off,
	}, true
}

func rawASMTime(raw []byte) time.Time {
	// DMASM timestamps are not needed for address resolution yet. Preserve a
	// zero value until their packed layout is established by a difference test.
	return time.Time{}
}

func (g *RawASMGroup) loadExtentChain(info RawASMFileInfo) ([]rawASMExtent, error) {
	if info.Extents == 0 {
		if info.Size == 0 {
			return nil, nil
		}
		return nil, fmt.Errorf("non-empty file has zero extents")
	}
	inodeDisk := g.disks[info.InodeDisk]
	if inodeDisk == nil {
		return nil, fmt.Errorf("missing INODE disk %d", info.InodeDisk)
	}
	entry := make([]byte, rawASMInodeSize)
	inodePos := rawASMReservedSize + int64(info.InodeAU)*rawASMAUSize + int64(info.InodeOff)
	if _, err := inodeDisk.file.ReadAt(entry, inodePos); err != nil {
		return nil, err
	}
	address := parseRawASMDAddr(entry[rawASMInodeFirstXDesc : rawASMInodeFirstXDesc+10])
	extents := make([]rawASMExtent, 0, info.Extents)
	seen := make(map[rawASMDAddr]bool, info.Extents)
	descriptor := make([]byte, rawASMXDescSize)
	for index := uint32(0); index < info.Extents; index++ {
		if seen[address] {
			return nil, fmt.Errorf("XDESC loop at disk=%d au=%d off=0x%x", address.diskID, address.auNo, address.offset)
		}
		seen[address] = true
		disk, descIndex, err := g.validateXDescAddress(address)
		if err != nil {
			return nil, err
		}
		position := rawASMReservedSize + int64(address.auNo)*rawASMAUSize + int64(address.offset)
		if _, err := disk.file.ReadAt(descriptor, position); err != nil {
			return nil, fmt.Errorf("read XDESC %d: %w", index, err)
		}
		dataAU := uint64(address.auNo) + uint64(descIndex)*uint64(rawASMExtentAUs)
		if dataAU+uint64(rawASMExtentAUs) > uint64(disk.lastAU)+1 {
			return nil, fmt.Errorf("XDESC %d maps outside disk %d: data AU %d", index, disk.diskID, dataAU)
		}
		extents = append(extents, rawASMExtent{copies: []rawASMCopy{{diskID: disk.diskID, auNo: uint32(dataAU)}}})
		if index+1 < info.Extents {
			address = parseRawASMDAddr(descriptor[10:20])
		}
	}
	if int64(len(extents))*rawASMExtentSize < info.Size {
		return nil, fmt.Errorf("extent chain covers %d bytes, file size is %d", int64(len(extents))*rawASMExtentSize, info.Size)
	}
	return extents, nil
}

func (g *RawASMGroup) validateXDescAddress(address rawASMDAddr) (*rawASMDisk, uint32, error) {
	disk := g.disks[address.diskID]
	if disk == nil {
		return nil, 0, fmt.Errorf("XDESC references missing disk %d", address.diskID)
	}
	if address.auNo > disk.lastAU {
		return nil, 0, fmt.Errorf("XDESC AU %d is outside disk %d", address.auNo, address.diskID)
	}
	if address.offset < rawASMXDescStart || (address.offset-rawASMXDescStart)%rawASMXDescSize != 0 || uint64(address.offset)+uint64(rawASMXDescSize) > uint64(rawASMAUSize) {
		return nil, 0, fmt.Errorf("invalid XDESC offset 0x%x", address.offset)
	}
	header := make([]byte, 20)
	position := rawASMReservedSize + int64(address.auNo)*rawASMAUSize
	if _, err := disk.file.ReadAt(header, position); err != nil {
		return nil, 0, fmt.Errorf("read XDESC AU header: %w", err)
	}
	if !validRawASMAUHeader(header, disk.groupID, disk.diskID, address.auNo, rawASMAUTypeXDesc) {
		return nil, 0, fmt.Errorf("address disk=%d au=%d does not reference an XDESC AU", address.diskID, address.auNo)
	}
	return disk, (address.offset - rawASMXDescStart) / rawASMXDescSize, nil
}

func parseRawASMDAddr(raw []byte) rawASMDAddr {
	return rawASMDAddr{
		diskID: binary.LittleEndian.Uint16(raw[0:2]),
		auNo:   binary.LittleEndian.Uint32(raw[2:6]),
		offset: binary.LittleEndian.Uint32(raw[6:10]),
	}
}

func normalizeASMPath(path string) string {
	path = strings.TrimSpace(strings.ReplaceAll(path, "\\", "/"))
	for strings.Contains(path, "//") {
		path = strings.ReplaceAll(path, "//", "/")
	}
	return strings.ToUpper(strings.TrimSuffix(path, "/"))
}

// IsASMPath reports whether path is a DMASM logical path.
func IsASMPath(path string) bool {
	return strings.HasPrefix(strings.TrimSpace(path), "+")
}

// DataFiles returns DBF files in the same ASM database directory as
// systemPath, with group/file identity read from each logical DBF header.
func (g *RawASMGroup) DataFiles(systemPath string) ([]OfflineDataSource, error) {
	dir := normalizeASMPath(pathpkg.Dir(strings.ReplaceAll(systemPath, "\\", "/")))
	var sources []OfflineDataSource
	seen := make(map[dataFileKey]string)
	for _, info := range g.Files() {
		if info.IsDir || !strings.EqualFold(pathpkg.Ext(info.Path), ".DBF") || normalizeASMPath(pathpkg.Dir(info.Path)) != dir {
			continue
		}
		file, err := g.Open(info.Path)
		if err != nil {
			return nil, err
		}
		var header [8]byte
		if _, err := file.ReadAt(header[:], 0); err != nil {
			return nil, fmt.Errorf("read DBF header %s: %w", info.Path, err)
		}
		if binary.LittleEndian.Uint32(header[4:8]) != 0 {
			continue
		}
		groupID := uint32(binary.LittleEndian.Uint16(header[0:2]))
		fileIDRaw := binary.LittleEndian.Uint16(header[2:4])
		if fileIDRaw > uint16(^uint16(0)>>1) {
			continue
		}
		fileID := int16(fileIDRaw)
		base := strings.ToUpper(strings.TrimSuffix(pathpkg.Base(info.Path), pathpkg.Ext(info.Path)))
		switch base {
		case "SYSTEM":
			groupID, fileID = 0, 0
		case "ROLL":
			groupID, fileID = 1, 0
		case "TEMP":
			groupID, fileID = 3, 0
		case "MAIN":
			groupID, fileID = 4, 0
		default:
			if strings.HasPrefix(base, "TEMP") {
				if parsed, err := strconv.ParseInt(strings.TrimPrefix(base, "TEMP"), 10, 16); err == nil && parsed >= 0 {
					groupID, fileID = 3, int16(parsed)
				}
			}
		}
		key := dataFileKey{groupID: groupID, fileID: fileID}
		if previous, exists := seen[key]; exists {
			return nil, fmt.Errorf("duplicate DBF identity group=%d file=%d: %s and %s", groupID, fileID, previous, info.Path)
		}
		seen[key] = info.Path
		sources = append(sources, OfflineDataSource{
			GroupID: groupID, FileID: fileID,
			Tablespace: inferTablespaceNameFromDataFile(info.Path, groupID),
			Path:       info.Path,
			Reader:     file,
		})
	}
	sort.Slice(sources, func(i, j int) bool {
		if sources[i].GroupID == sources[j].GroupID {
			return sources[i].FileID < sources[j].FileID
		}
		return sources[i].GroupID < sources[j].GroupID
	})
	if len(sources) == 0 {
		return nil, fmt.Errorf("no DBF files found under %s", pathpkg.Dir(systemPath))
	}
	return sources, nil
}

func cString(raw []byte) string {
	if index := strings.IndexByte(string(raw), 0); index >= 0 {
		raw = raw[:index]
	}
	return strings.TrimSpace(string(raw))
}

// Info returns the recovered INODE metadata for this logical file.
func (f *RawASMFile) Info() RawASMFileInfo { return f.info }

// Size returns the logical file size from its INODE.
func (f *RawASMFile) Size() int64 { return f.info.Size }

// ReadAt maps a logical file offset to its DMASM extent and reads directly
// from the member disk.
func (f *RawASMFile) ReadAt(p []byte, off int64) (int, error) {
	if f == nil || f.group == nil {
		return 0, fmt.Errorf("DMASM file is not open")
	}
	if off < 0 {
		return 0, fmt.Errorf("negative DMASM file offset %d", off)
	}
	if len(p) == 0 {
		return 0, nil
	}
	if off >= f.info.Size {
		return 0, io.EOF
	}
	want := len(p)
	if remaining := f.info.Size - off; int64(want) > remaining {
		want = int(remaining)
	}
	total := 0
	for total < want {
		logical := off + int64(total)
		if f.group.layout == rawASMLayoutMirror {
			extentIndex, within, err := f.mirrorLocation(logical)
			if err != nil {
				return total, err
			}
			if extentIndex < 0 || extentIndex >= int64(len(f.extents)) {
				return total, fmt.Errorf("logical offset %d has no DMASM mirror AU", logical)
			}
			boundary := f.group.auSize
			if f.info.StripingKB != 0 {
				boundary = int64(f.info.StripingKB) * 1024
			}
			chunk := want - total
			if max := int(boundary - within%boundary); chunk > max {
				chunk = max
			}
			n, err := f.readMirrorChunk(p[total:total+chunk], f.extents[extentIndex], within)
			total += n
			if err != nil {
				return total, err
			}
			continue
		}
		extentIndex := logical / rawASMExtentSize
		if extentIndex < 0 || extentIndex >= int64(len(f.extents)) {
			return total, fmt.Errorf("logical offset %d has no DMASM extent", logical)
		}
		within := logical % rawASMExtentSize
		chunk := want - total
		if max := int(rawASMExtentSize - within); chunk > max {
			chunk = max
		}
		extent := f.extents[extentIndex]
		if len(extent.copies) == 0 {
			return total, fmt.Errorf("legacy DMASM extent %d has no physical copy", extentIndex)
		}
		copy := extent.copies[0]
		disk := f.group.disks[copy.diskID]
		physical := rawASMReservedSize + int64(copy.auNo)*rawASMAUSize + within
		n, err := disk.file.ReadAt(p[total:total+chunk], physical)
		total += n
		if err != nil {
			return total, err
		}
		if n != chunk {
			return total, io.ErrUnexpectedEOF
		}
	}
	if want < len(p) {
		return total, io.EOF
	}
	return total, nil
}
