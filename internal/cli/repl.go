package cli

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
	"unicode"

	"dmdul/internal/dm"
	"dmdul/internal/version"
)

const replPrompt = "DMDUL> "

type interactiveSession struct {
	systemPath      string
	controlPath     string
	controlProvided bool
	dataDir         string
	dataDirSet      bool
	charset         string
	charsetExplicit bool
	dataFormat      string
	caseSensitive   string
	outputDir       string
	outputDirSet    bool
	logPath         string
	logPathSet      bool
	logOpenPath     string
	initSource      string
	pageCheckMode   uint32
	pageCheckSet    bool
	pageHashName    string
	metadata        dm.DatabaseMetadata
	dictionary      *dm.DictionaryInfo
	logFile         *os.File
	stderr          io.Writer
}

func RunInteractive(input io.Reader, stdout io.Writer, stderr io.Writer) error {
	session := newInteractiveSession()
	session.stderr = stderr
	session.loadConfigFile(stderr)
	if err := session.writeInitDUL(); err != nil {
		fmt.Fprintf(stderr, "warning: write init.dul: %v\n", err)
	}
	defer session.closeLog()

	fmt.Fprintf(stdout, "dmdul: Release %s - Dameng Database Offline Recovery & Data Unloader\n", version.Version)
	fmt.Fprintln(stdout, "Copyright (c) 2026 greatfinish. All rights reserved.")
	fmt.Fprintln(stdout, "https://github.com/greatfinish/dmdul")
	fmt.Fprintln(stdout, "Type help; for available commands.")

	session.autoProbeStartupSystem(stdout)

	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 0, 4096), 1024*1024)
	for {
		fmt.Fprint(stdout, replPrompt)
		if !scanner.Scan() {
			fmt.Fprintln(stdout)
			break
		}
		line := strings.TrimSpace(scanner.Text())
		for _, command := range splitInteractiveCommands(line) {
			exit, err := session.execute(command, stdout)
			if err != nil {
				fmt.Fprintf(stdout, "error: %v\n", err)
				session.log(replPrompt + command)
				session.log("ERROR " + err.Error())
				continue
			}
			session.log(replPrompt + command)
			if err := session.writeInitDUL(); err != nil {
				fmt.Fprintf(stderr, "warning: write init.dul: %v\n", err)
			}
			if exit {
				return nil
			}
		}
	}
	return scanner.Err()
}

func newInteractiveSession() *interactiveSession {
	session := &interactiveSession{
		systemPath:    defaultSystemFilePath(),
		charset:       "auto",
		dataFormat:    "sql",
		caseSensitive: "auto",
	}
	session.resetDatabaseMetadata()
	return session
}

// defaultSystemFilePath is the default SYSTEM.DBF location: next to the dmdul
// executable, so dropping the offline files beside the binary works with no
// setup. effectiveDataDir derives the default data_dir from this path's
// directory, so both default to the executable's folder.
func defaultSystemFilePath() string {
	if exe, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(exe), defaultSystemPath)
	}
	return defaultSystemPath
}

func (s *interactiveSession) execute(command string, stdout io.Writer) (bool, error) {
	command = strings.TrimSpace(strings.TrimSuffix(command, ";"))
	if command == "" {
		return false, nil
	}
	fields := splitCommandFields(command)
	if len(fields) == 0 {
		return false, nil
	}

	switch strings.ToLower(fields[0]) {
	case "help", "?":
		printInteractiveHelp(stdout)
	case "exit", "quit":
		fmt.Fprintln(stdout, "bye")
		return true, nil
	case "set":
		return false, s.executeSet(fields[1:], stdout)
	case "show":
		return false, s.executeShow(fields[1:], stdout)
	case "bootstrap":
		return false, s.bootstrap(stdout)
	case "load":
		return false, s.executeLoad(fields[1:], stdout)
	case "list":
		return false, s.executeList(fields[1:], stdout)
	case "unload":
		return false, s.executeUnload(fields[1:], stdout)
	case "recover":
		return false, s.executeRecover(fields[1:], stdout)
	case "check":
		return false, s.executeCheck(fields[1:], stdout)
	case "describe", "desc":
		return false, s.executeDescribe(fields[1:], stdout)
	default:
		return false, fmt.Errorf("unknown command %q", fields[0])
	}
	return false, nil
}

func printInteractiveHelp(stdout io.Writer) {
	fmt.Fprintln(stdout, "Commands:")
	fmt.Fprintln(stdout, "  bootstrap;")
	fmt.Fprintln(stdout, "      Load dictionary metadata from SYSTEM.DBF and write text dictionary files.")
	fmt.Fprintln(stdout, "  load dictionary;")
	fmt.Fprintln(stdout, "      Load dictionary metadata from dmdul_dict text files.")
	fmt.Fprintln(stdout, "  load init;")
	fmt.Fprintln(stdout, "      Reload parameters from the effective init.dul file.")
	fmt.Fprintln(stdout, "  load parameter;")
	fmt.Fprintln(stdout, "      Reload configuration and persisted bootstrap parameters from init.dul.")
	fmt.Fprintln(stdout, "  list datafile;")
	fmt.Fprintln(stdout, "      List resolved data files with tablespace, page count and read status (pre-flight check).")
	fmt.Fprintln(stdout, "  describe <owner.table_name>;")
	fmt.Fprintln(stdout, "      Show one table's recovered definition, physical location and columns (alias: desc).")
	fmt.Fprintln(stdout, "  list user;")
	fmt.Fprintln(stdout, "      List recovered users/owners and dictionary cache status.")
	fmt.Fprintln(stdout, "  list table <owner>;")
	fmt.Fprintln(stdout, "      List tables in one recovered schema.")
	fmt.Fprintln(stdout, "  list schema [owner];")
	fmt.Fprintln(stdout, "      List recovered schemas and their owning users.")
	fmt.Fprintln(stdout, "  unload table <owner.table_name>;")
	fmt.Fprintln(stdout, "      Export one table; DMP mode writes one native TABLES file with metadata and data.")
	fmt.Fprintln(stdout, "  unload object <owner|all>;")
	fmt.Fprintln(stdout, "      Export recovered dictionary DDL for one owner or the whole database.")
	fmt.Fprintln(stdout, "  unload user <owner>;")
	fmt.Fprintln(stdout, "      Export each owned table; DMP mode writes one native OWNER file.")
	fmt.Fprintln(stdout, "  unload schema <schema>[,<schema>...];")
	fmt.Fprintln(stdout, "      DMP mode: export one or more schemas to one native SCHEMAS file.")
	fmt.Fprintln(stdout, "  unload database;")
	fmt.Fprintln(stdout, "      Export all recovered objects; DMP mode writes one native FULL file.")
	fmt.Fprintln(stdout, "  recover table <owner.table_name>;")
	fmt.Fprintln(stdout, "      Scan residual pages by storage/assist id for TRUNCATE/DROP table recovery.")
	fmt.Fprintln(stdout, "  check pages [<dbf-name>[,<dbf-name>...]] [control];")
	fmt.Fprintln(stdout, "      Scan data files for corrupt pages (checksum + header + structure). Read-only.")
	fmt.Fprintln(stdout, "      Files are located in data_dir; add 'control' to follow dm.ctl absolute paths.")
	fmt.Fprintln(stdout, "  set system <SYSTEM.DBF path>;")
	fmt.Fprintln(stdout, "  set data_dir <DBF directory>;")
	fmt.Fprintln(stdout, "  set control <dm.ctl path>;")
	fmt.Fprintln(stdout, "  set output_dir <directory>;")
	fmt.Fprintln(stdout, "      Unload/recovery output directory. Defaults to ./output under the launch directory.")
	fmt.Fprintln(stdout, "  set data_format sql|fldr|dmp;")
	fmt.Fprintln(stdout, "      fldr writes a delimited .txt plus a dmfldr .ctl control file per table.")
	fmt.Fprintln(stdout, "  set case_sensitive auto|0|1;")
	fmt.Fprintln(stdout, "  set charset auto|utf-8|gb18030|gbk|euc-kr;")
	fmt.Fprintln(stdout, "  show parameter;")
	fmt.Fprintln(stdout, "  exit;")
}

func (s *interactiveSession) executeRecover(args []string, stdout io.Writer) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: recover table <owner.table_name>")
	}
	if err := s.ensureDictionaryLoaded(); err != nil {
		return err
	}
	switch strings.ToLower(args[0]) {
	case "table":
		if len(args) < 2 {
			return fmt.Errorf("usage: recover table <owner.table_name>")
		}
		return s.recoverTable(args[1:], stdout)
	default:
		return fmt.Errorf("usage: recover table <owner.table_name>")
	}
}

// executeCheck scans data files for corrupt pages (the `check` command).
// Usage: check pages [<dbf-name>[,<dbf-name>...]] [control]
//
// By default files are located strictly inside data_dir (safe: it never
// follows dm.ctl absolute paths that may point at the live database). The
// optional trailing `control` keyword opts into control-path resolution for
// databases whose DBF files are spread across directories.
func (s *interactiveSession) executeCheck(args []string, stdout io.Writer) error {
	target := "pages"
	if len(args) > 0 {
		target = strings.ToLower(args[0])
	}
	if target != "pages" && target != "page" {
		return fmt.Errorf("usage: check pages [<dbf-name>[,<dbf-name>...]] [control]")
	}
	if strings.TrimSpace(s.systemPath) == "" && !s.dataDirSet {
		return fmt.Errorf("check requires set system or set data_dir first")
	}
	rest := args[1:]
	followControl := false
	if n := len(rest); n > 0 && strings.EqualFold(rest[n-1], "control") {
		followControl = true
		rest = rest[:n-1]
	}
	var fileFilter []string
	if len(rest) > 0 {
		fileFilter = splitIdentifierList(rest[0])
	}
	pageSize := s.metadata.PageSize
	if pageSize == 0 {
		pageSize = dm.DefaultPageSize
	}
	// Attach the dictionary when available so bad pages are attributed to
	// owner.table and the leaf-chain and consistency checks run. It is loaded
	// best-effort: a missing dictionary just falls back to the physical scan.
	dict := s.dictionary
	if dict == nil {
		if loaded, err := s.tryLoadDictionaryForCheck(); err == nil {
			dict = loaded
		}
	}
	result, err := dm.CheckPages(dm.PageCheckOptions{
		SystemPath:         s.systemPath,
		ControlPath:        s.controlPath,
		ControlDULPath:     s.effectiveControlDULPath(),
		DataDir:            s.effectiveDataDir(),
		PageSize:           pageSize,
		PageCheckMode:      s.pageCheckMode,
		PageHashName:       s.pageHashName,
		FileFilter:         fileFilter,
		Dictionary:         dict,
		FollowControlPaths: followControl,
	})
	if err != nil {
		return err
	}
	s.printPageCheckResult(result, stdout)
	return nil
}

// tryLoadDictionaryForCheck loads the on-disk dictionary without failing the
// check when none exists.
func (s *interactiveSession) tryLoadDictionaryForCheck() (*dm.DictionaryInfo, error) {
	dict, _, err := dm.LoadDictionaryFiles(s.effectiveDictionaryDir())
	return dict, err
}

func (s *interactiveSession) printPageCheckResult(result *dm.PageCheckResult, stdout io.Writer) {
	fmt.Fprintf(stdout, "page size: %d bytes\n", result.PageSize)
	if result.PageCheckMode == 0 {
		fmt.Fprintln(stdout, "PAGE_CHECK: 0 (checksum disabled; using header + structure evidence)")
	} else {
		fmt.Fprintf(stdout, "PAGE_CHECK: %d\n", result.PageCheckMode)
	}
	for i := range result.Files {
		file := &result.Files[i]
		status := "OK"
		if file.SizeInvalid {
			status = "FILE-INVALID"
		} else if file.BadPages > 0 {
			status = "BAD"
		}
		fmt.Fprintf(stdout, "[%s] %s (group=%d file=%d tablespace=%s) pages=%d empty=%d bad=%d\n",
			status, file.Path, file.GroupID, file.FileID, defaultIfBlank(file.Tablespace, "?"),
			file.PagesChecked, file.PagesEmpty, file.BadPages)
		if file.SizeInvalid && file.SizeDetail != "" {
			fmt.Fprintf(stdout, "    file: %s\n", file.SizeDetail)
		}
		for _, bad := range file.Bad {
			owner := ""
			if bad.Owner != "" || bad.Table != "" {
				owner = fmt.Sprintf(" table=%s.%s", bad.Owner, bad.Table)
			}
			fmt.Fprintf(stdout, "    page(%d,%d,%d) %s storage_id=%d%s: %s\n",
				bad.GroupID, bad.FileID, bad.PageNo, bad.Kind, bad.StorageID, owner, bad.Detail)
		}
		if file.ReportTruncated {
			fmt.Fprintf(stdout, "    ... bad-page list truncated; %d total in this file\n", file.BadPages)
		}
	}
	if result.DictionaryUsed {
		if len(result.ChainIssues) > 0 {
			fmt.Fprintln(stdout, "B-tree leaf chain issues:")
			for _, issue := range result.ChainIssues {
				fmt.Fprintf(stdout, "    %s.%s storage_id=%d root=%d/%d: %s\n",
					issue.Owner, issue.Table, issue.StorageID, issue.RootFile, issue.RootPage, issue.Reason)
			}
		}
		if len(result.DictIssues) > 0 {
			fmt.Fprintln(stdout, "dictionary consistency issues:")
			for _, issue := range result.DictIssues {
				fmt.Fprintf(stdout, "    [%s] %s\n", issue.Category, issue.Detail)
			}
		}
	}
	fmt.Fprintf(stdout, "files checked: %d\n", result.FilesChecked)
	fmt.Fprintf(stdout, "pages checked: %d (empty: %d)\n", result.PagesChecked, result.PagesEmpty)
	fmt.Fprintf(stdout, "bad pages total: %d\n", result.BadPagesTotal)
	if result.BadPagesTotal > 0 {
		fmt.Fprintf(stdout, "  header invalid: %d\n", result.Corruption[dm.PageCorruptionHeader])
		fmt.Fprintf(stdout, "  checksum fail: %d\n", result.Corruption[dm.PageCorruptionChecksum])
		fmt.Fprintf(stdout, "  structure invalid: %d\n", result.Corruption[dm.PageCorruptionStructure])
	}
	if result.DictionaryUsed {
		fmt.Fprintf(stdout, "leaf chain issues: %d\n", len(result.ChainIssues))
		fmt.Fprintf(stdout, "dictionary issues: %d\n", len(result.DictIssues))
	}
	s.log(fmt.Sprintf("[CHECK] files=%d pages=%d empty=%d bad=%d header=%d checksum=%d structure=%d chain=%d dict=%d",
		result.FilesChecked, result.PagesChecked, result.PagesEmpty, result.BadPagesTotal,
		result.Corruption[dm.PageCorruptionHeader], result.Corruption[dm.PageCorruptionChecksum],
		result.Corruption[dm.PageCorruptionStructure], len(result.ChainIssues), len(result.DictIssues)))
	for _, bad := range result.SortedBadPages() {
		table := ""
		if bad.Owner != "" || bad.Table != "" {
			table = fmt.Sprintf(" table=%s.%s", bad.Owner, bad.Table)
		}
		s.log(fmt.Sprintf("[CHECK] page(%d,%d,%d) %s storage_id=%d%s %s",
			bad.GroupID, bad.FileID, bad.PageNo, bad.Kind, bad.StorageID, table, bad.Detail))
	}
	for _, issue := range result.ChainIssues {
		s.log(fmt.Sprintf("[CHECK] chain %s.%s storage_id=%d: %s", issue.Owner, issue.Table, issue.StorageID, issue.Reason))
	}
	for _, issue := range result.DictIssues {
		s.log(fmt.Sprintf("[CHECK] dict [%s] %s", issue.Category, issue.Detail))
	}
}

func (s *interactiveSession) executeSet(args []string, stdout io.Writer) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: set <parameter> <value>")
	}
	name := normalizeParameterName(args[0])
	value := strings.Join(args[1:], " ")
	switch name {
	case "system", "file":
		s.systemPath = value
		s.dictionary = nil
		s.charset = "auto"
		s.charsetExplicit = false
		s.resetDatabaseMetadata()
		fmt.Fprintf(stdout, "%s = %s\n", name, value)
		s.probeDatabaseIdentity(stdout, true)
		return nil
	case "data_dir", "datadir":
		s.dataDir = value
		s.dataDirSet = strings.TrimSpace(value) != ""
		s.dictionary = nil
	case "control", "ctl":
		s.controlPath = value
		s.controlProvided = strings.TrimSpace(value) != ""
		s.dictionary = nil
		s.resetDatabaseMetadata()
	case "output_dir", "outdir":
		s.outputDir = value
		s.outputDirSet = true
	case "data_format", "format":
		value = strings.ToLower(strings.TrimSpace(value))
		if value != "sql" && value != "csv" && value != "fldr" && value != "dmp" {
			return fmt.Errorf("data_format must be sql, fldr, or dmp")
		}
		if value == "csv" {
			// The delimited export is now the dmfldr text format; keep "csv"
			// accepted as an alias but report the format that is produced.
			value = "fldr"
		}
		s.dataFormat = value
	case "case_sensitive":
		normalized, ok := normalizeCaseSensitiveParameter(value)
		if !ok {
			return fmt.Errorf("case_sensitive must be auto, 0, or 1")
		}
		value = normalized
		s.caseSensitive = normalized
	case "charset":
		s.charset = value
		s.charsetExplicit = true
		s.dictionary = nil
	case "log":
		s.logPath = value
		s.logPathSet = strings.TrimSpace(value) != ""
	case "page_check", "pagecheck":
		mode, err := parsePageCheckMode(value)
		if err != nil {
			return err
		}
		s.pageCheckMode = mode
		s.pageCheckSet = true
	case "page_hash", "pagehash":
		s.pageHashName = strings.TrimSpace(value)
	default:
		return fmt.Errorf("unknown parameter %q", args[0])
	}
	fmt.Fprintf(stdout, "%s = %s\n", name, value)
	return nil
}

func parsePageCheckMode(value string) (uint32, error) {
	switch strings.TrimSpace(value) {
	case "0":
		return 0, nil
	case "1":
		return 1, nil
	case "2":
		return 2, nil
	case "3":
		return 3, nil
	default:
		return 0, fmt.Errorf("page_check must be 0 (off), 1 (CRC32), 2 (HASH), or 3 (CRC32C)")
	}
}

func (s *interactiveSession) executeShow(args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: show parameter")
	}
	switch strings.ToLower(args[0]) {
	case "parameter", "parameters":
		s.printParameters(stdout)
		return nil
	default:
		return fmt.Errorf("usage: show parameter")
	}
}

func (s *interactiveSession) executeList(args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: list datafile | list user | list schema [owner] | list table <owner>")
	}
	// list datafile inspects physical files and must work before bootstrap,
	// so it does not require a loaded dictionary.
	switch strings.ToLower(args[0]) {
	case "datafile", "datafiles", "file", "files":
		return s.printDataFiles(stdout)
	}
	if err := s.ensureDictionaryLoaded(); err != nil {
		return err
	}
	switch strings.ToLower(args[0]) {
	case "user", "users":
		s.printUsers(stdout)
	case "table", "tables":
		if len(args) < 2 {
			return fmt.Errorf("usage: list table <owner>")
		}
		s.printTables(stdout, args[1])
	case "schema", "schemas":
		owner := ""
		if len(args) > 1 {
			owner = args[1]
		}
		s.printSchemas(stdout, owner)
	default:
		return fmt.Errorf("usage: list datafile | list user | list schema [owner] | list table <owner>")
	}
	return nil
}

// executeDescribe prints one table's recovered definition and where it lives on
// disk (the spirit of DUL's "desc owner.table"): physical identity first, so a
// recovery operator can confirm the table was located, then the column list.
func (s *interactiveSession) executeDescribe(args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: describe <owner.table_name>")
	}
	owner, tableName, ok := parseOwnerTableToken(args[0])
	if !ok {
		return fmt.Errorf("invalid table name %q; use owner.table_name", args[0])
	}
	if err := s.ensureDictionaryLoaded(); err != nil {
		return err
	}
	table, found := s.findTable(owner, tableName)
	if !found {
		return fmt.Errorf("table %s.%s not found in dictionary", owner, tableName)
	}
	s.printTableDescription(stdout, table)
	return nil
}

func (s *interactiveSession) printTableDescription(stdout io.Writer, table dm.DictionaryTable) {
	tablespace := table.Tablespace
	if tablespace == "" && table.GroupID != 0 {
		tablespace = fmt.Sprintf("GROUP_%d", table.GroupID)
	}
	fmt.Fprintf(stdout, "Table %s.%s\n", table.Owner, table.Name)
	fmt.Fprintf(stdout, "  table_id= %d, tablespace= %s (group#= %d), columns= %d\n",
		table.ID, defaultIfBlank(tablespace, "?"), table.GroupID, table.ColumnCount)
	fmt.Fprintf(stdout, "  storage_id= %d, root= file#%d/page#%d, segment= file#%d/block#%d, blocks= %d, extents= %d\n",
		table.StorageID, table.RootFile, table.RootPage,
		table.HeaderFile, table.HeaderBlock, table.Blocks, table.Extents)
	attrs := defaultIfBlank(table.Storage, "?")
	if table.Partitioned {
		attrs += ", PARTITIONED"
	}
	if table.Temporary {
		attrs += ", TEMPORARY"
	}
	fmt.Fprintf(stdout, "  storage= %s, bytes= %d\n", attrs, table.Bytes)
	if len(table.AssistIDs) > 0 {
		fmt.Fprintf(stdout, "  assist_ids= %v\n", table.AssistIDs)
	}

	fmt.Fprintln(stdout, "Column information:")
	shown := 0
	for _, col := range s.dictionary.Columns {
		if col.TableID != table.ID {
			continue
		}
		shown++
		nullable := "NULL"
		if col.Nullable == "N" {
			nullable = "NOT NULL"
		}
		line := fmt.Sprintf("  col# %02d %-32s %-28s %-8s",
			col.ColID, col.Name, formatDescribeType(col), nullable)
		if strings.TrimSpace(col.Default) != "" {
			line += " DEFAULT " + col.Default
		}
		fmt.Fprintln(stdout, strings.TrimRight(line, " "))
	}
	if shown == 0 {
		fmt.Fprintln(stdout, "  (no columns recovered for this table)")
	}

	// Partition detail helps confirm which physical sub-tables carry the data.
	parts := 0
	for _, part := range s.dictionary.Partitions {
		if part.BaseTableID != table.ID {
			continue
		}
		if parts == 0 {
			fmt.Fprintln(stdout, "Partitions:")
		}
		parts++
		fmt.Fprintf(stdout, "  #%d %-24s type= %-6s part_table_id= %d\n",
			part.Position, part.Name, part.Type, part.PartTableID)
	}
}

// formatDescribeType renders the recovered column type with its length/scale,
// mirroring how the DDL writer would emit it.
func formatDescribeType(col dm.DictionaryColumn) string {
	base := strings.TrimSpace(col.DataType)
	if base == "" {
		return "?"
	}
	return fmt.Sprintf("%s len=%d scale=%d", base, col.Length, col.Scale)
}

// printDataFiles lists every resolved offline data file with its tablespace,
// group/file id, page count and read status — a pre-flight check that all DBFs
// were identified and are readable, in the spirit of DUL's "show datafiles".
func (s *interactiveSession) printDataFiles(stdout io.Writer) error {
	files, err := dm.ScanOfflineDataFiles(s.controlPath, s.effectiveControlDULPath(), s.effectiveDataDir())
	if err != nil {
		return err
	}
	if len(files) == 0 {
		fmt.Fprintf(stdout, "no data files found under %s\n", s.effectiveDataDir())
		return nil
	}
	pageSize := s.metadata.PageSize
	if pageSize == 0 {
		pageSize = dm.DefaultPageSize
	}
	spaces := make([]string, 0, len(files))
	paths := make([]string, 0, len(files))
	for _, f := range files {
		spaces = append(spaces, defaultIfBlank(f.Tablespace, "?"))
		paths = append(paths, f.Path)
	}
	format := printListHeader(stdout, []listColumn{
		{title: "group", width: 5, right: true},
		{title: "file", width: 5, right: true},
		{title: "tablespace", width: widestValue("tablespace", spaces)},
		{title: "pages", width: 10, right: true},
		{title: "size_MB", width: 9, right: true},
		{title: "status", width: 10},
		{title: "path", width: widestValue("path", paths)},
	})
	okCount := 0
	for _, f := range files {
		pages := int64(0)
		sizeMB := 0.0
		status := "OK"
		if info, statErr := os.Stat(f.Path); statErr != nil {
			status = "UNREADABLE"
		} else if info.IsDir() {
			status = "DIR?"
		} else {
			size := info.Size()
			sizeMB = float64(size) / (1024 * 1024)
			pages = size / int64(pageSize)
			if size%int64(pageSize) != 0 {
				status = "SIZE?" // not a whole number of pages (truncated / wrong page size)
			} else {
				okCount++
			}
		}
		fmt.Fprintf(stdout, format,
			fmt.Sprint(f.GroupID), fmt.Sprint(f.FileID), defaultIfBlank(f.Tablespace, "?"),
			fmt.Sprint(pages), fmt.Sprintf("%.1f", sizeMB), status, f.Path)
	}
	fmt.Fprintf(stdout, "data files: %d (readable & page-aligned: %d), page_size=%d\n", len(files), okCount, pageSize)
	return nil
}

func (s *interactiveSession) executeLoad(args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: load dictionary | load parameter")
	}
	switch strings.ToLower(args[0]) {
	case "dictionary":
		return s.loadDictionaryFiles(stdout)
	case "init":
		return s.loadInitDULCommand(stdout, "init")
	case "parameter", "parameters":
		return s.loadInitDULCommand(stdout, "parameter")
	default:
		return fmt.Errorf("usage: load dictionary | load parameter")
	}
}

func (s *interactiveSession) executeUnload(args []string, stdout io.Writer) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: unload table <owner.table_name> | unload object <owner|all> | unload user <owner> | unload schema <schema> | unload database")
	}
	if err := s.ensureDictionaryLoaded(); err != nil {
		return err
	}
	switch strings.ToLower(args[0]) {
	case "table":
		if len(args) < 2 {
			return fmt.Errorf("usage: unload table <owner.table_name>")
		}
		return s.unloadTable(args[1:], stdout)
	case "object", "objects":
		if len(args) < 2 {
			return fmt.Errorf("usage: unload object <owner|all>")
		}
		return s.unloadObject(args[1:], stdout)
	case "user":
		if len(args) < 2 {
			return fmt.Errorf("usage: unload user <owner>")
		}
		return s.unloadUser(args[1:], stdout)
	case "schema", "schemas":
		if len(args) < 2 {
			return fmt.Errorf("usage: unload schema <schema>[,<schema>...]")
		}
		return s.unloadSchema(args[1:], stdout)
	case "database", "db":
		return s.unloadDatabase(args[1:], stdout)
	default:
		return fmt.Errorf("usage: unload table <owner.table_name> | unload object <owner|all> | unload user <owner> | unload schema <schema> | unload database")
	}
}

func (s *interactiveSession) bootstrap(stdout io.Writer) error {
	startedAt := time.Now()
	// Never retain an older in-memory dictionary after a fresh bootstrap was
	// requested. If the scan fails, unload must require an explicit reload.
	s.dictionary = nil
	systemPath := defaultIfBlank(s.systemPath, defaultSystemPath)
	ctlPath, ctlProvided := optionalControlPathForSystem(systemPath, s.controlPath, s.controlProvided)
	if err := validateOptionalControlInputFiles("bootstrap", systemPath, ctlPath, ctlProvided); err != nil {
		return err
	}
	dataDir := s.effectiveDataDir()
	controlDULPath := s.effectiveControlDULPath()
	s.emitBootstrapLine(stdout, fmt.Sprintf("[bootstrap] phase=start status=RUNNING system=%q data_dir=%q", systemPath, dataDir))
	dataFiles, err := dm.ScanOfflineDataFiles(ctlPath, "", dataDir)
	if err != nil {
		return err
	}
	if err := dm.WriteControlDUL(controlDULPath, dataFiles); err != nil {
		return fmt.Errorf("write control.dul: %w", err)
	}
	configuredCharset := defaultIfBlank(s.charset, "auto")
	metadata := dm.InspectDatabaseMetadata(systemPath, ctlPath, "", configuredCharset)
	bootstrapCharset := configuredCharset
	if !s.charsetExplicit && metadata.HasCharsetFlag {
		if detected, ok := charsetParameterFromDictionary(metadata.Charset); ok {
			bootstrapCharset = detected
		}
	}
	caseSensitiveLog := "unknown"
	if metadata.HasCaseSensitive {
		caseSensitiveLog = "0"
		if metadata.CaseSensitive {
			caseSensitiveLog = "1"
		}
	}
	s.emitBootstrapLine(stdout, fmt.Sprintf("[bootstrap] phase=metadata status=OK db_name=%q db_source=%q instance_name=%q instance_source=%q page_size=%d page_size_source=%q extent_size=%d extent_size_source=%q page_count=%d page_count_source=%q charset=%q charset_source=%q unicode_flag=%d case_sensitive=%s case_sensitive_source=%q",
		metadata.DatabaseName, metadata.DatabaseNameSrc, metadata.InstanceName, metadata.InstanceNameSrc,
		metadata.PageSize, metadata.PageSizeSource, metadata.ExtentSize, metadata.ExtentSizeSource,
		metadata.PageCount, metadata.PageCountSource, metadata.Charset, metadata.CharsetSource,
		metadata.CharsetFlag, caseSensitiveLog, metadata.CaseSensitiveSource))
	fileWarnings := false
	for _, file := range dataFiles {
		line, warning := formatBootstrapFileDiagnostic(file, metadata.PageSize)
		fileWarnings = fileWarnings || warning
		s.emitBootstrapLine(stdout, line)
	}
	dict, err := dm.LoadDictionary(dm.DictionaryOptions{
		SystemPath:     systemPath,
		ControlPath:    ctlPath,
		ControlDULPath: controlDULPath,
		OwnerFilter:    "all",
		Charset:        bootstrapCharset,
		Diagnostic: func(diag dm.BootstrapDiagnostic) {
			s.emitBootstrapLine(stdout, formatBootstrapDiagnostic(diag))
		},
	})
	if err != nil {
		return err
	}
	dictDir := s.effectiveDictionaryDir()
	dict.Source = "SYSTEM.DBF"
	dict.DictionaryDir = dictDir
	dictFiles, dictionaryBackupDir, err := dm.RebuildDictionaryFiles(dictDir, dict)
	if err != nil {
		return fmt.Errorf("write dictionary files: %w", err)
	}
	s.systemPath = systemPath
	s.controlPath = ctlPath
	s.controlProvided = ctlProvided
	s.metadata = metadata
	s.dictionary = dict
	if detectedCharset, ok := charsetParameterFromDictionary(dict.Charset); ok {
		s.charset = detectedCharset
		s.charsetExplicit = false
	}
	debug.FreeOSMemory()
	status := "SUCCESS"
	if dict.BootstrapFallback || fileWarnings {
		status = "SUCCESS_WITH_WARNINGS"
	}
	s.emitBootstrapLine(stdout, fmt.Sprintf("[bootstrap] phase=output name=control.dul status=OK files=%d path=%q", len(dataFiles), controlDULPath))
	if dictionaryBackupDir != "" {
		s.emitBootstrapLine(stdout, fmt.Sprintf("[bootstrap] phase=output name=dmdul_dict-backup status=OK path=%q", dictionaryBackupDir))
	}
	s.emitBootstrapLine(stdout, fmt.Sprintf("[bootstrap] phase=output name=dmdul_dict status=OK users=%d schemas=%d tables=%d columns=%d views=%d sequences=%d routines=%d triggers=%d synonyms=%d tab_privs=%d partitions=%d partition_keys=%d path=%q",
		dictFiles.UserCount, dictFiles.SchemaCount, dictFiles.TableCount, dictFiles.ColumnCount, dictFiles.ViewCount, dictFiles.SequenceCount,
		dictFiles.RoutineCount, dictFiles.TriggerCount, dictFiles.SynonymCount, dictFiles.TabPrivilegeCount,
		dictFiles.PartitionCount, dictFiles.PartitionKeyCount, dictFiles.Dir))
	s.emitBootstrapLine(stdout, fmt.Sprintf("[bootstrap] phase=complete status=%s mode=%s objects=%d elapsed_ms=%d",
		status, dict.BootstrapMode, dict.ObjectCount, time.Since(startedAt).Milliseconds()))

	fmt.Fprintln(stdout, "bootstrap completed")
	fmt.Fprintf(stdout, "system file: %s\n", dict.SystemPath)
	if dict.ControlPath != "" {
		fmt.Fprintf(stdout, "control file: %s\n", dict.ControlPath)
	}
	fmt.Fprintf(stdout, "control.dul: %s (data files: %d)\n", controlDULPath, len(dataFiles))
	fmt.Fprintf(stdout, "dictionary dir: %s (users=%d schemas=%d tables=%d columns=%d views=%d sequences=%d routines=%d triggers=%d synonyms=%d tab_privs=%d partitions=%d partition_keys=%d)\n",
		dictFiles.Dir, dictFiles.UserCount, dictFiles.SchemaCount, dictFiles.TableCount, dictFiles.ColumnCount, dictFiles.ViewCount, dictFiles.SequenceCount, dictFiles.RoutineCount, dictFiles.TriggerCount, dictFiles.SynonymCount, dictFiles.TabPrivilegeCount, dictFiles.PartitionCount, dictFiles.PartitionKeyCount)
	if dictionaryBackupDir != "" {
		fmt.Fprintf(stdout, "previous dictionary backup: %s\n", dictionaryBackupDir)
	}
	fmt.Fprintf(stdout, "database name: %s (%s)\n", metadata.DatabaseName, metadata.DatabaseNameSrc)
	fmt.Fprintf(stdout, "instance name: %s (%s)\n", metadata.InstanceName, metadata.InstanceNameSrc)
	fmt.Fprintf(stdout, "page size: %d bytes\n", dict.PageSize)
	fmt.Fprintf(stdout, "extent size: %d pages (%s)\n", dict.ExtentSize, dict.ExtentSizeSource)
	fmt.Fprintf(stdout, "page count: %d\n", dict.PageCount)
	fmt.Fprintf(stdout, "charset: %s (%s)\n", dict.Charset, dict.CharsetSource)
	fmt.Fprintf(stdout, "unicode flag: %d (%s)\n", metadata.CharsetFlag, metadata.CharsetSource)
	fmt.Fprintf(stdout, "charset parameter: %s\n", s.charset)
	if dict.HasCaseSensitive {
		caseSensitive := 0
		if dict.CaseSensitive {
			caseSensitive = 1
		}
		fmt.Fprintf(stdout, "case sensitive: %d (%s)\n", caseSensitive, dict.CaseSensitiveSource)
	}
	fmt.Fprintf(stdout, "objects loaded: %d\n", dict.ObjectCount)
	fmt.Fprintf(stdout, "users loaded: %d\n", dict.UserCount)
	fmt.Fprintf(stdout, "tables loaded: %d\n", dict.TableCount)
	fmt.Fprintf(stdout, "columns loaded: %d\n", dict.ColumnCount)
	fmt.Fprintf(stdout, "views loaded: %d\n", dict.ViewCount)
	fmt.Fprintf(stdout, "sequences loaded: %d\n", dict.SequenceCount)
	fmt.Fprintf(stdout, "routines loaded: %d\n", dict.RoutineCount)
	fmt.Fprintf(stdout, "triggers loaded: %d\n", dict.TriggerCount)
	fmt.Fprintf(stdout, "synonyms loaded: %d\n", dict.SynonymCount)
	fmt.Fprintf(stdout, "tab privileges loaded: %d\n", dict.TabPrivilegeCount)
	fmt.Fprintf(stdout, "partitions loaded: %d\n", dict.PartitionCount)
	fmt.Fprintf(stdout, "partition keys loaded: %d\n", dict.PartitionKeyCount)
	return nil
}

func (s *interactiveSession) emitBootstrapLine(stdout io.Writer, line string) {
	fmt.Fprintln(stdout, line)
	s.log(line)
}

func formatBootstrapDiagnostic(diag dm.BootstrapDiagnostic) string {
	var out strings.Builder
	out.WriteString("[bootstrap]")
	if diag.Stage > 0 {
		fmt.Fprintf(&out, " stage=%d", diag.Stage)
	}
	if diag.Phase != "" {
		fmt.Fprintf(&out, " phase=%s", diag.Phase)
	}
	if diag.Name != "" {
		fmt.Fprintf(&out, " name=%s", diag.Name)
	}
	if diag.Mode != "" {
		fmt.Fprintf(&out, " mode=%s", diag.Mode)
	}
	if diag.Status != "" {
		fmt.Fprintf(&out, " status=%s", diag.Status)
	}
	if diag.RootFile >= 0 {
		fmt.Fprintf(&out, " root=%d/%d", diag.RootFile, diag.RootPage)
	}
	if diag.StorageID != 0 {
		fmt.Fprintf(&out, " storage=%d", diag.StorageID)
	}
	if diag.Pages > 0 {
		fmt.Fprintf(&out, " pages=%d", diag.Pages)
	}
	if diag.Rows > 0 || diag.Phase == "anchor" || diag.Phase == "extract" || diag.Phase == "source" || diag.Phase == "validate" {
		fmt.Fprintf(&out, " rows=%d", diag.Rows)
	}
	if diag.Reason != "" {
		fmt.Fprintf(&out, " reason=%q", diag.Reason)
	}
	return out.String()
}

func formatBootstrapFileDiagnostic(file dm.OfflineDataFile, pageSize uint32) (string, bool) {
	status := "OK"
	var size int64
	if info, err := os.Stat(file.Path); err == nil && !info.IsDir() {
		size = info.Size()
	} else {
		status = "MISSING"
	}
	pages := int64(0)
	aligned := false
	if pageSize > 0 && size >= 0 {
		pages = size / int64(pageSize)
		aligned = size%int64(pageSize) == 0
	}
	if status == "OK" && !aligned {
		status = "UNALIGNED"
	}
	headerGroup, headerFile, headerPage := uint16(0), uint16(0), uint32(0)
	if input, err := os.Open(file.Path); err == nil {
		var header [8]byte
		if _, err := io.ReadFull(input, header[:]); err == nil {
			headerGroup = binary.LittleEndian.Uint16(header[0:])
			headerFile = binary.LittleEndian.Uint16(header[2:])
			headerPage = binary.LittleEndian.Uint32(header[4:])
			if status == "OK" && (uint32(headerGroup) != file.GroupID || int16(headerFile) != file.FileID || headerPage != 0) {
				if file.GroupID == 3 && headerGroup == 0 && headerFile == 0 && headerPage == 0 {
					status = "IGNORED_TEMP"
				} else {
					status = "HEADER_MISMATCH"
				}
			}
		}
		_ = input.Close()
	}
	line := fmt.Sprintf("[bootstrap] phase=file status=%s group=%d file=%d header_group=%d header_file=%d header_page=%d tablespace=%q bytes=%d pages=%d aligned=%t path=%q",
		status, file.GroupID, file.FileID, headerGroup, headerFile, headerPage, file.Tablespace, size, pages, aligned, file.Path)
	return line, status != "OK" && status != "IGNORED_TEMP"
}

func (s *interactiveSession) printParameters(stdout io.Writer) {
	fmt.Fprintf(stdout, "system     = %s\n", s.systemPath)
	fmt.Fprintf(stdout, "control    = %s\n", defaultIfBlank(s.controlPath, "(auto)"))
	fmt.Fprintf(stdout, "control_dul= %s\n", s.effectiveControlDULPath())
	fmt.Fprintf(stdout, "init_dul   = %s\n", s.effectiveInitDULPath())
	fmt.Fprintf(stdout, "init_load  = %s\n", defaultIfBlank(s.initSource, "(not loaded)"))
	fmt.Fprintf(stdout, "dict_dir   = %s\n", s.effectiveDictionaryDir())
	fmt.Fprintf(stdout, "data_dir   = %s\n", defaultIfBlank(s.dataDir, "(SYSTEM.DBF directory)"))
	fmt.Fprintf(stdout, "output_dir = %s\n", s.effectiveOutputDir())
	fmt.Fprintf(stdout, "db_name    = %s (%s)\n", s.metadata.DatabaseName, s.metadata.DatabaseNameSrc)
	fmt.Fprintf(stdout, "instance_name= %s (%s)\n", s.metadata.InstanceName, s.metadata.InstanceNameSrc)
	fmt.Fprintf(stdout, "extent_size= %d pages (%s)\n", s.metadata.ExtentSize, s.metadata.ExtentSizeSource)
	fmt.Fprintf(stdout, "page_size  = %d bytes (%s)\n", s.metadata.PageSize, s.metadata.PageSizeSource)
	fmt.Fprintf(stdout, "page_count = %d (%s)\n", s.metadata.PageCount, defaultIfBlank(s.metadata.PageCountSource, "unknown"))
	fmt.Fprintf(stdout, "db_charset = %s (%s)\n", s.metadata.Charset, s.metadata.CharsetSource)
	if s.metadata.HasCharsetFlag {
		fmt.Fprintf(stdout, "unicode_flag= %d (%s)\n", s.metadata.CharsetFlag, s.metadata.CharsetSource)
	}
	fmt.Fprintf(stdout, "data_format= %s\n", s.dataFormat)
	fmt.Fprintf(stdout, "case_sensitive= %s\n", s.caseSensitive)
	if s.metadata.HasCaseSensitive {
		effective := 0
		if s.metadata.CaseSensitive {
			effective = 1
		}
		fmt.Fprintf(stdout, "case_effective= %d (%s)\n", effective, s.metadata.CaseSensitiveSource)
	}
	fmt.Fprintf(stdout, "charset    = %s\n", s.charset)
	fmt.Fprintf(stdout, "log        = %s\n", s.effectiveLogPath())
	if s.dictionary != nil {
		fmt.Fprintf(stdout, "dictionary = loaded (%s)\n", defaultIfBlank(s.dictionary.Source, "memory"))
	} else {
		fmt.Fprintln(stdout, "dictionary = not loaded")
	}
}

// listColumn describes one column of a `list` listing: the heading text and the
// field width shared by the heading, the rule under it and the rows below.
type listColumn struct {
	title string
	width int
	right bool
}

// listRowFormat builds the printf format the header, the rule and every data
// row share, so the three can never drift apart.
func listRowFormat(columns []listColumn) string {
	var format strings.Builder
	for i, col := range columns {
		if i > 0 {
			format.WriteByte(' ')
		}
		switch {
		case col.right:
			fmt.Fprintf(&format, "%%%ds", col.width)
		case i == len(columns)-1:
			// The last left-aligned column is not padded, so no line carries
			// trailing whitespace. The rule row still spans the full width
			// because it is passed a dash string of exactly that length.
			format.WriteString("%s")
		default:
			fmt.Fprintf(&format, "%%-%ds", col.width)
		}
	}
	format.WriteByte('\n')
	return format.String()
}

// printListHeader writes an upper-cased heading row followed by a rule of
// dashes spanning each column, the way disql and sqlplus render a result set.
// Upper case plus the rule makes it unmistakable which line names the columns
// and which lines are data.
func printListHeader(stdout io.Writer, columns []listColumn) string {
	format := listRowFormat(columns)
	titles := make([]any, 0, len(columns))
	rules := make([]any, 0, len(columns))
	for _, col := range columns {
		titles = append(titles, strings.ToUpper(col.title))
		rules = append(rules, strings.Repeat("-", col.width))
	}
	fmt.Fprintf(stdout, format, titles...)
	fmt.Fprintf(stdout, format, rules...)
	return format
}

// widestValue returns the column width needed to fit heading and every value.
func widestValue(title string, values []string) int {
	width := len(title)
	for _, value := range values {
		if n := len([]rune(value)); n > width {
			width = n
		}
	}
	return width
}

func (s *interactiveSession) printUsers(stdout io.Writer) {
	s.printDictionarySummary(stdout)
	counts := dictionaryUserObjectCounts(s.dictionary)
	names := make([]string, 0, len(s.dictionary.Users))
	for _, user := range s.dictionary.Users {
		names = append(names, user.Name)
	}
	format := printListHeader(stdout, []listColumn{
		{title: "user", width: widestValue("user", names)},
		{title: "schemas", width: 8, right: true},
		{title: "tables", width: 8, right: true},
		{title: "views", width: 8, right: true},
		{title: "synonyms", width: 10, right: true},
		{title: "sequences", width: 10, right: true},
		{title: "triggers", width: 9, right: true},
		{title: "functions", width: 10, right: true},
		{title: "procedures", width: 11, right: true},
		{title: "packages", width: 9, right: true},
	})
	for _, user := range s.dictionary.Users {
		count := counts[strings.ToUpper(user.Name)]
		fmt.Fprintf(stdout, format,
			user.Name,
			fmt.Sprint(count.Schemas),
			fmt.Sprint(count.Tables),
			fmt.Sprint(count.Views),
			fmt.Sprint(count.Synonyms),
			fmt.Sprint(count.Sequences),
			fmt.Sprint(count.Triggers),
			fmt.Sprint(count.Functions),
			fmt.Sprint(count.Procedures),
			fmt.Sprint(count.Packages),
		)
	}
}

type userObjectCounts struct {
	Schemas    int
	Tables     int
	Views      int
	Synonyms   int
	Sequences  int
	Triggers   int
	Functions  int
	Procedures int
	Packages   int
}

func dictionaryUserObjectCounts(dict *dm.DictionaryInfo) map[string]userObjectCounts {
	result := make(map[string]userObjectCounts)
	schemaOwners := make(map[string]string, len(dict.Schemas))
	for _, schema := range dict.Schemas {
		schemaOwners[strings.ToUpper(strings.TrimSpace(schema.Name))] = strings.ToUpper(strings.TrimSpace(schema.Owner))
		owner := strings.ToUpper(strings.TrimSpace(schema.Owner))
		if owner != "" {
			count := result[owner]
			count.Schemas++
			result[owner] = count
		}
	}
	increment := func(owner string, apply func(*userObjectCounts)) {
		key := strings.ToUpper(strings.TrimSpace(owner))
		if key == "" {
			return
		}
		if user := schemaOwners[key]; user != "" {
			key = user
		}
		count := result[key]
		apply(&count)
		result[key] = count
	}
	for _, table := range dict.Tables {
		increment(table.Owner, func(count *userObjectCounts) { count.Tables++ })
	}
	for _, view := range dict.Views {
		increment(view.Owner, func(count *userObjectCounts) { count.Views++ })
	}
	for _, syn := range dict.Synonyms {
		increment(syn.Owner, func(count *userObjectCounts) { count.Synonyms++ })
	}
	for _, seq := range dict.Sequences {
		increment(seq.Owner, func(count *userObjectCounts) { count.Sequences++ })
	}
	for _, trigger := range dict.Triggers {
		increment(trigger.Owner, func(count *userObjectCounts) { count.Triggers++ })
	}
	packageNamesByOwner := make(map[string]map[string]bool)
	for _, routine := range dict.Routines {
		owner := strings.ToUpper(strings.TrimSpace(routine.Owner))
		if owner == "" {
			continue
		}
		if user := schemaOwners[owner]; user != "" {
			owner = user
		}
		switch strings.Join(strings.Fields(strings.ToUpper(strings.TrimSpace(routine.ObjectType))), " ") {
		case "FUNCTION":
			increment(owner, func(count *userObjectCounts) { count.Functions++ })
		case "PROCEDURE":
			increment(owner, func(count *userObjectCounts) { count.Procedures++ })
		case "PACKAGE", "PACKAGE BODY":
			names := packageNamesByOwner[owner]
			if names == nil {
				names = make(map[string]bool)
				packageNamesByOwner[owner] = names
			}
			names[strings.ToUpper(strings.TrimSpace(routine.Name))] = true
		}
	}
	for owner, names := range packageNamesByOwner {
		count := result[owner]
		count.Packages = len(names)
		result[owner] = count
	}
	return result
}

func (s *interactiveSession) printDictionarySummary(stdout io.Writer) {
	if s.dictionary == nil {
		return
	}
	source := defaultIfBlank(s.dictionary.Source, "memory")
	dir := defaultIfBlank(s.dictionary.DictionaryDir, s.effectiveDictionaryDir())
	fmt.Fprintf(stdout, "dictionary source: %s\n", source)
	fmt.Fprintf(stdout, "dictionary dir: %s\n", dir)
	if s.dictionary.BootstrapMode != "" {
		fmt.Fprintf(stdout, "bootstrap mode: %s (fallback=%t)\n", s.dictionary.BootstrapMode, s.dictionary.BootstrapFallback)
	}
	fmt.Fprintf(stdout, "dictionary rows: users=%d schemas=%d tables=%d columns=%d views=%d sequences=%d routines=%d triggers=%d synonyms=%d tab_privs=%d partitions=%d partition_keys=%d objects=%d\n\n",
		len(s.dictionary.Users), len(s.dictionary.Schemas), len(s.dictionary.Tables), len(s.dictionary.Columns), len(s.dictionary.Views), len(s.dictionary.Sequences), len(s.dictionary.Routines), len(s.dictionary.Triggers), len(s.dictionary.Synonyms), len(s.dictionary.TabPrivileges), len(s.dictionary.Partitions), len(s.dictionary.PartitionKeys), s.dictionary.ObjectCount)
}

func (s *interactiveSession) printTables(stdout io.Writer, owner string) {
	owner = normalizeIdentifierInput(owner)
	var rows []dm.DictionaryTable
	for _, table := range s.dictionary.Tables {
		if strings.EqualFold(table.Owner, owner) {
			rows = append(rows, table)
		}
	}
	names := make([]string, 0, len(rows))
	spaces := make([]string, 0, len(rows))
	for _, table := range rows {
		names = append(names, truncateForTable(table.Name, 34))
		spaces = append(spaces, table.Tablespace)
	}
	format := printListHeader(stdout, []listColumn{
		{title: "owner", width: widestValue("owner", []string{owner})},
		{title: "table", width: widestValue("table", names)},
		{title: "table_id", width: 10},
		{title: "columns", width: 10},
		{title: "tablespace", width: widestValue("tablespace", spaces)},
		{title: "storage", width: 12},
		{title: "partition", width: 9},
	})
	for _, table := range rows {
		partitioned := "NO"
		if table.Partitioned {
			partitioned = "YES"
		}
		tablespace := table.Tablespace
		if tablespace == "" && table.GroupID != 0 {
			tablespace = fmt.Sprintf("GROUP_%d", table.GroupID)
		}
		fmt.Fprintf(stdout, format,
			table.Owner,
			truncateForTable(table.Name, 34),
			fmt.Sprint(table.ID),
			fmt.Sprint(table.ColumnCount),
			tablespace,
			table.Storage,
			partitioned,
		)
	}
	fmt.Fprintf(stdout, "%d table(s)\n", len(rows))
}

func (s *interactiveSession) printSchemas(stdout io.Writer, owner string) {
	owner = normalizeIdentifierInput(owner)
	schemaNames := make([]string, 0, len(s.dictionary.Schemas))
	ownerNames := make([]string, 0, len(s.dictionary.Schemas))
	for _, schema := range s.dictionary.Schemas {
		schemaNames = append(schemaNames, truncateForTable(schema.Name, 28))
		ownerNames = append(ownerNames, truncateForTable(schema.Owner, 28))
	}
	format := printListHeader(stdout, []listColumn{
		{title: "schema", width: widestValue("schema", schemaNames)},
		{title: "owner", width: widestValue("owner", ownerNames)},
		{title: "tables", width: 6, right: true},
	})
	count := 0
	for _, schema := range s.dictionary.Schemas {
		if owner != "" && !strings.EqualFold(schema.Owner, owner) {
			continue
		}
		tables := 0
		for _, table := range s.dictionary.Tables {
			if strings.EqualFold(table.Owner, schema.Name) {
				tables++
			}
		}
		fmt.Fprintf(stdout, format, truncateForTable(schema.Name, 28), truncateForTable(schema.Owner, 28), fmt.Sprint(tables))
		count++
	}
	fmt.Fprintf(stdout, "%d schema(s)\n", count)
}

func (s *interactiveSession) unloadTable(args []string, stdout io.Writer) error {
	if strings.EqualFold(s.dataFormat, "dmp") {
		return s.unloadDMPTable(args, stdout)
	}
	tableToken := args[0]
	owner, tableName, ok := parseOwnerTableToken(tableToken)
	if !ok {
		return fmt.Errorf("usage: unload table <owner.table_name>")
	}
	table, ok := s.findTable(owner, tableName)
	if !ok {
		return fmt.Errorf("table %s not found in dictionary", tableToken)
	}
	prefix := sanitizedFilePrefix(table.Owner + "_" + table.Name)
	if customPrefix, ok := optionalToPrefix(args[1:]); ok {
		prefix = sanitizedFilePrefix(customPrefix)
	}
	ddlPath := s.outputPath(prefix + "_ddl.sql")
	dataExt := dataOutputExtension(s.dataFormat)
	dataPath := s.outputPath(prefix + "_data." + dataExt)
	if err := s.ensureOutputDir(); err != nil {
		return err
	}
	ddl, err := dm.ExportDDL(dm.DDLExportOptions{
		SystemPath:     s.systemPath,
		ControlPath:    s.controlPath,
		ControlDULPath: s.effectiveControlDULPath(),
		OutputPath:     ddlPath,
		OwnerFilter:    table.Owner,
		TableFilter:    table.Owner + "." + table.Name,
		Charset:        s.charset,
		TablesOnly:     true,
		Dictionary:     s.dictionary,
	})
	if err != nil {
		return err
	}
	data, err := dm.ExportData(dm.DataExportOptions{
		SystemPath:       s.systemPath,
		ControlPath:      s.controlPath,
		ControlDULPath:   s.effectiveControlDULPath(),
		DataDir:          s.effectiveDataDir(),
		OutputPath:       dataPath,
		OwnerFilter:      table.Owner,
		TableFilter:      table.Owner + "." + table.Name,
		ExcludeTables:    "",
		Charset:          s.charset,
		OutputFormat:     s.dataFormat,
		DMPCaseSensitive: s.dmpCaseSensitiveValue(),
		Dictionary:       s.dictionary,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "ddl output: %s\n", ddl.OutputPath)
	if splitDataFormat(s.dataFormat) && data.OutputPath == "" {
		fmt.Fprintln(stdout, "data output: skipped (no rows)")
	} else {
		fmt.Fprintf(stdout, "data output: %s\n", data.OutputPath)
	}
	fmt.Fprintf(stdout, "tables exported: %d\n", ddl.TableCount)
	fmt.Fprintf(stdout, "triggers exported: %d\n", ddl.TriggerCount)
	fmt.Fprintf(stdout, "tab privileges exported: %d\n", ddl.TabPrivilegeCount)
	fmt.Fprintf(stdout, "rows exported: %d\n", data.RowsExported)
	fmt.Fprintf(stdout, "rows failed: %d\n", data.RowsFailed)
	s.printDataExportDiagnostics(stdout, data)
	printDataExportWarnings(stdout, data)
	return nil
}

func (s *interactiveSession) recoverTable(args []string, stdout io.Writer) error {
	tableToken := args[0]
	owner, tableName, ok := parseOwnerTableToken(tableToken)
	if !ok {
		return fmt.Errorf("usage: recover table <owner.table_name>")
	}
	table, ok := s.findTable(owner, tableName)
	if !ok {
		return fmt.Errorf("table %s not found in dictionary; load a pre-drop dictionary or add the table to dmdul_dict first", tableToken)
	}
	prefix := sanitizedFilePrefix(table.Owner + "_" + table.Name + "_recover")
	if customPrefix, ok := optionalToPrefix(args[1:]); ok {
		prefix = sanitizedFilePrefix(customPrefix)
	}
	ddlPath := s.outputPath(prefix + "_ddl.sql")
	dataExt := dataOutputExtension(s.dataFormat)
	dataPath := s.outputPath(prefix + "_data." + dataExt)
	if err := s.ensureOutputDir(); err != nil {
		return err
	}
	ddl, err := dm.ExportDDL(dm.DDLExportOptions{
		SystemPath:     s.systemPath,
		ControlPath:    s.controlPath,
		ControlDULPath: s.effectiveControlDULPath(),
		OutputPath:     ddlPath,
		OwnerFilter:    table.Owner,
		TableFilter:    table.Owner + "." + table.Name,
		Charset:        s.charset,
		TablesOnly:     true,
		Dictionary:     s.dictionary,
	})
	if err != nil {
		return err
	}
	data, err := dm.ExportData(dm.DataExportOptions{
		SystemPath:       s.systemPath,
		ControlPath:      s.controlPath,
		ControlDULPath:   s.effectiveControlDULPath(),
		DataDir:          s.effectiveDataDir(),
		OutputPath:       dataPath,
		OwnerFilter:      table.Owner,
		TableFilter:      table.Owner + "." + table.Name,
		ExcludeTables:    "",
		Charset:          s.charset,
		OutputFormat:     s.dataFormat,
		DMPCaseSensitive: s.dmpCaseSensitiveValue(),
		RecoveryMode:     true,
		Dictionary:       s.dictionary,
	})
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, "recovery mode: on")
	fmt.Fprintf(stdout, "ddl output: %s\n", ddl.OutputPath)
	if splitDataFormat(s.dataFormat) && data.OutputPath == "" {
		fmt.Fprintln(stdout, "data output: skipped (no rows)")
	} else {
		fmt.Fprintf(stdout, "data output: %s\n", data.OutputPath)
	}
	fmt.Fprintf(stdout, "tables exported: %d\n", ddl.TableCount)
	fmt.Fprintf(stdout, "rows exported: %d\n", data.RowsExported)
	fmt.Fprintf(stdout, "rows failed: %d\n", data.RowsFailed)
	s.printDataExportDiagnostics(stdout, data)
	printDataExportWarnings(stdout, data)
	return nil
}

func (s *interactiveSession) unloadObject(args []string, stdout io.Writer) error {
	ownerToken := normalizeIdentifierInput(args[0])
	ownerFilter := ownerToken
	prefix := sanitizedFilePrefix(ownerToken)
	if strings.EqualFold(ownerToken, "all") || ownerToken == "*" || strings.EqualFold(ownerToken, "database") {
		ownerFilter = "all"
		prefix = "DATABASE"
	} else if !s.hasOwner(ownerToken) {
		return fmt.Errorf("user/owner %s not found in dictionary", args[0])
	}
	if customPrefix, ok := optionalToPrefix(args[1:]); ok {
		prefix = sanitizedFilePrefix(customPrefix)
	}
	if err := s.ensureOutputDir(); err != nil {
		return err
	}
	ddl, err := dm.ExportDDL(dm.DDLExportOptions{
		SystemPath:     s.systemPath,
		ControlPath:    s.controlPath,
		ControlDULPath: s.effectiveControlDULPath(),
		OutputPath:     s.outputPath(prefix + "_objects.sql"),
		OwnerFilter:    ownerFilter,
		TableFilter:    "all",
		Charset:        s.charset,
		Dictionary:     s.dictionary,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "object ddl output: %s\n", ddl.OutputPath)
	fmt.Fprintf(stdout, "users exported: %d\n", ddl.UserCount)
	fmt.Fprintf(stdout, "roles exported: %d\n", ddl.RoleCount)
	fmt.Fprintf(stdout, "role grants exported: %d\n", ddl.RoleGrantCount)
	fmt.Fprintf(stdout, "tables exported: %d\n", ddl.TableCount)
	fmt.Fprintf(stdout, "views exported: %d\n", ddl.ViewCount)
	fmt.Fprintf(stdout, "sequences exported: %d\n", ddl.SequenceCount)
	fmt.Fprintf(stdout, "routines exported: %d\n", ddl.RoutineCount)
	fmt.Fprintf(stdout, "triggers exported: %d\n", ddl.TriggerCount)
	fmt.Fprintf(stdout, "synonyms exported: %d\n", ddl.SynonymCount)
	fmt.Fprintf(stdout, "tab privileges exported: %d\n", ddl.TabPrivilegeCount)
	return nil
}

func (s *interactiveSession) unloadUser(args []string, stdout io.Writer) error {
	if strings.EqualFold(s.dataFormat, "dmp") {
		return s.unloadDMPUser(args, stdout)
	}
	owner := normalizeIdentifierInput(args[0])
	if strings.EqualFold(owner, "all") || owner == "*" || strings.EqualFold(owner, "database") {
		return fmt.Errorf("unload user all has been removed; use unload database")
	}
	if !s.hasOwner(owner) {
		return fmt.Errorf("user/owner %s not found in dictionary", args[0])
	}
	prefix := sanitizedFilePrefix(owner)
	if customPrefix, ok := optionalToPrefix(args[1:]); ok {
		prefix = sanitizedFilePrefix(customPrefix)
	}
	if err := s.ensureOutputDir(); err != nil {
		return err
	}
	ddl, err := dm.ExportDDL(dm.DDLExportOptions{
		SystemPath:     s.systemPath,
		ControlPath:    s.controlPath,
		ControlDULPath: s.effectiveControlDULPath(),
		TableOutputPath: func(_ string, table string, _ uint32) string {
			tablePrefix := sanitizedFilePrefix(prefix + "_" + table)
			return s.outputPath(tablePrefix + "_ddl.sql")
		},
		OwnerFilter: owner,
		TableFilter: "all",
		Charset:     s.charset,
		TablesOnly:  true,
		Dictionary:  s.dictionary,
	})
	if err != nil {
		return err
	}
	dataExt := dataOutputExtension(s.dataFormat)
	data, err := dm.ExportData(dm.DataExportOptions{
		SystemPath:     s.systemPath,
		ControlPath:    s.controlPath,
		ControlDULPath: s.effectiveControlDULPath(),
		DataDir:        s.effectiveDataDir(),
		TableOutputPath: func(_ string, table string, _ uint32) string {
			tablePrefix := sanitizedFilePrefix(prefix + "_" + table)
			return s.outputPath(tablePrefix + "_data." + dataExt)
		},
		OwnerFilter:      owner,
		TableFilter:      "all",
		ExcludeTables:    "",
		Charset:          s.charset,
		OutputFormat:     s.dataFormat,
		DMPCaseSensitive: s.dmpCaseSensitiveValue(),
		Dictionary:       s.dictionary,
	})
	if err != nil {
		return err
	}
	dataOutputs := make(map[string]dm.DataTableOutput, len(data.TableOutputs))
	for _, output := range data.TableOutputs {
		dataOutputs[qualifiedTableOutputKey(output.Owner, output.Name)] = output
	}
	rowCounts := dataTableRowCountIndex(data)
	for _, output := range ddl.TableOutputs {
		key := qualifiedTableOutputKey(output.Owner, output.Name)
		fmt.Fprintln(stdout, formatUnloadingTableLine(output.Owner, output.Name, rowCounts[key]))
		fmt.Fprintf(stdout, "  ddl output: %s\n", output.OutputPath)
		if dataOutput, ok := dataOutputs[key]; ok {
			fmt.Fprintf(stdout, "  data output: %s\n", dataOutput.OutputPath)
		} else {
			table, found := s.findTable(output.Owner, output.Name)
			if found && table.Temporary {
				fmt.Fprintln(stdout, "  data output: skipped (temporary table)")
			} else {
				fmt.Fprintln(stdout, "  data output: skipped (no rows)")
			}
		}
	}
	fmt.Fprintf(stdout, "output dir: %s\n", s.effectiveOutputDir())
	fmt.Fprintf(stdout, "tables exported: %d\n", len(ddl.TableOutputs))
	fmt.Fprintf(stdout, "ddl files exported: %d\n", len(ddl.TableOutputs))
	fmt.Fprintf(stdout, "data files exported: %d\n", len(data.TableOutputs))
	fmt.Fprintf(stdout, "rows exported: %d\n", data.RowsExported)
	fmt.Fprintf(stdout, "rows failed: %d\n", data.RowsFailed)
	s.printDataExportDiagnostics(stdout, data)
	printDataExportWarnings(stdout, data)
	return nil
}

func (s *interactiveSession) unloadDatabase(args []string, stdout io.Writer) error {
	if strings.EqualFold(s.dataFormat, "dmp") {
		prefix := "DATABASE"
		if customPrefix, ok := optionalToPrefix(args); ok {
			prefix = sanitizedFilePrefix(customPrefix)
		}
		return s.unloadLogicalDMP(logicalDMPUnloadRequest{
			mode: dm.DMPModeFull, ownerFilter: "all", tableFilter: "all", prefix: prefix,
		}, stdout)
	}
	prefix := "DATABASE"
	if customPrefix, ok := optionalToPrefix(args); ok {
		prefix = sanitizedFilePrefix(customPrefix)
	}
	ddlPath := s.outputPath(prefix + "_ddl.sql")
	if err := s.ensureOutputDir(); err != nil {
		return err
	}
	ddl, err := dm.ExportDDL(dm.DDLExportOptions{
		SystemPath:     s.systemPath,
		ControlPath:    s.controlPath,
		ControlDULPath: s.effectiveControlDULPath(),
		OutputPath:     ddlPath,
		OwnerFilter:    "all",
		TableFilter:    "all",
		Charset:        s.charset,
		Dictionary:     s.dictionary,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "ddl output: %s\n", ddl.OutputPath)
	fmt.Fprintf(stdout, "users exported: %d\n", ddl.UserCount)
	fmt.Fprintf(stdout, "tables exported: %d\n", ddl.TableCount)
	fmt.Fprintf(stdout, "views exported: %d\n", ddl.ViewCount)
	fmt.Fprintf(stdout, "sequences exported: %d\n", ddl.SequenceCount)
	fmt.Fprintf(stdout, "routines exported: %d\n", ddl.RoutineCount)
	fmt.Fprintf(stdout, "triggers exported: %d\n", ddl.TriggerCount)
	fmt.Fprintf(stdout, "synonyms exported: %d\n", ddl.SynonymCount)
	fmt.Fprintf(stdout, "tab privileges exported: %d\n", ddl.TabPrivilegeCount)
	if splitDataFormat(s.dataFormat) {
		data, err := s.unloadDatabaseSplitData(prefix, s.dataFormat, stdout)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "%s output dir: %s\n", s.dataFormat, s.effectiveOutputDir())
		fmt.Fprintf(stdout, "%s files exported: %d\n", s.dataFormat, len(data.TableOutputs))
		fmt.Fprintf(stdout, "rows exported: %d\n", data.RowsExported)
		fmt.Fprintf(stdout, "rows failed: %d\n", data.RowsFailed)
		s.printDataExportDiagnostics(stdout, data)
		printDataExportWarnings(stdout, data)
		return nil
	}
	dataPath := s.outputPath(prefix + "_data.sql")
	data, err := dm.ExportData(dm.DataExportOptions{
		SystemPath:       s.systemPath,
		ControlPath:      s.controlPath,
		ControlDULPath:   s.effectiveControlDULPath(),
		DataDir:          s.effectiveDataDir(),
		OutputPath:       dataPath,
		OwnerFilter:      "all",
		TableFilter:      "all",
		ExcludeTables:    "",
		Charset:          s.charset,
		OutputFormat:     "sql",
		DMPCaseSensitive: s.dmpCaseSensitiveValue(),
		Dictionary:       s.dictionary,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "data output: %s\n", data.OutputPath)
	fmt.Fprintf(stdout, "rows exported: %d\n", data.RowsExported)
	fmt.Fprintf(stdout, "rows failed: %d\n", data.RowsFailed)
	s.printDataExportDiagnostics(stdout, data)
	printDataExportWarnings(stdout, data)
	return nil
}

func (s *interactiveSession) unloadDatabaseSplitData(prefix string, format string, stdout io.Writer) (*dm.DataExportResult, error) {
	extension := dataOutputExtension(format)
	data, err := dm.ExportData(dm.DataExportOptions{
		SystemPath:     s.systemPath,
		ControlPath:    s.controlPath,
		ControlDULPath: s.effectiveControlDULPath(),
		DataDir:        s.effectiveDataDir(),
		TableOutputPath: func(owner string, table string, _ uint32) string {
			tablePrefix := sanitizedFilePrefix(prefix + "_" + owner + "_" + table)
			return s.outputPath(tablePrefix + "_data." + extension)
		},
		OwnerFilter:      "all",
		TableFilter:      "all",
		ExcludeTables:    "",
		Charset:          s.charset,
		OutputFormat:     format,
		DMPCaseSensitive: s.dmpCaseSensitiveValue(),
		Dictionary:       s.dictionary,
	})
	if err != nil {
		return nil, err
	}
	outputs := make(map[string]bool, len(data.TableOutputs))
	for _, output := range data.TableOutputs {
		outputs[qualifiedTableOutputKey(output.Owner, output.Name)] = true
		fmt.Fprintf(stdout, "%s output: %s\n", format, output.OutputPath)
	}
	for _, table := range data.TableRowCounts {
		if !outputs[qualifiedTableOutputKey(table.Owner, table.Name)] {
			fmt.Fprintf(stdout, "%s skipped: %s.%s (no rows)\n", format, table.Owner, table.Name)
		}
	}
	return data, nil
}

// dataTableRowCountIndex keys the per-table row statistics by owner.table so
// each table's line can report how many rows it actually yielded.
func dataTableRowCountIndex(data *dm.DataExportResult) map[string]dm.DataTableRowCount {
	index := make(map[string]dm.DataTableRowCount)
	if data == nil {
		return index
	}
	for _, item := range data.TableRowCounts {
		index[qualifiedTableOutputKey(item.Owner, item.Name)] = item
	}
	return index
}

// formatUnloadingTableLine renders one per-table progress line in the spirit of
// DUL's ". unloading table T_XIFENFEI  2 rows unloaded", so a multi-table
// unload shows what each table yielded instead of only a final total.
func formatUnloadingTableLine(owner string, table string, counts dm.DataTableRowCount) string {
	line := fmt.Sprintf(". unloading table %-30s %10d rows unloaded", owner+"."+table, counts.RowsExported)
	if counts.RowsFailed > 0 {
		line += fmt.Sprintf(" (%d failed)", counts.RowsFailed)
	}
	return line
}

type logicalDMPUnloadRequest struct {
	mode        dm.DMPExportMode
	ownerFilter string
	tableFilter string
	prefix      string
	tablesOnly  bool
}

func (s *interactiveSession) unloadDMPTable(args []string, stdout io.Writer) error {
	tokens := splitIdentifierList(args[0])
	if len(tokens) == 0 {
		return fmt.Errorf("usage: unload table <owner.table_name>[,<owner.table_name>...]")
	}
	owners := make([]string, 0, len(tokens))
	filters := make([]string, 0, len(tokens))
	seenTables := make(map[string]bool)
	var selected []dm.DictionaryTable
	for _, token := range tokens {
		owner, tableName, ok := parseOwnerTableToken(token)
		if !ok {
			return fmt.Errorf("invalid table name %q; use owner.table_name", token)
		}
		table, ok := s.findTable(owner, tableName)
		if !ok {
			return fmt.Errorf("table %s not found in dictionary", token)
		}
		key := strings.ToUpper(table.Owner) + "\x00" + strings.ToUpper(table.Name)
		if seenTables[key] {
			continue
		}
		seenTables[key] = true
		selected = append(selected, table)
		owners = appendUniqueFold(owners, table.Owner)
		filters = append(filters, quotedFilterIdentifier(table.Owner)+"."+quotedFilterIdentifier(table.Name))
	}
	prefix := "TABLES"
	if len(selected) == 1 {
		prefix = sanitizedFilePrefix(selected[0].Owner + "_" + selected[0].Name)
	}
	if customPrefix, ok := optionalToPrefix(args[1:]); ok {
		prefix = sanitizedFilePrefix(customPrefix)
	}
	return s.unloadLogicalDMP(logicalDMPUnloadRequest{
		mode: dm.DMPModeTables, ownerFilter: strings.Join(owners, ","),
		tableFilter: strings.Join(filters, ","), prefix: prefix, tablesOnly: true,
	}, stdout)
}

func (s *interactiveSession) unloadDMPUser(args []string, stdout io.Writer) error {
	users := splitIdentifierList(args[0])
	if len(users) == 0 {
		return fmt.Errorf("usage: unload user <owner>[,<owner>...]")
	}
	var normalizedUsers []string
	var schemaFilters []string
	for _, value := range users {
		owner := normalizeIdentifierInput(value)
		if strings.EqualFold(owner, "all") || owner == "*" || strings.EqualFold(owner, "database") {
			return fmt.Errorf("unload user all has been removed; use unload database")
		}
		if !s.hasOwner(owner) {
			return fmt.Errorf("user %s not found in dictionary", value)
		}
		normalizedUsers = appendUniqueFold(normalizedUsers, owner)
		schemaFilters = appendUniqueFold(schemaFilters, owner)
		for _, schema := range s.dictionary.Schemas {
			if strings.EqualFold(schema.Owner, owner) {
				schemaFilters = appendUniqueFold(schemaFilters, schema.Name)
			}
		}
	}
	prefix := sanitizedFilePrefix(strings.Join(normalizedUsers, "_"))
	if len(normalizedUsers) > 1 {
		prefix = sanitizedFilePrefix("OWNER_" + strings.Join(normalizedUsers, "_"))
	}
	if customPrefix, ok := optionalToPrefix(args[1:]); ok {
		prefix = sanitizedFilePrefix(customPrefix)
	}
	return s.unloadLogicalDMP(logicalDMPUnloadRequest{
		mode: dm.DMPModeOwner, ownerFilter: strings.Join(schemaFilters, ","),
		tableFilter: "all", prefix: prefix,
	}, stdout)
}

func (s *interactiveSession) unloadSchema(args []string, stdout io.Writer) error {
	if !strings.EqualFold(s.dataFormat, "dmp") {
		return fmt.Errorf("unload schema currently requires: set data_format dmp")
	}
	tokens := splitIdentifierList(args[0])
	if len(tokens) == 0 {
		return fmt.Errorf("usage: unload schema <schema>[,<schema>...]")
	}
	var schemas []string
	for _, value := range tokens {
		schema := normalizeIdentifierInput(value)
		if !s.hasSchema(schema) {
			return fmt.Errorf("schema %s not found in dictionary", value)
		}
		schemas = appendUniqueFold(schemas, schema)
	}
	prefix := sanitizedFilePrefix(strings.Join(schemas, "_"))
	if len(schemas) > 1 {
		prefix = sanitizedFilePrefix("SCHEMAS_" + strings.Join(schemas, "_"))
	}
	if customPrefix, ok := optionalToPrefix(args[1:]); ok {
		prefix = sanitizedFilePrefix(customPrefix)
	}
	return s.unloadLogicalDMP(logicalDMPUnloadRequest{
		mode: dm.DMPModeSchemas, ownerFilter: strings.Join(schemas, ","),
		tableFilter: "all", prefix: prefix,
	}, stdout)
}

func (s *interactiveSession) unloadLogicalDMP(request logicalDMPUnloadRequest, stdout io.Writer) error {
	if err := s.ensureOutputDir(); err != nil {
		return err
	}
	ddlPath := s.outputPath(request.prefix + "_ddl.sql")
	dmpPath := s.outputPath(request.prefix + ".dmp")
	ddl, err := dm.ExportDDL(dm.DDLExportOptions{
		SystemPath:     s.systemPath,
		ControlPath:    s.controlPath,
		ControlDULPath: s.effectiveControlDULPath(),
		OutputPath:     ddlPath,
		OwnerFilter:    request.ownerFilter,
		TableFilter:    request.tableFilter,
		Charset:        s.charset,
		TablesOnly:     request.tablesOnly,
		DMPMode:        request.mode,
		Dictionary:     s.dictionary,
	})
	if err != nil {
		return err
	}
	if ddl.DMPMetadata == nil {
		return fmt.Errorf("logical dmp metadata was not generated")
	}

	spoolDir, err := os.MkdirTemp(s.effectiveOutputDir(), ".dmdul-dmp-spool-")
	if err != nil {
		return fmt.Errorf("create dmp spool directory: %w", err)
	}
	defer os.RemoveAll(spoolDir)
	data, err := dm.ExportData(dm.DataExportOptions{
		SystemPath:     s.systemPath,
		ControlPath:    s.controlPath,
		ControlDULPath: s.effectiveControlDULPath(),
		DataDir:        s.effectiveDataDir(),
		TableOutputPath: func(_ string, _ string, tableID uint32) string {
			return filepath.Join(spoolDir, fmt.Sprintf("%d.dmp", tableID))
		},
		OwnerFilter:      request.ownerFilter,
		TableFilter:      request.tableFilter,
		ExcludeTables:    "",
		Charset:          s.charset,
		OutputFormat:     "dmp",
		DMPCaseSensitive: s.dmpCaseSensitiveValue(),
		Dictionary:       s.dictionary,
	})
	if err != nil {
		return err
	}
	spools := make(map[uint32]string, len(data.TableOutputs))
	for _, output := range data.TableOutputs {
		spools[output.TableID] = output.OutputPath
	}
	info, err := dm.WriteLogicalDMP(dm.DMPLogicalOptions{
		OutputPath: dmpPath, Catalog: ddl.DMPMetadata, TableDataPaths: spools,
		Charset: s.effectiveDMPCharset(), CaseSensitive: s.dmpCaseSensitiveValue(),
		ExtentSize: ddl.ExtentSize, PageSize: ddl.PageSize,
	})
	if err != nil {
		return err
	}
	metadataCounts := ddl.DMPMetadata.Counts()
	fmt.Fprintf(stdout, "ddl output: %s\n", ddl.OutputPath)
	fmt.Fprintf(stdout, "dmp output: %s\n", info.Path)
	fmt.Fprintf(stdout, "dmp mode: %s\n", info.Mode)
	fmt.Fprintf(stdout, "schemas exported: %d\n", len(info.Schemas))
	fmt.Fprintf(stdout, "objects exported: %d\n", info.ObjectCount)
	fmt.Fprintf(stdout, "tables exported: %d\n", len(info.Tables))
	fmt.Fprintf(stdout, "users exported: %d\n", metadataCounts.Users)
	fmt.Fprintf(stdout, "roles exported: %d\n", metadataCounts.Roles)
	fmt.Fprintf(stdout, "role grants exported: %d\n", metadataCounts.RoleGrants)
	fmt.Fprintf(stdout, "indexes exported: %d\n", metadataCounts.Indexes)
	fmt.Fprintf(stdout, "constraints exported: %d\n", metadataCounts.Constraints)
	fmt.Fprintf(stdout, "views exported: %d\n", metadataCounts.Views)
	fmt.Fprintf(stdout, "sequences exported: %d\n", metadataCounts.Sequences)
	fmt.Fprintf(stdout, "routines exported: %d\n", metadataCounts.Routines)
	fmt.Fprintf(stdout, "triggers exported: %d\n", metadataCounts.Triggers)
	fmt.Fprintf(stdout, "synonyms exported: %d\n", metadataCounts.Synonyms)
	fmt.Fprintf(stdout, "tab privileges exported: %d\n", metadataCounts.Privileges)
	fmt.Fprintf(stdout, "rows exported: %d\n", data.RowsExported)
	fmt.Fprintf(stdout, "rows failed: %d\n", data.RowsFailed)
	s.printDataExportDiagnostics(stdout, data)
	printDataExportWarnings(stdout, data)
	s.log(fmt.Sprintf("[DMP] mode=%s output=%q schemas=%d objects=%d tables=%d rows=%d", info.Mode, info.Path, len(info.Schemas), info.ObjectCount, len(info.Tables), data.RowsExported))
	return nil
}

func (s *interactiveSession) effectiveDMPCharset() string {
	value := strings.TrimSpace(s.charset)
	if value != "" && !strings.EqualFold(value, "auto") {
		return value
	}
	if s.dictionary != nil {
		if detected, ok := charsetParameterFromDictionary(s.dictionary.Charset); ok {
			return detected
		}
	}
	return "utf-8"
}

func (s *interactiveSession) hasSchema(name string) bool {
	for _, schema := range s.dictionary.Schemas {
		if strings.EqualFold(schema.Name, name) {
			return true
		}
	}
	for _, table := range s.dictionary.Tables {
		if strings.EqualFold(table.Owner, name) {
			return true
		}
	}
	return false
}

func appendUniqueFold(values []string, value string) []string {
	for _, existing := range values {
		if strings.EqualFold(existing, value) {
			return values
		}
	}
	return append(values, value)
}

func splitIdentifierList(value string) []string {
	var result []string
	var current strings.Builder
	inQuote := false
	for _, r := range value {
		switch r {
		case '"':
			inQuote = !inQuote
			current.WriteRune(r)
		case ',':
			if inQuote {
				current.WriteRune(r)
				continue
			}
			if token := strings.TrimSpace(current.String()); token != "" {
				result = append(result, token)
			}
			current.Reset()
		default:
			current.WriteRune(r)
		}
	}
	if token := strings.TrimSpace(current.String()); token != "" {
		result = append(result, token)
	}
	return result
}

func quotedFilterIdentifier(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}

func dataOutputExtension(format string) string {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "csv", "fldr":
		return "txt"
	case "dmp":
		return "dmp"
	default:
		return "sql"
	}
}

func splitDataFormat(format string) bool {
	format = strings.ToLower(strings.TrimSpace(format))
	return format == "csv" || format == "fldr" || format == "dmp"
}

func printDataExportWarnings(stdout io.Writer, result *dm.DataExportResult) {
	if result == nil {
		return
	}
	if result.TimeFractionLoss > 0 {
		fmt.Fprintf(stdout, "warning: TIME fractional seconds are not representable in DM DMP and were cleared in %d row(s)\n", result.TimeFractionLoss)
	}
	if result.OversizedSQLStatements > 0 {
		fmt.Fprintf(stdout, "warning: %d INSERT statement(s) exceed disql's 160 KiB input buffer (tables: %s); disql aborts them with \"input too long\", so import this SQL with a JDBC/ODBC client or re-export with data_format dmp\n",
			result.OversizedSQLStatements, strings.Join(result.OversizedSQLTables, ", "))
	}
	for _, source := range result.RecoverySources {
		if source.Heuristic {
			fmt.Fprintln(stdout, "warning: orphan storage ownership is heuristic; verify recovery source evidence before import")
			break
		}
	}
}

func (s *interactiveSession) printDataExportDiagnostics(stdout io.Writer, result *dm.DataExportResult) {
	if result == nil {
		return
	}
	fmt.Fprintf(stdout, "planned pages: %d\n", result.PlannedPages)
	fmt.Fprintf(stdout, "direct pages read: %d\n", result.DirectPagesRead)
	fmt.Fprintf(stdout, "fallback pages scanned: %d\n", result.FallbackPagesScanned)
	s.log(fmt.Sprintf("[UNLOAD] planned_pages=%d direct_pages_read=%d fallback_pages_scanned=%d", result.PlannedPages, result.DirectPagesRead, result.FallbackPagesScanned))
	if len(result.FallbackReasons) == 0 {
		fmt.Fprintln(stdout, "fallback reason: none")
		s.log("[UNLOAD] fallback_reason=none")
	} else {
		for _, reason := range result.FallbackReasons {
			fmt.Fprintf(stdout, "fallback reason: %s\n", reason)
			s.log("[UNLOAD] fallback_reason=" + reason)
		}
	}
	for _, source := range result.RecoverySources {
		attribution := "dictionary"
		if source.Heuristic {
			attribution = "heuristic-orphan"
		}
		line := fmt.Sprintf(
			"recovery source: target=%s.%s group=%d file=%d storage_id=%d pages=%d page_range=%d-%d rows_located=%d rows_exported=%d rows_failed=%d attribution=%s",
			source.Owner,
			source.Name,
			source.GroupID,
			source.FileID,
			source.StorageID,
			source.Pages,
			source.FirstPage,
			source.LastPage,
			source.RowsLocated,
			source.RowsExported,
			source.RowsFailed,
			attribution,
		)
		fmt.Fprintln(stdout, line)
		s.log("[RECOVERY] " + line)
	}
}

func normalizeCaseSensitiveParameter(value string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "auto", "":
		return "auto", true
	case "1", "y", "yes", "true":
		return "1", true
	case "0", "n", "no", "false":
		return "0", true
	default:
		return "", false
	}
}

func (s *interactiveSession) dmpCaseSensitiveValue() *bool {
	value, ok := normalizeCaseSensitiveParameter(s.caseSensitive)
	if !ok {
		return nil
	}
	if value == "auto" {
		if s.dictionary == nil || !s.dictionary.HasCaseSensitive {
			return nil
		}
		enabled := s.dictionary.CaseSensitive
		return &enabled
	}
	enabled := value == "1"
	return &enabled
}

func (s *interactiveSession) findTable(owner string, tableName string) (dm.DictionaryTable, bool) {
	owner = normalizeIdentifierInput(owner)
	tableName = normalizeIdentifierInput(tableName)
	for _, table := range s.dictionary.Tables {
		if strings.EqualFold(table.Owner, owner) && strings.EqualFold(table.Name, tableName) {
			return table, true
		}
	}
	return dm.DictionaryTable{}, false
}

func (s *interactiveSession) hasOwner(owner string) bool {
	owner = normalizeIdentifierInput(owner)
	for _, user := range s.dictionary.Users {
		if strings.EqualFold(user.Name, owner) {
			return true
		}
	}
	return false
}

func (s *interactiveSession) ensureDictionaryLoaded() error {
	if s.dictionary != nil {
		return nil
	}
	if err := s.loadDictionaryFilesMode(io.Discard, true); err != nil {
		return fmt.Errorf("dictionary not loaded, run bootstrap first or load dictionary files from %s: %w", s.effectiveDictionaryDir(), err)
	}
	return nil
}

func (s *interactiveSession) loadDictionaryFiles(stdout io.Writer) error {
	return s.loadDictionaryFilesMode(stdout, false)
}

func (s *interactiveSession) loadDictionaryFilesMode(stdout io.Writer, validateCurrentSystem bool) error {
	dict, files, err := dm.LoadDictionaryFiles(s.effectiveDictionaryDir())
	if err != nil {
		return err
	}
	if validateCurrentSystem {
		if err := s.validateAutoLoadedDictionary(dict); err != nil {
			return err
		}
	}
	s.dictionary = dict
	if strings.TrimSpace(dict.SystemPath) != "" {
		s.systemPath = dict.SystemPath
	}
	if strings.TrimSpace(dict.ControlPath) != "" {
		s.controlPath = dict.ControlPath
		s.controlProvided = true
	}
	if detectedCharset, ok := charsetParameterFromDictionary(dict.Charset); ok && !s.charsetExplicit {
		s.charset = detectedCharset
	}
	s.applyDictionaryMetadata(dict)
	debug.FreeOSMemory()
	if stdout != io.Discard {
		fmt.Fprintf(stdout, "dictionary loaded: %s\n", files.Dir)
		fmt.Fprintf(stdout, "users=%d schemas=%d tables=%d columns=%d views=%d sequences=%d routines=%d triggers=%d synonyms=%d tab_privs=%d partitions=%d partition_keys=%d\n",
			files.UserCount, files.SchemaCount, files.TableCount, files.ColumnCount, files.ViewCount, files.SequenceCount, files.RoutineCount, files.TriggerCount, files.SynonymCount, files.TabPrivilegeCount, files.PartitionCount, files.PartitionKeyCount)
	}
	return nil
}

func (s *interactiveSession) validateAutoLoadedDictionary(dict *dm.DictionaryInfo) error {
	if dict == nil || strings.TrimSpace(dict.SystemPath) == "" || strings.TrimSpace(s.systemPath) == "" {
		return nil
	}
	if info, err := os.Stat(s.systemPath); err != nil || info.IsDir() {
		return nil
	}
	currentPath, err := filepath.Abs(s.systemPath)
	if err != nil {
		return nil
	}
	dictionaryPath, err := filepath.Abs(dict.SystemPath)
	if err != nil {
		return nil
	}
	if !strings.EqualFold(filepath.Clean(currentPath), filepath.Clean(dictionaryPath)) {
		return fmt.Errorf("existing dictionary belongs to SYSTEM.DBF %q, current system is %q; run bootstrap to rebuild it or explicitly load dictionary after verification", dict.SystemPath, s.systemPath)
	}
	metadata := dm.InspectDatabaseMetadata(s.systemPath, "", "", "auto")
	if dict.PageSize != 0 && metadata.PageSize != 0 && dict.PageSize != metadata.PageSize {
		return fmt.Errorf("existing dictionary page size %d does not match current SYSTEM.DBF page size %d; run bootstrap", dict.PageSize, metadata.PageSize)
	}
	if dict.PageCount != 0 && metadata.PageCount != 0 && dict.PageCount != metadata.PageCount {
		return fmt.Errorf("existing dictionary page count %d does not match current SYSTEM.DBF page count %d; run bootstrap", dict.PageCount, metadata.PageCount)
	}
	return nil
}

func (s *interactiveSession) outputPath(name string) string {
	if filepath.IsAbs(name) {
		return name
	}
	return filepath.Join(s.effectiveOutputDir(), name)
}

func (s *interactiveSession) effectiveDataDir() string {
	if s.dataDirSet && strings.TrimSpace(s.dataDir) != "" {
		return s.dataDir
	}
	dir := filepath.Dir(defaultIfBlank(s.systemPath, defaultSystemPath))
	if dir == "" {
		return "."
	}
	return dir
}

func (s *interactiveSession) effectiveOutputDir() string {
	if s.outputDirSet {
		return defaultIfBlank(s.outputDir, ".")
	}
	if currentDir, err := os.Getwd(); err == nil && strings.TrimSpace(currentDir) != "" {
		return filepath.Join(currentDir, defaultOutputDirName)
	}
	return defaultOutputDirName
}

func (s *interactiveSession) effectiveWorkDir() string {
	if s.dataDirSet && strings.TrimSpace(s.dataDir) != "" {
		return s.dataDir
	}
	return "."
}

func (s *interactiveSession) effectiveControlDULPath() string {
	return filepath.Join(s.effectiveWorkDir(), defaultControlDULPath)
}

func (s *interactiveSession) effectiveDictionaryDir() string {
	return filepath.Join(s.effectiveWorkDir(), dm.DefaultDictionaryDirName)
}

func (s *interactiveSession) effectiveInitDULPath() string {
	return filepath.Join(s.effectiveWorkDir(), defaultInitDULPath)
}

func (s *interactiveSession) effectiveLogPath() string {
	if s.logPathSet {
		if filepath.IsAbs(s.logPath) {
			return s.logPath
		}
		return filepath.Join(s.effectiveWorkDir(), s.logPath)
	}
	return filepath.Join(s.effectiveWorkDir(), "dul.log")
}

func (s *interactiveSession) ensureOutputDir() error {
	return os.MkdirAll(s.effectiveOutputDir(), 0755)
}

func (s *interactiveSession) loadConfigFile(stderr io.Writer) {
	path := defaultInitDULPath
	if err := s.loadInitDUL(path); err != nil {
		if !os.IsNotExist(err) {
			fmt.Fprintf(stderr, "warning: read %s: %v\n", path, err)
		}
	}
}

func (s *interactiveSession) loadInitDULCommand(stdout io.Writer, commandName string) error {
	path := s.effectiveInitDULPath()
	if err := s.loadInitDUL(path); err != nil {
		return err
	}
	s.dictionary = nil
	if commandName == "parameter" {
		fmt.Fprintf(stdout, "parameters loaded: %s\n", path)
	} else {
		fmt.Fprintf(stdout, "init loaded: %s\n", path)
	}
	return nil
}

func (s *interactiveSession) loadInitDUL(path string) error {
	s.charsetExplicit = false
	metadata := dm.DefaultDatabaseMetadata()
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		line = strings.TrimPrefix(line, "\ufeff")
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "--") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = normalizeParameterName(key)
		value = strings.TrimSpace(value)
		switch key {
		case "system", "system_file", "file":
			s.systemPath = value
		case "control", "ctl":
			s.controlPath = value
			s.controlProvided = value != ""
		case "data_dir", "datadir":
			s.dataDir = value
			s.dataDirSet = value != ""
		case "data_format", "format":
			value = strings.ToLower(value)
			if value == "sql" || value == "csv" || value == "fldr" || value == "dmp" {
				s.dataFormat = value
			}
		case "case_sensitive":
			if normalized, ok := normalizeCaseSensitiveParameter(value); ok {
				s.caseSensitive = normalized
			}
		case "charset":
			s.charset = value
		case "db_name", "database_name":
			metadata.DatabaseName = defaultIfBlank(value, dm.DefaultDatabaseName)
		case "db_name_source", "database_name_source":
			metadata.DatabaseNameSrc = defaultIfBlank(value, "DM default")
		case "instance_name":
			metadata.InstanceName = defaultIfBlank(value, dm.DefaultInstanceName)
		case "instance_name_source":
			metadata.InstanceNameSrc = defaultIfBlank(value, "DM default")
		case "extent_size":
			if parsed, ok := parsePersistedUint32(value); ok {
				metadata.ExtentSize = parsed
			}
		case "extent_size_source":
			metadata.ExtentSizeSource = value
		case "page_size":
			if parsed, ok := parsePersistedUint32(value); ok {
				metadata.PageSize = parsed
			}
		case "page_size_source":
			metadata.PageSizeSource = value
		case "page_count":
			if parsed, ok := parsePersistedUint32(value); ok {
				metadata.PageCount = parsed
			}
		case "page_count_source":
			metadata.PageCountSource = value
		case "database_charset", "db_charset":
			metadata.Charset = value
		case "unicode_flag", "charset_flag":
			if parsed, ok := parsePersistedUint8(value); ok {
				metadata.CharsetFlag = parsed
				metadata.HasCharsetFlag = true
				if display, ok := databaseCharsetDisplay(parsed); ok {
					metadata.Charset = display
				}
			}
		case "charset_source":
			metadata.CharsetSource = value
		case "case_sensitive_value", "case_effective":
			if normalized, ok := normalizeCaseSensitiveParameter(value); ok && normalized != "auto" {
				metadata.CaseSensitive = normalized == "1"
				metadata.HasCaseSensitive = true
			}
		case "case_sensitive_source":
			metadata.CaseSensitiveSource = value
		case "ini_path":
			metadata.IniPath = value
		case "output_dir", "outdir":
			s.outputDir = value
			s.outputDirSet = value != ""
		case "log":
			s.logPath = value
			s.logPathSet = value != ""
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	metadata.SystemPath = s.systemPath
	metadata.ControlPath = s.controlPath
	if strings.TrimSpace(metadata.IniPath) == "" {
		metadata.IniPath = dm.DefaultIniPathForSystem(s.systemPath)
	}
	s.metadata = metadata
	s.initSource = path
	return nil
}

func (s *interactiveSession) writeInitDUL() error {
	path := s.effectiveInitDULPath()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(s.initDULContent()), 0644)
}

func (s *interactiveSession) initDULContent() string {
	var out strings.Builder
	out.WriteString("# Generated by dmdul interactive shell.\n")
	out.WriteString("# This file mirrors show parameter; blank values keep automatic defaults.\n")
	out.WriteString(fmt.Sprintf("# effective_control_dul=%s\n", s.effectiveControlDULPath()))
	out.WriteString(fmt.Sprintf("# effective_dict_dir=%s\n", s.effectiveDictionaryDir()))
	out.WriteString(fmt.Sprintf("# effective_init_dul=%s\n", s.effectiveInitDULPath()))
	out.WriteString(fmt.Sprintf("# effective_output_dir=%s\n", s.effectiveOutputDir()))
	out.WriteString(fmt.Sprintf("# effective_log=%s\n", s.effectiveLogPath()))
	out.WriteString(fmt.Sprintf("# init_load=%s\n", defaultIfBlank(s.initSource, "(not loaded)")))
	if s.dictionary != nil {
		out.WriteString(fmt.Sprintf("# dictionary=%s\n", defaultIfBlank(s.dictionary.Source, "memory")))
		if s.dictionary.HasCaseSensitive {
			effective := 0
			if s.dictionary.CaseSensitive {
				effective = 1
			}
			out.WriteString(fmt.Sprintf("# effective_case_sensitive=%d (%s)\n", effective, s.dictionary.CaseSensitiveSource))
		}
	} else {
		out.WriteString("# dictionary=not loaded\n")
	}
	out.WriteString("# Bootstrap database parameters. Sources are persisted for offline reuse.\n")
	out.WriteString(fmt.Sprintf("db_name=%s\n", s.metadata.DatabaseName))
	out.WriteString(fmt.Sprintf("db_name_source=%s\n", s.metadata.DatabaseNameSrc))
	out.WriteString(fmt.Sprintf("instance_name=%s\n", s.metadata.InstanceName))
	out.WriteString(fmt.Sprintf("instance_name_source=%s\n", s.metadata.InstanceNameSrc))
	out.WriteString(fmt.Sprintf("extent_size=%d\n", s.metadata.ExtentSize))
	out.WriteString(fmt.Sprintf("extent_size_source=%s\n", s.metadata.ExtentSizeSource))
	out.WriteString(fmt.Sprintf("page_size=%d\n", s.metadata.PageSize))
	out.WriteString(fmt.Sprintf("page_size_source=%s\n", s.metadata.PageSizeSource))
	out.WriteString(fmt.Sprintf("page_count=%d\n", s.metadata.PageCount))
	out.WriteString(fmt.Sprintf("page_count_source=%s\n", s.metadata.PageCountSource))
	out.WriteString(fmt.Sprintf("database_charset=%s\n", s.metadata.Charset))
	if s.metadata.HasCharsetFlag {
		out.WriteString(fmt.Sprintf("unicode_flag=%d\n", s.metadata.CharsetFlag))
	}
	out.WriteString(fmt.Sprintf("charset_source=%s\n", s.metadata.CharsetSource))
	if s.metadata.HasCaseSensitive {
		caseSensitive := 0
		if s.metadata.CaseSensitive {
			caseSensitive = 1
		}
		out.WriteString(fmt.Sprintf("case_sensitive_value=%d\n", caseSensitive))
	}
	out.WriteString(fmt.Sprintf("case_sensitive_source=%s\n", s.metadata.CaseSensitiveSource))
	out.WriteString(fmt.Sprintf("ini_path=%s\n", s.metadata.IniPath))
	out.WriteString(fmt.Sprintf("system=%s\n", s.systemPath))
	out.WriteString(fmt.Sprintf("control=%s\n", s.controlPath))
	if s.dataDirSet {
		out.WriteString(fmt.Sprintf("data_dir=%s\n", s.dataDir))
	} else {
		out.WriteString("data_dir=\n")
	}
	if s.outputDirSet {
		out.WriteString(fmt.Sprintf("output_dir=%s\n", s.outputDir))
	} else {
		out.WriteString("output_dir=\n")
	}
	out.WriteString(fmt.Sprintf("data_format=%s\n", s.dataFormat))
	out.WriteString(fmt.Sprintf("case_sensitive=%s\n", s.caseSensitive))
	out.WriteString(fmt.Sprintf("charset=%s\n", s.charset))
	if s.logPathSet {
		out.WriteString(fmt.Sprintf("log=%s\n", s.logPath))
	} else {
		out.WriteString("log=\n")
	}
	return out.String()
}

func (s *interactiveSession) openLog() {
	path := s.effectiveLogPath()
	if s.logFile != nil && s.logOpenPath == path {
		return
	}
	if s.logFile != nil {
		_ = s.logFile.Close()
		s.logFile = nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		if s.stderr != nil {
			fmt.Fprintf(s.stderr, "warning: create log directory %s: %v\n", filepath.Dir(path), err)
		}
		return
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		if s.stderr != nil {
			fmt.Fprintf(s.stderr, "warning: open log %s: %v\n", path, err)
		}
		return
	}
	s.logFile = file
	s.logOpenPath = path
}

func (s *interactiveSession) closeLog() {
	if s.logFile == nil {
		return
	}
	s.log("DMDUL session ended")
	_ = s.logFile.Close()
}

func (s *interactiveSession) log(line string) {
	s.openLog()
	if s.logFile == nil {
		return
	}
	fmt.Fprintln(s.logFile, timestampedLogLine(line, time.Now()))
}

func timestampedLogLine(line string, at time.Time) string {
	return fmt.Sprintf("%s %s", at.Format("2006-01-02 15:04:05"), line)
}

func splitInteractiveCommands(line string) []string {
	var commands []string
	for _, part := range strings.Split(line, ";") {
		part = strings.TrimSpace(part)
		if part != "" {
			commands = append(commands, part)
		}
	}
	return commands
}

func splitCommandFields(command string) []string {
	var fields []string
	var current strings.Builder
	inQuote := false
	for _, r := range command {
		switch {
		case r == '"':
			inQuote = !inQuote
			current.WriteRune(r)
		case unicode.IsSpace(r) && !inQuote:
			if current.Len() > 0 {
				fields = append(fields, strings.TrimSpace(current.String()))
				current.Reset()
			}
		default:
			current.WriteRune(r)
		}
	}
	if current.Len() > 0 {
		fields = append(fields, strings.TrimSpace(current.String()))
	}
	return fields
}

func parseOwnerTableToken(token string) (string, string, bool) {
	parts := splitQualifiedToken(token)
	if len(parts) != 2 {
		return "", "", false
	}
	return normalizeIdentifierInput(parts[0]), normalizeIdentifierInput(parts[1]), true
}

func splitQualifiedToken(token string) []string {
	var parts []string
	var current strings.Builder
	inQuote := false
	for _, r := range token {
		switch r {
		case '"':
			inQuote = !inQuote
			current.WriteRune(r)
		case '.':
			if inQuote {
				current.WriteRune(r)
				continue
			}
			parts = append(parts, current.String())
			current.Reset()
		default:
			current.WriteRune(r)
		}
	}
	parts = append(parts, current.String())
	return parts
}

func normalizeIdentifierInput(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 2 && strings.HasPrefix(value, `"`) && strings.HasSuffix(value, `"`) {
		return strings.ReplaceAll(value[1:len(value)-1], `""`, `"`)
	}
	return strings.ToUpper(value)
}

func optionalToPrefix(args []string) (string, bool) {
	if len(args) == 0 {
		return "", false
	}
	if len(args) >= 2 && strings.EqualFold(args[0], "to") {
		return strings.Join(args[1:], " "), true
	}
	return "", false
}

func sanitizedFilePrefix(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "dmdul"
	}
	var out strings.Builder
	for _, r := range value {
		switch r {
		case '<', '>', ':', '"', '/', '\\', '|', '?', '*', '.', ' ', '\t':
			out.WriteByte('_')
		default:
			out.WriteRune(r)
		}
	}
	result := strings.Trim(out.String(), "_")
	if result == "" {
		return "dmdul"
	}
	return result
}

func qualifiedTableOutputKey(owner string, table string) string {
	return strings.ToUpper(strings.TrimSpace(owner)) + "\x00" + strings.ToUpper(strings.TrimSpace(table))
}

func normalizeParameterName(value string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(value)), "-", "_")
}

func (s *interactiveSession) resetDatabaseMetadata() {
	metadata := dm.DefaultDatabaseMetadata()
	metadata.SystemPath = s.systemPath
	metadata.ControlPath = s.controlPath
	metadata.IniPath = dm.DefaultIniPathForSystem(s.systemPath)
	s.metadata = metadata
}

// probeDatabaseIdentity does a lightweight inspection of the current SYSTEM.DBF
// (header + optional dm.ctl/ini only, no dictionary scan) and prints a one-line
// identity summary. It gives immediate "am I opening the right database?"
// feedback like Oracle DUL's "Found db_name = ...". announce=true always prints
// (including a not-found hint); announce=false stays silent when nothing is
// readable, so startup auto-probing never adds noise.
func (s *interactiveSession) probeDatabaseIdentity(stdout io.Writer, announce bool) {
	path := strings.TrimSpace(s.systemPath)
	if path == "" {
		return
	}
	if info, err := os.Stat(path); err != nil || info.IsDir() {
		if announce {
			fmt.Fprintf(stdout, "detected: SYSTEM.DBF not readable at %s\n", path)
		}
		return
	}
	meta := dm.InspectDatabaseMetadata(path, s.controlPath, dm.DefaultIniPathForSystem(path), s.charset)
	s.metadata = meta
	if meta.PageSize == 0 {
		if announce {
			fmt.Fprintf(stdout, "detected: %s is not a recognizable SYSTEM.DBF\n", path)
		}
		return
	}
	parts := []string{}
	if name := strings.TrimSpace(meta.DatabaseName); name != "" {
		parts = append(parts, "db_name="+name)
	}
	if name := strings.TrimSpace(meta.InstanceName); name != "" {
		parts = append(parts, "instance="+name)
	}
	parts = append(parts, fmt.Sprintf("page_size=%d", meta.PageSize))
	if meta.PageCount > 0 {
		parts = append(parts, fmt.Sprintf("pages=%d", meta.PageCount))
	}
	if charset := strings.TrimSpace(meta.Charset); charset != "" {
		parts = append(parts, "charset="+charset)
	}
	if meta.HasCaseSensitive {
		cs := 0
		if meta.CaseSensitive {
			cs = 1
		}
		parts = append(parts, fmt.Sprintf("case_sensitive=%d", cs))
	}
	fmt.Fprintf(stdout, "detected: %s (SYSTEM.DBF: %s)\n", strings.Join(parts, " "), path)
	s.log("[DETECT] " + strings.Join(parts, " ") + " path=" + path)
}

// autoProbeStartupSystem loads the default database on startup: a SYSTEM.DBF
// next to the dmdul executable (the drop-in location), or in the current
// directory. When found it becomes the active system/data_dir and its identity
// is printed, so the user can run bootstrap immediately with no setup. When
// none is found it prints a one-line hint to set the paths manually.
func (s *interactiveSession) autoProbeStartupSystem(stdout io.Writer) {
	var exeDir string
	if exe, err := os.Executable(); err == nil {
		exeDir = filepath.Dir(exe)
	}
	candidates := []string{}
	if exeDir != "" {
		candidates = append(candidates, filepath.Join(exeDir, defaultSystemPath))
	}
	candidates = append(candidates, defaultSystemPath) // current directory
	for _, candidate := range candidates {
		info, err := os.Stat(candidate)
		if err != nil || info.IsDir() {
			continue
		}
		s.systemPath = candidate
		s.dataDir = filepath.Dir(candidate)
		s.dataDirSet = true
		s.probeDatabaseIdentity(stdout, false)
		return
	}
	where := "the executable directory or current directory"
	if exeDir != "" {
		where = exeDir
	}
	fmt.Fprintf(stdout, "no SYSTEM.DBF found in %s; run: set system <SYSTEM.DBF path>; set data_dir <DBF directory>;\n", where)
}

func (s *interactiveSession) applyDictionaryMetadata(dict *dm.DictionaryInfo) {
	if dict == nil {
		return
	}
	s.metadata.SystemPath = defaultIfBlank(dict.SystemPath, s.systemPath)
	s.metadata.ControlPath = defaultIfBlank(dict.ControlPath, s.controlPath)
	if dict.ExtentSize != 0 {
		s.metadata.ExtentSize = dict.ExtentSize
		s.metadata.ExtentSizeSource = defaultIfBlank(dict.ExtentSizeSource, "dmdul_dict/meta.tsv")
	}
	if dict.PageSize != 0 {
		s.metadata.PageSize = dict.PageSize
		s.metadata.PageSizeSource = "dmdul_dict/meta.tsv"
	}
	s.metadata.PageCount = dict.PageCount
	s.metadata.PageCountSource = "dmdul_dict/meta.tsv"
	if strings.TrimSpace(dict.Charset) != "" {
		s.metadata.Charset = dict.Charset
		s.metadata.CharsetSource = defaultIfBlank(dict.CharsetSource, "dmdul_dict/meta.tsv")
		if flag, ok := unicodeFlagFromCharset(dict.Charset); ok {
			s.metadata.CharsetFlag = flag
			s.metadata.HasCharsetFlag = true
		}
	}
	if dict.HasCaseSensitive {
		s.metadata.CaseSensitive = dict.CaseSensitive
		s.metadata.HasCaseSensitive = true
		s.metadata.CaseSensitiveSource = defaultIfBlank(dict.CaseSensitiveSource, "dmdul_dict/meta.tsv")
	}
}

func parsePersistedUint32(value string) (uint32, bool) {
	parsed, err := strconv.ParseUint(strings.TrimSpace(value), 10, 32)
	return uint32(parsed), err == nil
}

func parsePersistedUint8(value string) (uint8, bool) {
	parsed, err := strconv.ParseUint(strings.TrimSpace(value), 10, 8)
	return uint8(parsed), err == nil
}

func databaseCharsetDisplay(flag uint8) (string, bool) {
	switch flag {
	case 0:
		return "GB18030 (UNICODE_FLAG=0)", true
	case 1:
		return "UTF-8 (UNICODE_FLAG=1)", true
	case 2:
		return "EUC-KR (UNICODE_FLAG=2)", true
	default:
		return fmt.Sprintf("unknown (UNICODE_FLAG=%d)", flag), false
	}
}

func unicodeFlagFromCharset(value string) (uint8, bool) {
	value = strings.ToUpper(strings.TrimSpace(value))
	switch {
	case strings.Contains(value, "UNICODE_FLAG=0") || strings.Contains(value, "GB18030") || strings.Contains(value, "GBK"):
		return 0, true
	case strings.Contains(value, "UNICODE_FLAG=1") || strings.Contains(value, "UTF-8") || strings.Contains(value, "UTF8"):
		return 1, true
	case strings.Contains(value, "UNICODE_FLAG=2") || strings.Contains(value, "EUC-KR") || strings.Contains(value, "EUCKR"):
		return 2, true
	default:
		return 0, false
	}
}

func charsetParameterFromDictionary(value string) (string, bool) {
	value = strings.ToUpper(strings.TrimSpace(value))
	switch {
	case strings.Contains(value, "GB18030") || strings.Contains(value, "GBK") || strings.Contains(value, "UNICODE_FLAG=0"):
		return "gb18030", true
	case strings.Contains(value, "UTF-8") || strings.Contains(value, "UTF8") || strings.Contains(value, "UNICODE_FLAG=1"):
		return "utf-8", true
	case strings.Contains(value, "EUC-KR") || strings.Contains(value, "EUCKR") || strings.Contains(value, "UNICODE_FLAG=2"):
		return "euc-kr", true
	default:
		return "", false
	}
}

func (s *interactiveSession) bootstrapCharset(systemPath string, controlPath string) string {
	configured := defaultIfBlank(s.charset, "auto")
	if s.charsetExplicit {
		return configured
	}
	metadata := dm.InspectDatabaseMetadata(systemPath, controlPath, "", configured)
	if metadata.HasCharsetFlag {
		if detected, ok := charsetParameterFromDictionary(metadata.Charset); ok {
			return detected
		}
	}
	return configured
}
