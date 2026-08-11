package dm

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const offlineCopyBufferSize = 4 * 1024 * 1024

// OfflineFileCopyResult describes one logical file materialized on a regular
// filesystem. SHA256 is calculated while bytes are read from the source.
type OfflineFileCopyResult struct {
	TargetPath string
	Bytes      int64
	SHA256     string
}

// CopyOfflineFile materializes a known-size ReaderAt, such as a logical DMASM
// file, on a regular filesystem. It streams through a bounded buffer, refuses
// to overwrite an existing destination, and publishes a fully synced temporary
// file with a same-directory rename.
func CopyOfflineFile(source SizedReaderAt, targetPath string) (result OfflineFileCopyResult, err error) {
	if source == nil {
		return result, fmt.Errorf("offline copy source is nil")
	}
	size := source.Size()
	if size < 0 {
		return result, fmt.Errorf("offline copy source has invalid size %d", size)
	}
	targetPath = strings.TrimSpace(targetPath)
	if targetPath == "" {
		return result, fmt.Errorf("offline copy target path is empty")
	}
	targetPath, err = filepath.Abs(filepath.Clean(targetPath))
	if err != nil {
		return result, fmt.Errorf("resolve offline copy target %s: %w", targetPath, err)
	}
	if _, statErr := os.Lstat(targetPath); statErr == nil {
		return result, fmt.Errorf("offline copy target already exists: %s", targetPath)
	} else if !os.IsNotExist(statErr) {
		return result, fmt.Errorf("inspect offline copy target %s: %w", targetPath, statErr)
	}

	dir := filepath.Dir(targetPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return result, fmt.Errorf("create offline copy directory %s: %w", dir, err)
	}
	temporary, err := os.CreateTemp(dir, "."+filepath.Base(targetPath)+".dmdul-cp-*")
	if err != nil {
		return result, fmt.Errorf("create temporary offline copy beside %s: %w", targetPath, err)
	}
	temporaryPath := temporary.Name()
	published := false
	closed := false
	defer func() {
		if !closed {
			if closeErr := temporary.Close(); err == nil && closeErr != nil {
				err = fmt.Errorf("close temporary offline copy %s: %w", temporaryPath, closeErr)
			}
		}
		if !published {
			_ = os.Remove(temporaryPath)
		}
	}()

	digest := sha256.New()
	reader := io.NewSectionReader(source, 0, size)
	written, err := io.CopyBuffer(io.MultiWriter(temporary, digest), reader, make([]byte, offlineCopyBufferSize))
	if err != nil {
		return result, fmt.Errorf("copy logical file to %s: %w", targetPath, err)
	}
	if written != size {
		return result, fmt.Errorf("copy logical file to %s wrote %d bytes, expected %d", targetPath, written, size)
	}
	if err := temporary.Chmod(0644); err != nil {
		return result, fmt.Errorf("set offline copy permissions on %s: %w", temporaryPath, err)
	}
	if err := temporary.Sync(); err != nil {
		return result, fmt.Errorf("sync offline copy %s: %w", temporaryPath, err)
	}
	closeErr := temporary.Close()
	closed = true
	if closeErr != nil {
		return result, fmt.Errorf("close offline copy %s: %w", temporaryPath, closeErr)
	}
	info, err := os.Stat(temporaryPath)
	if err != nil {
		return result, fmt.Errorf("inspect temporary offline copy %s: %w", temporaryPath, err)
	}
	if info.Size() != size {
		return result, fmt.Errorf("temporary offline copy %s has size %d, expected %d", temporaryPath, info.Size(), size)
	}
	// Recheck immediately before rename. On Windows rename also refuses an
	// existing destination; on Unix this minimizes the already narrow race.
	if _, statErr := os.Lstat(targetPath); statErr == nil {
		return result, fmt.Errorf("offline copy target appeared during copy: %s", targetPath)
	} else if !os.IsNotExist(statErr) {
		return result, fmt.Errorf("recheck offline copy target %s: %w", targetPath, statErr)
	}
	if err := os.Rename(temporaryPath, targetPath); err != nil {
		return result, fmt.Errorf("publish offline copy %s: %w", targetPath, err)
	}
	published = true
	return OfflineFileCopyResult{
		TargetPath: targetPath,
		Bytes:      written,
		SHA256:     hex.EncodeToString(digest.Sum(nil)),
	}, nil
}
