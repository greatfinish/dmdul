package dm

import (
	"encoding/binary"
	"fmt"
	pathpkg "path"
	"sort"
	"strconv"
	"strings"
)

// RawASMStorage combines one or more offline DMASM disk groups. It keeps the
// existing single-group reader intact while allowing a database whose DBFs are
// distributed across different ASM groups to expose one logical data source.
type RawASMStorage struct {
	groups map[uint16]*RawASMGroup
	byName map[string]*RawASMGroup
}

// OpenRawASMStorage opens all supplied members read-only and groups them by
// their on-disk group id. Members of one group must belong to the same point-in-
// time snapshot.
func OpenRawASMStorage(paths ...string) (*RawASMStorage, error) {
	groupPaths := make(map[uint16][]string)
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		disk, err := openRawASMDisk(path)
		if err != nil {
			return nil, err
		}
		groupID := disk.groupID
		_ = disk.file.Close()
		groupPaths[groupID] = append(groupPaths[groupID], path)
	}
	if len(groupPaths) == 0 {
		return nil, fmt.Errorf("at least one non-empty DMASM disk path is required")
	}
	storage := &RawASMStorage{groups: make(map[uint16]*RawASMGroup), byName: make(map[string]*RawASMGroup)}
	ids := make([]int, 0, len(groupPaths))
	for id := range groupPaths {
		ids = append(ids, int(id))
	}
	sort.Ints(ids)
	for _, rawID := range ids {
		id := uint16(rawID)
		group, err := OpenRawASMGroup(groupPaths[id]...)
		if err != nil {
			storage.Close()
			return nil, err
		}
		if group.name == "" {
			group.name = inferRawASMGroupName(group.Files())
		}
		storage.groups[id] = group
		if group.name != "" {
			key := strings.ToUpper(group.name)
			if previous := storage.byName[key]; previous != nil {
				storage.Close()
				return nil, fmt.Errorf("duplicate DMASM group name %s", group.name)
			}
			storage.byName[key] = group
		}
	}
	return storage, nil
}

func inferRawASMGroupName(files []RawASMFileInfo) string {
	for _, info := range files {
		path := strings.TrimPrefix(strings.ReplaceAll(info.Path, "\\", "/"), "+")
		if cut := strings.IndexByte(path, '/'); cut >= 0 {
			path = path[:cut]
		}
		if path != "" {
			return path
		}
	}
	return ""
}

// Close releases all member handles.
func (s *RawASMStorage) Close() error {
	if s == nil {
		return nil
	}
	var result error
	for _, group := range s.groups {
		if err := group.Close(); err != nil && result == nil {
			result = err
		}
	}
	s.groups = nil
	s.byName = nil
	return result
}

// GroupCount reports the number of opened ASM disk groups.
func (s *RawASMStorage) GroupCount() int {
	if s == nil {
		return 0
	}
	return len(s.groups)
}

// DiskCount reports the total number of opened member disks.
func (s *RawASMStorage) DiskCount() int {
	if s == nil {
		return 0
	}
	count := 0
	for _, group := range s.groups {
		count += len(group.disks)
	}
	return count
}

// Files returns all recovered INODE entries ordered by group and path.
func (s *RawASMStorage) Files() []RawASMFileInfo {
	if s == nil {
		return nil
	}
	var files []RawASMFileInfo
	for _, group := range s.groups {
		for _, info := range group.Files() {
			if info.GroupName == "" {
				info.GroupName = group.name
			}
			files = append(files, info)
		}
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].GroupID == files[j].GroupID {
			return files[i].Path < files[j].Path
		}
		return files[i].GroupID < files[j].GroupID
	})
	return files
}

// Open resolves an ASM path to its owning disk group and opens the logical
// file. If an early layout did not expose a group name, each group is tried.
func (s *RawASMStorage) Open(path string) (*RawASMFile, error) {
	if s == nil {
		return nil, fmt.Errorf("DMASM storage is not open")
	}
	if name := asmGroupNameFromPath(path); name != "" {
		if group := s.byName[strings.ToUpper(name)]; group != nil {
			return group.Open(path)
		}
	}
	for _, group := range s.groups {
		if file, err := group.Open(path); err == nil {
			return file, nil
		}
	}
	return nil, fmt.Errorf("DMASM file not found: %s", path)
}

func asmGroupNameFromPath(path string) string {
	path = strings.TrimSpace(strings.ReplaceAll(path, "\\", "/"))
	if !strings.HasPrefix(path, "+") {
		return ""
	}
	path = strings.TrimPrefix(path, "+")
	if cut := strings.IndexByte(path, '/'); cut >= 0 {
		path = path[:cut]
	}
	return path
}

func asmDatabaseDirSuffix(path string) string {
	path = normalizeASMPath(path)
	if cut := strings.IndexByte(path, '/'); cut >= 0 {
		path = path[cut+1:]
	}
	return pathpkg.Dir(path)
}

// DataFiles discovers DBFs for the same database directory across every
// configured ASM group, then reads each DBF page-zero identity.
func (s *RawASMStorage) DataFiles(systemPath string) ([]OfflineDataSource, error) {
	dir := asmDatabaseDirSuffix(systemPath)
	var sources []OfflineDataSource
	seen := make(map[dataFileKey]string)
	for _, group := range s.groups {
		for _, info := range group.Files() {
			if info.IsDir || !strings.EqualFold(pathpkg.Ext(info.Path), ".DBF") || asmDatabaseDirSuffix(info.Path) != dir {
				continue
			}
			file, err := group.Open(info.Path)
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
				Path:       info.Path, Reader: file,
			})
		}
	}
	sort.Slice(sources, func(i, j int) bool {
		if sources[i].GroupID == sources[j].GroupID {
			return sources[i].FileID < sources[j].FileID
		}
		return sources[i].GroupID < sources[j].GroupID
	})
	if len(sources) == 0 {
		return nil, fmt.Errorf("no DBF files found for ASM database directory %s", dir)
	}
	return sources, nil
}
