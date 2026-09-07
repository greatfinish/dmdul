package dm

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestRawASMMirrorReadsFineStripingAndFallsBackToSecondCopy(t *testing.T) {
	paths := createRawASMMirrorFixture(t, t.TempDir())

	group, err := OpenRawASMGroup(paths...)
	if err != nil {
		t.Fatal(err)
	}
	defer group.Close()
	if group.GroupID() != 1 || group.GroupName() != "NORM1" {
		t.Fatalf("group identity = %d/%q", group.GroupID(), group.GroupName())
	}
	info := group.Files()
	if len(info) != 4 {
		t.Fatalf("files = %#v", info)
	}
	file, err := group.Open("+norm1/test.bin")
	if err != nil {
		t.Fatal(err)
	}
	if file.Size() != 4*1024*1024 || file.Info().ID != 0x81000012 || file.Info().StripingKB != 32 || file.Info().Copies != 2 {
		t.Fatalf("unexpected mirror inode: %#v", file.Info())
	}

	got := make([]byte, 160*1024)
	if _, err := file.ReadAt(got, 0); err != nil {
		t.Fatal(err)
	}
	want := mirrorTestLogicalBytes(0, len(got))
	if !bytes.Equal(got, want) {
		t.Fatal("fine-striped logical bytes do not match")
	}

	// The first copy is disk 0 after deterministic sorting. Closing it forces
	// the reader to retry the identical NORMAL-redundancy copy on disk 1.
	if err := group.disks[0].file.Close(); err != nil {
		t.Fatal(err)
	}
	got = make([]byte, 96*1024)
	if _, err := file.ReadAt(got, 64*1024); err != nil {
		t.Fatal(err)
	}
	want = mirrorTestLogicalBytes(64*1024, len(got))
	if !bytes.Equal(got, want) {
		t.Fatal("second mirror copy does not match")
	}

	// A NORMAL group remains readable when only either one of its two member
	// disks is supplied. The descriptor's copy array may reference the absent
	// member, but the local physical AU is still a complete copy.
	for _, path := range paths {
		single, err := OpenRawASMGroup(path)
		if err != nil {
			t.Fatalf("open one NORMAL member %s: %v", path, err)
		}
		oneFile, err := single.Open("+NORM1/test.bin")
		if err != nil {
			single.Close()
			t.Fatal(err)
		}
		one := make([]byte, 128*1024)
		if _, err := oneFile.ReadAt(one, 31*1024); err != nil {
			single.Close()
			t.Fatalf("read one NORMAL member %s: %v", path, err)
		}
		if !bytes.Equal(one, mirrorTestLogicalBytes(31*1024, len(one))) {
			single.Close()
			t.Fatalf("one-member bytes differ for %s", path)
		}
		if err := single.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRawASMMirrorIgnoresDeletedMemberFromOlderGeneration(t *testing.T) {
	paths := createRawASMMirrorFixture(t, t.TempDir())
	stalePath := filepath.Join(filepath.Dir(paths[0]), "stale-replaced.raw")
	stale := createRawASMMirrorTestDisk(t, stalePath, 2)
	writeRawASMMirrorGroupMetadata(t, stale, 1, []rawASMMirrorTestMember{
		{state: rawASMMirrorMemberNormal, diskID: 0},
		{state: rawASMMirrorMemberNormal, diskID: 1},
		{state: rawASMMirrorMemberNormal, diskID: 2},
	})
	if err := stale.Close(); err != nil {
		t.Fatal(err)
	}

	group, err := OpenRawASMGroup(append(paths, stalePath)...)
	if err != nil {
		t.Fatal(err)
	}
	defer group.Close()
	if group.generation != 2 {
		t.Fatalf("group generation = %d, want 2", group.generation)
	}
	if len(group.disks) != 2 || group.disks[0] == nil || group.disks[1] == nil || group.disks[2] != nil {
		t.Fatalf("current member disks = %#v", group.disks)
	}
	file, err := group.Open("+NORM1/test.bin")
	if err != nil {
		t.Fatal(err)
	}
	for sequence, extent := range file.extents {
		if len(extent.copies) != 2 {
			t.Fatalf("logical AU %d exposes %d copies, want 2", sequence, len(extent.copies))
		}
		for _, copy := range extent.copies {
			if copy.diskID == 2 {
				t.Fatalf("logical AU %d retains stale disk 2", sequence)
			}
		}
	}
}

func TestRawASMMirrorDoesNotReadOfflineOrReconnectMember(t *testing.T) {
	paths := createRawASMMirrorFixture(t, t.TempDir())
	for _, path := range paths {
		file, err := os.OpenFile(path, os.O_RDWR, 0600)
		if err != nil {
			t.Fatal(err)
		}
		writeRawASMMirrorGroupMetadata(t, file, 3, []rawASMMirrorTestMember{
			{state: rawASMMirrorMemberNormal, diskID: 0},
			{state: rawASMMirrorMemberUnavailable, diskID: 1},
		})
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
	}

	group, err := OpenRawASMGroup(paths...)
	if err != nil {
		t.Fatal(err)
	}
	defer group.Close()
	if len(group.disks) != 1 || group.disks[0] == nil || group.disks[1] != nil {
		t.Fatalf("readable member disks = %#v", group.disks)
	}
	file, err := group.Open("+NORM1/test.bin")
	if err != nil {
		t.Fatal(err)
	}
	page := make([]byte, 4096)
	if _, err := file.ReadAt(page, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(page, mirrorTestLogicalBytes(0, len(page))) {
		t.Fatal("NORMAL copy does not match while the other member is unavailable")
	}
}

func TestRawASMMirrorReadsDescriptorFromSecondRegion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "large-mirror.raw")
	const (
		diskAUs = uint32(32780)
		dataAU  = uint32(32769)
		auSize  = int64(1024 * 1024)
	)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := truncateSparseTestFile(file, int64(diskAUs)*auSize); err != nil {
		file.Close()
		t.Fatal(err)
	}
	header := make([]byte, 128)
	binary.LittleEndian.PutUint16(header[0:2], 1)
	binary.LittleEndian.PutUint32(header[8:12], rawASMMirrorFormat)
	binary.LittleEndian.PutUint32(header[12:16], rawASMMirrorSignature)
	binary.LittleEndian.PutUint16(header[16:18], 1)
	binary.LittleEndian.PutUint32(header[24:28], diskAUs)
	copy(header[28:60], []byte(rawASMMirrorTestDiskName(0)))
	writeRawASMBytes(t, file, 0, header)
	writeRawASMMirrorGroupMetadata(t, file, 2, []rawASMMirrorTestMember{{state: rawASMMirrorMemberNormal, diskID: 0, diskAUs: diskAUs}})
	writeRawASMMirrorDescriptor(t, file, 8, 2, 0, rawASMCopy{}, rawASMCopy{})
	writeRawASMMirrorDescriptor(t, file, dataAU, 18, 0, rawASMCopy{}, rawASMCopy{})
	writeRawASMMirrorInode(t, file, 8, 0, 1, 1, "+NORM1", 0, 0, 1, 0)
	writeRawASMMirrorInode(t, file, 8, 512, 2, 2, "+NORM1/.inode", auSize, 1, 1, 0)
	writeRawASMMirrorInode(t, file, 8, 1024, 18, 4, "+NORM1/large.bin", 4096, 1, 1, 0)
	want := bytes.Repeat([]byte{0x5a}, 4096)
	writeRawASMBytes(t, file, int64(dataAU)*auSize, want)
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	group, err := OpenRawASMGroup(path)
	if err != nil {
		t.Fatal(err)
	}
	defer group.Close()
	asmFile, err := group.Open("+NORM1/large.bin")
	if err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(want))
	if _, err := asmFile.ReadAt(got, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("file data from the second descriptor region does not match")
	}
}

func TestRawASMMirrorDescriptorOffsetsAcrossRegions(t *testing.T) {
	const auSize = int64(1024 * 1024)
	for _, test := range []struct {
		au   uint32
		want int64
	}{
		{8, 8 * rawASMMirrorDescriptorSize},
		{32767, 32767 * rawASMMirrorDescriptorSize},
		{32768, 32768 * auSize},
		{32769, 32768*auSize + rawASMMirrorDescriptorSize},
		{65536, 65536 * auSize},
		{65537, 65536*auSize + rawASMMirrorDescriptorSize},
	} {
		if got := rawASMMirrorDescriptorOffset(auSize, test.au); got != test.want {
			t.Fatalf("descriptor offset for AU %d = %d, want %d", test.au, got, test.want)
		}
	}
}

func TestRawASMMirrorLoadsThreeInodeAUs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "three-inode-aus.raw")
	file := createRawASMMirrorTestDisk(t, path, 0)
	writeRawASMMirrorDescriptor(t, file, 8, 2, 0, rawASMCopy{}, rawASMCopy{diskID: 0, auNo: 13})
	writeRawASMMirrorDescriptor(t, file, 13, 2, 1, rawASMCopy{diskID: 0, auNo: 8}, rawASMCopy{diskID: 0, auNo: 14})
	writeRawASMMirrorDescriptor(t, file, 14, 2, 2, rawASMCopy{diskID: 0, auNo: 13}, rawASMCopy{})
	writeRawASMMirrorInode(t, file, 8, 0, 1, 1, "+NORM1", 0, 0, 1, 0)
	// The self record deliberately reports only two AUs. The linked allocation
	// map is authoritative while an INODE growth update is being persisted.
	writeRawASMMirrorInode(t, file, 8, 512, 2, 2, "+NORM1/.inode", 2*1024*1024, 2, 1, 0)
	writeRawASMMirrorInode(t, file, 13, 0, 18, 4, "+NORM1/second.empty", 0, 0, 1, 0)
	writeRawASMMirrorInode(t, file, 14, 0, 19, 4, "+NORM1/third.empty", 0, 0, 1, 0)
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	group, err := OpenRawASMGroup(path)
	if err != nil {
		t.Fatal(err)
	}
	defer group.Close()
	if len(group.maps[2]) != 3 || len(group.Files()) != 4 {
		t.Fatalf("inode maps/files = %d/%d, want 3/4", len(group.maps[2]), len(group.Files()))
	}
	third, ok := group.files[normalizeASMPath("+NORM1/third.empty")]
	if !ok || third.InodeAU != 14 || third.InodeOff != 0 {
		t.Fatalf("third INODE AU entry = %+v, exists=%v", third, ok)
	}
}

func TestRawASMMirrorParsesWideExtentCount(t *testing.T) {
	entry := make([]byte, rawASMMirrorInodeSize)
	binary.LittleEndian.PutUint16(entry[0:2], 18)
	entry[2] = 4
	copy(entry[3:rawASMMirrorPathEnd], []byte("+META1/large.dat"))
	binary.LittleEndian.PutUint16(entry[0x103:0x105], 33000)
	entry[0x113] = 1
	entry[0x115] = 4
	binary.LittleEndian.PutUint64(entry[0x118:0x120], uint64(33000*1024*1024/256))
	group := &RawASMGroup{groupID: 5, auSize: 1024 * 1024}
	info, ok := group.parseMirrorInode(entry, 0, 8, 1024)
	if !ok {
		t.Fatal("wide-extent INODE was rejected")
	}
	if info.Extents != 33000 || info.Size != 33000*1024*1024 {
		t.Fatalf("unexpected wide-extent INODE: %+v", info)
	}
}

func createRawASMMirrorFixture(t *testing.T, dir string) []string {
	t.Helper()
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	paths := []string{filepath.Join(dir, "mirror0.raw"), filepath.Join(dir, "mirror1.raw")}
	files := make([]*os.File, 2)
	for diskID, path := range paths {
		files[diskID] = createRawASMMirrorTestDisk(t, path, uint16(diskID))
		writeRawASMMirrorDescriptor(t, files[diskID], 8, 2, 0, rawASMCopy{}, rawASMCopy{diskID: 1, auNo: 13}, rawASMCopy{diskID: uint16(1 - diskID), auNo: 8})
		writeRawASMMirrorDescriptor(t, files[diskID], 13, 2, 1, rawASMCopy{diskID: 0, auNo: 8}, rawASMCopy{}, rawASMCopy{diskID: uint16(1 - diskID), auNo: 13})
		for sequence := uint32(0); sequence < 4; sequence++ {
			var previous, next rawASMCopy
			if sequence > 0 {
				previous = rawASMCopy{diskID: uint16((sequence - 1) % 2), auNo: 9 + sequence - 1}
			}
			if sequence+1 < 4 {
				next = rawASMCopy{diskID: uint16((sequence + 1) % 2), auNo: 9 + sequence + 1}
			}
			writeRawASMMirrorDescriptor(t, files[diskID], 9+sequence, 18, sequence, previous, next, rawASMCopy{diskID: uint16(1 - diskID), auNo: 9 + sequence})
		}
		writeRawASMMirrorInode(t, files[diskID], 8, 0, 1, 1, "+NORM1", 0, 0, 0, 0)
		writeRawASMMirrorInode(t, files[diskID], 8, 512, 2, 2, "+NORM1/.inode", 2*1024*1024, 2, 2, 0)
		writeRawASMMirrorInode(t, files[diskID], 8, 1024, 18, 4, "+NORM1/test.bin", 4*1024*1024, 4, 2, 32)
		// Real DBF INODEs may use +0x110 as an independent allocation flag.
		// It must not inflate the redundancy count stored at +0x113.
		writeRawASMBytes(t, files[diskID], 8*1024*1024+1024+0x110, []byte{0x7f})
		writeRawASMMirrorInode(t, files[diskID], 13, 0, 19, 4, "+NORM1/extra.empty", 0, 0, 2, 0)
		writeRawASMMirrorStripedData(t, files[diskID])
		if err := files[diskID].Close(); err != nil {
			t.Fatal(err)
		}
	}
	return paths
}

func TestParseRawASMMirrorDescriptorCopyArray(t *testing.T) {
	// NORM4 disk 0 AU 43 from the archived deterministic sample. The current
	// AU is disk 0/AU 43; +0x0C records the other NORMAL copy at disk 1/AU 43.
	raw := []byte{
		0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
		0x01, 0x00, 0x2c, 0x00, 0x00, 0x00,
		0x01, 0x00, 0x2b, 0x00, 0x00, 0x00,
		0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
		0x12, 0x00, 0x00, 0x00, 0x00, 0x00, 0x5f, 0xe1,
	}
	desc, ok := parseRawASMMirrorDescriptor(raw)
	if !ok {
		t.Fatal("deterministic mirror descriptor was rejected")
	}
	if desc.hasPrevious || !desc.hasNext || desc.next != (rawASMCopy{diskID: 1, auNo: 44}) {
		t.Fatalf("links = prev(%t)=%+v next(%t)=%+v", desc.hasPrevious, desc.previous, desc.hasNext, desc.next)
	}
	if len(desc.mirrors) != 1 || desc.mirrors[0] != (rawASMCopy{diskID: 1, auNo: 43}) {
		t.Fatalf("mirror copies = %+v", desc.mirrors)
	}
	if desc.fileID != 18 || desc.sequence != 0 || desc.checksum != 0xe15f {
		t.Fatalf("identity = file=%d sequence=%d checksum=0x%04x", desc.fileID, desc.sequence, desc.checksum)
	}
}

func TestParseRawASMMirrorDescriptorSeparatesFailedCopyBitmap(t *testing.T) {
	raw := make([]byte, rawASMMirrorDescriptorSize)
	for index := 0; index < 24; index++ {
		raw[index] = 0xff
	}
	binary.LittleEndian.PutUint16(raw[24:26], 34)
	binary.LittleEndian.PutUint16(raw[27:29], 1)
	raw[29] = 0x02
	binary.LittleEndian.PutUint16(raw[30:32], 0xe548)

	desc, ok := parseRawASMMirrorDescriptor(raw)
	if !ok {
		t.Fatal("descriptor was not recognized")
	}
	if desc.fileID != 34 || desc.sequence != 1 || desc.failedCopies != 0x02 || desc.checksum != 0xe548 {
		t.Fatalf("descriptor = file=%d sequence=%d failed=0x%02x checksum=0x%04x", desc.fileID, desc.sequence, desc.failedCopies, desc.checksum)
	}
}

func TestRawASMStorageRoutesMultipleDiskGroups(t *testing.T) {
	dir := t.TempDir()
	mirrorPaths := createRawASMMirrorFixture(t, filepath.Join(dir, "mirror"))
	legacyPath := filepath.Join(dir, "legacy.raw")
	legacy := createRawASMTestDisk(t, legacyPath)
	writeRawASMAUHeader(t, legacy, 0, rawASMAUTypeXDesc)
	writeRawASMAUHeader(t, legacy, 4, rawASMAUTypeInode)
	first := rawASMDAddr{diskID: 0, auNo: 0, offset: 0x4a0}
	writeRawASMXDesc(t, legacy, first, rawASMDAddr{}, rawASMDAddr{})
	writeRawASMInode(t, legacy, 4, 0x400, 0x82000009, "+DMDATA/DB/MAIN.DBF", 16, 1, first)
	writeRawASMBytes(t, legacy, rawASMReservedSize+20*rawASMAUSize, []byte("LEGACY-GROUP-TWO"))
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	paths := append(append([]string{}, mirrorPaths...), legacyPath)
	storage, err := OpenRawASMStorage(paths...)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	if storage.GroupCount() != 2 || storage.DiskCount() != 3 {
		t.Fatalf("storage groups/disks = %d/%d", storage.GroupCount(), storage.DiskCount())
	}
	if _, err := storage.Open("+NORM1/test.bin"); err != nil {
		t.Fatal(err)
	}
	main, err := storage.Open("+DMDATA/DB/MAIN.DBF")
	if err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 16)
	if _, err := main.ReadAt(got, 0); err != nil {
		t.Fatal(err)
	}
	if string(got) != "LEGACY-GROUP-TWO" {
		t.Fatalf("legacy group bytes = %q", got)
	}
}

func TestRawASMMirrorArchivedEvidence(t *testing.T) {
	root := strings.TrimSpace(os.Getenv("DMDUL_TEST_DMASM_MIRROR_EVIDENCE"))
	if root == "" {
		t.Skip("set DMDUL_TEST_DMASM_MIRROR_EVIDENCE to the archived mirror evidence directory")
	}
	type diskWant struct {
		name               string
		auMiB, group, disk uint16
		diskAUs            uint32
		diskName           string
	}
	for _, want := range []diskWant{
		{"ext4a-first32m.bin", 4, 0, 0, 2500, "DMASMEXT4A"},
		{"ext4b-first32m.bin", 4, 0, 1, 2500, "DMASMEXT4B"},
		{"norm4a-first32m.bin", 4, 1, 0, 10000, "DMASMNORM4A"},
		{"norm4b-first32m.bin", 4, 1, 1, 10000, "DMASMNORM4B"},
		{"ext32a-first32m.bin", 32, 2, 0, 125, "DMASMEXT32A"},
		{"dcrv01-first32m.bin", 1, 126, 0, 2000, "DMASMDCRV01"},
		{"dcrv02-first32m.bin", 1, 126, 1, 2000, "DMASMDCRV02"},
		{"dcrv03-first32m.bin", 1, 126, 2, 2000, "DMASMDCRV03"},
	} {
		t.Run(want.name, func(t *testing.T) {
			raw := readTestFilePrefix(t, filepath.Join(root, "raw-heads", want.name), 128)
			if got := binary.LittleEndian.Uint16(raw[0:2]); got != want.auMiB {
				t.Fatalf("AU MiB = %d, want %d", got, want.auMiB)
			}
			if got := binary.LittleEndian.Uint16(raw[2:4]); got != want.disk {
				t.Fatalf("disk id = %d, want %d", got, want.disk)
			}
			if got := binary.LittleEndian.Uint32(raw[8:12]); got != rawASMMirrorFormat {
				t.Fatalf("version = 0x%x", got)
			}
			if got := binary.LittleEndian.Uint32(raw[12:16]); got != rawASMMirrorSignature {
				t.Fatalf("signature = 0x%x", got)
			}
			if got := binary.LittleEndian.Uint16(raw[16:18]); got != want.group {
				t.Fatalf("group id = %d, want %d", got, want.group)
			}
			if got := binary.LittleEndian.Uint32(raw[24:28]); got != want.diskAUs {
				t.Fatalf("disk AUs = %d, want %d", got, want.diskAUs)
			}
			if got := cString(raw[28:60]); got != want.diskName {
				t.Fatalf("disk name = %q, want %q", got, want.diskName)
			}
		})
	}

	// These six deterministic INODE records cover AU=4/32 MiB,
	// EXTERNAL/NORMAL redundancy and striping=0/32 KiB.
	type inodeWant struct {
		capture, path               string
		group                       uint16
		auMiB, sizeMiB              int64
		lowID                       uint16
		extents, copies, stripingKB uint32
	}
	for _, want := range []inodeWant{
		{"ext4-inode-au8.bin", "+EXT4/external_stripe0_au4.dat", 0, 4, 256, 18, 64, 1, 0},
		{"ext4-inode-au8.bin", "+EXT4/external_stripe32_au4.dat", 0, 4, 256, 19, 64, 1, 32},
		{"norm4a-inode-au10.bin", "+NORM4/normal_stripe0_au4.dat", 1, 4, 256, 18, 64, 2, 0},
		{"norm4a-inode-au10.bin", "+NORM4/normal_stripe32_au4.dat", 1, 4, 256, 19, 64, 2, 32},
		{"ext32-inode-au8.bin", "+EXT32/external_stripe0_au32.dat", 2, 32, 512, 18, 16, 1, 0},
		{"ext32-inode-au8.bin", "+EXT32/external_stripe32_au32.dat", 2, 32, 512, 19, 16, 1, 32},
	} {
		t.Run(strings.TrimPrefix(want.path, "+"), func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(root, "targeted-live-read", want.capture))
			if err != nil {
				t.Fatal(err)
			}
			pathAt := bytes.Index(raw, []byte(want.path))
			if pathAt < 3 || (pathAt-3)%int(rawASMMirrorInodeSize) != 0 {
				t.Fatalf("aligned INODE for %s not found", want.path)
			}
			start := pathAt - 3
			group := &RawASMGroup{groupID: want.group, auSize: want.auMiB * 1024 * 1024}
			info, ok := group.parseMirrorInode(raw[start:start+int(rawASMMirrorInodeSize)], 0, 0, uint32(start))
			if !ok {
				t.Fatal("INODE was rejected")
			}
			if info.ID != mirrorASMFileID(want.group, group.auSize, want.lowID) || info.Path != want.path ||
				info.Size != want.sizeMiB*1024*1024 || info.Extents != want.extents || info.Copies != want.copies ||
				info.StripingKB != want.stripingKB || info.AUGroup != 4 {
				t.Fatalf("unexpected INODE: %+v", info)
			}
		})
	}

	// Cross-check one real NORMAL descriptor from each member. Each member
	// records the other member in copy[0].
	for _, item := range []struct {
		name        string
		self, other uint16
	}{
		{"norm4a-first32m.bin", 0, 1},
		{"norm4b-first32m.bin", 1, 0},
	} {
		raw := readTestFileAt(t, filepath.Join(root, "raw-heads", item.name), 43*rawASMMirrorDescriptorSize, int(rawASMMirrorDescriptorSize))
		desc, ok := parseRawASMMirrorDescriptor(raw)
		if !ok || desc.fileID != 18 || desc.sequence != 0 || len(desc.mirrors) != 1 || desc.mirrors[0] != (rawASMCopy{diskID: item.other, auNo: 43}) {
			t.Fatalf("%s descriptor does not expose the expected copy array: %+v", item.name, desc)
		}
	}
}

func TestRawASMMirrorDeterministicFiles(t *testing.T) {
	diskList := strings.TrimSpace(os.Getenv("DMDUL_TEST_DMASM_MIRROR_DISKS"))
	if diskList == "" {
		t.Skip("set DMDUL_TEST_DMASM_MIRROR_DISKS for the read-only six-file integration test")
	}
	storage, err := OpenRawASMStorage(strings.Split(diskList, ",")...)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	verifyRawASMDeterministicFiles(t, storage, []rawASMDeterministicSample{
		{"+EXT4/external_stripe0_au4.dat", 0x4558543453300001, 0x40, 256, 0, 1, 64, 0},
		{"+EXT4/external_stripe32_au4.dat", 0x4558543453333202, 0x41, 256, 0, 1, 64, 32},
		{"+NORM4/normal_stripe0_au4.dat", 0x4e4f524d53300003, 0x42, 256, 1, 2, 64, 0},
		{"+NORM4/normal_stripe32_au4.dat", 0x4e4f524d53333204, 0x43, 256, 1, 2, 64, 32},
		{"+EXT32/external_stripe0_au32.dat", 0x4558333253300005, 0x44, 512, 2, 1, 16, 0},
		{"+EXT32/external_stripe32_au32.dat", 0x4558333253333206, 0x45, 512, 2, 1, 16, 32},
	})
}

func TestRawASMMirrorMetadataScaleDisk(t *testing.T) {
	diskPath := strings.TrimSpace(os.Getenv("DMDUL_TEST_DMASM_METADATA_SCALE_DISK"))
	if diskPath == "" {
		t.Skip("set DMDUL_TEST_DMASM_METADATA_SCALE_DISK for the read-only metadata-scale integration test")
	}
	fileCount := 5200
	if raw := strings.TrimSpace(os.Getenv("DMDUL_TEST_DMASM_METADATA_SCALE_FILES")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 {
			t.Fatalf("invalid DMDUL_TEST_DMASM_METADATA_SCALE_FILES %q", raw)
		}
		fileCount = parsed
	}

	group, err := OpenRawASMGroup(diskPath)
	if err != nil {
		t.Fatal(err)
	}
	defer group.Close()
	if group.GroupID() != 5 || group.GroupName() != "META1" || group.auSize != 1024*1024 {
		t.Fatalf("unexpected metadata-scale group: id=%d name=%q au=%d", group.GroupID(), group.GroupName(), group.auSize)
	}
	disk := group.disks[0]
	if disk == nil || disk.lastAU+1 < 65537 {
		t.Fatalf("metadata-scale disk does not span three descriptor regions: %+v", disk)
	}

	extraEntries := 0
	type spanWant struct {
		path       string
		extents    uint32
		regionAU   uint32
		regionName string
	}
	spans := []spanWant{
		{strings.TrimSpace(os.Getenv("DMDUL_TEST_DMASM_METADATA_SCALE_SPAN")), 33000, 32768, "second"},
		{strings.TrimSpace(os.Getenv("DMDUL_TEST_DMASM_METADATA_SCALE_SPAN2")), 28000, 65536, "third"},
	}
	for _, span := range spans {
		if span.path != "" {
			extraEntries++
		}
	}
	wantCatalogEntries := fileCount + 68 + extraEntries // group root, .inode, aredo, catalog, 64 child directories and optional span file
	if got := len(group.Files()); got != wantCatalogEntries {
		t.Fatalf("catalog entries = %d, want %d", got, wantCatalogEntries)
	}
	inodeEntriesPerAU := int(group.auSize / rawASMMirrorInodeSize)
	wantInodeAUs := (wantCatalogEntries + inodeEntriesPerAU - 1) / inodeEntriesPerAU
	inodeMaps := group.maps[2]
	if len(inodeMaps) != wantInodeAUs {
		t.Fatalf(".inode AU count = %d, want %d", len(inodeMaps), wantInodeAUs)
	}
	for sequence := 0; sequence < wantInodeAUs; sequence++ {
		if len(inodeMaps[uint32(sequence)]) == 0 {
			t.Fatalf(".inode logical AU %d is missing", sequence)
		}
	}

	for _, number := range []int{1, 1980, 1981, 4028, 4029, fileCount} {
		if number > fileCount {
			continue
		}
		dir := (number - 1) % 64
		path := fmt.Sprintf("+META1/catalog/d%03d/f%06d.dat", dir, number)
		file, err := group.Open(path)
		if err != nil {
			t.Fatalf("open boundary file %s: %v", path, err)
		}
		if file.Size() != 1024*1024 || file.Info().Extents != 1 || file.Info().Copies != 1 {
			t.Fatalf("unexpected boundary file metadata for %s: %+v", path, file.Info())
		}
	}
	for _, span := range spans {
		if span.path == "" {
			continue
		}
		file, err := group.Open(span.path)
		if err != nil {
			t.Fatalf("open descriptor-span file %s: %v", span.path, err)
		}
		if file.Size() != int64(span.extents)*group.auSize || file.Info().Extents != span.extents {
			t.Fatalf("unexpected descriptor-span metadata: %+v", file.Info())
		}
		if got := len(group.maps[uint16(file.Info().ID)]); got != int(span.extents) {
			t.Fatalf("descriptor-span AU map count = %d, want %d", got, span.extents)
		}
		firstHighSequence := -1
		for sequence, extent := range file.extents {
			if len(extent.copies) != 1 {
				t.Fatalf("descriptor-span logical AU %d has %d copies", sequence, len(extent.copies))
			}
			if extent.copies[0].auNo >= span.regionAU {
				firstHighSequence = sequence
				break
			}
		}
		if firstHighSequence < 0 {
			t.Fatalf("descriptor-span file has no allocation in the %s descriptor region", span.regionName)
		}
		probe := make([]byte, 4096)
		for _, offset := range []int64{int64(firstHighSequence) * group.auSize, file.Size() - int64(len(probe))} {
			if _, err := file.ReadAt(probe, offset); err != nil {
				t.Fatalf("read descriptor-span file at logical offset %d: %v", offset, err)
			}
		}
		t.Logf("span_file=%s extents=%d first_%s_region_sequence=%d", span.path, file.Info().Extents, span.regionName, firstHighSequence)
	}
	t.Logf("group=%s disk_aus=%d files=%d inode_aus=%d", group.GroupName(), disk.lastAU+1, len(group.Files()), len(inodeMaps))
}

func TestRawASMHighDeterministicFiles(t *testing.T) {
	diskList := strings.TrimSpace(os.Getenv("DMDUL_TEST_DMASM_HIGH_DISKS"))
	if diskList == "" {
		t.Skip("set DMDUL_TEST_DMASM_HIGH_DISKS for the read-only HIGH redundancy integration test")
	}
	storage, err := OpenRawASMStorage(strings.Split(diskList, ",")...)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	samples := []rawASMDeterministicSample{
		{"+HIGH4/high_stripe0_au4.dat", 0x4849474830000001, 0x48, 256, 3, 3, 64, 0},
		{"+HIGH4/high_stripe32_au4.dat", 0x4849474833320002, 0x49, 256, 3, 3, 64, 32},
	}
	verifyRawASMDeterministicFiles(t, storage, samples)
	for _, sample := range samples {
		verifyRawASMDeterministicFileEveryPage(t, storage, sample)
	}
	if copiedDBF := strings.TrimSpace(os.Getenv("DMDUL_TEST_DMASM_HIGH_DBF_COPY")); copiedDBF != "" {
		compareRawASMFileWithCopy(t, storage, "+HIGH4/data/MIRRORDB/TS_HIGH4_01.DBF", copiedDBF)
	}
}

func TestRawASMNormalRebalanceFiles(t *testing.T) {
	diskList := strings.TrimSpace(os.Getenv("DMDUL_TEST_DMASM_REBALANCE_DISKS"))
	if diskList == "" {
		t.Skip("set DMDUL_TEST_DMASM_REBALANCE_DISKS for the read-only NORMAL rebalance integration test")
	}
	storage, err := OpenRawASMStorage(strings.Split(diskList, ",")...)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	samples := []rawASMDeterministicSample{
		{"+RBLN4/rebalance_stripe0_au4.dat", 0x52424c4e30000001, 0x52, 512, 4, 2, 128, 0},
		{"+RBLN4/rebalance_stripe32_au4.dat", 0x52424c4e33320002, 0x53, 512, 4, 2, 128, 32},
		{"+RBLN4/rebalance_fill0_au4.dat", 0x52424c4e46494c30, 0x54, 512, 4, 2, 128, 0},
		{"+RBLN4/rebalance_fill1_au4.dat", 0x52424c4e46494c31, 0x55, 512, 4, 2, 128, 0},
		{"+RBLN4/rebalance_fill2_au4.dat", 0x52424c4e46494c32, 0x56, 512, 4, 2, 128, 0},
		{"+RBLN4/rebalance_fill3_au4.dat", 0x52424c4e46494c33, 0x57, 512, 4, 2, 128, 0},
		{"+RBLN4/rebalance_fill4_au4.dat", 0x52424c4e46494c34, 0x58, 512, 4, 2, 128, 0},
		{"+RBLN4/rebalance_fill5_au4.dat", 0x52424c4e46494c35, 0x59, 512, 4, 2, 128, 0},
		{"+RBLN4/rebalance_fill6_au4.dat", 0x52424c4e46494c36, 0x5a, 512, 4, 2, 128, 0},
		{"+RBLN4/rebalance_fill7_au4.dat", 0x52424c4e46494c37, 0x5b, 512, 4, 2, 128, 0},
		{"+RBLN4/rebalance_fill8_au4.dat", 0x52424c4e46494c38, 0x5c, 512, 4, 2, 128, 0},
		{"+RBLN4/rebalance_fill9_au4.dat", 0x52424c4e46494c39, 0x5d, 512, 4, 2, 128, 0},
		{"+RBLN4/rebalance_fill10_au4.dat", 0x52424c4e46494c3a, 0x5e, 512, 4, 2, 128, 0},
		{"+RBLN4/rebalance_fill11_au4.dat", 0x52424c4e46494c3b, 0x5f, 512, 4, 2, 128, 0},
	}
	verifyRawASMDeterministicFiles(t, storage, samples)
	for _, sample := range samples {
		verifyRawASMDeterministicFileEveryPage(t, storage, sample)
		logRawASMExtentMap(t, storage, sample.path, int(sample.copies), strings.Contains(sample.path, "stripe"))
	}
}

func logRawASMExtentMap(t *testing.T, storage *RawASMStorage, path string, expectedCopies int, verbose bool) {
	t.Helper()
	file, err := storage.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	counts := make(map[uint16]int)
	for sequence, extent := range file.extents {
		if len(extent.copies) != expectedCopies {
			t.Fatalf("%s logical AU %d exposes %d copies, want %d", path, sequence, len(extent.copies), expectedCopies)
		}
		parts := make([]string, 0, len(extent.copies))
		seenDisks := make(map[uint16]bool, len(extent.copies))
		for _, copy := range extent.copies {
			if seenDisks[copy.diskID] {
				t.Fatalf("%s logical AU %d repeats disk %d", path, sequence, copy.diskID)
			}
			seenDisks[copy.diskID] = true
			counts[copy.diskID]++
			parts = append(parts, fmt.Sprintf("%d/%d", copy.diskID, copy.auNo))
		}
		if verbose {
			t.Logf("AU_MAP path=%s sequence=%d copies=%s", path, sequence, strings.Join(parts, ","))
		}
	}
	for diskID := uint16(0); diskID < 32; diskID++ {
		if counts[diskID] != 0 {
			t.Logf("AU_DISTRIBUTION path=%s disk=%d copies=%d", path, diskID, counts[diskID])
		}
	}
}

type rawASMDeterministicSample struct {
	path                        string
	tag                         uint64
	fill                        byte
	sizeMiB                     int64
	group                       uint16
	copies, extents, stripingKB uint32
}

func verifyRawASMDeterministicFiles(t *testing.T, storage *RawASMStorage, samples []rawASMDeterministicSample) {
	t.Helper()
	for _, want := range samples {
		t.Run(strings.TrimPrefix(want.path, "+"), func(t *testing.T) {
			file, err := storage.Open(want.path)
			if err != nil {
				t.Fatal(err)
			}
			info := file.Info()
			if info.Size != want.sizeMiB*1024*1024 || info.GroupID != want.group || info.Copies != want.copies ||
				info.Extents != want.extents || info.StripingKB != want.stripingKB || info.AUGroup != 4 {
				t.Fatalf("unexpected file metadata: %+v", info)
			}
			offsets := []int64{0, 4 * 1024, 32 * 1024, 128 * 1024, file.group.auSize - 4096,
				file.group.auSize, 4*file.group.auSize - 4096, 4 * file.group.auSize, info.Size/2 - 4096, info.Size - 4096}
			seen := make(map[int64]bool)
			for _, offset := range offsets {
				if offset < 0 || offset+4096 > info.Size || seen[offset] {
					continue
				}
				seen[offset] = true
				page := make([]byte, 4096)
				if _, err := file.ReadAt(page, offset); err != nil {
					t.Fatalf("read offset %d: %v", offset, err)
				}
				if got := binary.LittleEndian.Uint64(page[0:8]); got != want.tag {
					t.Fatalf("offset %d tag = 0x%x, want 0x%x", offset, got, want.tag)
				}
				if got := binary.LittleEndian.Uint64(page[8:16]); got != uint64(offset) {
					t.Fatalf("offset marker = %d, want %d", got, offset)
				}
				if !bytes.Equal(page[16:], bytes.Repeat([]byte{want.fill}, len(page)-16)) {
					t.Fatalf("offset %d payload fill differs", offset)
				}
			}
		})
	}
}

func verifyRawASMDeterministicFileEveryPage(t *testing.T, storage *RawASMStorage, want rawASMDeterministicSample) {
	t.Helper()
	file, err := storage.Open(want.path)
	if err != nil {
		t.Fatal(err)
	}
	const pageSize = int64(4096)
	page := make([]byte, pageSize)
	for offset := int64(0); offset < file.Size(); offset += pageSize {
		if _, err := file.ReadAt(page, offset); err != nil {
			t.Fatalf("full scan %s offset %d: %v", want.path, offset, err)
		}
		if got := binary.LittleEndian.Uint64(page[0:8]); got != want.tag {
			t.Fatalf("full scan %s offset %d tag = 0x%x, want 0x%x", want.path, offset, got, want.tag)
		}
		if got := binary.LittleEndian.Uint64(page[8:16]); got != uint64(offset) {
			t.Fatalf("full scan %s offset marker = %d, want %d", want.path, got, offset)
		}
		for index, value := range page[16:] {
			if value != want.fill {
				t.Fatalf("full scan %s offset %d payload byte %d = 0x%02x, want 0x%02x", want.path, offset, index+16, value, want.fill)
			}
		}
	}
}

func compareRawASMFileWithCopy(t *testing.T, storage *RawASMStorage, asmPath, copiedPath string) {
	t.Helper()
	asmFile, err := storage.Open(asmPath)
	if err != nil {
		t.Fatal(err)
	}
	copyFile, err := os.Open(copiedPath)
	if err != nil {
		t.Fatal(err)
	}
	defer copyFile.Close()
	copyInfo, err := copyFile.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if copyInfo.Size() != asmFile.Size() {
		t.Fatalf("copied DBF size = %d, raw ASM size = %d", copyInfo.Size(), asmFile.Size())
	}
	const chunkSize = 1024 * 1024
	rawChunk := make([]byte, chunkSize)
	copyChunk := make([]byte, chunkSize)
	for offset := int64(0); offset < asmFile.Size(); offset += chunkSize {
		count := int64(chunkSize)
		if remaining := asmFile.Size() - offset; count > remaining {
			count = remaining
		}
		if _, err := asmFile.ReadAt(rawChunk[:count], offset); err != nil {
			t.Fatalf("read raw ASM DBF offset %d: %v", offset, err)
		}
		if _, err := copyFile.ReadAt(copyChunk[:count], offset); err != nil {
			t.Fatalf("read copied DBF offset %d: %v", offset, err)
		}
		if bytes.Equal(rawChunk[:count], copyChunk[:count]) {
			continue
		}
		for index := int64(0); index < count; index++ {
			if rawChunk[index] != copyChunk[index] {
				t.Fatalf("raw ASM DBF differs from official copy at offset %d: raw=0x%02x copied=0x%02x", offset+index, rawChunk[index], copyChunk[index])
			}
		}
		t.Fatalf("raw ASM DBF differs from official copy in chunk at offset %d", offset)
	}
}

func readTestFilePrefix(t *testing.T, path string, count int) []byte {
	t.Helper()
	return readTestFileAt(t, path, 0, count)
}

func readTestFileAt(t *testing.T, path string, offset int64, count int) []byte {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	raw := make([]byte, count)
	if _, err := file.ReadAt(raw, offset); err != nil {
		t.Fatal(err)
	}
	return raw
}

func createRawASMMirrorTestDisk(t *testing.T, path string, diskID uint16) *os.File {
	t.Helper()
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0600)
	if err != nil {
		t.Fatal(err)
	}
	const diskAUs = uint32(16)
	const auSize = int64(1024 * 1024)
	if err := truncateSparseTestFile(file, int64(diskAUs)*auSize); err != nil {
		t.Fatal(err)
	}
	header := make([]byte, 128)
	binary.LittleEndian.PutUint16(header[0:2], 1)
	binary.LittleEndian.PutUint16(header[2:4], diskID)
	binary.LittleEndian.PutUint32(header[8:12], rawASMMirrorFormat)
	binary.LittleEndian.PutUint32(header[12:16], rawASMMirrorSignature)
	binary.LittleEndian.PutUint16(header[16:18], 1)
	binary.LittleEndian.PutUint32(header[24:28], diskAUs)
	copy(header[28:60], []byte(rawASMMirrorTestDiskName(diskID)))
	writeRawASMBytes(t, file, 0, header)
	writeRawASMMirrorGroupMetadata(t, file, 2, []rawASMMirrorTestMember{
		{state: rawASMMirrorMemberNormal, diskID: 0},
		{state: rawASMMirrorMemberNormal, diskID: 1},
		{state: rawASMMirrorMemberDeleted, diskID: 2},
	})
	return file
}

type rawASMMirrorTestMember struct {
	state   uint16
	diskID  uint16
	diskAUs uint32
}

func writeRawASMMirrorGroupMetadata(t *testing.T, file *os.File, generation uint32, members []rawASMMirrorTestMember) {
	t.Helper()
	const auSize = int64(1024 * 1024)
	groupHeader := make([]byte, 128)
	binary.LittleEndian.PutUint16(groupHeader[0:2], 1)
	binary.LittleEndian.PutUint32(groupHeader[6:10], generation)
	copy(groupHeader[12:44], []byte("NORM1"))
	writeRawASMBytes(t, file, auSize, groupHeader)

	for index, member := range members {
		record := make([]byte, rawASMMirrorMemberSize)
		binary.LittleEndian.PutUint16(record[0:2], member.state)
		binary.LittleEndian.PutUint16(record[2:4], member.diskID)
		diskAUs := member.diskAUs
		if diskAUs == 0 {
			diskAUs = 16
		}
		binary.LittleEndian.PutUint32(record[8:12], diskAUs)
		copy(record[44:76], []byte(fmt.Sprintf("NORM_FG%d", member.diskID+1)))
		copy(record[76:108], []byte(rawASMMirrorTestDiskName(member.diskID)))
		record[127] = 0x80
		writeRawASMBytes(t, file, 2*auSize+int64(index)*rawASMMirrorMemberSize, record)
	}
}

func rawASMMirrorTestDiskName(diskID uint16) string {
	return fmt.Sprintf("DMASMNORM1%c", 'A'+rune(diskID))
}

func writeRawASMMirrorDescriptor(t *testing.T, file *os.File, auNo uint32, fileID uint16, sequence uint32, previous, next rawASMCopy, mirrors ...rawASMCopy) {
	t.Helper()
	if sequence > uint32(^uint16(0)) {
		t.Fatalf("synthetic mirror sequence %d exceeds the decoded 16-bit field", sequence)
	}
	desc := make([]byte, rawASMMirrorDescriptorSize)
	for i := 0; i < 24; i++ {
		desc[i] = 0xff
	}
	putRawASMMirrorDAddr(desc[0:6], previous)
	putRawASMMirrorDAddr(desc[6:12], next)
	for index, mirror := range mirrors {
		if index >= 2 {
			break
		}
		putRawASMMirrorDAddr(desc[12+index*6:18+index*6], mirror)
	}
	binary.LittleEndian.PutUint16(desc[24:26], fileID)
	binary.LittleEndian.PutUint16(desc[27:29], uint16(sequence))
	writeRawASMBytes(t, file, rawASMMirrorDescriptorOffset(1024*1024, auNo), desc)
}

func putRawASMMirrorDAddr(raw []byte, address rawASMCopy) {
	if address == (rawASMCopy{}) {
		for index := range raw {
			raw[index] = 0xff
		}
		return
	}
	binary.LittleEndian.PutUint16(raw[0:2], address.diskID)
	binary.LittleEndian.PutUint32(raw[2:6], address.auNo)
}

func writeRawASMMirrorInode(t *testing.T, file *os.File, inodeAU uint32, off int64, lowID uint16, typeCode byte, path string, size int64, extents, copies uint32, stripingKB uint32) {
	t.Helper()
	if extents > uint32(^uint16(0)) {
		t.Fatalf("synthetic mirror extent count %d exceeds the decoded 16-bit field", extents)
	}
	entry := make([]byte, rawASMMirrorInodeSize)
	binary.LittleEndian.PutUint16(entry[0:2], lowID)
	entry[2] = typeCode
	copy(entry[3:rawASMMirrorPathEnd], []byte(path))
	binary.LittleEndian.PutUint16(entry[0x103:0x105], uint16(extents))
	binary.BigEndian.PutUint32(entry[0x110:0x114], copies)
	if stripingKB != 0 {
		var shift byte
		for value := stripingKB; value > 1; value >>= 1 {
			shift++
		}
		entry[0x114] = shift
	}
	entry[0x115] = 4
	binary.LittleEndian.PutUint64(entry[0x118:0x120], uint64(size/256))
	writeRawASMBytes(t, file, int64(inodeAU)*1024*1024+off, entry)
}

func writeRawASMMirrorStripedData(t *testing.T, file *os.File) {
	t.Helper()
	const (
		auSize     = int64(1024 * 1024)
		stripeSize = int64(32 * 1024)
		auGroup    = int64(4)
		logicalLen = int64(4 * 1024 * 1024)
	)
	for logical := int64(0); logical < logicalLen; logical += stripeSize {
		stripeIndex := logical / stripeSize
		mapIndex := stripeIndex % auGroup
		physicalWithin := stripeIndex / auGroup * stripeSize
		data := mirrorTestLogicalBytes(int(logical), int(stripeSize))
		writeRawASMBytes(t, file, (9+mapIndex)*auSize+physicalWithin, data)
	}
}

func mirrorTestLogicalBytes(start, length int) []byte {
	data := make([]byte, length)
	for i := range data {
		data[i] = byte((start + i) / 4096)
	}
	return data
}
