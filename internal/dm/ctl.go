package dm

import (
	"encoding/binary"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
)

var controlNamePattern = regexp.MustCompile(`^[A-Z][A-Z0-9_$#]{1,127}$`)

type ControlInfo struct {
	Path         string
	Size         int64
	BlockSize    uint32
	DatabaseName string
	Entries      []ControlEntry
}

type ControlEntry struct {
	ID         uint32
	Name       string
	NameOffset uint64
	Paths      []ControlPath
}

type ControlPath struct {
	Offset uint64
	Value  string
}

func (e ControlEntry) PathSummary() string {
	if len(e.Paths) == 0 {
		return ""
	}
	parts := make([]string, 0, len(e.Paths))
	for _, p := range e.Paths {
		parts = append(parts, fmt.Sprintf("0x%X:%s", p.Offset, p.Value))
	}
	return strings.Join(parts, "; ")
}

func InspectControlFile(path string) (*ControlInfo, error) {
	buf, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read dm.ctl: %w", err)
	}
	if len(buf) < 8 {
		return nil, fmt.Errorf("dm.ctl is too small")
	}

	info := &ControlInfo{
		Path:         path,
		Size:         int64(len(buf)),
		BlockSize:    binary.LittleEndian.Uint32(buf[0:]),
		DatabaseName: readCString(buf[4:min(len(buf), 0x84)]),
	}

	stringsInFile := printableStrings(buf, 2)
	nameEntries := controlNameEntries(buf, stringsInFile)
	pathEntries := controlPathEntries(stringsInFile)

	for i, name := range nameEntries {
		nextNameOffset := uint64(len(buf))
		if i+1 < len(nameEntries) {
			nextNameOffset = nameEntries[i+1].NameOffset
		}

		entry := ControlEntry{
			ID:         name.ID,
			Name:       name.Name,
			NameOffset: name.NameOffset,
		}
		for _, path := range pathEntries {
			if path.Offset > name.NameOffset && path.Offset < nextNameOffset {
				entry.Paths = append(entry.Paths, path)
			}
		}
		info.Entries = append(info.Entries, entry)
	}

	return info, nil
}

type printableString struct {
	Offset uint64
	Value  string
}

type controlNameEntry struct {
	ID         uint32
	Name       string
	NameOffset uint64
}

func controlNameEntries(buf []byte, stringsInFile []printableString) []controlNameEntry {
	var result []controlNameEntry
	for _, item := range stringsInFile {
		if strings.ContainsAny(item.Value, `/\.`) {
			continue
		}
		if item.Value == "DAMENG" || item.Value == "NORMAL" {
			continue
		}
		if !controlNamePattern.MatchString(item.Value) {
			continue
		}
		if item.Offset < 6 {
			continue
		}
		if buf[item.Offset-2] != 0 || buf[item.Offset-1] != 0 {
			continue
		}
		idOffset := item.Offset - 6
		if idOffset+4 > uint64(len(buf)) {
			continue
		}
		id := binary.LittleEndian.Uint32(buf[idOffset:])
		if id > 65535 {
			continue
		}
		result = append(result, controlNameEntry{
			ID:         id,
			Name:       item.Value,
			NameOffset: item.Offset,
		})
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].NameOffset < result[j].NameOffset
	})
	return result
}

func controlPathEntries(stringsInFile []printableString) []ControlPath {
	var result []ControlPath
	for _, item := range stringsInFile {
		if !strings.ContainsAny(item.Value, `/\`) {
			continue
		}
		result = append(result, ControlPath(item))
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Offset < result[j].Offset
	})
	return result
}

func printableStrings(buf []byte, minLen int) []printableString {
	var result []printableString
	start := -1
	for i, b := range buf {
		if b >= 0x20 && b <= 0x7E {
			if start < 0 {
				start = i
			}
			continue
		}
		if start >= 0 && i-start >= minLen {
			result = append(result, printableString{
				Offset: uint64(start),
				Value:  string(buf[start:i]),
			})
		}
		start = -1
	}
	if start >= 0 && len(buf)-start >= minLen {
		result = append(result, printableString{
			Offset: uint64(start),
			Value:  string(buf[start:]),
		})
	}
	return result
}

func readCString(buf []byte) string {
	for i, b := range buf {
		if b == 0 {
			return string(buf[:i])
		}
	}
	return string(buf)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
