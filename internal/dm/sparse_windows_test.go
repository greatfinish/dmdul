//go:build windows

package dm

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func truncateSparseTestFile(file *os.File, size int64) error {
	// NTFS otherwise reserves the entire logical size of large ASM fixtures.
	const fsctlSetSparse = 0x000900C4
	var returned uint32
	if err := syscall.DeviceIoControl(syscall.Handle(file.Fd()), fsctlSetSparse, nil, 0, nil, 0, &returned, nil); err != nil {
		return fmt.Errorf("mark test fixture sparse: %w", err)
	}
	return file.Truncate(size)
}

func TestSparseFixtureHasWindowsSparseAttribute(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sparse.raw")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	const size = int64(64 * 1024 * 1024)
	if err := truncateSparseTestFile(file, size); err != nil {
		t.Fatal(err)
	}
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	attrs, err := syscall.GetFileAttributes(name)
	if err != nil {
		t.Fatal(err)
	}
	if attrs&0x200 == 0 {
		t.Fatal("fixture lacks FILE_ATTRIBUTE_SPARSE_FILE")
	}
	info, err := file.Stat()
	if err != nil || info.Size() != size {
		t.Fatalf("logical size mismatch: info=%v err=%v", info, err)
	}
}
