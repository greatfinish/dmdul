package dm

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const (
	ASMDatabaseCatalogFileName = "asm_databases.tsv"
	ASMDataFileCatalogFileName = "asm_datafiles.tsv"
)

// ASMCatalogDatabase is one database candidate recovered from the configured
// offline DMASM members. SystemPath is the stable key shared with its data
// file rows.
type ASMCatalogDatabase struct {
	CandidateNo      int
	Selected         bool
	DatabaseName     string
	DatabaseNameSrc  string
	SystemPath       string
	ControlPath      string
	ASMMembers       string
	Charset          string
	CharsetFlag      uint8
	HasCharsetFlag   bool
	PageSize         uint32
	PageCount        uint32
	ExtentSize       uint32
	CaseSensitive    bool
	HasCaseSensitive bool
	DataFileCount    int
	Status           string
	Error            string
}

// ASMCatalogDataFile records one logical DBF belonging to a database candidate.
type ASMCatalogDataFile struct {
	CandidateNo  int
	DatabaseName string
	SystemPath   string
	GroupID      uint32
	FileID       int16
	Tablespace   string
	Pages        int64
	SizeBytes    int64
	Status       string
	Path         string
}

type ASMCatalog struct {
	Databases []ASMCatalogDatabase
	DataFiles []ASMCatalogDataFile
}

type ASMCatalogFilesResult struct {
	Dir           string
	DatabasesPath string
	DataFilesPath string
	DatabaseCount int
	DataFileCount int
}

// WriteASMCatalogFiles persists the complete multi-database discovery result
// beside the normal dictionary TSV files. Both files are staged before either
// active file is replaced.
func WriteASMCatalogFiles(dir string, catalog *ASMCatalog) (*ASMCatalogFilesResult, error) {
	if catalog == nil {
		return nil, fmt.Errorf("ASM catalog is nil")
	}
	dir = filepath.Clean(strings.TrimSpace(dir))
	if dir == "" || dir == "." {
		return nil, fmt.Errorf("ASM catalog directory is empty or unsafe: %q", dir)
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	result := asmCatalogFilesResultForDir(dir)

	databases := append([]ASMCatalogDatabase(nil), catalog.Databases...)
	dataFiles := append([]ASMCatalogDataFile(nil), catalog.DataFiles...)
	sort.SliceStable(databases, func(i, j int) bool {
		if databases[i].CandidateNo != databases[j].CandidateNo {
			return databases[i].CandidateNo < databases[j].CandidateNo
		}
		return databases[i].SystemPath < databases[j].SystemPath
	})
	sort.SliceStable(dataFiles, func(i, j int) bool {
		if dataFiles[i].CandidateNo != dataFiles[j].CandidateNo {
			return dataFiles[i].CandidateNo < dataFiles[j].CandidateNo
		}
		if dataFiles[i].GroupID != dataFiles[j].GroupID {
			return dataFiles[i].GroupID < dataFiles[j].GroupID
		}
		if dataFiles[i].FileID != dataFiles[j].FileID {
			return dataFiles[i].FileID < dataFiles[j].FileID
		}
		return dataFiles[i].Path < dataFiles[j].Path
	})

	databaseRows := make([][]string, 0, len(databases))
	for _, database := range databases {
		unicodeFlag := ""
		if database.HasCharsetFlag {
			unicodeFlag = strconv.FormatUint(uint64(database.CharsetFlag), 10)
		}
		caseSensitive := ""
		if database.HasCaseSensitive {
			caseSensitive = "0"
			if database.CaseSensitive {
				caseSensitive = "1"
			}
		}
		databaseRows = append(databaseRows, []string{
			strconv.Itoa(database.CandidateNo), formatBoolField(database.Selected),
			database.DatabaseName, database.DatabaseNameSrc, database.SystemPath,
			database.ControlPath, database.ASMMembers,
			database.Charset, unicodeFlag,
			strconv.FormatUint(uint64(database.PageSize), 10),
			strconv.FormatUint(uint64(database.PageCount), 10),
			strconv.FormatUint(uint64(database.ExtentSize), 10),
			caseSensitive, strconv.Itoa(database.DataFileCount),
			database.Status, database.Error,
		})
	}
	dataFileRows := make([][]string, 0, len(dataFiles))
	for _, file := range dataFiles {
		dataFileRows = append(dataFileRows, []string{
			strconv.Itoa(file.CandidateNo), file.DatabaseName, file.SystemPath,
			strconv.FormatUint(uint64(file.GroupID), 10), strconv.FormatInt(int64(file.FileID), 10),
			file.Tablespace, strconv.FormatInt(file.Pages, 10), strconv.FormatInt(file.SizeBytes, 10),
			file.Status, file.Path,
		})
	}

	stagedDatabases, err := stageASMCatalogTSV(dir, ASMDatabaseCatalogFileName,
		[]string{"candidate_no", "selected", "database_name", "database_name_source", "system_path", "control_path", "asm_members", "charset", "unicode_flag", "page_size", "page_count", "extent_size", "case_sensitive", "data_file_count", "status", "error"},
		databaseRows)
	if err != nil {
		return nil, err
	}
	defer os.Remove(stagedDatabases)
	stagedDataFiles, err := stageASMCatalogTSV(dir, ASMDataFileCatalogFileName,
		[]string{"candidate_no", "database_name", "system_path", "group_id", "file_id", "tablespace", "pages", "size_bytes", "status", "path"},
		dataFileRows)
	if err != nil {
		return nil, err
	}
	defer os.Remove(stagedDataFiles)
	if err := replaceASMCatalogFiles([]asmCatalogReplacement{
		{staged: stagedDatabases, target: result.DatabasesPath},
		{staged: stagedDataFiles, target: result.DataFilesPath},
	}); err != nil {
		return nil, err
	}
	result.DatabaseCount = len(databases)
	result.DataFileCount = len(dataFiles)
	return result, nil
}

func LoadASMCatalogFiles(dir string) (*ASMCatalog, *ASMCatalogFilesResult, error) {
	result := asmCatalogFilesResultForDir(dir)
	databaseRecords, err := readTSV(result.DatabasesPath)
	if err != nil {
		return nil, nil, err
	}
	dataFileRecords, err := readTSV(result.DataFilesPath)
	if err != nil {
		return nil, nil, err
	}
	catalog := &ASMCatalog{}
	for index, record := range databaseRecords {
		if index == 0 {
			continue
		}
		if len(record) < 16 {
			return nil, nil, fmt.Errorf("invalid %s row %d: got %d fields", ASMDatabaseCatalogFileName, index+1, len(record))
		}
		database := ASMCatalogDatabase{
			CandidateNo:     parseIntField(record[0]),
			Selected:        parseBoolField(record[1]),
			DatabaseName:    record[2],
			DatabaseNameSrc: record[3],
			SystemPath:      record[4],
			ControlPath:     record[5],
			ASMMembers:      record[6],
			Charset:         record[7],
			PageSize:        parseUint32Field(record[9]),
			PageCount:       parseUint32Field(record[10]),
			ExtentSize:      parseUint32Field(record[11]),
			DataFileCount:   parseIntField(record[13]),
			Status:          record[14],
			Error:           record[15],
		}
		if strings.TrimSpace(record[8]) != "" {
			value, parseErr := strconv.ParseUint(strings.TrimSpace(record[8]), 10, 8)
			if parseErr != nil {
				return nil, nil, fmt.Errorf("invalid unicode_flag in %s row %d: %w", ASMDatabaseCatalogFileName, index+1, parseErr)
			}
			database.CharsetFlag = uint8(value)
			database.HasCharsetFlag = true
		}
		if strings.TrimSpace(record[12]) != "" {
			database.CaseSensitive = record[12] == "1"
			database.HasCaseSensitive = true
		}
		catalog.Databases = append(catalog.Databases, database)
	}
	for index, record := range dataFileRecords {
		if index == 0 {
			continue
		}
		if len(record) < 10 {
			return nil, nil, fmt.Errorf("invalid %s row %d: got %d fields", ASMDataFileCatalogFileName, index+1, len(record))
		}
		fileID, parseErr := strconv.ParseInt(strings.TrimSpace(record[4]), 10, 16)
		if parseErr != nil {
			return nil, nil, fmt.Errorf("invalid file_id in %s row %d: %w", ASMDataFileCatalogFileName, index+1, parseErr)
		}
		catalog.DataFiles = append(catalog.DataFiles, ASMCatalogDataFile{
			CandidateNo: parseIntField(record[0]), DatabaseName: record[1], SystemPath: record[2],
			GroupID: parseUint32Field(record[3]), FileID: int16(fileID), Tablespace: record[5],
			Pages: parseInt64Field(record[6]), SizeBytes: parseInt64Field(record[7]), Status: record[8], Path: record[9],
		})
	}
	result.DatabaseCount = len(catalog.Databases)
	result.DataFileCount = len(catalog.DataFiles)
	return catalog, result, nil
}

func asmCatalogFilesResultForDir(dir string) *ASMCatalogFilesResult {
	return &ASMCatalogFilesResult{
		Dir:           dir,
		DatabasesPath: filepath.Join(dir, ASMDatabaseCatalogFileName),
		DataFilesPath: filepath.Join(dir, ASMDataFileCatalogFileName),
	}
}

func stageASMCatalogTSV(dir string, name string, header []string, rows [][]string) (string, error) {
	file, err := os.CreateTemp(dir, "."+name+"-")
	if err != nil {
		return "", err
	}
	path := file.Name()
	if err := file.Close(); err != nil {
		os.Remove(path)
		return "", err
	}
	if err := writeTSV(path, header, rows); err != nil {
		os.Remove(path)
		return "", err
	}
	if syncFile, err := os.OpenFile(path, os.O_RDWR, 0); err == nil {
		err = syncFile.Sync()
		_ = syncFile.Close()
		if err != nil {
			os.Remove(path)
			return "", err
		}
	}
	return path, nil
}

type asmCatalogReplacement struct {
	staged    string
	target    string
	backup    string
	activated bool
}

func replaceASMCatalogFiles(replacements []asmCatalogReplacement) error {
	rollback := func() {
		for index := len(replacements) - 1; index >= 0; index-- {
			replacement := &replacements[index]
			if replacement.activated {
				_ = os.Remove(replacement.target)
			}
			if replacement.backup != "" {
				_ = os.Rename(replacement.backup, replacement.target)
			}
		}
	}
	for index := range replacements {
		replacement := &replacements[index]
		if _, err := os.Stat(replacement.target); os.IsNotExist(err) {
			continue
		} else if err != nil {
			rollback()
			return fmt.Errorf("inspect ASM catalog %s: %w", replacement.target, err)
		}
		backup, err := os.CreateTemp(filepath.Dir(replacement.target), "."+filepath.Base(replacement.target)+".previous-")
		if err != nil {
			rollback()
			return err
		}
		replacement.backup = backup.Name()
		if err := backup.Close(); err != nil {
			_ = os.Remove(replacement.backup)
			replacement.backup = ""
			rollback()
			return err
		}
		if err := os.Remove(replacement.backup); err != nil {
			replacement.backup = ""
			rollback()
			return err
		}
		if err := os.Rename(replacement.target, replacement.backup); err != nil {
			_ = os.Remove(replacement.backup)
			replacement.backup = ""
			rollback()
			return fmt.Errorf("stage previous ASM catalog %s: %w", replacement.target, err)
		}
	}
	for index := range replacements {
		replacement := &replacements[index]
		if err := os.Rename(replacement.staged, replacement.target); err != nil {
			rollback()
			return fmt.Errorf("activate ASM catalog %s: %w", replacement.target, err)
		}
		replacement.activated = true
	}
	for index := range replacements {
		if replacements[index].backup != "" {
			_ = os.Remove(replacements[index].backup)
		}
	}
	return nil
}
