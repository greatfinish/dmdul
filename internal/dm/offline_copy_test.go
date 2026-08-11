package dm

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

type offlineCopyBytes []byte

func (r offlineCopyBytes) ReadAt(p []byte, off int64) (int, error) {
	return bytes.NewReader(r).ReadAt(p, off)
}

func (r offlineCopyBytes) Size() int64 { return int64(len(r)) }

type failingOfflineCopyReader struct{ size int64 }

func (r failingOfflineCopyReader) ReadAt([]byte, int64) (int, error) {
	return 0, errors.New("injected read failure")
}

func (r failingOfflineCopyReader) Size() int64 { return r.size }

func TestCopyOfflineFilePublishesExactBytes(t *testing.T) {
	source := offlineCopyBytes(bytes.Repeat([]byte("dmdul-asm-copy\x00"), 8192))
	target := filepath.Join(t.TempDir(), "nested", "SYSTEM.DBF")
	result, err := CopyOfflineFile(source, target)
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(content, source) {
		t.Fatal("copied file differs from logical source")
	}
	wantHash := sha256.Sum256(source)
	if result.Bytes != int64(len(source)) || result.SHA256 != bytesToHex(wantHash[:]) {
		t.Fatalf("unexpected copy result: %+v", result)
	}
	if result.TargetPath != target {
		t.Fatalf("target path = %q, want %q", result.TargetPath, target)
	}
}

func TestCopyOfflineFileRefusesOverwrite(t *testing.T) {
	target := filepath.Join(t.TempDir(), "MAIN.DBF")
	if err := os.WriteFile(target, []byte("keep"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := CopyOfflineFile(offlineCopyBytes("replacement"), target); err == nil {
		t.Fatal("expected existing target error")
	}
	content, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "keep" {
		t.Fatalf("existing target was changed: %q", content)
	}
}

func TestCopyOfflineFileRemovesTemporaryFileOnReadFailure(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "ROLL.DBF")
	if _, err := CopyOfflineFile(failingOfflineCopyReader{size: 8192}, target); err == nil {
		t.Fatal("expected injected read failure")
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("failed copy left target behind: %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(dir, ".ROLL.DBF.dmdul-cp-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("failed copy left temporary files: %v", matches)
	}
}

func TestCopyOfflineFileFromFragmentedRawASM(t *testing.T) {
	diskPath := filepath.Join(t.TempDir(), "dmasm.raw")
	disk := createRawASMTestDisk(t, diskPath)
	writeRawASMAUHeader(t, disk, 0, rawASMAUTypeXDesc)
	writeRawASMAUHeader(t, disk, 1, rawASMAUTypeXDesc)
	writeRawASMAUHeader(t, disk, 4, rawASMAUTypeInode)
	first := rawASMDAddr{diskID: 0, auNo: 0, offset: 0x4a0}
	second := rawASMDAddr{diskID: 0, auNo: 1, offset: 0x420}
	writeRawASMXDesc(t, disk, first, rawASMDAddr{}, second)
	writeRawASMXDesc(t, disk, second, first, rawASMDAddr{})
	writeRawASMInode(t, disk, 4, 0x400, 9, "+DMDATA/DMDB/MAIN.DBF", rawASMExtentSize+8, 2, first)
	writeRawASMBytes(t, disk, rawASMReservedSize+20*rawASMAUSize+rawASMExtentSize-4, []byte("ABCD"))
	writeRawASMBytes(t, disk, rawASMReservedSize+5*rawASMAUSize, []byte("EFGHIJKL"))
	if err := disk.Close(); err != nil {
		t.Fatal(err)
	}

	group, err := OpenRawASMGroup(diskPath)
	if err != nil {
		t.Fatal(err)
	}
	defer group.Close()
	source, err := group.Open("+DMDATA/DMDB/MAIN.DBF")
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "MAIN.DBF")
	result, err := CopyOfflineFile(source, target)
	if err != nil {
		t.Fatal(err)
	}
	if result.Bytes != rawASMExtentSize+8 {
		t.Fatalf("copied bytes = %d", result.Bytes)
	}
	output, err := os.Open(target)
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	boundary := make([]byte, 12)
	if _, err := output.ReadAt(boundary, rawASMExtentSize-4); err != nil && err != io.EOF {
		t.Fatal(err)
	}
	if string(boundary) != "ABCDEFGHIJKL" {
		t.Fatalf("copied extent boundary = %q", boundary)
	}
}

func bytesToHex(raw []byte) string {
	const digits = "0123456789abcdef"
	encoded := make([]byte, len(raw)*2)
	for i, value := range raw {
		encoded[i*2] = digits[value>>4]
		encoded[i*2+1] = digits[value&0x0f]
	}
	return string(encoded)
}
