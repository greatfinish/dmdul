//go:build !windows

package dm

import "os"

func truncateSparseTestFile(file *os.File, size int64) error {
	return file.Truncate(size)
}
