package dm

import (
	"encoding/binary"
	"fmt"
	"os"
	pathpkg "path"
	"path/filepath"
	"sort"
	"strings"
)

func resolveDataFiles(controlPath string, controlDULPath string, dataDir string) ([]dataFileRef, error) {
	var refs []dataFileRef
	tablespaceNames := defaultTablespaceNames()
	mergeControlDULTablespaceNames(tablespaceNames, controlDULPath)
	seenKeys := make(map[dataFileKey]bool)
	addRef := func(key dataFileKey, path string, tablespaceName string) {
		if key.groupID < 4 || path == "" || seenKeys[key] {
			return
		}
		refs = append(refs, dataFileRef{
			key:            key,
			path:           path,
			tablespaceName: tablespaceName,
		})
		seenKeys[key] = true
	}
	if strings.TrimSpace(controlPath) != "" {
		ctl, err := InspectControlFile(controlPath)
		if err != nil {
			return nil, fmt.Errorf("inspect dm.ctl: %w", err)
		}
		for _, entry := range ctl.Entries {
			tablespaceNames[entry.ID] = entry.Name
			if entry.ID < 4 {
				continue
			}
			fileID := int16(0)
			for _, controlPath := range entry.Paths {
				if !strings.EqualFold(pathpkg.Ext(strings.ReplaceAll(controlPath.Value, "\\", "/")), ".DBF") {
					continue
				}
				resolved, ok := resolveDataFilePath(controlPath.Value, dataDir)
				if !ok {
					fileID++
					continue
				}
				addRef(dataFileKey{groupID: entry.ID, fileID: fileID}, resolved, entry.Name)
				fileID++
			}
		}
	}
	if files, ok := readControlDUL(controlDULPath); ok {
		for _, file := range files {
			if file.GroupID < 4 || strings.TrimSpace(file.Path) == "" {
				continue
			}
			if file.Tablespace != "" {
				tablespaceNames[file.GroupID] = file.Tablespace
			}
			resolved, ok := resolveDataFilePath(file.Path, dataDir)
			if !ok {
				continue
			}
			addRef(dataFileKey{groupID: file.GroupID, fileID: file.FileID}, resolved, tablespaceNames[file.GroupID])
		}
	}
	refs = append(refs, scanDataFilesByPageHeader(dataDir, tablespaceNames, seenKeys)...)
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].key.groupID != refs[j].key.groupID {
			return refs[i].key.groupID < refs[j].key.groupID
		}
		return refs[i].key.fileID < refs[j].key.fileID
	})
	return refs, nil
}

func dataFileRefsFromSources(sources []OfflineDataSource) ([]dataFileRef, error) {
	refs := make([]dataFileRef, 0, len(sources))
	seen := make(map[dataFileKey]string)
	for _, source := range sources {
		if source.Reader == nil || source.GroupID < 4 {
			continue
		}
		key := dataFileKey{groupID: source.GroupID, fileID: source.FileID}
		if previous, exists := seen[key]; exists {
			return nil, fmt.Errorf("duplicate data file identity group=%d file=%d: %s and %s", key.groupID, key.fileID, previous, source.Path)
		}
		seen[key] = source.Path
		refs = append(refs, dataFileRef{
			key: key, path: source.Path, tablespaceName: source.Tablespace, reader: source.Reader,
		})
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].key.groupID == refs[j].key.groupID {
			return refs[i].key.fileID < refs[j].key.fileID
		}
		return refs[i].key.groupID < refs[j].key.groupID
	})
	return refs, nil
}

func forEachDataFileRefPage(file dataFileRef, pageSize uint32, visit func(page []byte, pageNo uint32) error) (int, error) {
	if file.reader != nil {
		return forEachSizedReaderPage(file.reader, pageSize, visit)
	}
	return forEachDataFilePage(file.path, pageSize, visit)
}

func scanDataFilesByPageHeader(dataDir string, tablespaceNames map[uint32]string, seenKeys map[dataFileKey]bool) []dataFileRef {
	if dataDir == "" {
		return nil
	}
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return nil
	}
	var refs []dataFileRef
	for _, entry := range entries {
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ".DBF") {
			continue
		}
		path := filepath.Join(dataDir, entry.Name())
		key, ok := dataFileKeyFromPageHeader(path)
		if !ok || key.groupID < 4 || seenKeys[key] {
			continue
		}
		tablespaceName := tablespaceNames[key.groupID]
		if tablespaceName == "" {
			tablespaceName = inferTablespaceNameFromDataFile(path, key.groupID)
			tablespaceNames[key.groupID] = tablespaceName
		}
		refs = append(refs, dataFileRef{
			key:            key,
			path:           path,
			tablespaceName: tablespaceName,
		})
		seenKeys[key] = true
	}
	return refs
}

func dataFileKeyFromPageHeader(path string) (dataFileKey, bool) {
	f, err := os.Open(path)
	if err != nil {
		return dataFileKey{}, false
	}
	defer f.Close()
	var head [8]byte
	if _, err := f.Read(head[:]); err != nil {
		return dataFileKey{}, false
	}
	pageNo := binary.LittleEndian.Uint32(head[4:])
	if pageNo != 0 {
		return dataFileKey{}, false
	}
	fileID := binary.LittleEndian.Uint16(head[2:])
	if fileID > uint16(^uint16(0)>>1) {
		return dataFileKey{}, false
	}
	return dataFileKey{
		groupID: uint32(binary.LittleEndian.Uint16(head[0:])),
		fileID:  int16(fileID),
	}, true
}

// resolveDataFilePath maps a dm.ctl / control.dul path entry to an actual file.
// The offline files the user placed in data_dir are authoritative: a same-named
// file there wins over the recorded (often absolute) path. dm.ctl/control.dul
// therefore act only as a cross-reference for the group/file/tablespace mapping,
// never dragging the read to the live database's original location when
// recovering on the same host. The recorded absolute path is used only as a
// fallback when no matching file exists in data_dir.
func resolveDataFilePath(controlValue string, dataDir string) (string, bool) {
	base := pathpkg.Base(strings.ReplaceAll(controlValue, "\\", "/"))
	if base != "." && base != "/" && base != "" && strings.TrimSpace(dataDir) != "" {
		candidate := filepath.Join(dataDir, base)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, true
		}
	}
	if info, err := os.Stat(controlValue); err == nil && !info.IsDir() {
		return controlValue, true
	}
	return "", false
}
