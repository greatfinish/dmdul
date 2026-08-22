package dm

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"sync"
)

const (
	dmPageKindOff              = 0x14
	dmPageNextRefOff           = 0x0E
	dmBTreeLeftmostChildOff    = 0x52
	dmPageKindBTreeLeaf        = 0x14
	dmPageKindBTreeRoot        = 0x15
	maxBTreeDescentDepth       = 64
	maxLeafChainWalkMultiplier = 2
	maxCachedDataFilePages     = 256
	dmIndexXTypeNoBranch       = 0x20
)

type dataFilePageCache struct {
	// mu guards every map below: parallel unload workers resolve LOB and
	// Long Row chains through one shared cache while decoding pages.
	mu       sync.Mutex
	pageSize uint32
	refs     map[dataFileKey]dataFileRef
	sizes    map[dataFileKey]int64
	pages    map[dataPageRef][]byte
	pageFIFO []dataPageRef
	// restoreProtection selects the broader SYSTEM dictionary restoration path.
	// User pages use the stricter structural detector before restoring protected
	// sector-boundary bytes, so ordinary fixed-tail and HASH pages stay intact.
	restoreProtection bool
}

type dataFilePageReader struct {
	pageSize uint32
	refs     map[dataFileKey]dataFileRef
	files    map[dataFileKey]io.ReaderAt
	closers  map[dataFileKey]io.Closer
}

type fixedSizeReaderAt struct {
	io.ReaderAt
	size int64
}

func (r fixedSizeReaderAt) Size() int64 { return r.size }

func sizedReaderAt(reader io.ReaderAt, size int64) SizedReaderAt {
	if source, ok := reader.(SizedReaderAt); ok {
		return source
	}
	return fixedSizeReaderAt{ReaderAt: reader, size: size}
}

func newDataFilePageReader(files []dataFileRef, pageSize uint32) *dataFilePageReader {
	reader := &dataFilePageReader{
		pageSize: pageSize,
		refs:     make(map[dataFileKey]dataFileRef, len(files)),
		files:    make(map[dataFileKey]io.ReaderAt),
		closers:  make(map[dataFileKey]io.Closer),
	}
	for _, file := range files {
		reader.refs[file.key] = file
	}
	return reader
}

func (r *dataFilePageReader) readPage(ref dataPageRef) ([]byte, error) {
	if r == nil || r.pageSize == 0 {
		return nil, fmt.Errorf("invalid data page reader")
	}
	fileRef, ok := r.refs[ref.key]
	if !ok || (fileRef.path == "" && fileRef.reader == nil) {
		return nil, fmt.Errorf("data file group=%d file=%d is unavailable", ref.key.groupID, ref.key.fileID)
	}
	file := r.files[ref.key]
	if file == nil {
		if fileRef.reader != nil {
			file = fileRef.reader
		} else {
			opened, err := os.Open(fileRef.path)
			if err != nil {
				return nil, fmt.Errorf("open data file %s: %w", fileRef.path, err)
			}
			file = opened
			r.closers[ref.key] = opened
		}
		r.files[ref.key] = file
	}
	page := make([]byte, int(r.pageSize))
	n, err := file.ReadAt(page, int64(ref.pageNo)*int64(r.pageSize))
	if err != nil && err != io.EOF {
		return nil, fmt.Errorf("read data page %d from %s: %w", ref.pageNo, fileRef.path, err)
	}
	if n != len(page) {
		return nil, fmt.Errorf("read data page %d from %s: short read %d/%d", ref.pageNo, fileRef.path, n, len(page))
	}
	restoreUserDataPageProtectionBytes(page, r.pageSize)
	return page, nil
}

func (r *dataFilePageReader) close() error {
	if r == nil {
		return nil
	}
	var firstErr error
	for key, file := range r.closers {
		if err := file.Close(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("close data file group=%d file=%d: %w", key.groupID, key.fileID, err)
		}
	}
	r.files = make(map[dataFileKey]io.ReaderAt)
	r.closers = make(map[dataFileKey]io.Closer)
	return firstErr
}

func newDataFilePageCache(files []dataFileRef, pageSize uint32) *dataFilePageCache {
	cache := &dataFilePageCache{
		pageSize: pageSize,
		refs:     make(map[dataFileKey]dataFileRef, len(files)),
		sizes:    make(map[dataFileKey]int64),
		pages:    make(map[dataPageRef][]byte),
	}
	for _, file := range files {
		cache.refs[file.key] = file
	}
	return cache
}

// newSystemDictionaryPageCache builds a page cache for SYSTEM.DBF dictionary
// reads, where sector-boundary protection-byte restoration is proven safe.
func newSystemDictionaryPageCache(files []dataFileRef, pageSize uint32) *dataFilePageCache {
	cache := newDataFilePageCache(files, pageSize)
	cache.restoreProtection = true
	return cache
}

func (c *dataFilePageCache) readPage(ref dataPageRef) ([]byte, bool) {
	if c == nil || c.pageSize == 0 {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if page, ok := c.pages[ref]; ok {
		return page, true
	}
	pageSize := int(c.pageSize)
	if pageSize <= 0 || int64(ref.pageNo) >= int64(c.pageCountLocked(ref.key)) {
		return nil, false
	}
	file, ok := c.refs[ref.key]
	if !ok || (file.path == "" && file.reader == nil) {
		return nil, false
	}
	var reader io.ReaderAt
	var closer io.Closer
	if file.reader != nil {
		reader = file.reader
	} else {
		opened, err := os.Open(file.path)
		if err != nil {
			return nil, false
		}
		reader, closer = opened, opened
	}
	if closer != nil {
		defer closer.Close()
	}
	page := make([]byte, pageSize)
	n, err := reader.ReadAt(page, int64(ref.pageNo)*int64(pageSize))
	if err != nil || n != pageSize {
		return nil, false
	}
	if c.restoreProtection {
		restorePageProtectionBytes(page, c.pageSize)
	} else {
		restoreUserDataPageProtectionBytes(page, c.pageSize)
	}
	if len(c.pages) >= maxCachedDataFilePages && len(c.pageFIFO) > 0 {
		oldest := c.pageFIFO[0]
		c.pageFIFO = c.pageFIFO[1:]
		delete(c.pages, oldest)
	}
	c.pages[ref] = page
	c.pageFIFO = append(c.pageFIFO, ref)
	return page, true
}

func (c *dataFilePageCache) pageCount(key dataFileKey) int {
	if c == nil || c.pageSize == 0 {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pageCountLocked(key)
}

func (c *dataFilePageCache) hasFile(key dataFileKey) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	file, ok := c.refs[key]
	return ok && (file.path != "" || file.reader != nil)
}

func (c *dataFilePageCache) pageCountLocked(key dataFileKey) int {
	if c == nil || c.pageSize == 0 {
		return 0
	}
	size, ok := c.sizes[key]
	if !ok {
		file, ok := c.refs[key]
		if !ok || (file.path == "" && file.reader == nil) {
			return 0
		}
		if file.reader != nil {
			size = file.reader.Size()
		} else {
			info, err := os.Stat(file.path)
			if err != nil || info.IsDir() {
				return 0
			}
			size = info.Size()
		}
		c.sizes[key] = size
	}
	if size < int64(c.pageSize) {
		return 0
	}
	return int(size / int64(c.pageSize))
}

func (c *dataFilePageCache) totalPageCount() int {
	if c == nil {
		return 0
	}
	total := 0
	for key := range c.refs {
		total += c.pageCount(key)
	}
	return total
}

func buildStoragePagePlan(storage indexDef, cache *dataFilePageCache) map[dataPageRef]bool {
	plan, _ := buildStoragePagePlanDetailed(storage, cache)
	return plan
}

func buildStoragePagePlanDetailed(storage indexDef, cache *dataFilePageCache) (map[dataPageRef]bool, string) {
	if cache == nil || storage.ID == 0 || storage.RootFile < 0 || storage.RootPage < 0 {
		return nil, "storage root metadata is incomplete"
	}
	root := dataPageRef{
		key: dataFileKey{
			groupID: uint32(storage.GroupID),
			fileID:  storage.RootFile,
		},
		pageNo: uint32(storage.RootPage),
	}
	if !cache.hasFile(root.key) {
		return nil, fmt.Sprintf("data file group=%d file=%d is unavailable", root.key.groupID, root.key.fileID)
	}
	rootPage, ok := cache.readPage(root)
	if !ok {
		return nil, fmt.Sprintf("cannot read root page %d/%d", storage.RootFile, storage.RootPage)
	}
	if !pageHeaderMatchesRef(rootPage, root) {
		return nil, fmt.Sprintf("root page identity mismatch at %d/%d", storage.RootFile, storage.RootPage)
	}
	if dataPageStorageID(rootPage) != storage.ID {
		return nil, fmt.Sprintf("root page storage_id=%d, expected %d", dataPageStorageID(rootPage), storage.ID)
	}
	if storage.XType&dmIndexXTypeNoBranch != 0 {
		return buildNoBranchPagePlanDetailed(storage, cache, root, rootPage)
	}
	switch dataPageKind(rootPage) {
	case dmPageKindBTreeLeaf:
		plan, complete, reason := walkLeafChainDetailed(cache, root, storage.ID)
		if !complete {
			return nil, reason
		}
		return plan, ""
	case dmPageKindBTreeRoot:
		leaf, reason, ok := descendLeftmostLeafDetailed(cache, root, storage.ID)
		if !ok {
			return nil, reason
		}
		plan, complete, reason := walkLeafChainDetailed(cache, leaf, storage.ID)
		if !complete {
			return nil, reason
		}
		return plan, ""
	default:
		return nil, fmt.Sprintf("unsupported root page kind 0x%X", dataPageKind(rootPage))
	}
}

func buildNoBranchPagePlanDetailed(storage indexDef, cache *dataFilePageCache, root dataPageRef, rootPage []byte) (map[dataPageRef]bool, string) {
	if dataPageKind(rootPage) != dmPageKindBTreeLeaf {
		return nil, fmt.Sprintf("unsupported NOBRANCH root page kind 0x%X", dataPageKind(rootPage))
	}
	anchors := noBranchChildPageRefs(rootPage, root, storage.ID, cache)
	if len(anchors) == 0 {
		// A newly created or single-page heap can keep rows on the dictionary
		// root itself. Only accept it as a leaf when it has live records.
		if len(rootPage) >= dataPageRecordCountOff+2 && binary.LittleEndian.Uint16(rootPage[dataPageRecordCountOff:]) > 0 {
			return map[dataPageRef]bool{root: true}, ""
		}
		return nil, "NOBRANCH root has no validated data-chain reference"
	}
	plan := make(map[dataPageRef]bool)
	for _, anchor := range anchors {
		chain, complete, reason := walkLeafChainDetailed(cache, anchor, storage.ID)
		if !complete {
			return nil, reason
		}
		for ref := range chain {
			plan[ref] = true
		}
	}
	return plan, ""
}

func noBranchChildPageRefs(page []byte, root dataPageRef, storageID uint32, cache *dataFilePageCache) []dataPageRef {
	if len(page) < dataPageSlotCountOff+2 {
		return nil
	}
	branchCount := int(binary.LittleEndian.Uint16(page[dataPageSlotCountOff:]))
	if branchCount <= 0 || branchCount >= 2048 {
		return nil
	}
	// NOBRANCH roots use 12-byte branch descriptors starting at the ordinary
	// row-area boundary. The six-byte file/page reference begins four bytes
	// into each descriptor. Scan the bounded descriptor area and only retain
	// references whose target page identity and storage id both validate.
	end := dataRowAreaStart + branchCount*12
	if end > len(page) {
		end = len(page)
	}
	seen := make(map[dataPageRef]bool)
	var refs []dataPageRef
	for off := dataRowAreaStart + 4; off+6 <= end; off += 2 {
		fileID, pageNo, ok := readDMPageRef(page, off)
		if !ok || pageNo == 0 {
			continue
		}
		ref := dataPageRef{key: dataFileKey{groupID: root.key.groupID, fileID: fileID}, pageNo: pageNo}
		if ref == root || seen[ref] {
			continue
		}
		child, ok := cache.readPage(ref)
		if !ok || !pageHeaderMatchesRef(child, ref) || dataPageKind(child) != dmPageKindBTreeLeaf || dataPageStorageID(child) != storageID {
			continue
		}
		seen[ref] = true
		refs = append(refs, ref)
	}
	return refs
}

func descendLeftmostLeafDetailed(cache *dataFilePageCache, start dataPageRef, storageID uint32) (dataPageRef, string, bool) {
	current := start
	seen := make(map[dataPageRef]bool)
	for depth := 0; depth < maxBTreeDescentDepth; depth++ {
		if seen[current] {
			return dataPageRef{}, fmt.Sprintf("internal page cycle at file=%d page=%d", current.key.fileID, current.pageNo), false
		}
		seen[current] = true
		page, ok := cache.readPage(current)
		if !ok {
			return dataPageRef{}, fmt.Sprintf("cannot read internal page file=%d page=%d", current.key.fileID, current.pageNo), false
		}
		if !pageHeaderMatchesRef(page, current) {
			return dataPageRef{}, fmt.Sprintf("internal page identity mismatch at file=%d page=%d", current.key.fileID, current.pageNo), false
		}
		if dataPageStorageID(page) != storageID {
			return dataPageRef{}, fmt.Sprintf("internal page storage_id=%d, expected %d", dataPageStorageID(page), storageID), false
		}
		switch dataPageKind(page) {
		case dmPageKindBTreeLeaf:
			return current, "", true
		case dmPageKindBTreeRoot:
			childPage, ok := btreeLeftmostChildPage(page)
			if ok {
				childRef := dataPageRef{key: current.key, pageNo: childPage}
				if childPage, childOK := cache.readPage(childRef); childOK && pageHeaderMatchesRef(childPage, childRef) && dataPageStorageID(childPage) == storageID && isBTreePlanPageKind(dataPageKind(childPage)) {
					current = childRef
					continue
				}
			}
			nextRef, ok := btreeNextInternalPage(page, current.key.groupID)
			if !ok {
				return dataPageRef{}, fmt.Sprintf("internal page file=%d page=%d has no valid child or next reference", current.key.fileID, current.pageNo), false
			}
			current = nextRef
		default:
			return dataPageRef{}, fmt.Sprintf("unexpected internal page kind 0x%X at file=%d page=%d", dataPageKind(page), current.key.fileID, current.pageNo), false
		}
	}
	return dataPageRef{}, fmt.Sprintf("internal descent exceeded %d pages", maxBTreeDescentDepth), false
}

func isBTreePlanPageKind(kind uint32) bool {
	return kind == dmPageKindBTreeLeaf || kind == dmPageKindBTreeRoot
}

func btreeNextInternalPage(page []byte, groupID uint32) (dataPageRef, bool) {
	nextFileID, nextPageNo, ok := readDMPageRef(page, dmPageNextRefOff)
	if !ok {
		return dataPageRef{}, false
	}
	return dataPageRef{
		key: dataFileKey{
			groupID: groupID,
			fileID:  nextFileID,
		},
		pageNo: nextPageNo,
	}, true
}

func walkLeafChainDetailed(cache *dataFilePageCache, start dataPageRef, storageID uint32) (map[dataPageRef]bool, bool, string) {
	planned := make(map[dataPageRef]bool)
	current := start
	maxSteps := cache.totalPageCount() * maxLeafChainWalkMultiplier
	if maxSteps <= 0 {
		maxSteps = 1
	}
	for steps := 0; steps < maxSteps; steps++ {
		if planned[current] {
			return nil, false, fmt.Sprintf("leaf page cycle at file=%d page=%d", current.key.fileID, current.pageNo)
		}
		page, ok := cache.readPage(current)
		if !ok {
			return nil, false, fmt.Sprintf("cannot read leaf page file=%d page=%d", current.key.fileID, current.pageNo)
		}
		if !pageHeaderMatchesRef(page, current) {
			return nil, false, fmt.Sprintf("leaf page identity mismatch at file=%d page=%d", current.key.fileID, current.pageNo)
		}
		if dataPageKind(page) != dmPageKindBTreeLeaf {
			return nil, false, fmt.Sprintf("unexpected leaf page kind 0x%X at file=%d page=%d", dataPageKind(page), current.key.fileID, current.pageNo)
		}
		if dataPageStorageID(page) != storageID {
			return nil, false, fmt.Sprintf("leaf page storage_id=%d, expected %d", dataPageStorageID(page), storageID)
		}
		planned[current] = true
		if isNullDMPageRef(page, dmPageNextRefOff) {
			return planned, true, ""
		}
		nextFileID, nextPageNo, ok := readDMPageRef(page, dmPageNextRefOff)
		if !ok {
			return nil, false, fmt.Sprintf("invalid leaf next reference at file=%d page=%d", current.key.fileID, current.pageNo)
		}
		current = dataPageRef{
			key: dataFileKey{
				groupID: current.key.groupID,
				fileID:  nextFileID,
			},
			pageNo: nextPageNo,
		}
	}
	return nil, false, fmt.Sprintf("leaf chain exceeded %d pages", maxSteps)
}

func isNullDMPageRef(page []byte, offset int) bool {
	if offset < 0 || len(page) < offset+6 {
		return false
	}
	for _, value := range page[offset : offset+6] {
		if value != 0xFF {
			return false
		}
	}
	return true
}

func pageHeaderMatchesRef(page []byte, ref dataPageRef) bool {
	if len(page) < dataPageAssistIndexOff+4 {
		return false
	}
	if binary.LittleEndian.Uint16(page[0:]) != uint16(ref.key.groupID) {
		return false
	}
	if binary.LittleEndian.Uint16(page[2:]) != uint16(ref.key.fileID) {
		return false
	}
	return binary.LittleEndian.Uint32(page[4:]) == ref.pageNo
}

func dataPageKind(page []byte) uint32 {
	if len(page) < dmPageKindOff+4 {
		return 0
	}
	return binary.LittleEndian.Uint32(page[dmPageKindOff:])
}

func dataPageStorageID(page []byte) uint32 {
	if len(page) < dataPageAssistIndexOff+4 {
		return 0
	}
	return binary.LittleEndian.Uint32(page[dataPageAssistIndexOff:])
}

func btreeLeftmostChildPage(page []byte) (uint32, bool) {
	if len(page) < dmBTreeLeftmostChildOff+4 {
		return 0, false
	}
	pageNo := binary.LittleEndian.Uint32(page[dmBTreeLeftmostChildOff:])
	return pageNo, pageNo > 0
}

func readDMPageRef(page []byte, offset int) (int16, uint32, bool) {
	if offset < 0 || len(page) < offset+6 {
		return 0, 0, false
	}
	raw := page[offset : offset+6]
	allFF := true
	for _, b := range raw {
		if b != 0xFF {
			allFF = false
			break
		}
	}
	if allFF {
		return 0, 0, false
	}
	fileID := binary.LittleEndian.Uint16(raw[0:2])
	if fileID > uint16(^uint16(0)>>1) {
		return 0, 0, false
	}
	return int16(fileID), binary.LittleEndian.Uint32(raw[2:6]), true
}
