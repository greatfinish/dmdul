package cli

import (
	"fmt"
	"io"
	"os"
	pathpkg "path"
	"path/filepath"
	"strings"
	"time"

	"dmdul/internal/dm"
)

type asmFileCopyPlan struct {
	sourcePath string
	targetPath string
	reader     dm.SizedReaderAt
}

func (s *interactiveSession) executeCopy(args []string, stdout io.Writer) error {
	const usage = "usage: cp <+GROUP/path/file> <filesystem file|directory> | cp datafile <filesystem directory>"
	if len(args) == 0 {
		return fmt.Errorf("%s", usage)
	}
	if strings.EqualFold(trimCommandPath(args[0]), "datafile") || strings.EqualFold(trimCommandPath(args[0]), "datafiles") {
		destination, err := copyCommandDestination(args[1:])
		if err != nil {
			return fmt.Errorf("%s", usage)
		}
		return s.copyASMDataFiles(destination, stdout)
	}
	if len(args) < 2 {
		return fmt.Errorf("%s", usage)
	}
	sourcePath := trimCommandPath(args[0])
	if !dm.IsASMPath(sourcePath) {
		return fmt.Errorf("cp source must be an ASM logical path beginning with '+': %s", sourcePath)
	}
	destination, err := copyCommandDestination(args[1:])
	if err != nil {
		return fmt.Errorf("%s", usage)
	}
	if err := s.ensureASMStorageOpen(); err != nil {
		return err
	}
	source, err := s.asmStorage.Open(sourcePath)
	if err != nil {
		return err
	}
	targetPath, err := resolveSingleASMCopyTarget(source.Info().Path, destination)
	if err != nil {
		return err
	}
	_, err = s.copyASMFile(source.Info().Path, source, targetPath, stdout)
	return err
}

func copyCommandDestination(args []string) (string, error) {
	if len(args) > 0 && strings.EqualFold(trimCommandPath(args[0]), "to") {
		args = args[1:]
	}
	if len(args) == 0 {
		return "", fmt.Errorf("copy destination is required")
	}
	destination := trimCommandPath(strings.Join(args, " "))
	if destination == "" {
		return "", fmt.Errorf("copy destination is required")
	}
	return destination, nil
}

func trimCommandPath(value string) string {
	value = strings.TrimSpace(value)
	if len(value) < 2 {
		return value
	}
	first, last := value[0], value[len(value)-1]
	if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
		return strings.TrimSpace(value[1 : len(value)-1])
	}
	return value
}

func resolveSingleASMCopyTarget(sourcePath string, destination string) (string, error) {
	destination = strings.TrimSpace(destination)
	if destination == "" {
		return "", fmt.Errorf("copy destination is required")
	}
	base := pathpkg.Base(strings.ReplaceAll(sourcePath, "\\", "/"))
	if base == "" || base == "." || base == "/" {
		return "", fmt.Errorf("cannot derive a filename from ASM path %s", sourcePath)
	}
	directoryHint := strings.HasSuffix(destination, "/") || strings.HasSuffix(destination, "\\")
	if info, err := os.Stat(destination); err == nil {
		if info.IsDir() {
			return filepath.Join(destination, base), nil
		}
		return destination, nil
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("inspect copy destination %s: %w", destination, err)
	}
	if directoryHint {
		return filepath.Join(destination, base), nil
	}
	return destination, nil
}

func planASMDataFileCopies(sources []dm.OfflineDataSource, destination string) ([]asmFileCopyPlan, error) {
	if len(sources) == 0 {
		return nil, fmt.Errorf("no ASM database files are available to copy")
	}
	destination = strings.TrimSpace(destination)
	if destination == "" {
		return nil, fmt.Errorf("copy destination directory is required")
	}
	absDestination, err := filepath.Abs(filepath.Clean(destination))
	if err != nil {
		return nil, fmt.Errorf("resolve copy destination directory %s: %w", destination, err)
	}
	if info, statErr := os.Stat(absDestination); statErr == nil {
		if !info.IsDir() {
			return nil, fmt.Errorf("datafile copy destination is not a directory: %s", absDestination)
		}
	} else if !os.IsNotExist(statErr) {
		return nil, fmt.Errorf("inspect datafile copy destination %s: %w", absDestination, statErr)
	}

	plans := make([]asmFileCopyPlan, 0, len(sources))
	seenNames := make(map[string]string, len(sources))
	for _, source := range sources {
		if source.Reader == nil {
			return nil, fmt.Errorf("ASM datafile %s has no logical reader", source.Path)
		}
		base := pathpkg.Base(strings.ReplaceAll(source.Path, "\\", "/"))
		if base == "" || base == "." || base == "/" {
			return nil, fmt.Errorf("cannot derive a filename from ASM path %s", source.Path)
		}
		key := strings.ToUpper(base)
		if previous := seenNames[key]; previous != "" {
			return nil, fmt.Errorf("ASM datafiles %s and %s both map to target filename %s; copy them individually with distinct targets", previous, source.Path, base)
		}
		seenNames[key] = source.Path
		targetPath := filepath.Join(absDestination, base)
		if _, statErr := os.Lstat(targetPath); statErr == nil {
			return nil, fmt.Errorf("offline copy target already exists: %s", targetPath)
		} else if !os.IsNotExist(statErr) {
			return nil, fmt.Errorf("inspect offline copy target %s: %w", targetPath, statErr)
		}
		plans = append(plans, asmFileCopyPlan{sourcePath: source.Path, targetPath: targetPath, reader: source.Reader})
	}
	return plans, nil
}

func (s *interactiveSession) copyASMDataFiles(destination string, stdout io.Writer) error {
	if !dm.IsASMPath(s.systemPath) {
		return fmt.Errorf("cp datafile requires an ASM system path such as +DMDATA/.../SYSTEM.DBF")
	}
	if err := s.ensureASMOpen(); err != nil {
		return err
	}
	plans, err := planASMDataFileCopies(s.asmDataSources, destination)
	if err != nil {
		return err
	}
	startedAt := time.Now()
	var totalBytes int64
	for _, plan := range plans {
		result, err := s.copyASMFile(plan.sourcePath, plan.reader, plan.targetPath, stdout)
		if err != nil {
			return err
		}
		totalBytes += result.Bytes
	}
	fmt.Fprintf(stdout, "asm datafiles copied: %d\n", len(plans))
	fmt.Fprintf(stdout, "bytes copied: %d (%.1f MiB)\n", totalBytes, float64(totalBytes)/(1024*1024))
	fmt.Fprintf(stdout, "elapsed: %s\n", time.Since(startedAt).Round(time.Millisecond))
	s.log(fmt.Sprintf("[CP] status=COMPLETED files=%d bytes=%d elapsed=%s", len(plans), totalBytes, time.Since(startedAt).Round(time.Millisecond)))
	return nil
}

func (s *interactiveSession) copyASMFile(sourcePath string, reader dm.SizedReaderAt, targetPath string, stdout io.Writer) (dm.OfflineFileCopyResult, error) {
	startedAt := time.Now()
	fmt.Fprintf(stdout, "copying: %s -> %s (%d bytes)\n", sourcePath, targetPath, reader.Size())
	s.log(fmt.Sprintf("[CP] status=STARTED source=%q target=%q bytes=%d", sourcePath, targetPath, reader.Size()))
	result, err := dm.CopyOfflineFile(reader, targetPath)
	if err != nil {
		s.log(fmt.Sprintf("[CP] status=FAILED source=%q target=%q error=%q", sourcePath, targetPath, err.Error()))
		return dm.OfflineFileCopyResult{}, err
	}
	elapsed := time.Since(startedAt).Round(time.Millisecond)
	fmt.Fprintf(stdout, "copied: %s\n", result.TargetPath)
	fmt.Fprintf(stdout, "bytes: %d (%.1f MiB)\n", result.Bytes, float64(result.Bytes)/(1024*1024))
	fmt.Fprintf(stdout, "sha256: %s\n", result.SHA256)
	fmt.Fprintf(stdout, "elapsed: %s\n", elapsed)
	s.log(fmt.Sprintf("[CP] status=COMPLETED source=%q target=%q bytes=%d sha256=%s elapsed=%s", sourcePath, result.TargetPath, result.Bytes, result.SHA256, elapsed))
	return result, nil
}
