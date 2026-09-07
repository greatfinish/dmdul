package dm

import (
	"encoding/binary"
	"encoding/csv"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

type StorageScanOptions struct{ DataDir, OutputDir string }
type StorageScanResult struct {
	Files, Pages, Storages, Samples, InvalidPages int
	SummaryPath, SamplesPath, ErrorsPath          string
}
type rawStorageFile struct {
	dataFileRef
	pageSize   uint32
	extentSize uint32
	size       int64
}
type storageScanKey struct {
	file          int
	storage, kind uint32
}
type storageScanCount struct {
	pages, slots, samples int
	first, last           uint32
}

// Raw scans deliberately ignore dm.ctl, control.dul and dmdul_dict. Identity
// and geometry are established from the actual files supplied by the operator.
func rawStorageFiles(dir string) ([]rawStorageFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var files []rawStorageFile
	seen := make(map[dataFileKey]string)
	for _, e := range entries {
		if e.IsDir() || !strings.EqualFold(filepath.Ext(e.Name()), ".dbf") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		if e.Type()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("raw rescue refuses symlink: %s", path)
		}
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		st, err := f.Stat()
		if err != nil {
			f.Close()
			return nil, err
		}
		ps, _, err := ProbePageSize(f, st.Size())
		if err != nil {
			f.Close()
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if st.Size()/int64(ps) > int64(^uint32(0)) {
			f.Close()
			return nil, fmt.Errorf("%s exceeds the 32-bit page identity range", path)
		}
		header, _, err := readSystemHeader(path)
		if err != nil {
			f.Close()
			return nil, err
		}
		extentSize, _ := detectSystemExtentSize(header)
		votes := make(map[dataFileKey]int)
		var head [24]byte
		for p := int64(1); p < st.Size()/int64(ps) && p <= 128; p++ {
			n, readErr := f.ReadAt(head[:], p*int64(ps))
			if readErr != nil || n != len(head) {
				continue
			}
			if binary.LittleEndian.Uint32(head[4:]) != uint32(p) {
				continue
			}
			kind := binary.LittleEndian.Uint32(head[20:])
			if kind == 0 || kind > 0x40 {
				continue
			}
			key := dataFileKey{uint32(binary.LittleEndian.Uint16(head[:])), int16(binary.LittleEndian.Uint16(head[2:]))}
			if key.fileID >= 0 && key.groupID < 65535 {
				votes[key]++
			}
		}
		f.Close()
		var key dataFileKey
		best := 0
		ambiguous := false
		for candidate, n := range votes {
			if n > best {
				key, best, ambiguous = candidate, n, false
			} else if n == best {
				ambiguous = true
			}
		}
		if best < 2 || ambiguous {
			return nil, fmt.Errorf("%s: cannot establish unique group/file identity from physical pages", path)
		}
		if prev, ok := seen[key]; ok {
			return nil, fmt.Errorf("duplicate group=%d file=%d: %s and %s", key.groupID, key.fileID, prev, path)
		}
		seen[key] = path
		files = append(files, rawStorageFile{dataFileRef: dataFileRef{key: key, path: path}, pageSize: ps, extentSize: extentSize, size: st.Size()})
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no DBF files found in %s", dir)
	}
	return files, nil
}

func createStorageTSV(dir, name string, header []string) (*os.File, *csv.Writer, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, nil, err
	}
	f, err := os.CreateTemp(dir, "."+name+"-*")
	if err != nil {
		return nil, nil, err
	}
	w := csv.NewWriter(f)
	w.Comma = '\t'
	if err := w.Write(header); err != nil {
		f.Close()
		os.Remove(f.Name())
		return nil, nil, err
	}
	return f, w, nil
}
func finishStorageTSV(f *os.File, w *csv.Writer, target string) error {
	w.Flush()
	if err := w.Error(); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), target)
}

func ScanStorages(opts StorageScanOptions) (*StorageScanResult, error) {
	files, err := rawStorageFiles(opts.DataDir)
	if err != nil {
		return nil, err
	}
	r := &StorageScanResult{Files: len(files), SummaryPath: filepath.Join(opts.OutputDir, "storage_scan.tsv"), SamplesPath: filepath.Join(opts.OutputDir, "storage_samples.tsv"), ErrorsPath: filepath.Join(opts.OutputDir, "storage_errors.tsv")}
	f, w, err := createStorageTSV(opts.OutputDir, "storage_samples.tsv", []string{"file_path", "group_id", "file_id", "page_size", "storage_id", "page_kind", "page_no", "slot", "row_offset", "row_length", "deleted", "raw_prefix_hex", "prefix_truncated", "transaction_id", "roll_address", "undo_evidence"})
	if err != nil {
		return nil, err
	}
	defer func() { f.Close(); os.Remove(f.Name()) }()
	ef, ew, err := createStorageTSV(opts.OutputDir, "storage_errors.tsv", []string{"file_path", "page_no", "reason"})
	if err != nil {
		return nil, err
	}
	defer func() { ef.Close(); os.Remove(ef.Name()) }()
	counts := make(map[storageScanKey]*storageScanCount)
	undoCaches := make(map[uint32]*dataFilePageCache)
	for i, file := range files {
		if undoCaches[file.pageSize] == nil {
			var matching []dataFileRef
			for _, other := range files {
				if other.pageSize == file.pageSize {
					matching = append(matching, other.dataFileRef)
				}
			}
			undoCaches[file.pageSize] = newDataFilePageCache(matching, file.pageSize)
		}
		if file.size%int64(file.pageSize) != 0 {
			r.InvalidPages++
			_ = ew.Write([]string{file.path, "", "file has a truncated final page"})
		}
		_, err := forEachDataFileRefPage(file.dataFileRef, file.pageSize, func(page []byte, p uint32) error {
			r.Pages++
			if p == 0 {
				return nil // DBF file header is not a data-page identity.
			}
			if isEmptyDMPage(page) || isFillDMPage(page) {
				return nil
			}
			if !pageHeaderMatchesRef(page, dataPageRef{file.key, p}) {
				r.InvalidPages++
				return ew.Write([]string{file.path, strconv.FormatUint(uint64(p), 10), "page identity mismatch"})
			}
			kind := dataPageKind(page)
			sid := dataPageStorageID(page)
			if sid == 0 || (kind != 0x14 && kind != 0x15 && kind != 0x16 && kind != 0x20 && kind != 0x22) {
				return nil
			}
			key := storageScanKey{i, sid, kind}
			c := counts[key]
			if c == nil {
				if len(counts) >= 1_000_000 {
					return fmt.Errorf("storage inventory exceeds 1000000 buckets")
				}
				c = &storageScanCount{first: p}
				counts[key] = c
			}
			c.pages++
			c.last = p
			if kind != 0x14 && kind != 0x16 {
				return nil
			}
			if binary.LittleEndian.Uint16(page[dataPageTreeLevelOff:]) != 0 {
				return nil
			}
			if reason, ok := checkRowPageStructure(page, file.pageSize); !ok {
				r.InvalidPages++
				return ew.Write([]string{file.path, strconv.FormatUint(uint64(p), 10), reason})
			}
			rows := locateRowsInDataPageForRecovery(page, file.pageSize)
			sort.SliceStable(rows, func(i, j int) bool {
				if rows[i].fromSlot != rows[j].fromSlot {
					return rows[i].fromSlot
				}
				// Prefer a sample with a ROLL reference when the page has one.
				hasUndo := func(row locatedDataRow) bool {
					if row.length < 22 {
						return false
					}
					tail, ok := decodeDataRowControlTail(page[int(row.offset) : int(row.offset)+int(row.length)])
					return ok && tail.hasRollbackAddress()
				}
				if hasUndo(rows[i]) != hasUndo(rows[j]) {
					return hasUndo(rows[i])
				}
				return rows[i].slotNo < rows[j].slotNo
			})
			c.slots += len(locateRowsInDataPage(page, file.pageSize, 0))
			for _, row := range rows {
				if c.samples >= 3 {
					break
				}
				raw := page[int(row.offset) : int(row.offset)+int(row.length)]
				truncated := len(raw) > 256
				if truncated {
					raw = raw[:256]
				}
				deleted := binary.BigEndian.Uint16(page[row.offset:])&dataRowDeletedMask != 0
				fullRow := page[int(row.offset) : int(row.offset)+int(row.length)]
				trx, roll := "", ""
				if tail, ok := decodeDataRowControlTail(fullRow); ok && len(fullRow) >= 22 {
					trx = fmt.Sprint(tail.transactionID)
					if tail.hasRollbackAddress() {
						roll = fmt.Sprintf("%d:%d:%d", tail.rollFile, tail.rollPage, tail.rollOffset)
					}
				}
				if err := w.Write([]string{file.path, fmt.Sprint(file.key.groupID), fmt.Sprint(file.key.fileID), fmt.Sprint(file.pageSize), fmt.Sprint(sid), fmt.Sprintf("0x%X", kind), fmt.Sprint(p), fmt.Sprint(row.slotNo), fmt.Sprint(row.offset), fmt.Sprint(row.length), strconv.FormatBool(deleted), hex.EncodeToString(raw), strconv.FormatBool(truncated), trx, roll, traceRowUndoEvidence(fullRow, undoCaches[file.pageSize])}); err != nil {
					return err
				}
				c.samples++
				r.Samples++
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sf, sw, err := createStorageTSV(opts.OutputDir, "storage_scan.tsv", []string{"file_path", "group_id", "file_id", "page_size", "storage_id", "page_kind", "pages", "first_page", "last_page", "slot_rows", "samples", "attribution"})
	if err != nil {
		return nil, err
	}
	defer func() { sf.Close(); os.Remove(sf.Name()) }()
	keys := make([]storageScanKey, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.file != b.file {
			return a.file < b.file
		}
		if a.storage != b.storage {
			return a.storage < b.storage
		}
		return a.kind < b.kind
	})
	for _, k := range keys {
		file, c := files[k.file], counts[k]
		if err := sw.Write([]string{file.path, fmt.Sprint(file.key.groupID), fmt.Sprint(file.key.fileID), fmt.Sprint(file.pageSize), fmt.Sprint(k.storage), fmt.Sprintf("0x%X", k.kind), fmt.Sprint(c.pages), fmt.Sprint(c.first), fmt.Sprint(c.last), fmt.Sprint(c.slots), fmt.Sprint(c.samples), "UNATTRIBUTED"}); err != nil {
			return nil, err
		}
	}
	r.Storages = len(counts)
	if err = finishStorageTSV(f, w, r.SamplesPath); err != nil {
		return nil, err
	}
	if err = finishStorageTSV(ef, ew, r.ErrorsPath); err != nil {
		return nil, err
	}
	if err = finishStorageTSV(sf, sw, r.SummaryPath); err != nil {
		return nil, err
	}
	return r, nil
}

type StorageRecoveryOptions struct {
	DataDir, OutputPath, ColumnsPath, Owner, Table, Charset, OutputFormat string
	GroupID, StorageID                                                    uint32
	IncludeResidual                                                       bool
	CaseSensitive                                                         *bool
}
type StorageRecoveryResult struct {
	Pages, RowsExported, RowsFailed int
	EvidencePath                    string
}

func readStorageColumns(path string) ([]columnDef, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if st.Size() > 1<<20 {
		return nil, fmt.Errorf("manual columns TSV exceeds 1 MiB")
	}
	r := csv.NewReader(io.LimitReader(f, 1<<20))
	r.Comma = '\t'
	head, err := r.Read()
	if err != nil {
		return nil, err
	}
	want := []string{"col_id", "name", "data_type", "length", "scale", "nullable"}
	if strings.Join(head, "\t") != strings.Join(want, "\t") {
		return nil, fmt.Errorf("columns TSV header must be: %s", strings.Join(want, "\t"))
	}
	var cols []columnDef
	names := make(map[string]bool)
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		for _, field := range rec {
			if !utf8.ValidString(field) {
				return nil, fmt.Errorf("columns TSV must be UTF-8")
			}
		}
		if len(cols) >= 1024 {
			return nil, fmt.Errorf("manual column limit is 1024")
		}
		id, e1 := strconv.ParseUint(rec[0], 10, 16)
		length, e2 := strconv.ParseUint(rec[3], 10, 32)
		scale, e3 := strconv.ParseInt(rec[4], 10, 16)
		if e1 != nil || e2 != nil || e3 != nil || id != uint64(len(cols)) || rec[1] == "" || names[rec[1]] || (rec[5] != "Y" && rec[5] != "N") {
			return nil, fmt.Errorf("invalid column row %d: require contiguous zero-based IDs, unique names, numeric length/scale and Y/N nullable", len(cols)+1)
		}
		typ := normalizeDataType(rec[2])
		valid := false
		for _, known := range catalogDataTypeNames {
			if typ == known {
				valid = true
				break
			}
		}
		if typ == "VECTOR" {
			valid = true
		}
		if !valid {
			return nil, fmt.Errorf("unsupported manual column type %q", rec[2])
		}
		names[rec[1]] = true
		cols = append(cols, columnDef{ColID: uint16(id), Name: rec[1], DataType: typ, Length: uint32(length), Scale: int16(scale), Nullable: rec[5]})
	}
	if len(cols) == 0 {
		return nil, fmt.Errorf("columns TSV is empty")
	}
	return cols, nil
}

// RecoverStorage requires operator-supplied identity and column definitions.
// It preserves physical provenance and makes no original-ownership claim.
func RecoverStorage(opts StorageRecoveryOptions) (*StorageRecoveryResult, error) {
	if strings.TrimSpace(opts.OutputPath) == "" {
		return nil, fmt.Errorf("output path is required")
	}
	if opts.StorageID == 0 || opts.Owner == "" || opts.Table == "" {
		return nil, fmt.Errorf("explicit storage_id and destination owner.table are required")
	}
	if opts.Charset == "" || strings.EqualFold(opts.Charset, "auto") {
		return nil, fmt.Errorf("set charset explicitly before dictionary-free recovery")
	}
	switch strings.ToLower(opts.Charset) {
	case "utf-8", "gb18030", "gbk", "euc-kr":
	default:
		return nil, fmt.Errorf("unsupported rescue charset %q", opts.Charset)
	}
	cols, err := readStorageColumns(opts.ColumnsPath)
	if err != nil {
		return nil, err
	}
	files, err := rawStorageFiles(opts.DataDir)
	if err != nil {
		return nil, err
	}
	var refs []dataFileRef
	var ps uint32
	var extentSize uint32
	extentsKnown := true
	for _, f := range files {
		if f.key.groupID == opts.GroupID {
			if f.size%int64(f.pageSize) != 0 {
				return nil, fmt.Errorf("%s has a truncated final page; refusing a silently incomplete recovery", f.path)
			}
			if ps != 0 && ps != f.pageSize {
				return nil, fmt.Errorf("mixed page sizes in group")
			}
			ps = f.pageSize
			if f.extentSize == 0 {
				extentsKnown = false
			}
			if extentSize != 0 && extentSize != f.extentSize && strings.EqualFold(opts.OutputFormat, "dmp") {
				return nil, fmt.Errorf("mixed extent sizes in group")
			}
			extentSize = f.extentSize
			refs = append(refs, f.dataFileRef)
		}
	}
	if len(refs) == 0 {
		return nil, fmt.Errorf("group %d not found", opts.GroupID)
	}
	format := normalizeDataOutputFormat(opts.OutputFormat)
	if format == "" {
		return nil, fmt.Errorf("invalid data format")
	}
	if format == "dmp" && (!extentsKnown || extentSize == 0) {
		return nil, fmt.Errorf("DMP requires a verified extent size; use SQL or fldr rescue")
	}
	if format == "dmp" && opts.CaseSensitive == nil {
		return nil, fmt.Errorf("DMP rescue requires explicit case_sensitive=0 or 1")
	}
	if format == "fldr" && fldrControlFilePath(opts.OutputPath) == opts.OutputPath {
		return nil, fmt.Errorf("fldr data and control paths must differ")
	}
	// A completed output is never silently replaced by a retry.
	for _, path := range []string{opts.OutputPath, opts.OutputPath + ".evidence.tsv", fldrControlFilePath(opts.OutputPath)} {
		if _, err := os.Stat(path); err == nil {
			return nil, fmt.Errorf("output already exists: %s", path)
		} else if !os.IsNotExist(err) {
			return nil, err
		}
	}
	var lobRefs []dataFileRef
	for _, file := range files {
		if file.pageSize == ps {
			lobRefs = append(lobRefs, file.dataFileRef)
		}
	}
	info := dataTableInfo{table: dictionaryObject{ID: opts.StorageID, Owner: opts.Owner, Name: opts.Table}, columns: cols, dataStorageID: opts.StorageID, fldrDialect: fldrDialectForColumns(cols), lobReader: &dmLOBReader{cache: newDataFilePageCache(lobRefs, ps)}}
	config := dataDMPOutputConfig{pageSize: ps, extentSize: extentSize, caseSensitive: opts.CaseSensitive}
	if format == "dmp" {
		config.charset, err = dmpCharsetForDataExport(opts.Charset)
		if err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(filepath.Dir(opts.OutputPath), 0755); err != nil {
		return nil, err
	}
	stage, err := os.MkdirTemp(filepath.Dir(opts.OutputPath), ".storage-recovery-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(stage)
	stagedOutput := filepath.Join(stage, filepath.Base(opts.OutputPath))
	output, err := newDataOutputRouter(DataExportOptions{OutputPath: stagedOutput}, format, map[uint32]dataTableInfo{opts.StorageID: info}, config)
	if err != nil {
		return nil, err
	}
	closed := false
	defer func() {
		if !closed {
			_ = output.close()
		}
	}()
	r := &StorageRecoveryResult{EvidencePath: opts.OutputPath + ".evidence.tsv"}
	f, w, err := createStorageTSV(stage, "storage-recovery", []string{"source_path", "group_id", "file_id", "storage_id", "page_no", "slot", "row_offset", "row_length", "deleted", "destination", "attribution", "status", "reason"})
	if err != nil {
		return nil, err
	}
	defer func() { f.Close(); os.Remove(f.Name()) }()
	decoder := textDecoder{preferred: opts.Charset}
	for _, file := range refs {
		_, err := forEachDataFileRefPage(file, ps, func(page []byte, p uint32) error {
			if !pageHeaderMatchesRef(page, dataPageRef{file.key, p}) || dataPageStorageID(page) != opts.StorageID {
				return nil
			}
			if k := dataPageKind(page); k != 0x14 && k != 0x16 {
				return nil
			}
			if binary.LittleEndian.Uint16(page[dataPageTreeLevelOff:]) != 0 {
				return nil
			}
			r.Pages++
			if reason, ok := checkRowPageStructure(page, ps); !ok {
				return fmt.Errorf("group=%d file=%d page=%d: %s", file.key.groupID, file.key.fileID, p, reason)
			}
			rows := locateRowsInDataPage(page, ps, 0)
			if opts.IncludeResidual {
				rows = locateRowsInDataPageForRecovery(page, ps)
			}
			for _, row := range rows {
				raw := page[int(row.offset) : int(row.offset)+int(row.length)]
				deleted := binary.BigEndian.Uint16(raw)&dataRowDeletedMask != 0
				// Rescue must not make an incorrect manual layout appear valid by
				// guessing historical columns or a different metadata start offset.
				_, _, _, decodeErr := parseDataRowValuesWithMetadata(raw, cols, decoder, info.lobReader)
				status, reason := "EXPORTED", ""
				if decodeErr == nil {
					line, record, fields, _, _, renderErr := renderDataRowForExport(format, info, raw, decoder, config.charset)
					decodeErr = renderErr
					if renderErr == nil {
						if err := output.writeRow(info, line, record, fields); err != nil {
							return err
						}
					}
				}
				if decodeErr != nil {
					r.RowsFailed++
					status = "FAILED"
					reason = decodeErr.Error()
				} else {
					r.RowsExported++
				}
				if err := w.Write([]string{file.path, fmt.Sprint(file.key.groupID), fmt.Sprint(file.key.fileID), fmt.Sprint(opts.StorageID), fmt.Sprint(p), fmt.Sprint(row.slotNo), fmt.Sprint(row.offset), fmt.Sprint(row.length), strconv.FormatBool(deleted), opts.Owner + "." + opts.Table, "OPERATOR_SUPPLIED", status, reason}); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	if r.Pages == 0 {
		return nil, fmt.Errorf("no ordinary leaf pages for group=%d storage_id=%d", opts.GroupID, opts.StorageID)
	}
	err = output.close()
	closed = true
	if err != nil {
		return nil, err
	}
	stagedEvidence := stagedOutput + ".evidence.tsv"
	if err := finishStorageTSV(f, w, stagedEvidence); err != nil {
		return nil, err
	}
	// Hard-link publication is atomic and refuses to replace an existing file,
	// including an output concurrently created after the initial existence check.
	var published []string
	for _, source := range []string{stagedOutput, fldrControlFilePath(stagedOutput), stagedEvidence} {
		if _, err := os.Stat(source); os.IsNotExist(err) {
			continue
		}
		target := filepath.Join(filepath.Dir(opts.OutputPath), filepath.Base(source))
		if err := os.Link(source, target); err != nil {
			for _, p := range published {
				_ = os.Remove(p)
			}
			return nil, fmt.Errorf("publish rescue output: %w", err)
		}
		published = append(published, target)
	}
	return r, nil
}
