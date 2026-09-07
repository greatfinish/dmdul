package cli

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"dmdul/internal/dm"
)

func (s *interactiveSession) executeStorageScan(args []string, out io.Writer) error {
	if len(args) != 1 || !strings.EqualFold(args[0], "storage") {
		return fmt.Errorf("usage: scan storage")
	}
	if len(s.asmDisks) != 0 {
		return fmt.Errorf("storage rescue currently requires filesystem DBFs; use cp datafile first")
	}
	r, err := dm.ScanStorages(dm.StorageScanOptions{DataDir: s.effectiveDataDir(), OutputDir: s.effectiveOutputDir()})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "storage inventory: %s\nraw samples: %s\nscan errors: %s\n", r.SummaryPath, r.SamplesPath, r.ErrorsPath)
	fmt.Fprintf(out, "files scanned: %d\npages scanned: %d\nstorage buckets: %d\ninvalid pages: %d\n", r.Files, r.Pages, r.Storages, r.InvalidPages)
	fmt.Fprintln(out, "attribution: UNATTRIBUTED (no dictionary used)")
	s.log(fmt.Sprintf("[STORAGE_SCAN] files=%d pages=%d buckets=%d invalid=%d report=%s", r.Files, r.Pages, r.Storages, r.InvalidPages, r.SummaryPath))
	return nil
}

func (s *interactiveSession) executeStorageRecovery(args []string, out io.Writer) error {
	usage := "usage: recover storage <group>.<storage_id> using <columns.tsv> as <owner.table> [residual]"
	if (len(args) != 5 && len(args) != 6) || !strings.EqualFold(args[1], "using") || !strings.EqualFold(args[3], "as") {
		return fmt.Errorf("%s", usage)
	}
	residual := len(args) == 6
	if residual && !strings.EqualFold(args[5], "residual") {
		return fmt.Errorf("%s", usage)
	}
	parts := strings.Split(args[0], ".")
	if len(parts) != 2 {
		return fmt.Errorf("%s", usage)
	}
	group, e1 := strconv.ParseUint(parts[0], 10, 16)
	storage, e2 := strconv.ParseUint(parts[1], 10, 32)
	owner, table, ok := parseOwnerTableToken(args[4])
	if e1 != nil || e2 != nil || !ok || storage == 0 {
		return fmt.Errorf("%s", usage)
	}
	if len(s.asmDisks) != 0 {
		return fmt.Errorf("storage rescue currently requires filesystem DBFs; use cp datafile first")
	}
	path := s.outputPath(sanitizedFilePrefix(fmt.Sprintf("%s_%s_storage_%d_%d", owner, table, group, storage)) + "." + dataOutputExtension(s.dataFormat))
	columnsPath := args[2]
	if n := len(columnsPath); n >= 2 && (columnsPath[0] == '"' || columnsPath[0] == '\'') && columnsPath[n-1] == columnsPath[0] {
		columnsPath = columnsPath[1 : n-1]
	}
	var caseSensitive *bool
	if s.caseSensitive == "0" || s.caseSensitive == "1" {
		enabled := s.caseSensitive == "1"
		caseSensitive = &enabled
	}
	r, err := dm.RecoverStorage(dm.StorageRecoveryOptions{
		DataDir: s.effectiveDataDir(), OutputPath: path, ColumnsPath: columnsPath, Owner: owner, Table: table,
		Charset: s.charset, OutputFormat: s.dataFormat, GroupID: uint32(group), StorageID: uint32(storage), IncludeResidual: residual, CaseSensitive: caseSensitive,
	})
	if err != nil {
		return err
	}
	if r.RowsExported == 0 && s.dataFormat != "sql" {
		fmt.Fprintln(out, "data output: skipped (no rows)")
	} else {
		fmt.Fprintf(out, "data output: %s\n", path)
	}
	fmt.Fprintf(out, "row evidence: %s\npages matched: %d\nrows exported: %d\nrows failed: %d\n", r.EvidencePath, r.Pages, r.RowsExported, r.RowsFailed)
	fmt.Fprintln(out, "attribution: OPERATOR_SUPPLIED; transaction visibility is not established")
	s.log(fmt.Sprintf("[STORAGE_RECOVERY] group=%d storage=%d residual=%t exported=%d failed=%d evidence=%s", group, storage, residual, r.RowsExported, r.RowsFailed, r.EvidencePath))
	return nil
}
