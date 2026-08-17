package dm

// Offline page-corruption diagnosis (the `check` command).
//
// dmdul does not trust PAGE_CHECK alone: DM instances often run PAGE_CHECK=0,
// and the official dmdbchk manual notes that damage outside the checksum area
// is invisible to a pure checksum scan. So every page is judged on three
// independent layers of evidence, from cheapest to strongest:
//
//	1. checksum   — verify PAGE_CHECK (CRC32 / HASH / CRC32C) when enabled;
//	2. header     — the self-describing (group, file, page) triple must match
//	                the page's physical position, and page_kind must be known;
//	3. structure  — for row/data pages, the slot count, free-space end, record
//	                count and slot-directory offsets must be mutually consistent
//	                and rows must decode, mirroring dmdbchk's rec_total_len check.
//
// A page is reported once, tagged with the first (most fundamental) layer that
// fails. Empty/unused pages (all-zero body, kind 0) are not corruption.

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// PageCorruptionKind classifies why a page failed, ordered by how fundamental
// the failure is. The scanner reports the most fundamental failing layer.
type PageCorruptionKind string

const (
	PageCorruptionHeader    PageCorruptionKind = "HEADER_INVALID"
	PageCorruptionChecksum  PageCorruptionKind = "CHECKSUM_FAIL"
	PageCorruptionStructure PageCorruptionKind = "STRUCTURE_INVALID"
)

// PageCheckOptions configures an offline page-corruption scan.
type PageCheckOptions struct {
	SystemPath     string
	ControlPath    string
	ControlDULPath string
	DataDir        string
	PageSize       uint32
	// PageCheckMode is the PAGE_CHECK value (0..3). When 0, the checksum layer
	// is skipped and only header/structure evidence is used.
	PageCheckMode uint32
	PageHashName  string
	// FileFilter, when non-empty, restricts the scan to data files whose path
	// base name matches one of the entries (case-insensitive).
	FileFilter []string
	// MaxReported caps the per-file bad-page list retained in memory; the
	// totals stay exact. Zero means the built-in default.
	MaxReported int
	// Dictionary, when set, enables storage-scoped diagnosis: bad pages are
	// attributed to owner.table, B-tree leaf chains are walked for break/cycle
	// detection, and dictionary self-consistency is checked. Without it, only
	// the whole-file physical page scan runs.
	Dictionary *DictionaryInfo
	// FollowControlPaths resolves data files through dm.ctl / control.dul
	// absolute paths. It is OFF by default for check: those paths often point
	// at the live database's original location, so following them would scan
	// the running files instead of the offline copies under DataDir. When off,
	// files are located by scanning DataDir and reading page headers; control
	// metadata is still used only for tablespace names.
	FollowControlPaths bool
	// OnBadPage receives every corrupt page after dictionary attribution. It is
	// called synchronously while scanning, so callers can stream a complete
	// detail report without retaining every bad page in memory. Returning an
	// error stops the scan.
	OnBadPage func(BadPage) error
}

// PageObjectType is the strongest object class the recovered dictionary can
// prove for a page. TABLE_ASSIST deliberately does not claim INDEX or LOB:
// the durable dictionary currently maps assist storage to its parent table,
// but does not yet preserve a complete physical index/LOB object catalogue.
type PageObjectType string

const (
	PageObjectTable        PageObjectType = "TABLE"
	PageObjectTableAssist  PageObjectType = "TABLE_ASSIST"
	PageObjectUnattributed PageObjectType = "UNATTRIBUTED"
)

// PageAttributionMethod records the evidence used to map a bad page.
type PageAttributionMethod string

const (
	PageAttributionStorageID       PageAttributionMethod = "storage_id"
	PageAttributionAssistStorageID PageAttributionMethod = "assist_storage_id"
	PageAttributionSegmentRange    PageAttributionMethod = "segment_range"
	PageAttributionNone            PageAttributionMethod = "none"
)

// PageAttributionConfidence makes the strength of object attribution
// explicit in reports. A page-owned storage id is stronger than a segment
// range fallback, while ambiguous/unmapped pages have no attribution.
type PageAttributionConfidence string

const (
	PageAttributionHigh   PageAttributionConfidence = "HIGH"
	PageAttributionMedium PageAttributionConfidence = "MEDIUM"
	PageAttributionNo     PageAttributionConfidence = "NONE"
)

// BadPage locates and classifies one corrupt page. GroupID doubles as the
// tablespace id in dmdbchk's page(tablespace, file, page) coordinate.
type BadPage struct {
	Path                  string
	Tablespace            string
	GroupID               uint32
	FileID                int16
	PageNo                uint32
	StorageID             uint32
	Owner                 string // attributed table owner, when known
	Table                 string // attributed parent table, when known
	TableID               uint32
	ObjectType            PageObjectType
	ObjectStorageID       uint32
	ObjectGroupID         uint32
	ObjectHeaderFile      int16
	ObjectHeaderBlock     uint32
	Attribution           PageAttributionMethod
	AttributionConfidence PageAttributionConfidence
	UnattributedReason    string
	SegmentBytes          uint64
	Kind                  PageCorruptionKind
	Detail                string
}

// PageAffectedObject aggregates corrupt pages for one physical storage object.
// TABLE_ASSIST rows identify the parent table and assist storage id without
// guessing whether the storage is an index, LOB, partition, or another helper.
type PageAffectedObject struct {
	Owner                 string
	Table                 string
	TableID               uint32
	ObjectType            PageObjectType
	StorageID             uint32
	GroupID               uint32
	Tablespace            string
	HeaderFile            int16
	HeaderBlock           uint32
	Attribution           string
	AttributionConfidence PageAttributionConfidence
	BadPages              int
	SegmentHeaderBadPages int
	HeaderInvalid         int
	ChecksumFail          int
	StructureInvalid      int
	SegmentBytes          uint64
}

// ChainIssue records a broken or cyclic B-tree leaf chain for one table
// storage, mirroring dmdbchk's "page is broken, unable to get" index errors.
type ChainIssue struct {
	Owner     string
	Table     string
	StorageID uint32
	RootFile  int16
	RootPage  uint32
	Reason    string
}

// DictIssue records a dictionary self-consistency problem (dangling reference,
// duplicate id). It targets the same goal as dmdbchk's object-id validity
// check — catching impossible catalog entries — without depending on the DM
// id-reserve page layout.
type DictIssue struct {
	Category string
	Detail   string
}

// PageCheckFileResult summarises one data file.
type PageCheckFileResult struct {
	Path            string
	GroupID         uint32
	FileID          int16
	Tablespace      string
	PagesChecked    int
	PagesEmpty      int
	BadPages        int
	Corruption      map[PageCorruptionKind]int
	SizeInvalid     bool
	SizeDetail      string
	Bad             []BadPage
	ReportTruncated bool
}

// PageCheckResult is the whole-scan report.
type PageCheckResult struct {
	PageSize             uint32
	PageCheckMode        uint32
	PageHashName         string
	DictionaryUsed       bool
	DictionarySource     string
	Files                []PageCheckFileResult
	FilesChecked         int
	PagesChecked         int
	PagesEmpty           int
	BadPagesTotal        int
	Corruption           map[PageCorruptionKind]int
	AttributedBadPages   int
	UnattributedBadPages int
	UnattributedReasons  map[string]int
	AffectedObjects      []PageAffectedObject
	AffectedTables       int
	TotalTables          int
	AffectedTableBytes   uint64
	TotalTableBytes      uint64
	ChainIssues          []ChainIssue
	DictIssues           []DictIssue
}

const defaultMaxReportedBadPages = 4096

// CheckPages scans every resolved data file for corrupt pages. It never
// modifies the input files.
func CheckPages(opts PageCheckOptions) (*PageCheckResult, error) {
	pageSize := opts.PageSize
	if pageSize == 0 {
		pageSize = 8192
	}
	if !validPageSize(pageSize) {
		return nil, fmt.Errorf("invalid page size %d", pageSize)
	}
	maxReported := opts.MaxReported
	if maxReported <= 0 {
		maxReported = defaultMaxReportedBadPages
	}
	dataDir := opts.DataDir
	if dataDir == "" && opts.SystemPath != "" {
		dataDir = filepathDir(opts.SystemPath)
	}
	var files []dataFileRef
	var err error
	if opts.FollowControlPaths {
		files, err = resolveDataFiles(opts.ControlPath, opts.ControlDULPath, dataDir)
		if err != nil {
			return nil, err
		}
	} else {
		if strings.TrimSpace(dataDir) == "" {
			return nil, fmt.Errorf("check requires data_dir; set data_dir to the directory holding the DBF files")
		}
		files = resolveDataFilesInDir(dataDir, opts.ControlDULPath)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no DBF files found in data_dir for check")
	}
	filter := newFileBaseFilter(opts.FileFilter)

	result := &PageCheckResult{
		PageSize:      pageSize,
		PageCheckMode: opts.PageCheckMode,
		PageHashName:  opts.PageHashName,
		Corruption:    make(map[PageCorruptionKind]int),
	}
	sortDataFileRefs(files)

	var attribution *pageAttribution
	if opts.Dictionary != nil {
		result.DictionaryUsed = true
		result.DictionarySource = opts.Dictionary.Source
		attribution = newPageAttribution(opts.Dictionary)
	}
	impact := newPageImpactAccumulator(opts.Dictionary)

	for fi := range files {
		file := files[fi]
		if file.path == "" || !filter.allows(file.path) {
			continue
		}
		fileResult, scanErr := checkOneDataFile(file, pageSize, opts, maxReported, attribution, func(bad BadPage) error {
			impact.add(bad)
			if opts.OnBadPage != nil {
				return opts.OnBadPage(bad)
			}
			return nil
		})
		if scanErr != nil {
			return nil, scanErr
		}
		result.Files = append(result.Files, fileResult)
		result.FilesChecked++
		result.PagesChecked += fileResult.PagesChecked
		result.PagesEmpty += fileResult.PagesEmpty
		result.BadPagesTotal += fileResult.BadPages
		for kind, count := range fileResult.Corruption {
			result.Corruption[kind] += count
		}
	}
	impact.apply(result)

	if opts.Dictionary != nil {
		result.ChainIssues = checkLeafChains(opts.Dictionary, files, pageSize)
		result.DictIssues = checkDictionaryConsistency(opts.Dictionary)
	}
	return result, nil
}

// CheckPhysicalPageSource checks one filesystem or logical data file without
// using a recovered dictionary. Bootstrap uses it to validate SYSTEM.DBF
// before attempting catalog recovery; callers can still stream every bad page
// through PageCheckOptions.OnBadPage.
func CheckPhysicalPageSource(source OfflineDataSource, opts PageCheckOptions) (*PageCheckResult, error) {
	pageSize := opts.PageSize
	if pageSize == 0 {
		pageSize = 8192
	}
	if !validPageSize(pageSize) {
		return nil, fmt.Errorf("invalid page size %d", pageSize)
	}
	maxReported := opts.MaxReported
	if maxReported <= 0 {
		maxReported = defaultMaxReportedBadPages
	}
	if source.Reader == nil && strings.TrimSpace(source.Path) == "" {
		return nil, fmt.Errorf("page source has no reader or path")
	}
	file := dataFileRef{
		key:            dataFileKey{groupID: source.GroupID, fileID: source.FileID},
		tablespaceName: source.Tablespace,
		path:           source.Path,
	}
	impact := newPageImpactAccumulator(nil)
	onBadPage := func(bad BadPage) error {
		impact.add(bad)
		if opts.OnBadPage != nil {
			return opts.OnBadPage(bad)
		}
		return nil
	}
	var fileResult PageCheckFileResult
	var err error
	if source.Reader != nil {
		fileResult, err = checkOneDataReader(file, source.Reader, pageSize, opts, maxReported, nil, onBadPage)
	} else {
		fileResult, err = checkOneDataFile(file, pageSize, opts, maxReported, nil, onBadPage)
	}
	if err != nil {
		return nil, err
	}
	result := &PageCheckResult{
		PageSize:      pageSize,
		PageCheckMode: opts.PageCheckMode,
		PageHashName:  opts.PageHashName,
		Files:         []PageCheckFileResult{fileResult},
		FilesChecked:  1,
		PagesChecked:  fileResult.PagesChecked,
		PagesEmpty:    fileResult.PagesEmpty,
		BadPagesTotal: fileResult.BadPages,
		Corruption:    make(map[PageCorruptionKind]int),
	}
	for kind, count := range fileResult.Corruption {
		result.Corruption[kind] = count
	}
	impact.apply(result)
	return result, nil
}

func checkOneDataFile(file dataFileRef, pageSize uint32, opts PageCheckOptions, maxReported int, attribution *pageAttribution, onBadPage func(BadPage) error) (PageCheckFileResult, error) {
	res := newPageCheckFileResult(file)
	size, sizeErr := fileSizeBytes(file.path)
	if sizeErr != nil {
		res.SizeInvalid = true
		res.SizeDetail = sizeErr.Error()
		return res, nil
	}
	return checkOnePageSource(res, size, pageSize, opts, maxReported, attribution, onBadPage,
		func(visit func(page []byte, pageNo uint32) error) (int, error) {
			return forEachDataFilePage(file.path, pageSize, visit)
		})
}

func checkOneDataReader(file dataFileRef, reader SizedReaderAt, pageSize uint32, opts PageCheckOptions, maxReported int, attribution *pageAttribution, onBadPage func(BadPage) error) (PageCheckFileResult, error) {
	res := newPageCheckFileResult(file)
	if reader == nil {
		res.SizeInvalid = true
		res.SizeDetail = "data file reader is nil"
		return res, nil
	}
	return checkOnePageSource(res, reader.Size(), pageSize, opts, maxReported, attribution, onBadPage,
		func(visit func(page []byte, pageNo uint32) error) (int, error) {
			return forEachSizedReaderPage(reader, pageSize, visit)
		})
}

func newPageCheckFileResult(file dataFileRef) PageCheckFileResult {
	return PageCheckFileResult{
		Path:       file.path,
		GroupID:    file.key.groupID,
		FileID:     file.key.fileID,
		Tablespace: file.tablespaceName,
		Corruption: make(map[PageCorruptionKind]int),
	}
}

func checkOnePageSource(res PageCheckFileResult, size int64, pageSize uint32, opts PageCheckOptions, maxReported int, attribution *pageAttribution, onBadPage func(BadPage) error, forEach func(func(page []byte, pageNo uint32) error) (int, error)) (PageCheckFileResult, error) {
	if size < 0 {
		res.SizeInvalid = true
		res.SizeDetail = fmt.Sprintf("invalid negative file size %d", size)
		return res, nil
	}
	if size%int64(pageSize) != 0 {
		res.SizeInvalid = true
		res.SizeDetail = fmt.Sprintf("file size %d is not a multiple of page size %d (truncated or wrong page size)", size, pageSize)
	}

	key := dataFileKey{groupID: res.GroupID, fileID: res.FileID}
	var sinkErr error
	pagesScanned, scanErr := forEach(func(page []byte, pageNo uint32) error {
		res.PagesChecked++
		if isEmptyDMPage(page) {
			res.PagesEmpty++
			return nil
		}
		kind, detail, ok := classifyPageCorruption(page, key, pageNo, pageSize, opts)
		if ok {
			return nil
		}
		res.BadPages++
		res.Corruption[kind]++
		bad := BadPage{
			Path:                  res.Path,
			Tablespace:            res.Tablespace,
			GroupID:               res.GroupID,
			FileID:                res.FileID,
			PageNo:                pageNo,
			StorageID:             dataPageStorageID(page),
			ObjectType:            PageObjectUnattributed,
			Attribution:           PageAttributionNone,
			AttributionConfidence: PageAttributionNo,
			Kind:                  kind,
			Detail:                detail,
		}
		if attribution != nil {
			attribution.apply(&bad)
		} else {
			bad.UnattributedReason = "dictionary_not_loaded"
		}
		if onBadPage != nil {
			if err := onBadPage(bad); err != nil {
				sinkErr = err
				return err
			}
		}
		if len(res.Bad) < maxReported {
			res.Bad = append(res.Bad, bad)
		} else {
			res.ReportTruncated = true
		}
		return nil
	})
	if sinkErr != nil {
		return res, sinkErr
	}
	if scanErr != nil {
		res.SizeInvalid = true
		if res.SizeDetail == "" {
			res.SizeDetail = fmt.Sprintf("read failed after %d pages: %v", pagesScanned, scanErr)
		}
	}
	return res, nil
}

// classifyPageCorruption returns (kind, detail, ok=false) for a corrupt page,
// or ("", "", true) when the page passes every applicable layer.
//
// The scan reports only positive evidence of damage. An unknown page kind is
// NOT corruption on its own: DM allocates many internal page classes (space
// bitmaps, id maps, undo, dictionary) this offline tool does not model, and a
// reserved-but-unused page carries a fill pattern. Flagging "unknown kind"
// floods the report with false positives (verified against dmdbchk, which does
// not flag those pages). We flag exactly three signatures:
//
//   - self-id (group,file,page) mismatches the physical position — a real
//     header smash on a page that is neither empty nor a fill page;
//   - a page stamped with a valid self-id but zeroed kind AND body — a data
//     page zeroed after formatting (distinct from an unused reserved page,
//     which keeps a non-zero fill kind);
//   - a row/data page (kind 0x14/0x16) whose slot directory, free end, record
//     count or row headers are mutually inconsistent (dmdbchk rec_total_len).
func classifyPageCorruption(page []byte, key dataFileKey, pageNo uint32, pageSize uint32, opts PageCheckOptions) (PageCorruptionKind, string, bool) {
	ref := dataPageRef{key: key, pageNo: pageNo}
	kind := dataPageKind(page)
	sid := dataPageStorageID(page)
	nSlot := binary.LittleEndian.Uint16(page[dataPageSlotCountOff:])
	nRec := binary.LittleEndian.Uint16(page[dataPageRecordCountOff:])
	freeEnd := binary.LittleEndian.Uint16(page[dataPageFreeEndOff:])
	bodyBlank := sid == 0 && nSlot == 0 && nRec == 0 && freeEnd == 0

	if !pageHeaderMatchesRef(page, ref) {
		// A fill page (all 0xFF) with a smashed self-id is unformatted space,
		// not corruption of live data.
		if isFillDMPage(page) {
			return "", "", true
		}
		gotGroup := binary.LittleEndian.Uint16(page[0:])
		gotFile := binary.LittleEndian.Uint16(page[2:])
		gotPage := binary.LittleEndian.Uint32(page[4:])
		return PageCorruptionHeader, fmt.Sprintf("page self-id (%d,%d,%d) does not match physical position (%d,%d,%d)",
			gotGroup, gotFile, gotPage, key.groupID, key.fileID, pageNo), false
	}

	// Self-id is valid below. A page stamped in place but with a zeroed kind and
	// blank body was formatted then wiped: a reserved/unused page keeps a
	// non-zero fill kind (e.g. 0xFFFF00FF), so kind==0 here is damage.
	if kind == 0 && bodyBlank {
		return PageCorruptionHeader, "page zeroed after formatting (self-id present but kind and body are zero)", false
	}
	// Reserved/unformatted page with a fill kind: legitimate empty space.
	if bodyBlank {
		return "", "", true
	}

	if opts.PageCheckMode != 0 {
		ok, err := verifyDMPageCheck(page, opts.PageCheckMode, opts.PageHashName)
		if err == nil && !ok {
			return PageCorruptionChecksum, fmt.Sprintf("PAGE_CHECK=%d checksum mismatch", opts.PageCheckMode), false
		}
	}

	// Structure layer: only row/data pages carry a slot directory to validate.
	if kind == dmPageKindRowData || kind == dmPageKindRowOverflow {
		if detail, ok := checkRowPageStructure(page, pageSize); !ok {
			return PageCorruptionStructure, detail, false
		}
	}
	return "", "", true
}

// isFillDMPage reports whether the page is an unformatted fill page (every byte
// 0x00 or every byte 0xFF), which DM uses for reserved/unused space.
func isFillDMPage(page []byte) bool {
	if len(page) == 0 {
		return false
	}
	first := page[0]
	if first != 0x00 && first != 0xFF {
		return false
	}
	for _, b := range page {
		if b != first {
			return false
		}
	}
	return true
}

// dmBoundaryKeySlotOffsets are the fixed leaf-page boundary-key (min/max
// infinity) slot offsets. They sit below the normal row area start and must
// not be validated as ordinary rows; scoreDMPageSlotLayout treats them as the
// strongest healthy signal.
var dmBoundaryKeySlotOffsets = map[uint16]bool{0x52: true, 0x5A: true}

const dmMinSlotOffset = 0x40

// checkRowPageStructure mirrors dmdbchk's page4_validate_check: the slot
// directory, free-space end and record count must be mutually consistent, and
// live rows must tile the used space without overlap or overflow — the
// rec_total_len test. Returns (detail, ok). Boundary-key slots (0x52/0x5A) are
// legitimate markers and are not decoded as rows.
func checkRowPageStructure(page []byte, pageSize uint32) (string, bool) {
	if len(page) < int(pageSize) {
		return "page shorter than page size", false
	}
	nSlot := binary.LittleEndian.Uint16(page[dataPageSlotCountOff:])
	freeEnd := binary.LittleEndian.Uint16(page[dataPageFreeEndOff:])
	nRec := binary.LittleEndian.Uint16(page[dataPageRecordCountOff:])
	treeLevel := binary.LittleEndian.Uint16(page[dataPageTreeLevelOff:])

	if nSlot >= 2048 {
		return fmt.Sprintf("slot count %d exceeds sane bound", nSlot), false
	}
	if nRec > nSlot {
		return fmt.Sprintf("record count %d exceeds slot count %d", nRec, nSlot), false
	}
	if freeEnd < dataRowAreaStart || uint32(freeEnd) > pageSize {
		return fmt.Sprintf("free-space end 0x%X out of range [0x%X, 0x%X]", freeEnd, dataRowAreaStart, pageSize), false
	}
	// A non-zero tree level marks a B-tree internal/branch page (also page
	// kind 0x14) whose body is a key/child array, not a row directory. An empty
	// leaf page (no records) has only boundary-key slots. Neither carries rows
	// to validate, and both are legitimate, so accept them.
	if treeLevel != 0 || nRec == 0 {
		return "", true
	}
	slotArrayStart := int(pageSize) - pageSlotTrailerLenForPage(page) - int(nSlot)*2
	if nSlot > 0 && (slotArrayStart < dmMinSlotOffset || slotArrayStart+int(nSlot)*2 > int(pageSize)) {
		return fmt.Sprintf("slot array start 0x%X out of range for %d slots", slotArrayStart, nSlot), false
	}

	// Collect live row spans (offset, length) from ordinary slots, then verify
	// they lie within the used area and do not overlap — a corrupt row length
	// (e.g. smashed header bytes) produces an overflow or overlap here.
	type rowSpan struct{ start, end int }
	spans := make([]rowSpan, 0, nSlot)
	for slotNo := uint16(1); slotNo <= nSlot; slotNo++ {
		pos := slotArrayStart + int(slotNo-1)*2
		rowOff := binary.LittleEndian.Uint16(page[pos:])
		if rowOff == 0 || rowOff == 0xFFFF || dmBoundaryKeySlotOffsets[rowOff] {
			continue
		}
		if rowOff < dmMinSlotOffset || uint32(rowOff) >= uint32(freeEnd) {
			return fmt.Sprintf("slot %d offset 0x%X outside used area [0x%X, 0x%X)", slotNo, rowOff, dmMinSlotOffset, freeEnd), false
		}
		header, ok := decodeDataRowHeader(page, rowOff, pageSize, freeEnd)
		if !ok {
			return fmt.Sprintf("slot %d row header at 0x%X is undecodable (rec length inconsistent with page used space)", slotNo, rowOff), false
		}
		spans = append(spans, rowSpan{start: int(rowOff), end: int(rowOff) + int(header.length)})
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	for i := 1; i < len(spans); i++ {
		if spans[i].start < spans[i-1].end {
			return fmt.Sprintf("row at 0x%X overlaps previous row ending at 0x%X (corrupt row length)", spans[i].start, spans[i-1].end), false
		}
	}
	if n := len(spans); n > 0 && spans[n-1].end > int(freeEnd) {
		return fmt.Sprintf("last row ends at 0x%X beyond free-space end 0x%X", spans[n-1].end, freeEnd), false
	}
	return "", true
}

// isEmptyDMPage reports whether a page is unused (all-zero) rather than corrupt.
// A never-written page has a zero page-kind and no self-id.
func isEmptyDMPage(page []byte) bool {
	if len(page) < dataPageAssistIndexOff+4 {
		return true
	}
	if dataPageKind(page) != 0 {
		return false
	}
	if binary.LittleEndian.Uint32(page[4:]) != 0 {
		return false
	}
	for _, b := range page[:0x40] {
		if b != 0 {
			return false
		}
	}
	return true
}

// SortedBadPages returns all bad pages across files in (group, file, page)
// order for stable reporting and dmdbchk cross-checking.
func (r *PageCheckResult) SortedBadPages() []BadPage {
	var all []BadPage
	for i := range r.Files {
		all = append(all, r.Files[i].Bad...)
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].GroupID != all[j].GroupID {
			return all[i].GroupID < all[j].GroupID
		}
		if all[i].FileID != all[j].FileID {
			return all[i].FileID < all[j].FileID
		}
		return all[i].PageNo < all[j].PageNo
	})
	return all
}

// resolveDataFilesInDir locates DBF files strictly inside dataDir by reading
// their page headers, never following control absolute paths. Tablespace names
// are still taken from control.dul metadata for readable labels.
func resolveDataFilesInDir(dataDir string, controlDULPath string) []dataFileRef {
	tablespaceNames := defaultTablespaceNames()
	mergeControlDULTablespaceNames(tablespaceNames, controlDULPath)
	refs := scanDataFilesByPageHeader(dataDir, tablespaceNames, make(map[dataFileKey]bool))
	sortDataFileRefs(refs)
	return refs
}

func filepathDir(path string) string {
	if path == "" {
		return "."
	}
	return filepath.Dir(path)
}

func fileSizeBytes(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	if info.IsDir() {
		return 0, fmt.Errorf("%s is a directory", path)
	}
	return info.Size(), nil
}

func sortDataFileRefs(files []dataFileRef) {
	sort.Slice(files, func(i, j int) bool {
		if files[i].key.groupID != files[j].key.groupID {
			return files[i].key.groupID < files[j].key.groupID
		}
		if files[i].key.fileID != files[j].key.fileID {
			return files[i].key.fileID < files[j].key.fileID
		}
		return files[i].path < files[j].path
	})
}

type fileBaseFilter struct {
	bases map[string]bool
}

func newFileBaseFilter(names []string) fileBaseFilter {
	if len(names) == 0 {
		return fileBaseFilter{}
	}
	bases := make(map[string]bool, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		bases[strings.ToUpper(filepath.Base(name))] = true
	}
	return fileBaseFilter{bases: bases}
}

func (f fileBaseFilter) allows(path string) bool {
	if len(f.bases) == 0 {
		return true
	}
	return f.bases[strings.ToUpper(filepath.Base(path))]
}
