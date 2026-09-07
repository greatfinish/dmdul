package dm

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	DefaultDatabaseName     = "DAMENG"
	DefaultInstanceName     = "DMSERVER"
	DefaultExtentSize       = uint32(16)
	DefaultPageSize         = uint32(8192)
	DefaultUnicodeFlag      = uint8(0)
	DefaultCaseSensitive    = true
	databaseParameterSource = "DM default"
)

type DatabaseMetadata struct {
	SystemPath          string
	ControlPath         string
	IniPath             string
	DatabaseName        string
	DatabaseNameSrc     string
	InstanceName        string
	InstanceNameSrc     string
	Port                string
	PortSrc             string
	ExtentSize          uint32
	ExtentSizeSource    string
	PageSize            uint32
	PageSizeSource      string
	PageCount           uint32
	PageCountSource     string
	Charset             string
	CharsetSource       string
	CharsetFlag         uint8
	HasCharsetFlag      bool
	CaseSensitive       bool
	CaseSensitiveSource string
	HasCaseSensitive    bool
	IniExtentSize       string
	IniPageSize         string
	IniCharset          string
}

func DefaultControlPathForSystem(systemPath string) string {
	return filepath.Join(systemDir(systemPath), "dm.ctl")
}

func DefaultIniPathForSystem(systemPath string) string {
	return filepath.Join(systemDir(systemPath), "dm.ini")
}

func DefaultDatabaseMetadata() DatabaseMetadata {
	return DatabaseMetadata{
		DatabaseName:        DefaultDatabaseName,
		DatabaseNameSrc:     databaseParameterSource,
		InstanceName:        DefaultInstanceName,
		InstanceNameSrc:     databaseParameterSource,
		ExtentSize:          DefaultExtentSize,
		ExtentSizeSource:    databaseParameterSource,
		PageSize:            DefaultPageSize,
		PageSizeSource:      databaseParameterSource,
		PageCountSource:     "unknown",
		Charset:             "GB18030 (UNICODE_FLAG=0)",
		CharsetSource:       databaseParameterSource,
		CharsetFlag:         DefaultUnicodeFlag,
		HasCharsetFlag:      true,
		CaseSensitive:       DefaultCaseSensitive,
		CaseSensitiveSource: databaseParameterSource,
		HasCaseSensitive:    true,
	}
}

func InspectDatabaseMetadata(systemPath string, controlPath string, iniPath string, charsetPreference string) DatabaseMetadata {
	meta := DefaultDatabaseMetadata()
	meta.SystemPath = systemPath
	if preferred := strings.TrimSpace(charsetPreference); preferred != "" && !strings.EqualFold(preferred, "auto") {
		meta.Charset = preferred
		meta.CharsetSource = "decoder setting"
		meta.HasCharsetFlag = false
	}

	if controlPath == "" {
		defaultControlPath := DefaultControlPathForSystem(systemPath)
		if info, err := os.Stat(defaultControlPath); err == nil && !info.IsDir() {
			controlPath = defaultControlPath
		}
	}
	meta.ControlPath = controlPath
	if iniPath == "" {
		iniPath = DefaultIniPathForSystem(systemPath)
	}
	meta.IniPath = iniPath

	if header, size, err := readSystemHeader(systemPath); err == nil {
		if extentSize, source := detectSystemExtentSize(header); extentSize != 0 {
			meta.ExtentSize, meta.ExtentSizeSource = extentSize, source
		}
		meta.PageSize, meta.PageSizeSource = detectSystemPageSize(header, size)
		if meta.PageSize == 0 {
			meta.PageSize, meta.PageSizeSource, _ = probeFilePageSize(systemPath)
		}
		meta.PageCount, meta.PageCountSource = detectSystemPageCount(header, size, meta.PageSize)
		if charset, ok := detectSystemCharsetFromFile(systemPath, meta.PageSize); ok {
			meta.Charset = charset.DisplayName
			meta.CharsetSource = charset.Source
			meta.CharsetFlag = charset.Flag
			meta.HasCharsetFlag = true
		}
		if caseSensitive, ok := detectSystemCaseSensitiveFromFile(systemPath, meta.PageSize); ok {
			meta.CaseSensitive = caseSensitive
			meta.CaseSensitiveSource = "SYSTEM.DBF page 4 + 0x2C"
			meta.HasCaseSensitive = true
		}
		if instanceName, source, ok := detectSystemInstanceNameFromFile(systemPath, meta.PageSize); ok {
			meta.InstanceName = instanceName
			meta.InstanceNameSrc = source
		}
	}

	if controlPath != "" {
		if ctl, err := InspectControlFile(controlPath); err == nil {
			if strings.TrimSpace(ctl.DatabaseName) != "" {
				meta.DatabaseName = ctl.DatabaseName
				meta.DatabaseNameSrc = "dm.ctl"
			}
		}
	}

	if ini, ok := loadDMIni(iniPath); ok {
		if instanceName := strings.TrimSpace(ini["INSTANCE_NAME"]); instanceName != "" && meta.InstanceNameSrc == databaseParameterSource {
			meta.InstanceName = instanceName
			meta.InstanceNameSrc = "dm.ini"
		}
		meta.Port = ini["PORT_NUM"]
		if meta.Port != "" {
			meta.PortSrc = "dm.ini"
		}
		meta.IniExtentSize = firstIniValue(ini, "EXTENT_SIZE", "EXTENT_SIZE_IN_PAGE")
		meta.IniPageSize = ini["PAGE_SIZE"]
		meta.IniCharset = firstIniValue(ini, "CHARSET", "UNICODE_FLAG")
		if meta.IniCharset != "" && !meta.HasCharsetFlag {
			meta.Charset = meta.IniCharset
			meta.CharsetSource = "dm.ini"
		}
	}

	return meta
}

// InspectDatabaseHeaderMetadataFromReader reads database geometry and
// persistent flags from a logical SYSTEM.DBF without scanning its dictionary
// pages. It is intended for lightweight discovery, including listing multiple
// databases found in offline DMASM storage.
func InspectDatabaseHeaderMetadataFromReader(systemPath string, reader io.ReaderAt, size int64, charsetPreference string) (DatabaseMetadata, error) {
	meta := DefaultDatabaseMetadata()
	meta.SystemPath = systemPath
	if preferred := strings.TrimSpace(charsetPreference); preferred != "" && !strings.EqualFold(preferred, "auto") {
		meta.Charset = preferred
		meta.CharsetSource = "decoder setting"
		meta.HasCharsetFlag = false
	}
	header, err := readSystemHeaderFromReader(reader, size)
	if err != nil {
		return meta, err
	}
	if extentSize, source := detectSystemExtentSize(header); extentSize != 0 {
		meta.ExtentSize, meta.ExtentSizeSource = extentSize, source
	}
	// Lightweight ASM discovery reads only the header and control flags.
	// Opening the dictionary stream performs the full conflict-aware probe.
	meta.PageSize, meta.PageSizeSource = detectSystemPageSize(header, size)
	if meta.PageSize == 0 {
		meta.PageSize, meta.PageSizeSource, err = detectPageSizeFromReader(reader, size, header)
		if err != nil {
			return meta, err
		}
	}
	meta.PageCount, meta.PageCountSource = detectSystemPageCount(header, size, meta.PageSize)
	if charset, ok := detectSystemCharsetFromReader(reader, meta.PageSize); ok {
		meta.Charset = charset.DisplayName
		meta.CharsetSource = charset.Source
		meta.CharsetFlag = charset.Flag
		meta.HasCharsetFlag = true
	}
	if caseSensitive, ok := detectSystemCaseSensitiveFromReader(reader, meta.PageSize); ok {
		meta.CaseSensitive = caseSensitive
		meta.CaseSensitiveSource = "SYSTEM.DBF page 4 + 0x2C"
		meta.HasCaseSensitive = true
	}
	return meta, nil
}

// InspectDatabaseMetadataFromReader reads database geometry, persistent flags,
// and the latest instance identity from a logical SYSTEM.DBF source such as a
// file inside offline DMASM.
func InspectDatabaseMetadataFromReader(systemPath string, reader io.ReaderAt, size int64, charsetPreference string) DatabaseMetadata {
	meta, _ := InspectDatabaseHeaderMetadataFromReader(systemPath, reader, size, charsetPreference)
	if stream, streamErr := openSystemPageStreamReader(systemPath, reader, size); streamErr == nil {
		if instanceName, source, ok := detectSystemInstanceNameFromStream(stream); ok {
			meta.InstanceName = instanceName
			meta.InstanceNameSrc = source
		}
	}
	return meta
}

func (m DatabaseMetadata) ExtentComparison() string {
	return compareIniUint(m.IniExtentSize, uint64(m.ExtentSize), "not set")
}

func (m DatabaseMetadata) PageSizeComparison() string {
	return compareIniUint(m.IniPageSize, uint64(m.PageSize), "not set")
}

func (m DatabaseMetadata) CharsetComparison() string {
	if strings.TrimSpace(m.IniCharset) == "" {
		return "not set"
	}
	if m.HasCharsetFlag {
		if charsetIniMatchesFlag(m.IniCharset, m.CharsetFlag) {
			return "match"
		}
		return "mismatch"
	}
	if strings.EqualFold(normalizeCharsetToken(m.IniCharset), normalizeCharsetToken(m.Charset)) {
		return "match"
	}
	return "configured"
}

type systemCharset struct {
	DisplayName string
	DecoderName string
	Flag        uint8
	Source      string
}

func detectSystemCaseSensitiveFromFile(path string, pageSize uint32) (bool, bool) {
	if pageSize == 0 {
		return false, false
	}
	file, err := os.Open(path)
	if err != nil {
		return false, false
	}
	defer file.Close()

	return detectSystemCaseSensitiveFromReader(file, pageSize)
}

func detectSystemCaseSensitiveFromReader(reader interface {
	ReadAt([]byte, int64) (int, error)
}, pageSize uint32) (bool, bool) {
	if reader == nil || pageSize == 0 {
		return false, false
	}
	offset := int64(pageSize)*systemControlPage4No + systemCaseSensitiveFlagOffset
	buf := []byte{0}
	if _, err := reader.ReadAt(buf, offset); err != nil {
		return false, false
	}
	return systemCaseSensitiveFromFlag(buf[0])
}

func detectSystemCaseSensitiveFromData(data []byte, pageSize uint32) (bool, bool) {
	if pageSize == 0 {
		return false, false
	}
	offset := int(pageSize)*systemControlPage4No + systemCaseSensitiveFlagOffset
	if offset < 0 || offset >= len(data) {
		return false, false
	}
	return systemCaseSensitiveFromFlag(data[offset])
}

func systemCaseSensitiveFromFlag(flag byte) (bool, bool) {
	switch flag {
	case 0:
		return false, true
	case 1:
		return true, true
	default:
		return false, false
	}
}

func detectSystemCharsetFromFile(path string, pageSize uint32) (systemCharset, bool) {
	if pageSize == 0 {
		return systemCharset{}, false
	}
	file, err := os.Open(path)
	if err != nil {
		return systemCharset{}, false
	}
	defer file.Close()

	return detectSystemCharsetFromReader(file, pageSize)
}

func detectSystemCharsetFromReader(reader interface {
	ReadAt([]byte, int64) (int, error)
}, pageSize uint32) (systemCharset, bool) {
	if reader == nil || pageSize == 0 {
		return systemCharset{}, false
	}
	offset := int64(pageSize)*systemControlPage4No + systemUnicodeFlagOffset
	buf := []byte{0}
	if _, err := reader.ReadAt(buf, offset); err != nil {
		return systemCharset{}, false
	}
	return systemCharsetFromFlag(buf[0])
}

func detectSystemCharsetFromData(data []byte, pageSize uint32) (systemCharset, bool) {
	if pageSize == 0 {
		return systemCharset{}, false
	}
	offset := int(pageSize)*systemControlPage4No + systemUnicodeFlagOffset
	if offset < 0 || offset >= len(data) {
		return systemCharset{}, false
	}
	return systemCharsetFromFlag(data[offset])
}

func systemCharsetFromFlag(flag byte) (systemCharset, bool) {
	charset := systemCharset{
		Flag:   flag,
		Source: "SYSTEM.DBF page 4 + 0x2D",
	}
	switch flag {
	case 0:
		charset.DisplayName = "GB18030 (UNICODE_FLAG=0)"
		charset.DecoderName = "gb18030"
	case 1:
		charset.DisplayName = "UTF-8 (UNICODE_FLAG=1)"
		charset.DecoderName = "utf-8"
	case 2:
		charset.DisplayName = "EUC-KR (UNICODE_FLAG=2)"
		charset.DecoderName = "euc-kr"
	default:
		charset.DisplayName = fmt.Sprintf("unknown (UNICODE_FLAG=%d)", flag)
	}
	return charset, true
}

func readSystemHeader(path string) ([]byte, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer file.Close()

	stat, err := file.Stat()
	if err != nil {
		return nil, 0, err
	}
	header, err := readSystemHeaderFromReader(file, stat.Size())
	return header, stat.Size(), err
}

func readSystemHeaderFromReader(reader interface {
	ReadAt([]byte, int64) (int, error)
}, size int64) ([]byte, error) {
	if reader == nil {
		return nil, fmt.Errorf("SYSTEM.DBF reader is nil")
	}
	if size < systemHeaderReadSize {
		return nil, fmt.Errorf("SYSTEM.DBF header is too small")
	}
	header := make([]byte, systemHeaderReadSize)
	n, err := reader.ReadAt(header, 0)
	if err != nil && err != io.EOF {
		return nil, err
	}
	if n < systemHeaderReadSize {
		return nil, fmt.Errorf("SYSTEM.DBF header is too small")
	}
	return header, nil
}

func loadDMIni(path string) (map[string]string, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	result := make(map[string]string)
	for _, line := range strings.Split(string(raw), "\n") {
		if idx := strings.IndexByte(line, '#'); idx >= 0 {
			line = line[:idx]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.ToUpper(strings.TrimSpace(key))
		value = strings.TrimSpace(value)
		if key != "" {
			result[key] = value
		}
	}
	return result, true
}

func firstIniValue(values map[string]string, keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(values[strings.ToUpper(key)]); value != "" {
			return value
		}
	}
	return ""
}

func compareIniUint(iniValue string, systemValue uint64, missing string) string {
	iniValue = strings.TrimSpace(iniValue)
	if iniValue == "" {
		return missing
	}
	value, err := strconv.ParseUint(iniValue, 10, 64)
	if err != nil {
		return "configured"
	}
	if value == systemValue {
		return "match"
	}
	return "mismatch"
}

func charsetIniMatchesFlag(iniValue string, flag uint8) bool {
	switch normalizeCharsetToken(iniValue) {
	case "0", "GB18030":
		return flag == 0
	case "1", "UTF-8":
		return flag == 1
	case "2", "EUC-KR":
		return flag == 2
	default:
		return false
	}
}

func normalizeCharsetToken(value string) string {
	value = strings.ToUpper(strings.TrimSpace(value))
	value = strings.TrimPrefix(value, "CHARSET=")
	value = strings.TrimPrefix(value, "UNICODE_FLAG=")
	value = strings.TrimSpace(value)
	value = strings.ReplaceAll(value, "_", "-")
	switch value {
	case "UTF8":
		return "UTF-8"
	case "EUCKR", "KR":
		return "EUC-KR"
	default:
		return value
	}
}

func systemDir(systemPath string) string {
	dir := filepath.Dir(systemPath)
	if dir == "." || dir == "" {
		return "."
	}
	return dir
}

func defaultIfEmpty(value string, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
