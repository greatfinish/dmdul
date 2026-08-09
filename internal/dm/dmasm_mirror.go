package dm

import (
	"encoding/binary"
	"fmt"
	"io"
	"math/bits"
	"sort"
	"strings"
)

const (
	rawASMMirrorDescriptorSize = int64(32)
	rawASMMirrorInodeSize      = int64(512)
	rawASMMirrorPathEnd        = 0x100
	rawASMMirrorMemberSize     = int64(128)
	rawASMMirrorMemberScanSize = int64(1024 * 1024)
	// The first 256 bytes of AU 0 are the mirror disk header. Allocation
	// descriptors therefore begin with physical AU 8 at byte offset 0x100.
	rawASMMirrorFirstAllocAU = uint32(8)
)

const (
	rawASMMirrorMemberDeleted     = uint16(0)
	rawASMMirrorMemberNormal      = uint16(1)
	rawASMMirrorMemberUnavailable = uint16(5)
)

type rawASMMirrorDescriptor struct {
	previous     rawASMCopy
	hasPrevious  bool
	next         rawASMCopy
	hasNext      bool
	mirrors      []rawASMCopy
	fileID       uint16
	sequence     uint32
	failedCopies byte
	checksum     uint16
}

type rawASMMirrorMember struct {
	state     uint16
	diskID    uint16
	diskAUs   uint32
	failGroup string
	diskName  string
}

func parseRawASMMirrorDAddr(raw []byte) (rawASMCopy, bool) {
	if len(raw) < 6 {
		return rawASMCopy{}, false
	}
	diskID := binary.LittleEndian.Uint16(raw[0:2])
	auNo := binary.LittleEndian.Uint32(raw[2:6])
	if diskID == ^uint16(0) || auNo == ^uint32(0) {
		return rawASMCopy{}, false
	}
	return rawASMCopy{diskID: diskID, auNo: auNo}, true
}

func parseRawASMMirrorDescriptor(raw []byte) (rawASMMirrorDescriptor, bool) {
	if len(raw) < int(rawASMMirrorDescriptorSize) {
		return rawASMMirrorDescriptor{}, false
	}
	fileID := binary.LittleEndian.Uint16(raw[24:26])
	// 0xFFFE/0xFFFF are allocator-management markers. Their address fields do
	// not follow ordinary file copy/link semantics and must not enter a logical
	// file AU map.
	if fileID == 0 || fileID >= ^uint16(0)-1 {
		return rawASMMirrorDescriptor{}, false
	}
	desc := rawASMMirrorDescriptor{
		fileID:       fileID,
		sequence:     uint32(binary.LittleEndian.Uint16(raw[27:29])),
		failedCopies: raw[29],
		checksum:     binary.LittleEndian.Uint16(raw[30:32]),
	}
	if value, ok := parseRawASMMirrorDAddr(raw[0:6]); ok {
		desc.previous, desc.hasPrevious = value, true
	}
	if value, ok := parseRawASMMirrorDAddr(raw[6:12]); ok {
		desc.next, desc.hasNext = value, true
	}
	for _, offset := range []int{12, 18} {
		if value, ok := parseRawASMMirrorDAddr(raw[offset : offset+6]); ok {
			desc.mirrors = appendUniqueASMCopy(desc.mirrors, value)
		}
	}
	return desc, true
}

func (g *RawASMGroup) loadMirrorGroupHeader() error {
	diskIDs := make([]int, 0, len(g.disks))
	for diskID := range g.disks {
		diskIDs = append(diskIDs, int(diskID))
	}
	sort.Ints(diskIDs)

	var candidates []*rawASMDisk
	for _, rawDiskID := range diskIDs {
		disk := g.disks[uint16(rawDiskID)]
		var header [128]byte
		if _, err := disk.file.ReadAt(header[:], disk.auSize); err != nil {
			continue
		}
		if binary.LittleEndian.Uint16(header[0:2]) != disk.groupID {
			continue
		}
		name := cString(header[12:44])
		if name == "" || strings.ContainsAny(name, "/\\") {
			continue
		}
		disk.groupGen = binary.LittleEndian.Uint32(header[6:10])
		if len(candidates) == 0 || disk.groupGen > candidates[0].groupGen {
			candidates = []*rawASMDisk{disk}
			g.name = name
			g.generation = disk.groupGen
		} else if disk.groupGen == candidates[0].groupGen {
			if !strings.EqualFold(name, g.name) {
				return fmt.Errorf("DMASM mirror group %d generation %d has conflicting names %q and %q", g.groupID, disk.groupGen, g.name, name)
			}
			candidates = append(candidates, disk)
		}
	}
	if len(candidates) == 0 {
		return fmt.Errorf("DMASM mirror group %d has no readable group header", g.groupID)
	}

	// AU 2 stores the current member catalog. A replaced disk can reappear
	// with the same group id and a complete but stale allocation map. Read the
	// catalog from the newest available group-header generation so DELETED and
	// non-NORMAL members cannot add stale physical copies to a logical AU.
	var members map[uint16]rawASMMirrorMember
	var memberErr error
	for _, disk := range candidates {
		other, err := readRawASMMirrorMembers(disk)
		if err != nil {
			memberErr = err
			continue
		}
		if len(other) == 0 {
			continue
		}
		if members == nil {
			members = other
			continue
		}
		if !sameRawASMMirrorMembers(members, other) {
			return fmt.Errorf("DMASM mirror group %s generation %d has inconsistent member catalogs", g.name, g.generation)
		}
	}
	if len(members) == 0 {
		if memberErr != nil {
			return memberErr
		}
		// Synthetic and early evidence may not contain a decoded member catalog.
		// Keep the previous behavior rather than guessing at disk health.
		return nil
	}
	for diskID, disk := range g.disks {
		member, exists := members[diskID]
		if exists && member.state == rawASMMirrorMemberNormal && member.diskAUs == disk.lastAU+1 && strings.EqualFold(member.diskName, disk.name) {
			continue
		}
		_ = disk.file.Close()
		delete(g.disks, diskID)
	}
	if len(g.disks) == 0 {
		return fmt.Errorf("DMASM mirror group %s generation %d has no supplied NORMAL members", g.name, g.generation)
	}
	return nil
}

func readRawASMMirrorMembers(disk *rawASMDisk) (map[uint16]rawASMMirrorMember, error) {
	scanSize := rawASMMirrorMemberScanSize
	if scanSize > disk.auSize {
		scanSize = disk.auSize
	}
	raw := make([]byte, scanSize)
	if _, err := disk.file.ReadAt(raw, 2*disk.auSize); err != nil {
		return nil, fmt.Errorf("read DMASM mirror member catalog from %s: %w", disk.path, err)
	}
	members := make(map[uint16]rawASMMirrorMember)
	for off := int64(0); off+rawASMMirrorMemberSize <= int64(len(raw)); off += rawASMMirrorMemberSize {
		record := raw[off : off+rawASMMirrorMemberSize]
		name := cString(record[76:108])
		failGroup := cString(record[44:76])
		diskAUs := binary.LittleEndian.Uint32(record[8:12])
		if !strings.HasPrefix(name, "DMASM") || failGroup == "" || diskAUs == 0 || record[127]&0x80 == 0 {
			continue
		}
		member := rawASMMirrorMember{
			state:     binary.LittleEndian.Uint16(record[0:2]),
			diskID:    binary.LittleEndian.Uint16(record[2:4]),
			diskAUs:   diskAUs,
			failGroup: failGroup,
			diskName:  name,
		}
		if member.diskID == ^uint16(0) {
			continue
		}
		if previous, exists := members[member.diskID]; exists && previous != member {
			return nil, fmt.Errorf("DMASM mirror member catalog on %s repeats disk id %d", disk.path, member.diskID)
		}
		members[member.diskID] = member
	}
	return members, nil
}

func sameRawASMMirrorMembers(left, right map[uint16]rawASMMirrorMember) bool {
	if len(left) != len(right) {
		return false
	}
	for diskID, member := range left {
		if other, ok := right[diskID]; !ok || other != member {
			return false
		}
	}
	return true
}

func (g *RawASMGroup) loadMirrorAllocationMaps() error {
	g.maps = make(map[uint16]map[uint32][]rawASMCopy)
	descriptors := make(map[rawASMCopy]rawASMMirrorDescriptor)
	for _, disk := range g.disks {
		capacity := uint64(disk.auSize / rawASMMirrorDescriptorSize)
		if capacity == 0 {
			return fmt.Errorf("DMASM mirror disk %s has invalid descriptor capacity", disk.path)
		}
		lastAU := uint64(disk.lastAU)
		for regionStart := uint64(0); regionStart <= lastAU; regionStart += capacity {
			count := capacity
			if remaining := lastAU - regionStart + 1; count > remaining {
				count = remaining
			}
			region := make([]byte, int64(count)*rawASMMirrorDescriptorSize)
			position := int64(regionStart) * disk.auSize
			if _, err := disk.file.ReadAt(region, position); err != nil {
				return fmt.Errorf("read DMASM mirror descriptor region at AU %d from %s: %w", regionStart, disk.path, err)
			}
			for localAU := uint64(0); localAU < count; localAU++ {
				auNo := uint32(regionStart + localAU)
				if auNo < rawASMMirrorFirstAllocAU {
					continue
				}
				off := int64(localAU) * rawASMMirrorDescriptorSize
				desc, ok := parseRawASMMirrorDescriptor(region[off : off+rawASMMirrorDescriptorSize])
				if !ok {
					continue
				}
				physical := rawASMCopy{diskID: disk.diskID, auNo: auNo}
				descriptors[physical] = desc
				bySequence := g.maps[desc.fileID]
				if bySequence == nil {
					bySequence = make(map[uint32][]rawASMCopy)
					g.maps[desc.fileID] = bySequence
				}
				bySequence[desc.sequence] = appendUniqueASMCopy(bySequence[desc.sequence], physical)
				for _, mirror := range desc.mirrors {
					bySequence[desc.sequence] = appendUniqueASMCopy(bySequence[desc.sequence], mirror)
				}
			}
		}
	}
	if err := g.validateMirrorDescriptors(descriptors); err != nil {
		return err
	}
	for _, bySequence := range g.maps {
		for sequence := range bySequence {
			sort.Slice(bySequence[sequence], func(i, j int) bool {
				if bySequence[sequence][i].diskID == bySequence[sequence][j].diskID {
					return bySequence[sequence][i].auNo < bySequence[sequence][j].auNo
				}
				return bySequence[sequence][i].diskID < bySequence[sequence][j].diskID
			})
		}
	}
	return nil
}

func rawASMMirrorDescriptorOffset(auSize int64, auNo uint32) int64 {
	capacity := uint64(auSize / rawASMMirrorDescriptorSize)
	regionStart := uint64(auNo) / capacity * capacity
	localAU := uint64(auNo) - regionStart
	return int64(regionStart)*auSize + int64(localAU)*rawASMMirrorDescriptorSize
}

func (g *RawASMGroup) validateMirrorDescriptors(descriptors map[rawASMCopy]rawASMMirrorDescriptor) error {
	for physical, desc := range descriptors {
		for _, mirror := range desc.mirrors {
			if g.disks[mirror.diskID] == nil {
				continue
			}
			other, ok := descriptors[mirror]
			if !ok {
				return fmt.Errorf("DMASM mirror descriptor disk=%d au=%d points to unallocated copy disk=%d au=%d", physical.diskID, physical.auNo, mirror.diskID, mirror.auNo)
			}
			if other.fileID != desc.fileID || other.sequence != desc.sequence {
				return fmt.Errorf("DMASM mirror descriptor disk=%d au=%d copy disk=%d au=%d has identity %d/%d, expected %d/%d", physical.diskID, physical.auNo, mirror.diskID, mirror.auNo, other.fileID, other.sequence, desc.fileID, desc.sequence)
			}
		}
		links := []struct {
			label string
			value rawASMCopy
			valid bool
		}{
			{"previous", desc.previous, desc.hasPrevious},
			{"next", desc.next, desc.hasNext},
		}
		for _, link := range links {
			if !link.valid || g.disks[link.value.diskID] == nil {
				continue
			}
			other, ok := descriptors[link.value]
			if !ok {
				return fmt.Errorf("DMASM mirror descriptor disk=%d au=%d has missing %s link disk=%d au=%d", physical.diskID, physical.auNo, link.label, link.value.diskID, link.value.auNo)
			}
			wantSequence := desc.sequence
			if link.label == "previous" {
				if desc.sequence == 0 {
					return fmt.Errorf("DMASM mirror descriptor disk=%d au=%d sequence 0 has a previous link", physical.diskID, physical.auNo)
				}
				wantSequence--
			} else {
				wantSequence++
			}
			if other.fileID != desc.fileID || other.sequence != wantSequence {
				return fmt.Errorf("DMASM mirror descriptor disk=%d au=%d %s link has identity %d/%d, expected %d/%d", physical.diskID, physical.auNo, link.label, other.fileID, other.sequence, desc.fileID, wantSequence)
			}
		}
	}
	return nil
}

func appendUniqueASMCopy(copies []rawASMCopy, candidate rawASMCopy) []rawASMCopy {
	for _, copy := range copies {
		if copy == candidate {
			return copies
		}
	}
	return append(copies, candidate)
}

func (g *RawASMGroup) loadMirrorInodes() error {
	inodeMaps := g.maps[2]
	if len(inodeMaps) == 0 || len(inodeMaps[0]) == 0 {
		return fmt.Errorf("DMASM mirror group %s has no .inode allocation", g.name)
	}
	sequences := make([]int, 0, len(inodeMaps))
	for sequence := range inodeMaps {
		sequences = append(sequences, int(sequence))
	}
	sort.Ints(sequences)
	files := make(map[string]RawASMFileInfo)
	for expected, rawSequence := range sequences {
		sequence := uint32(rawSequence)
		if sequence != uint32(expected) {
			// Fail before records after an INODE AU hole can be mistaken for a
			// complete catalog.
			return fmt.Errorf("DMASM mirror INODE is missing logical AU before sequence %d", sequence)
		}
		if err := g.scanMirrorInodeAU(inodeMaps[sequence], files); err != nil {
			return fmt.Errorf("read DMASM mirror INODE sequence %d: %w", sequence, err)
		}
	}
	if len(files) == 0 {
		return fmt.Errorf("no DMASM mirror INODE entries found in group %s", g.name)
	}
	g.files = files
	return nil
}

func (g *RawASMGroup) scanMirrorInodeAU(copies []rawASMCopy, files map[string]RawASMFileInfo) error {
	const scanChunkSize = int64(1024 * 1024)
	var lastErr error
	for _, copy := range copies {
		disk := g.disks[copy.diskID]
		if disk == nil {
			lastErr = fmt.Errorf("missing disk %d", copy.diskID)
			continue
		}
		base := int64(copy.auNo) * disk.auSize
		parsed := make(map[string]RawASMFileInfo)
		for chunkOff := int64(0); chunkOff < disk.auSize; chunkOff += scanChunkSize {
			chunkLen := scanChunkSize
			if remaining := disk.auSize - chunkOff; chunkLen > remaining {
				chunkLen = remaining
			}
			chunk := make([]byte, chunkLen)
			if _, err := disk.file.ReadAt(chunk, base+chunkOff); err != nil {
				lastErr = err
				parsed = nil
				break
			}
			for off := int64(0); off+rawASMMirrorInodeSize <= chunkLen; off += rawASMMirrorInodeSize {
				info, ok := g.parseMirrorInode(chunk[off:off+rawASMMirrorInodeSize], copy.diskID, copy.auNo, uint32(chunkOff+off))
				if ok {
					parsed[normalizeASMPath(info.Path)] = info
				}
			}
		}
		if parsed != nil {
			for key, info := range parsed {
				files[key] = info
			}
			return nil
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("INODE AU has no configured copies")
	}
	return lastErr
}

func (g *RawASMGroup) parseMirrorInode(entry []byte, diskID uint16, auNo, off uint32) (RawASMFileInfo, bool) {
	if len(entry) < int(rawASMMirrorInodeSize) || entry[3] != '+' {
		return RawASMFileInfo{}, false
	}
	path := cString(entry[3:rawASMMirrorPathEnd])
	if path == "" || !strings.HasPrefix(path, "+") {
		return RawASMFileInfo{}, false
	}
	lowID := binary.LittleEndian.Uint16(entry[0:2])
	if lowID == 0 {
		return RawASMFileInfo{}, false
	}
	var sizeUnits uint64
	for index, value := range entry[0x118:0x11f] {
		sizeUnits |= uint64(value) << (8 * index)
	}
	if sizeUnits > uint64(^uint64(0)>>1)/256 {
		return RawASMFileInfo{}, false
	}
	size := sizeUnits * 256
	stripShift := entry[0x114]
	var stripingKB uint32
	if stripShift != 0 {
		if stripShift >= 31 {
			return RawASMFileInfo{}, false
		}
		stripingKB = 1 << stripShift
	}
	typeCode := entry[2]
	return RawASMFileInfo{
		ID:   mirrorASMFileID(g.groupID, g.auSize, lowID),
		Path: strings.ReplaceAll(path, "\\", "/"),
		Size: int64(size),
		// The logical AU count is an unaligned little-endian uint16. Reading
		// +0x100 as a big-endian uint32 only works accidentally below 256 AUs.
		Extents:   uint32(binary.LittleEndian.Uint16(entry[0x103:0x105])),
		IsDir:     typeCode == 1 || typeCode == 3,
		InodeDisk: diskID,
		InodeAU:   auNo,
		InodeOff:  off,
		GroupID:   g.groupID,
		GroupName: g.name,
		// +0x110 is a separate allocation flag in some DBF records. The
		// redundancy count itself is the byte at +0x113.
		Copies:     uint32(entry[0x113]),
		StripingKB: stripingKB,
		AUGroup:    entry[0x115],
	}, true
}

func mirrorASMFileID(groupID uint16, auSize int64, lowID uint16) uint32 {
	auMiB := uint32(auSize / (1024 * 1024))
	auCode := uint32(0)
	if auMiB > 0 && auMiB&(auMiB-1) == 0 {
		auCode = uint32(bits.TrailingZeros32(auMiB) * 2)
	}
	return uint32(uint8(0x80+groupID))<<24 | auCode<<16 | uint32(lowID)
}

func (g *RawASMGroup) loadMirrorExtentMap(info RawASMFileInfo) ([]rawASMExtent, error) {
	if info.Extents == 0 {
		if info.Size == 0 {
			return nil, nil
		}
		return nil, fmt.Errorf("non-empty mirror file has zero AUs")
	}
	bySequence := g.maps[uint16(info.ID)]
	if bySequence == nil {
		return nil, fmt.Errorf("no mirror AU descriptors for file id %d", uint16(info.ID))
	}
	extents := make([]rawASMExtent, info.Extents)
	for sequence := uint32(0); sequence < info.Extents; sequence++ {
		copies := bySequence[sequence]
		if len(copies) == 0 {
			return nil, fmt.Errorf("mirror file %s is missing logical AU %d", info.Path, sequence)
		}
		if info.Copies > 0 && uint32(len(copies)) < info.Copies {
			return nil, fmt.Errorf("mirror file %s logical AU %d exposes %d copies, INODE requires %d", info.Path, sequence, len(copies), info.Copies)
		}
		extents[sequence] = rawASMExtent{copies: append([]rawASMCopy(nil), copies...)}
	}
	if int64(info.Extents)*g.auSize < info.Size {
		return nil, fmt.Errorf("mirror AU map covers %d bytes, file size is %d", int64(info.Extents)*g.auSize, info.Size)
	}
	return extents, nil
}

func (f *RawASMFile) mirrorLocation(logical int64) (int64, int64, error) {
	if f.group.auSize <= 0 {
		return 0, 0, fmt.Errorf("invalid DMASM mirror AU size")
	}
	if f.info.StripingKB == 0 {
		return logical / f.group.auSize, logical % f.group.auSize, nil
	}
	stripeSize := int64(f.info.StripingKB) * 1024
	auGroup := int64(f.info.AUGroup)
	if stripeSize <= 0 || auGroup <= 0 || f.group.auSize%stripeSize != 0 {
		return 0, 0, fmt.Errorf("invalid DMASM striping metadata: stripe=%dKB au_group=%d", f.info.StripingKB, f.info.AUGroup)
	}
	groupBytes := f.group.auSize * auGroup
	mapBase := logical / groupBytes * auGroup
	withinGroup := logical % groupBytes
	stripeIndex := withinGroup / stripeSize
	mapIndex := mapBase + stripeIndex%auGroup
	physicalWithin := stripeIndex/auGroup*stripeSize + withinGroup%stripeSize
	return mapIndex, physicalWithin, nil
}

func (f *RawASMFile) readMirrorChunk(dst []byte, extent rawASMExtent, physicalWithin int64) (int, error) {
	var result error
	for _, copy := range extent.copies {
		disk := f.group.disks[copy.diskID]
		if disk == nil {
			result = fmt.Errorf("missing DMASM mirror disk %d", copy.diskID)
			continue
		}
		n, err := disk.file.ReadAt(dst, int64(copy.auNo)*disk.auSize+physicalWithin)
		if err == nil && n == len(dst) {
			return n, nil
		}
		if err == nil {
			err = io.ErrUnexpectedEOF
		}
		result = fmt.Errorf("read mirror copy disk=%d au=%d: %w", copy.diskID, copy.auNo, err)
	}
	if result == nil {
		result = fmt.Errorf("mirror AU has no readable copies")
	}
	return 0, result
}
