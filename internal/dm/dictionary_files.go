package dm

import (
	"encoding/csv"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const DefaultDictionaryDirName = "dmdul_dict"

type DictionaryFilesResult struct {
	Dir                  string
	MetaPath             string
	UsersPath            string
	SchemasPath          string
	TablesPath           string
	ColumnsPath          string
	ViewsPath            string
	SequencesPath        string
	RoutinesPath         string
	TriggersPath         string
	SynonymsPath         string
	TabPrivilegesPath    string
	SystemPrivilegesPath string
	PartitionsPath       string
	PartitionKeysPath    string
	UserCount            int
	SchemaCount          int
	TableCount           int
	ColumnCount          int
	ViewCount            int
	SequenceCount        int
	RoutineCount         int
	TriggerCount         int
	SynonymCount         int
	TabPrivilegeCount    int
	SystemPrivilegeCount int
	PartitionCount       int
	PartitionKeyCount    int
}

func WriteDictionaryFiles(dir string, dict *DictionaryInfo) (*DictionaryFilesResult, error) {
	if dict == nil {
		return nil, fmt.Errorf("dictionary is nil")
	}
	if strings.TrimSpace(dir) == "" {
		return nil, fmt.Errorf("dictionary directory is empty")
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	result := dictionaryFilesResultForDir(dir)
	schemas := dictionarySchemasForPersistence(dict)
	if err := writeDictionaryMeta(result.MetaPath, dict, len(schemas)); err != nil {
		return nil, err
	}
	if err := writeDictionaryUsers(result.UsersPath, dict.Users); err != nil {
		return nil, err
	}
	if err := writeDictionarySchemas(result.SchemasPath, schemas); err != nil {
		return nil, err
	}
	if err := writeDictionaryTables(result.TablesPath, dict.Tables); err != nil {
		return nil, err
	}
	if err := writeDictionaryColumns(result.ColumnsPath, dict.Columns); err != nil {
		return nil, err
	}
	if err := writeDictionaryViews(result.ViewsPath, dict.Views); err != nil {
		return nil, err
	}
	if err := writeDictionarySequences(result.SequencesPath, dict.Sequences); err != nil {
		return nil, err
	}
	if err := writeDictionaryRoutines(result.RoutinesPath, dict.Routines); err != nil {
		return nil, err
	}
	if err := writeDictionaryTriggers(result.TriggersPath, dict.Triggers); err != nil {
		return nil, err
	}
	if err := writeDictionarySynonyms(result.SynonymsPath, dict.Synonyms); err != nil {
		return nil, err
	}
	if err := writeDictionaryTabPrivileges(result.TabPrivilegesPath, dict.TabPrivileges); err != nil {
		return nil, err
	}
	if err := writeDictionarySystemPrivileges(result.SystemPrivilegesPath, dict.SystemPrivileges); err != nil {
		return nil, err
	}
	if err := writeDictionaryPartitions(result.PartitionsPath, dict.Partitions); err != nil {
		return nil, err
	}
	if err := writeDictionaryPartitionKeys(result.PartitionKeysPath, dict.PartitionKeys); err != nil {
		return nil, err
	}
	result.UserCount = len(dict.Users)
	result.SchemaCount = len(schemas)
	result.TableCount = len(dict.Tables)
	result.ColumnCount = len(dict.Columns)
	result.ViewCount = len(dict.Views)
	result.SequenceCount = len(dict.Sequences)
	result.RoutineCount = len(dict.Routines)
	result.TriggerCount = len(dict.Triggers)
	result.SynonymCount = len(dict.Synonyms)
	result.TabPrivilegeCount = len(dict.TabPrivileges)
	result.SystemPrivilegeCount = len(dict.SystemPrivileges)
	result.PartitionCount = len(dict.Partitions)
	result.PartitionKeyCount = len(dict.PartitionKeys)
	return result, nil
}

func dictionarySchemasForPersistence(dict *DictionaryInfo) []DictionarySchema {
	if dict == nil {
		return nil
	}
	result := append([]DictionarySchema(nil), dict.Schemas...)
	seen := make(map[string]bool, len(result))
	for _, schema := range result {
		seen[strings.ToUpper(strings.TrimSpace(schema.Name))] = true
	}
	for _, user := range dict.Users {
		key := strings.ToUpper(strings.TrimSpace(user.Name))
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		result = append(result, DictionarySchema{ID: user.ID, Name: user.Name, OwnerID: user.ID, Owner: user.Name})
	}
	for _, table := range dict.Tables {
		key := strings.ToUpper(strings.TrimSpace(table.Owner))
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		result = append(result, DictionarySchema{Name: table.Owner, Owner: table.Owner})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Owner != result[j].Owner {
			return result[i].Owner < result[j].Owner
		}
		if result[i].Name != result[j].Name {
			return result[i].Name < result[j].Name
		}
		return result[i].ID < result[j].ID
	})
	return result
}

// RebuildDictionaryFiles writes and validates a complete dictionary in a
// sibling staging directory before replacing the active directory. An
// existing dictionary is preserved as a timestamped backup so interrupted or
// mistaken bootstraps do not destroy manual corrections.
func RebuildDictionaryFiles(dir string, dict *DictionaryInfo) (*DictionaryFilesResult, string, error) {
	if dict == nil {
		return nil, "", fmt.Errorf("dictionary is nil")
	}
	dir = filepath.Clean(strings.TrimSpace(dir))
	base := filepath.Base(dir)
	if dir == "" || dir == "." || base == "" || base == "." || base == string(os.PathSeparator) {
		return nil, "", fmt.Errorf("dictionary directory is empty or unsafe: %q", dir)
	}
	parent := filepath.Dir(dir)
	if err := os.MkdirAll(parent, 0755); err != nil {
		return nil, "", err
	}
	stagingDir, err := os.MkdirTemp(parent, "."+base+".bootstrap-")
	if err != nil {
		return nil, "", fmt.Errorf("create dictionary staging directory: %w", err)
	}
	stagingActive := true
	defer func() {
		if stagingActive {
			_ = os.RemoveAll(stagingDir)
		}
	}()

	staged, err := WriteDictionaryFiles(stagingDir, dict)
	if err != nil {
		return nil, "", fmt.Errorf("write staged dictionary: %w", err)
	}
	loaded, verified, err := LoadDictionaryFiles(stagingDir)
	if err != nil {
		return nil, "", fmt.Errorf("validate staged dictionary: %w", err)
	}
	if loaded.ObjectCount != dict.ObjectCount || verified.UserCount != staged.UserCount || verified.SchemaCount != staged.SchemaCount || verified.TableCount != staged.TableCount || verified.ColumnCount != staged.ColumnCount {
		return nil, "", fmt.Errorf("validate staged dictionary: count mismatch")
	}

	backupDir := ""
	catalogOnlyDir := ""
	if info, statErr := os.Stat(dir); statErr == nil {
		if !info.IsDir() {
			return nil, "", fmt.Errorf("dictionary path exists but is not a directory: %s", dir)
		}
		catalogOnly, catalogErr := isASMCatalogOnlyDirectory(dir)
		if catalogErr != nil {
			return nil, "", catalogErr
		}
		if catalogOnly {
			catalogOnlyDir, err = allocateTemporarySiblingDirectory(dir, ".catalog-")
			if err != nil {
				return nil, "", err
			}
			if err := os.Rename(dir, catalogOnlyDir); err != nil {
				return nil, "", fmt.Errorf("stage ASM-only dictionary directory %s: %w", dir, err)
			}
		} else {
			backupDir, err = nextDictionaryBackupDir(dir, time.Now())
			if err != nil {
				return nil, "", err
			}
			if err := os.Rename(dir, backupDir); err != nil {
				return nil, "", fmt.Errorf("archive previous dictionary to %s: %w", backupDir, err)
			}
		}
	} else if !os.IsNotExist(statErr) {
		return nil, "", fmt.Errorf("inspect dictionary directory: %w", statErr)
	}

	if err := os.Rename(stagingDir, dir); err != nil {
		if catalogOnlyDir != "" {
			_ = os.Rename(catalogOnlyDir, dir)
		} else if backupDir != "" {
			_ = os.Rename(backupDir, dir)
		}
		return nil, "", fmt.Errorf("activate staged dictionary: %w", err)
	}
	if catalogOnlyDir != "" {
		_ = os.RemoveAll(catalogOnlyDir)
	}
	stagingActive = false
	result := dictionaryFilesResultForDir(dir)
	result.UserCount = staged.UserCount
	result.SchemaCount = staged.SchemaCount
	result.TableCount = staged.TableCount
	result.ColumnCount = staged.ColumnCount
	result.ViewCount = staged.ViewCount
	result.SequenceCount = staged.SequenceCount
	result.RoutineCount = staged.RoutineCount
	result.TriggerCount = staged.TriggerCount
	result.SynonymCount = staged.SynonymCount
	result.TabPrivilegeCount = staged.TabPrivilegeCount
	result.PartitionCount = staged.PartitionCount
	result.PartitionKeyCount = staged.PartitionKeyCount
	return result, backupDir, nil
}

func isASMCatalogOnlyDirectory(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, fmt.Errorf("inspect dictionary directory %s: %w", dir, err)
	}
	if len(entries) == 0 {
		return false, nil
	}
	allowed := map[string]bool{
		strings.ToLower(ASMDatabaseCatalogFileName): true,
		strings.ToLower(ASMDataFileCatalogFileName): true,
	}
	for _, entry := range entries {
		if entry.IsDir() || !allowed[strings.ToLower(entry.Name())] {
			return false, nil
		}
	}
	return true, nil
}

func allocateTemporarySiblingDirectory(dir string, suffix string) (string, error) {
	parent := filepath.Dir(dir)
	temporary, err := os.MkdirTemp(parent, "."+filepath.Base(dir)+suffix)
	if err != nil {
		return "", fmt.Errorf("allocate temporary dictionary path: %w", err)
	}
	if err := os.Remove(temporary); err != nil {
		return "", fmt.Errorf("prepare temporary dictionary path: %w", err)
	}
	return temporary, nil
}

func nextDictionaryBackupDir(dir string, now time.Time) (string, error) {
	base := dir + ".backup-" + now.Format("20060102-150405")
	for suffix := 0; suffix < 1000; suffix++ {
		candidate := base
		if suffix > 0 {
			candidate = fmt.Sprintf("%s-%02d", base, suffix)
		}
		_, err := os.Stat(candidate)
		if os.IsNotExist(err) {
			return candidate, nil
		}
		if err != nil {
			return "", fmt.Errorf("inspect dictionary backup path: %w", err)
		}
	}
	return "", fmt.Errorf("cannot allocate dictionary backup path for %s", dir)
}

func LoadDictionaryFiles(dir string) (*DictionaryInfo, *DictionaryFilesResult, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, nil, fmt.Errorf("dictionary directory is empty")
	}
	result := dictionaryFilesResultForDir(dir)
	meta, err := readDictionaryMeta(result.MetaPath)
	if err != nil {
		return nil, nil, err
	}
	users, err := readDictionaryUsers(result.UsersPath)
	if err != nil {
		return nil, nil, err
	}
	schemas, err := readDictionarySchemas(result.SchemasPath)
	if err != nil && !os.IsNotExist(err) {
		return nil, nil, err
	}
	if err != nil && os.IsNotExist(err) {
		schemas = nil
	}
	tables, err := readDictionaryTables(result.TablesPath)
	if err != nil {
		return nil, nil, err
	}
	columns, err := readDictionaryColumns(result.ColumnsPath)
	if err != nil && !os.IsNotExist(err) {
		return nil, nil, err
	}
	if err != nil && os.IsNotExist(err) {
		columns = nil
	}
	views, err := readDictionaryViews(result.ViewsPath)
	if err != nil && !os.IsNotExist(err) {
		return nil, nil, err
	}
	if err != nil && os.IsNotExist(err) {
		views = nil
	}
	sequences, err := readDictionarySequences(result.SequencesPath)
	if err != nil && !os.IsNotExist(err) {
		return nil, nil, err
	}
	if err != nil && os.IsNotExist(err) {
		sequences = nil
	}
	routines, err := readDictionaryRoutines(result.RoutinesPath)
	if err != nil && !os.IsNotExist(err) {
		return nil, nil, err
	}
	if err != nil && os.IsNotExist(err) {
		routines = nil
	}
	triggers, err := readDictionaryTriggers(result.TriggersPath)
	if err != nil && !os.IsNotExist(err) {
		return nil, nil, err
	}
	if err != nil && os.IsNotExist(err) {
		triggers = nil
	}
	synonyms, err := readDictionarySynonyms(result.SynonymsPath)
	if err != nil && !os.IsNotExist(err) {
		return nil, nil, err
	}
	if err != nil && os.IsNotExist(err) {
		synonyms = nil
	}
	tabPrivileges, err := readDictionaryTabPrivileges(result.TabPrivilegesPath)
	if err != nil && !os.IsNotExist(err) {
		return nil, nil, err
	}
	if err != nil && os.IsNotExist(err) {
		tabPrivileges = nil
	}
	systemPrivileges, err := readDictionarySystemPrivileges(result.SystemPrivilegesPath)
	if err != nil && !os.IsNotExist(err) {
		return nil, nil, err
	}
	partitions, err := readDictionaryPartitions(result.PartitionsPath)
	if err != nil && !os.IsNotExist(err) {
		return nil, nil, err
	}
	if err != nil && os.IsNotExist(err) {
		partitions = nil
	}
	partitionKeys, err := readDictionaryPartitionKeys(result.PartitionKeysPath)
	if err != nil && !os.IsNotExist(err) {
		return nil, nil, err
	}
	if err != nil && os.IsNotExist(err) {
		partitionKeys = nil
	}
	users, schemas, tables, columns, views, sequences, routines, triggers, synonyms, tabPrivileges = normalizeDictionaryFromFiles(users, schemas, tables, columns, views, sequences, routines, triggers, synonyms, tabPrivileges)
	partitions, partitionKeys = normalizeDictionaryPartitionsFromFiles(tables, columns, partitions, partitionKeys)
	dict := &DictionaryInfo{
		SystemPath:          meta["system_path"],
		ControlPath:         meta["control_path"],
		Source:              "dictionary files",
		DictionaryDir:       dir,
		ExtentSize:          parseMetaUint32(meta["extent_size"]),
		ExtentSizeSource:    meta["extent_size_source"],
		PageSize:            parseMetaUint32(meta["page_size"]),
		PageCount:           parseMetaUint32(meta["page_count"]),
		Charset:             meta["charset"],
		CharsetSource:       meta["charset_source"],
		CaseSensitive:       parseBoolField(meta["case_sensitive"]),
		CaseSensitiveSource: meta["case_sensitive_source"],
		HasCaseSensitive:    strings.TrimSpace(meta["case_sensitive"]) != "",
		ObjectCount:         parseMetaInt(meta["object_count"]),
		UserCount:           len(users),
		SchemaCount:         len(schemas),
		TableCount:          len(tables),
		ColumnCount:         len(columns),
		ViewCount:           len(views),
		SequenceCount:       len(sequences),
		RoutineCount:        len(routines),
		TriggerCount:        len(triggers),
		SynonymCount:        len(synonyms),
		TabPrivilegeCount:   len(tabPrivileges),
		PartitionCount:      len(partitions),
		PartitionKeyCount:   len(partitionKeys),
		BootstrapMode:       meta["bootstrap_mode"],
		BootstrapFallback:   parseBoolField(meta["bootstrap_fallback"]),
		Users:               users,
		Schemas:             schemas,
		Tables:              tables,
		Columns:             columns,
		Views:               views,
		Sequences:           sequences,
		Routines:            routines,
		Triggers:            triggers,
		Synonyms:            synonyms,
		TabPrivileges:       tabPrivileges,
		SystemPrivileges:    systemPrivileges,
		Partitions:          partitions,
		PartitionKeys:       partitionKeys,
	}
	result.UserCount = len(users)
	result.SchemaCount = len(schemas)
	result.TableCount = len(tables)
	result.ColumnCount = len(columns)
	result.ViewCount = len(views)
	result.SequenceCount = len(sequences)
	result.RoutineCount = len(routines)
	result.TriggerCount = len(triggers)
	result.SynonymCount = len(synonyms)
	result.TabPrivilegeCount = len(tabPrivileges)
	result.SystemPrivilegeCount = len(systemPrivileges)
	result.PartitionCount = len(partitions)
	result.PartitionKeyCount = len(partitionKeys)
	return dict, result, nil
}

func dictionaryFilesResultForDir(dir string) *DictionaryFilesResult {
	return &DictionaryFilesResult{
		Dir:                  dir,
		MetaPath:             filepath.Join(dir, "meta.tsv"),
		UsersPath:            filepath.Join(dir, "users.tsv"),
		SchemasPath:          filepath.Join(dir, "schemas.tsv"),
		TablesPath:           filepath.Join(dir, "tables.tsv"),
		ColumnsPath:          filepath.Join(dir, "columns.tsv"),
		ViewsPath:            filepath.Join(dir, "views.tsv"),
		SequencesPath:        filepath.Join(dir, "sequences.tsv"),
		RoutinesPath:         filepath.Join(dir, "routines.tsv"),
		TriggersPath:         filepath.Join(dir, "triggers.tsv"),
		SynonymsPath:         filepath.Join(dir, "synonyms.tsv"),
		TabPrivilegesPath:    filepath.Join(dir, "tab_privs.tsv"),
		SystemPrivilegesPath: filepath.Join(dir, "sys_privs.tsv"),
		PartitionsPath:       filepath.Join(dir, "partitions.tsv"),
		PartitionKeysPath:    filepath.Join(dir, "partition_keys.tsv"),
	}
}

func writeDictionaryMeta(path string, dict *DictionaryInfo, schemaCount int) error {
	caseSensitive := ""
	if dict.HasCaseSensitive {
		caseSensitive = "0"
		if dict.CaseSensitive {
			caseSensitive = "1"
		}
	}
	rows := [][]string{
		{"format_version", "3"},
		{"source", dict.Source},
		{"system_path", dict.SystemPath},
		{"control_path", dict.ControlPath},
		{"extent_size", formatUint32Field(dict.ExtentSize)},
		{"extent_size_source", dict.ExtentSizeSource},
		{"page_size", formatUint32Field(dict.PageSize)},
		{"page_count", formatUint32Field(dict.PageCount)},
		{"charset", dict.Charset},
		{"charset_source", dict.CharsetSource},
		{"case_sensitive", caseSensitive},
		{"case_sensitive_source", dict.CaseSensitiveSource},
		{"bootstrap_mode", dict.BootstrapMode},
		{"bootstrap_fallback", formatBoolField(dict.BootstrapFallback)},
		{"object_count", strconv.Itoa(dict.ObjectCount)},
		{"user_count", strconv.Itoa(len(dict.Users))},
		{"schema_count", strconv.Itoa(schemaCount)},
		{"table_count", strconv.Itoa(len(dict.Tables))},
		{"column_count", strconv.Itoa(len(dict.Columns))},
		{"view_count", strconv.Itoa(len(dict.Views))},
		{"sequence_count", strconv.Itoa(len(dict.Sequences))},
		{"routine_count", strconv.Itoa(len(dict.Routines))},
		{"trigger_count", strconv.Itoa(len(dict.Triggers))},
		{"synonym_count", strconv.Itoa(len(dict.Synonyms))},
		{"tab_privilege_count", strconv.Itoa(len(dict.TabPrivileges))},
		{"system_privilege_count", strconv.Itoa(len(dict.SystemPrivileges))},
		{"partition_count", strconv.Itoa(len(dict.Partitions))},
		{"partition_key_count", strconv.Itoa(len(dict.PartitionKeys))},
	}
	return writeTSV(path, []string{"key", "value"}, rows)
}

func writeDictionaryUsers(path string, users []DictionaryUser) error {
	rows := make([][]string, 0, len(users))
	for _, user := range users {
		rows = append(rows, []string{formatUint32Field(user.ID), user.Name})
	}
	return writeTSV(path, []string{"user_id", "user_name"}, rows)
}

func writeDictionarySchemas(path string, schemas []DictionarySchema) error {
	rows := make([][]string, 0, len(schemas))
	for _, schema := range schemas {
		rows = append(rows, []string{
			formatUint32Field(schema.ID), schema.Name,
			formatUint32Field(schema.OwnerID), schema.Owner,
		})
	}
	return writeTSV(path, []string{"schema_id", "schema_name", "owner_user_id", "owner_user_name"}, rows)
}

func writeDictionaryTables(path string, tables []DictionaryTable) error {
	rows := make([][]string, 0, len(tables))
	for _, table := range tables {
		headerFile := ""
		if dictionaryTableHasSegment(table) {
			headerFile = strconv.FormatInt(int64(table.HeaderFile), 10)
		}
		rows = append(rows, []string{
			formatUint32Field(table.ID),
			table.Owner,
			table.Name,
			strconv.Itoa(table.ColumnCount),
			table.Tablespace,
			formatUint32Field(table.GroupID),
			headerFile,
			formatUint32Field(table.HeaderBlock),
			formatUint64Field(table.Bytes),
			formatUint32Field(table.Blocks),
			formatUint32Field(table.Extents),
			formatBoolField(table.Temporary),
			table.Storage,
			formatBoolField(table.Partitioned),
			formatUint32Field(table.StorageID),
			formatInt16Field(table.RootFile),
			formatUint32Field(table.RootPage),
			formatUint32ListField(table.AssistIDs),
			formatBoolField(table.Huge),
			formatBoolField(table.HugeWithDelta),
			formatUint32Field(table.HugeSectionRows),
			formatUint32Field(table.HugeFileSizeMB),
			formatUint32Field(table.HugeAuxTableID),
			formatUint32Field(table.HugeRAuxTableID),
			formatUint32Field(table.HugeDAuxTableID),
			formatUint32Field(table.HugeUAuxTableID),
		})
	}
	return writeTSV(path, []string{"table_id", "owner", "table_name", "column_count", "tablespace", "group_id", "header_file", "header_block", "bytes", "blocks", "extents", "temporary", "storage", "partitioned", "storage_id", "root_file", "root_page", "assist_ids", "huge", "huge_with_delta", "huge_section_rows", "huge_file_size_mb", "huge_aux_table_id", "huge_raux_table_id", "huge_daux_table_id", "huge_uaux_table_id"}, rows)
}

func writeDictionaryColumns(path string, columns []DictionaryColumn) error {
	rows := make([][]string, 0, len(columns))
	for _, col := range columns {
		rows = append(rows, []string{
			formatUint32Field(col.TableID),
			col.TableOwner,
			col.TableName,
			strconv.Itoa(int(col.ColID)),
			col.Name,
			col.DataType,
			formatUint32Field(col.Length),
			strconv.Itoa(int(col.Scale)),
			col.Nullable,
			col.Default,
		})
	}
	return writeTSV(path, []string{"table_id", "owner", "table_name", "col_id", "column_name", "data_type", "length", "scale", "nullable", "default"}, rows)
}

func writeDictionaryViews(path string, views []DictionaryView) error {
	rows := make([][]string, 0, len(views))
	for _, view := range views {
		rows = append(rows, []string{
			formatUint32Field(view.ID),
			view.Owner,
			view.Name,
			view.Valid,
			cleanRecoveredSQLText(view.SQL),
			cleanRecoveredSQLText(view.QuerySQL),
		})
	}
	return writeTSV(path, []string{"view_id", "owner", "view_name", "valid", "sql", "query_sql"}, rows)
}

func writeDictionarySequences(path string, sequences []DictionarySequence) error {
	rows := make([][]string, 0, len(sequences))
	for _, seq := range sequences {
		rows = append(rows, []string{
			formatUint32Field(seq.ID),
			seq.Owner,
			seq.Name,
			seq.Valid,
			formatKnownInt64Field(seq.StartWith, seq.HasStartWith || seq.StartWith != 0),
			formatKnownInt64Field(seq.MinValue, seq.HasMinValue || seq.MinValue != 0),
			formatKnownInt64Field(seq.MaxValue, seq.HasMaxValue || seq.MaxValue != 0),
			formatInt64Field(seq.IncrementBy),
			seq.CycleFlag,
			seq.OrderFlag,
			formatUint32Field(seq.CacheSize),
			cleanRecoveredSQLText(seq.SQL),
			formatKnownInt64Field(seq.LastNumber, seq.HasLastNumber),
			formatSequenceRuntimeUint(seq.RuntimeFile, seq.HasRuntimeLocator),
			formatSequenceRuntimeUint(seq.RuntimePage, seq.HasRuntimeLocator),
			formatSequenceRuntimeUint(seq.RuntimeSlot, seq.HasRuntimeLocator),
			formatSequenceRuntimeState(seq.RuntimeState, seq.HasLastNumber),
		})
	}
	return writeTSV(path, []string{"sequence_id", "owner", "sequence_name", "valid", "start_with", "min_value", "max_value", "increment_by", "cycle_flag", "order_flag", "cache_size", "sql", "last_number", "runtime_file", "runtime_page", "runtime_slot", "runtime_state"}, rows)
}

func writeDictionaryRoutines(path string, routines []DictionaryRoutine) error {
	rows := make([][]string, 0, len(routines))
	for _, routine := range routines {
		rows = append(rows, []string{
			formatUint32Field(routine.ID),
			routine.Owner,
			routine.Name,
			normalizeRoutineObjectType(routine.ObjectType),
			strconv.FormatUint(uint64(routine.SeqNo), 10),
			routine.Valid,
			cleanRecoveredSQLText(routine.SQL),
		})
	}
	return writeTSV(path, []string{"routine_id", "owner", "routine_name", "object_type", "seq_no", "valid", "sql"}, rows)
}

func writeDictionaryTriggers(path string, triggers []DictionaryTrigger) error {
	rows := make([][]string, 0, len(triggers))
	for _, trigger := range triggers {
		rows = append(rows, []string{
			formatUint32Field(trigger.ID),
			trigger.Owner,
			trigger.Name,
			trigger.TableOwner,
			trigger.TableName,
			trigger.Valid,
			cleanRecoveredSQLText(trigger.SQL),
		})
	}
	return writeTSV(path, []string{"trigger_id", "owner", "trigger_name", "table_owner", "table_name", "valid", "sql"}, rows)
}

func writeDictionarySynonyms(path string, synonyms []DictionarySynonym) error {
	rows := make([][]string, 0, len(synonyms))
	for _, syn := range synonyms {
		rows = append(rows, []string{
			formatUint32Field(syn.ID),
			syn.Owner,
			syn.Name,
			syn.TableOwner,
			syn.TableName,
			formatBoolField(syn.Public),
		})
	}
	return writeTSV(path, []string{"synonym_id", "owner", "synonym_name", "table_owner", "table_name", "public"}, rows)
}

func writeDictionaryTabPrivileges(path string, privileges []DictionaryTabPrivilege) error {
	rows := make([][]string, 0, len(privileges))
	for _, priv := range privileges {
		columnID := ""
		if priv.ColumnID != nil {
			columnID = strconv.Itoa(int(*priv.ColumnID))
		}
		rows = append(rows, []string{
			priv.Grantee,
			priv.Owner,
			priv.ObjectName,
			priv.ObjectType,
			priv.Privilege,
			priv.Grantable,
			columnID, priv.ColumnName, priv.Grantor,
		})
	}
	return writeTSV(path, []string{"grantee", "owner", "object_name", "object_type", "privilege", "grantable", "column_id", "column_name", "grantor"}, rows)
}

func writeDictionaryPartitions(path string, partitions []DictionaryPartition) error {
	rows := make([][]string, 0, len(partitions))
	for _, part := range partitions {
		rows = append(rows, []string{
			formatUint32Field(part.BaseTableID),
			part.Owner,
			part.TableName,
			strconv.FormatUint(uint64(part.Position), 10),
			strings.ToUpper(strings.TrimSpace(part.Type)),
			part.Name,
			formatUint32Field(part.PartTableID),
			strings.ToUpper(hex.EncodeToString(part.HighValue)),
			formatUint32Field(part.PageNo),
			strconv.FormatUint(uint64(part.SlotNo), 10),
			strconv.FormatUint(uint64(part.SlotOffset), 10),
			formatUint64Field(part.RowOffset),
		})
	}
	return writeTSV(path, []string{"base_table_id", "owner", "table_name", "position", "partition_type", "partition_name", "part_table_id", "high_value_hex", "page_no", "slot_no", "slot_offset", "row_offset"}, rows)
}

func writeDictionaryPartitionKeys(path string, keys []DictionaryPartitionKey) error {
	rows := make([][]string, 0, len(keys))
	for _, key := range keys {
		rows = append(rows, []string{
			formatUint32Field(key.TableID),
			key.Owner,
			key.TableName,
			strconv.FormatUint(uint64(key.Position), 10),
			strconv.FormatUint(uint64(key.ColID), 10),
			key.ColumnName,
		})
	}
	return writeTSV(path, []string{"table_id", "owner", "table_name", "position", "col_id", "column_name"}, rows)
}

func readDictionaryMeta(path string) (map[string]string, error) {
	records, err := readTSV(path)
	if err != nil {
		return nil, err
	}
	meta := make(map[string]string)
	for _, rec := range records {
		if len(rec) < 2 || rec[0] == "key" {
			continue
		}
		meta[rec[0]] = rec[1]
	}
	if len(meta) == 0 {
		return nil, fmt.Errorf("dictionary meta is empty: %s", path)
	}
	return meta, nil
}

func readDictionaryUsers(path string) ([]DictionaryUser, error) {
	records, err := readTSV(path)
	if err != nil {
		return nil, err
	}
	var users []DictionaryUser
	for _, rec := range records {
		if len(rec) < 2 || rec[0] == "user_id" {
			continue
		}
		users = append(users, DictionaryUser{
			ID:   parseUint32Field(rec[0]),
			Name: rec[1],
		})
	}
	return users, nil
}

func readDictionarySchemas(path string) ([]DictionarySchema, error) {
	records, err := readTSV(path)
	if err != nil {
		return nil, err
	}
	var schemas []DictionarySchema
	for _, rec := range records {
		if len(rec) < 4 || rec[0] == "schema_id" {
			continue
		}
		schemas = append(schemas, DictionarySchema{
			ID: parseUint32Field(rec[0]), Name: rec[1],
			OwnerID: parseUint32Field(rec[2]), Owner: rec[3],
		})
	}
	return schemas, nil
}

func readDictionaryTables(path string) ([]DictionaryTable, error) {
	records, err := readTSV(path)
	if err != nil {
		return nil, err
	}
	var tables []DictionaryTable
	for _, rec := range records {
		if len(rec) < 9 || rec[0] == "table_id" {
			continue
		}
		table := DictionaryTable{
			ID:          parseUint32Field(rec[0]),
			Owner:       rec[1],
			Name:        rec[2],
			ColumnCount: parseIntField(rec[3]),
			Tablespace:  rec[4],
			GroupID:     parseUint32Field(rec[5]),
		}
		if len(rec) >= 14 {
			table.HeaderFile = int16(parseIntField(rec[6]))
			table.HeaderBlock = parseUint32Field(rec[7])
			table.Bytes = parseUint64Field(rec[8])
			table.Blocks = parseUint32Field(rec[9])
			table.Extents = parseUint32Field(rec[10])
			table.Temporary = parseBoolField(rec[11])
			table.Storage = rec[12]
			table.Partitioned = parseBoolField(rec[13])
			if len(rec) >= 18 {
				table.StorageID = parseUint32Field(rec[14])
				table.RootFile = parseOptionalInt16Field(rec[15], -1)
				table.RootPage = parseUint32Field(rec[16])
				table.AssistIDs = parseUint32ListField(rec[17])
			}
			if len(rec) >= 26 {
				table.Huge = parseBoolField(rec[18])
				table.HugeWithDelta = parseBoolField(rec[19])
				table.HugeSectionRows = parseUint32Field(rec[20])
				table.HugeFileSizeMB = parseUint32Field(rec[21])
				table.HugeAuxTableID = parseUint32Field(rec[22])
				table.HugeRAuxTableID = parseUint32Field(rec[23])
				table.HugeDAuxTableID = parseUint32Field(rec[24])
				table.HugeUAuxTableID = parseUint32Field(rec[25])
			}
			tables = append(tables, table)
			continue
		}
		tables = append(tables, DictionaryTable{
			ID:          table.ID,
			Owner:       table.Owner,
			Name:        table.Name,
			ColumnCount: table.ColumnCount,
			Tablespace:  table.Tablespace,
			GroupID:     table.GroupID,
			Temporary:   parseBoolField(rec[6]),
			Storage:     rec[7],
			Partitioned: parseBoolField(rec[8]),
		})
	}
	return tables, nil
}

func readDictionaryColumns(path string) ([]DictionaryColumn, error) {
	records, err := readTSV(path)
	if err != nil {
		return nil, err
	}
	var columns []DictionaryColumn
	for _, rec := range records {
		if len(rec) < 10 || rec[0] == "table_id" {
			continue
		}
		scale := int16(parseIntField(rec[7]))
		columns = append(columns, DictionaryColumn{
			TableID:    parseUint32Field(rec[0]),
			TableOwner: rec[1],
			TableName:  rec[2],
			ColID:      uint16(parseUint32Field(rec[3])),
			Name:       rec[4],
			DataType:   normalizeCatalogColumnType(rec[5], scale),
			Length:     parseUint32Field(rec[6]),
			Scale:      scale,
			Nullable:   rec[8],
			Default:    rec[9],
		})
	}
	return columns, nil
}

func readDictionaryViews(path string) ([]DictionaryView, error) {
	records, err := readTSV(path)
	if err != nil {
		return nil, err
	}
	var views []DictionaryView
	for _, rec := range records {
		if len(rec) < 5 || rec[0] == "view_id" {
			continue
		}
		view := DictionaryView{
			ID:    parseUint32Field(rec[0]),
			Owner: rec[1],
			Name:  rec[2],
			Valid: rec[3],
			SQL:   cleanRecoveredSQLText(rec[4]),
		}
		if len(rec) >= 6 {
			view.QuerySQL = cleanRecoveredSQLText(rec[5])
		}
		views = append(views, view)
	}
	return views, nil
}

func readDictionarySequences(path string) ([]DictionarySequence, error) {
	records, err := readTSV(path)
	if err != nil {
		return nil, err
	}
	var sequences []DictionarySequence
	for _, rec := range records {
		if len(rec) < 7 || rec[0] == "sequence_id" {
			continue
		}
		seq := DictionarySequence{
			ID:    parseUint32Field(rec[0]),
			Owner: rec[1],
			Name:  rec[2],
			Valid: rec[3],
		}
		if len(rec) >= 12 {
			seq.StartWith = parseInt64Field(rec[4])
			seq.HasStartWith = strings.TrimSpace(rec[4]) != ""
			seq.MinValue = parseInt64Field(rec[5])
			seq.HasMinValue = strings.TrimSpace(rec[5]) != ""
			seq.MaxValue = parseInt64Field(rec[6])
			seq.HasMaxValue = strings.TrimSpace(rec[6]) != ""
			seq.IncrementBy = parseInt64Field(rec[7])
			seq.CycleFlag = rec[8]
			seq.OrderFlag = rec[9]
			seq.CacheSize = parseUint32Field(rec[10])
			seq.SQL = cleanRecoveredSQLText(rec[11])
			if len(rec) >= 13 && strings.TrimSpace(rec[12]) != "" {
				seq.LastNumber = parseInt64Field(rec[12])
				seq.HasLastNumber = true
			}
			if len(rec) >= 16 && (strings.TrimSpace(rec[13]) != "" || strings.TrimSpace(rec[14]) != "" || strings.TrimSpace(rec[15]) != "") {
				seq.RuntimeFile = uint16(parseUint32Field(rec[13]))
				seq.RuntimePage = parseUint32Field(rec[14])
				seq.RuntimeSlot = uint16(parseUint32Field(rec[15]))
				seq.HasRuntimeLocator = true
			}
			if len(rec) >= 17 {
				seq.RuntimeState = parseSequenceRuntimeState(rec[16])
			}
		} else {
			seq.IncrementBy = parseInt64Field(rec[4])
			seq.CycleFlag = rec[5]
			seq.OrderFlag = rec[6]
			if len(rec) >= 8 {
				seq.SQL = cleanRecoveredSQLText(rec[7])
			}
		}
		sequences = append(sequences, seq)
	}
	return sequences, nil
}

func formatKnownInt64Field(value int64, known bool) string {
	if !known {
		return ""
	}
	return strconv.FormatInt(value, 10)
}

func formatSequenceRuntimeUint[T ~uint16 | ~uint32](value T, known bool) string {
	if !known {
		return ""
	}
	return strconv.FormatUint(uint64(value), 10)
}

func formatSequenceRuntimeState(value uint8, known bool) string {
	if !known {
		return ""
	}
	return fmt.Sprintf("0x%02X", value)
}

func parseSequenceRuntimeState(value string) uint8 {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	parsed, err := strconv.ParseUint(value, 0, 8)
	if err != nil {
		return 0
	}
	return uint8(parsed)
}

func readDictionaryRoutines(path string) ([]DictionaryRoutine, error) {
	records, err := readTSV(path)
	if err != nil {
		return nil, err
	}
	var routines []DictionaryRoutine
	for _, rec := range records {
		if len(rec) < 7 || rec[0] == "routine_id" {
			continue
		}
		routines = append(routines, DictionaryRoutine{
			ID:         parseUint32Field(rec[0]),
			Owner:      rec[1],
			Name:       rec[2],
			ObjectType: normalizeRoutineObjectType(rec[3]),
			SeqNo:      parseUint32Field(rec[4]),
			Valid:      rec[5],
			SQL:        cleanRecoveredSQLText(rec[6]),
		})
	}
	return routines, nil
}

func readDictionaryTriggers(path string) ([]DictionaryTrigger, error) {
	records, err := readTSV(path)
	if err != nil {
		return nil, err
	}
	var triggers []DictionaryTrigger
	for _, rec := range records {
		if len(rec) < 7 || rec[0] == "trigger_id" {
			continue
		}
		triggers = append(triggers, DictionaryTrigger{
			ID:         parseUint32Field(rec[0]),
			Owner:      rec[1],
			Name:       rec[2],
			TableOwner: rec[3],
			TableName:  rec[4],
			Valid:      rec[5],
			SQL:        cleanRecoveredSQLText(rec[6]),
		})
	}
	return triggers, nil
}

func readDictionarySynonyms(path string) ([]DictionarySynonym, error) {
	records, err := readTSV(path)
	if err != nil {
		return nil, err
	}
	var synonyms []DictionarySynonym
	for _, rec := range records {
		if len(rec) < 6 || rec[0] == "synonym_id" {
			continue
		}
		synonyms = append(synonyms, DictionarySynonym{
			ID:         parseUint32Field(rec[0]),
			Owner:      rec[1],
			Name:       rec[2],
			TableOwner: rec[3],
			TableName:  rec[4],
			Public:     parseBoolField(rec[5]),
		})
	}
	return synonyms, nil
}

func readDictionaryTabPrivileges(path string) ([]DictionaryTabPrivilege, error) {
	records, err := readTSV(path)
	if err != nil {
		return nil, err
	}
	var privileges []DictionaryTabPrivilege
	for _, rec := range records {
		if len(rec) < 6 || rec[0] == "grantee" {
			continue
		}
		privileges = append(privileges, DictionaryTabPrivilege{
			Grantee:    rec[0],
			Owner:      rec[1],
			ObjectName: rec[2],
			ObjectType: rec[3],
			Privilege:  rec[4],
			Grantable:  rec[5],
		})
		priv := &privileges[len(privileges)-1]
		if len(rec) > 6 && rec[6] != "" {
			id, err := strconv.ParseUint(rec[6], 10, 16)
			if err != nil {
				return nil, fmt.Errorf("invalid tab_privs column_id %q: %w", rec[6], err)
			}
			colID := uint16(id)
			priv.ColumnID = &colID
		}
		if len(rec) > 7 {
			priv.ColumnName = rec[7]
		}
		if len(rec) > 8 {
			priv.Grantor = rec[8]
		}
	}
	return privileges, nil
}

func readDictionaryPartitions(path string) ([]DictionaryPartition, error) {
	records, err := readTSV(path)
	if err != nil {
		return nil, err
	}
	var partitions []DictionaryPartition
	for rowNo, rec := range records {
		if len(rec) < 8 || rec[0] == "base_table_id" {
			continue
		}
		highValueText := strings.TrimSpace(rec[7])
		var highValue []byte
		if highValueText != "" {
			highValue, err = hex.DecodeString(highValueText)
			if err != nil {
				return nil, fmt.Errorf("read %s row %d high_value_hex: %w", path, rowNo+1, err)
			}
		}
		part := DictionaryPartition{
			BaseTableID: parseUint32Field(rec[0]),
			Owner:       rec[1],
			TableName:   rec[2],
			Position:    parseUint32Field(rec[3]),
			Type:        strings.ToUpper(strings.TrimSpace(rec[4])),
			Name:        rec[5],
			PartTableID: parseUint32Field(rec[6]),
			HighValue:   normalizePartitionHighValue(highValue),
		}
		if len(rec) >= 12 {
			part.PageNo = parseUint32Field(rec[8])
			part.SlotNo = uint16(parseUint32Field(rec[9]))
			part.SlotOffset = uint16(parseUint32Field(rec[10]))
			part.RowOffset = parseUint64Field(rec[11])
		}
		partitions = append(partitions, part)
	}
	return partitions, nil
}

func readDictionaryPartitionKeys(path string) ([]DictionaryPartitionKey, error) {
	records, err := readTSV(path)
	if err != nil {
		return nil, err
	}
	var keys []DictionaryPartitionKey
	for _, rec := range records {
		if len(rec) < 5 || rec[0] == "table_id" {
			continue
		}
		key := DictionaryPartitionKey{
			TableID:   parseUint32Field(rec[0]),
			Owner:     rec[1],
			TableName: rec[2],
			Position:  parseUint32Field(rec[3]),
			ColID:     uint16(parseUint32Field(rec[4])),
		}
		if len(rec) >= 6 {
			key.ColumnName = rec[5]
		}
		keys = append(keys, key)
	}
	return keys, nil
}

func normalizeDictionaryFromFiles(users []DictionaryUser, schemas []DictionarySchema, tables []DictionaryTable, columns []DictionaryColumn, views []DictionaryView, sequences []DictionarySequence, routines []DictionaryRoutine, triggers []DictionaryTrigger, synonyms []DictionarySynonym, privileges []DictionaryTabPrivilege) ([]DictionaryUser, []DictionarySchema, []DictionaryTable, []DictionaryColumn, []DictionaryView, []DictionarySequence, []DictionaryRoutine, []DictionaryTrigger, []DictionarySynonym, []DictionaryTabPrivilege) {
	columnCounts := make(map[uint32]int)
	for _, col := range columns {
		columnCounts[col.TableID]++
	}
	for i := range tables {
		if tables[i].ColumnCount == 0 {
			tables[i].ColumnCount = columnCounts[tables[i].ID]
		}
	}
	userNames := make(map[string]bool)
	userNamesByID := make(map[uint32]string)
	for _, user := range users {
		userNames[strings.ToUpper(user.Name)] = true
		if user.ID != 0 {
			userNamesByID[user.ID] = user.Name
		}
	}
	schemaOwners := make(map[string]string)
	for i := range schemas {
		if schemas[i].Owner == "" {
			schemas[i].Owner = userNamesByID[schemas[i].OwnerID]
		}
		if schemas[i].Owner == "" {
			schemas[i].Owner = schemas[i].Name
		}
		schemaOwners[strings.ToUpper(schemas[i].Name)] = schemas[i].Owner
	}
	for _, table := range tables {
		key := strings.ToUpper(table.Owner)
		if key == "" || userNames[key] || schemaOwners[key] != "" {
			continue
		}
		users = append(users, DictionaryUser{Name: table.Owner})
		userNames[key] = true
	}
	sort.Slice(users, func(i, j int) bool {
		return users[i].Name < users[j].Name
	})
	if len(schemas) == 0 {
		for _, user := range users {
			schemas = append(schemas, DictionarySchema{ID: user.ID, Name: user.Name, OwnerID: user.ID, Owner: user.Name})
		}
	}
	sort.Slice(schemas, func(i, j int) bool {
		if schemas[i].Owner != schemas[j].Owner {
			return schemas[i].Owner < schemas[j].Owner
		}
		if schemas[i].Name != schemas[j].Name {
			return schemas[i].Name < schemas[j].Name
		}
		return schemas[i].ID < schemas[j].ID
	})
	sort.Slice(tables, func(i, j int) bool {
		if tables[i].Owner == tables[j].Owner {
			if tables[i].Name == tables[j].Name {
				return tables[i].ID < tables[j].ID
			}
			return tables[i].Name < tables[j].Name
		}
		return tables[i].Owner < tables[j].Owner
	})
	sort.Slice(columns, func(i, j int) bool {
		if columns[i].TableOwner != columns[j].TableOwner {
			return columns[i].TableOwner < columns[j].TableOwner
		}
		if columns[i].TableName != columns[j].TableName {
			return columns[i].TableName < columns[j].TableName
		}
		return columns[i].ColID < columns[j].ColID
	})
	sortDictionaryViews(views)
	sortDictionarySequences(sequences)
	sortDictionaryRoutines(routines)
	sortDictionaryTriggers(triggers)
	sortDictionarySynonyms(synonyms)
	sortDictionaryTabPrivileges(privileges)
	return users, schemas, tables, columns, views, sequences, routines, triggers, synonyms, privileges
}

func normalizeDictionaryPartitionsFromFiles(tables []DictionaryTable, columns []DictionaryColumn, partitions []DictionaryPartition, keys []DictionaryPartitionKey) ([]DictionaryPartition, []DictionaryPartitionKey) {
	tableIndexByID := make(map[uint32]int)
	tableIDByName := make(map[string]uint32)
	for i := range tables {
		tableIndexByID[tables[i].ID] = i
		tableIDByName[dictionaryOwnerTableNameKey(tables[i].Owner, tables[i].Name)] = tables[i].ID
	}
	columnByID := make(map[tableColKey]DictionaryColumn)
	columnIDByName := make(map[string]uint16)
	for _, column := range columns {
		columnByID[tableColKey{tableID: column.TableID, colID: column.ColID}] = column
		columnIDByName[fmt.Sprintf("%d\x00%s", column.TableID, strings.ToUpper(column.Name))] = column.ColID
	}
	partPositions := make(map[uint32]uint32)
	for i := range partitions {
		part := &partitions[i]
		if part.BaseTableID == 0 {
			part.BaseTableID = tableIDByName[dictionaryOwnerTableNameKey(part.Owner, part.TableName)]
		}
		if tableIndex, ok := tableIndexByID[part.BaseTableID]; ok {
			table := tables[tableIndex]
			if part.Owner == "" {
				part.Owner = table.Owner
			}
			if part.TableName == "" {
				part.TableName = table.Name
			}
			tables[tableIndex].Partitioned = true
		}
		if part.Position == 0 {
			partPositions[part.BaseTableID]++
			part.Position = partPositions[part.BaseTableID]
		} else if part.Position > partPositions[part.BaseTableID] {
			partPositions[part.BaseTableID] = part.Position
		}
		part.Type = strings.ToUpper(strings.TrimSpace(part.Type))
		part.HighValue = normalizePartitionHighValue(part.HighValue)
	}
	keyPositions := make(map[uint32]uint32)
	for i := range keys {
		key := &keys[i]
		if key.TableID == 0 {
			key.TableID = tableIDByName[dictionaryOwnerTableNameKey(key.Owner, key.TableName)]
		}
		if tableIndex, ok := tableIndexByID[key.TableID]; ok {
			table := tables[tableIndex]
			if key.Owner == "" {
				key.Owner = table.Owner
			}
			if key.TableName == "" {
				key.TableName = table.Name
			}
		}
		column, hasColumn := columnByID[tableColKey{tableID: key.TableID, colID: key.ColID}]
		if key.ColumnName != "" && (!hasColumn || !strings.EqualFold(column.Name, key.ColumnName)) {
			if colID, ok := columnIDByName[fmt.Sprintf("%d\x00%s", key.TableID, strings.ToUpper(key.ColumnName))]; ok {
				key.ColID = colID
				column = columnByID[tableColKey{tableID: key.TableID, colID: colID}]
				hasColumn = true
			}
		}
		if key.ColumnName == "" && hasColumn {
			key.ColumnName = column.Name
		}
		if key.Position == 0 {
			keyPositions[key.TableID]++
			key.Position = keyPositions[key.TableID]
		} else if key.Position > keyPositions[key.TableID] {
			keyPositions[key.TableID] = key.Position
		}
	}
	sort.Slice(partitions, func(i, j int) bool {
		if partitions[i].Owner != partitions[j].Owner {
			return partitions[i].Owner < partitions[j].Owner
		}
		if partitions[i].TableName != partitions[j].TableName {
			return partitions[i].TableName < partitions[j].TableName
		}
		return partitions[i].Position < partitions[j].Position
	})
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Owner != keys[j].Owner {
			return keys[i].Owner < keys[j].Owner
		}
		if keys[i].TableName != keys[j].TableName {
			return keys[i].TableName < keys[j].TableName
		}
		return keys[i].Position < keys[j].Position
	})
	return partitions, keys
}

func dictionaryOwnerTableNameKey(owner string, table string) string {
	return strings.ToUpper(strings.TrimSpace(owner)) + "\x00" + strings.ToUpper(strings.TrimSpace(table))
}

func writeTSV(path string, header []string, rows [][]string) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()
	writer := csv.NewWriter(file)
	writer.Comma = '\t'
	if len(header) > 0 {
		if err := writer.Write(header); err != nil {
			return err
		}
	}
	for _, row := range rows {
		if err := writer.Write(row); err != nil {
			return err
		}
	}
	writer.Flush()
	return writer.Error()
}

func readTSV(path string) ([][]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	reader := csv.NewReader(file)
	reader.Comma = '\t'
	reader.FieldsPerRecord = -1
	reader.Comment = '#'
	var records [][]string
	for {
		rec, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if len(rec) > 0 {
			rec[0] = strings.TrimPrefix(rec[0], "\ufeff")
		}
		records = append(records, rec)
	}
	return records, nil
}

func formatUint32Field(value uint32) string {
	if value == 0 {
		return ""
	}
	return strconv.FormatUint(uint64(value), 10)
}

func formatInt16Field(value int16) string {
	if value < 0 {
		return ""
	}
	return strconv.FormatInt(int64(value), 10)
}

func formatUint32ListField(values []uint32) string {
	if len(values) == 0 {
		return ""
	}
	seen := make(map[uint32]bool, len(values))
	var parts []string
	for _, value := range values {
		if value == 0 || seen[value] {
			continue
		}
		seen[value] = true
		parts = append(parts, strconv.FormatUint(uint64(value), 10))
	}
	return strings.Join(parts, ",")
}

func formatUint64Field(value uint64) string {
	if value == 0 {
		return ""
	}
	return strconv.FormatUint(value, 10)
}

func formatBoolField(value bool) string {
	if value {
		return "YES"
	}
	return "NO"
}

func parseMetaUint32(value string) uint32 {
	return parseUint32Field(value)
}

func parseMetaInt(value string) int {
	return parseIntField(value)
}

func parseUint32Field(value string) uint32 {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	parsed, err := strconv.ParseUint(value, 10, 32)
	if err != nil {
		return 0
	}
	return uint32(parsed)
}

func parseUint32ListField(value string) []uint32 {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	seen := make(map[uint32]bool)
	var result []uint32
	for _, part := range strings.FieldsFunc(value, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\t'
	}) {
		id := parseUint32Field(part)
		if id == 0 || seen[id] {
			continue
		}
		seen[id] = true
		result = append(result, id)
	}
	return result
}

func parseUint64Field(value string) uint64 {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	parsed, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return 0
	}
	return parsed
}

func parseIntField(value string) int {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0
	}
	return parsed
}

func parseOptionalInt16Field(value string, emptyValue int16) int16 {
	value = strings.TrimSpace(value)
	if value == "" {
		return emptyValue
	}
	parsed, err := strconv.ParseInt(value, 10, 16)
	if err != nil {
		return emptyValue
	}
	return int16(parsed)
}

func parseBoolField(value string) bool {
	switch strings.ToUpper(strings.TrimSpace(value)) {
	case "1", "T", "TRUE", "Y", "YES":
		return true
	default:
		return false
	}
}
