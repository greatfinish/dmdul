package dm

import (
	"encoding/binary"
	"fmt"
	"sort"
)

type segmentPageStats struct {
	files map[dataFileKey]map[uint32]bool
}

func inferDictionaryTableSegments(controlPath string, controlDULPath string, dataDir string, sources []OfflineDataSource, pageSize uint32, extentSize uint32, tables map[uint32]dictionaryObject, indexObjects map[uint32]dictionaryObject, indexes map[uint32]indexDef, partitionsByTable map[uint32][]PartitionInfo, tableList []DictionaryTable) (map[uint32]tableSegment, error) {
	if pageSize == 0 || len(tableList) == 0 {
		return nil, nil
	}
	tableSet := make(map[uint32]bool, len(tableList))
	for _, table := range tableList {
		if table.Temporary {
			continue
		}
		tableSet[table.ID] = true
	}
	if len(tableSet) == 0 {
		return nil, nil
	}

	var refs []dataFileRef
	var err error
	if len(sources) > 0 {
		refs, err = dataFileRefsFromSources(sources)
	} else {
		refs, err = resolveDataFiles(controlPath, controlDULPath, dataDir)
	}
	if err != nil {
		return nil, err
	}
	if len(refs) == 0 {
		return nil, nil
	}

	stats, completePlans := inferDictionarySegmentStatsFromPagePlans(refs, pageSize, extentSize, tables, indexObjects, indexes, partitionsByTable, tableList)
	result := dictionarySegmentsFromStats(stats, pageSize, extentSize)
	if len(sources) > 0 || len(completePlans) == len(tableSet) {
		return result, nil
	}

	assistToTables := dictionaryAssistIDsByTable(tableSet, tables, indexObjects, indexes, partitionsByTable)
	if len(assistToTables) == 0 {
		return result, nil
	}
	for _, ref := range refs {
		_, err := forEachDataFilePage(ref.path, pageSize, func(page []byte, pageNo uint32) error {
			if !isProbableSegmentAssistPage(page, pageSize) {
				return nil
			}
			assistID := binary.LittleEndian.Uint32(page[dataPageAssistIndexOff:])
			tableIDs := assistToTables[assistID]
			if len(tableIDs) == 0 {
				return nil
			}
			for _, tableID := range tableIDs {
				if completePlans[tableID] {
					continue
				}
				stat := stats[tableID]
				if stat == nil {
					stat = &segmentPageStats{files: make(map[dataFileKey]map[uint32]bool)}
					stats[tableID] = stat
				}
				pages := stat.files[ref.key]
				if pages == nil {
					pages = make(map[uint32]bool)
					stat.files[ref.key] = pages
				}
				extentStart := pageNo
				if extentSize > 0 {
					extentStart = (pageNo / extentSize) * extentSize
				}
				pages[extentStart] = true
			}
			return nil
		})
		if err != nil {
			continue
		}
	}
	for tableID, segment := range dictionarySegmentsFromStats(stats, pageSize, extentSize) {
		result[tableID] = segment
	}
	return result, nil
}

func inferDictionarySegmentStatsFromPagePlans(refs []dataFileRef, pageSize uint32, extentSize uint32, tables map[uint32]dictionaryObject, indexObjects map[uint32]dictionaryObject, indexes map[uint32]indexDef, partitionsByTable map[uint32][]PartitionInfo, tableList []DictionaryTable) (map[uint32]*segmentPageStats, map[uint32]bool) {
	cache := newDataFilePageCache(refs, pageSize)
	stats := make(map[uint32]*segmentPageStats)
	complete := make(map[uint32]bool)
	for _, table := range tableList {
		if table.Temporary {
			continue
		}
		var roots []indexDef
		if table.StorageID != 0 && table.RootFile >= 0 && table.RootPage > 0 && table.GroupID <= uint32(^uint16(0)) {
			roots = append(roots, indexDef{ID: table.StorageID, GroupID: uint16(table.GroupID), RootFile: table.RootFile, RootPage: int32(table.RootPage), Flag: 1})
		} else {
			roots = append(roots, dictionaryPhysicalStorageRoots(table.ID, tables[table.ID], indexObjects, indexes)...)
		}
		for _, part := range partitionsByTable[table.ID] {
			roots = append(roots, dictionaryPhysicalStorageRoots(part.PartTableID, tables[table.ID], indexObjects, indexes)...)
		}
		seenRoots := make(map[string]bool)
		attemptedRoots := 0
		failedRoot := false
		for _, root := range roots {
			key := fmt.Sprintf("%d/%d/%d/%d", root.ID, root.GroupID, root.RootFile, root.RootPage)
			if root.ID == 0 || root.RootFile < 0 || root.RootPage <= 0 || seenRoots[key] {
				continue
			}
			seenRoots[key] = true
			attemptedRoots++
			plan, _ := buildStoragePagePlanDetailed(root, cache)
			if len(plan) == 0 {
				failedRoot = true
				continue
			}
			addDictionarySegmentPage(stats, table.ID, dataPageRef{key: dataFileKey{groupID: uint32(root.GroupID), fileID: root.RootFile}, pageNo: uint32(root.RootPage)}, extentSize)
			for ref := range plan {
				addDictionarySegmentPage(stats, table.ID, ref, extentSize)
			}
		}
		if attemptedRoots > 0 && !failedRoot {
			complete[table.ID] = true
		}
	}
	return stats, complete
}

func dictionaryPhysicalStorageRoots(physicalTableID uint32, table dictionaryObject, indexObjects map[uint32]dictionaryObject, indexes map[uint32]indexDef) []indexDef {
	var roots []indexDef
	for indexID, obj := range indexObjects {
		if uint32(obj.ParentID) != physicalTableID {
			continue
		}
		idx, ok := indexes[indexID]
		if !ok || idx.RootFile < 0 || idx.RootPage <= 0 {
			continue
		}
		if idx.Flag&1 != 0 && idx.KeyNum == 0 || table.isIOTTable() && idx.Flag&0x4 != 0 {
			roots = append(roots, idx)
		}
	}
	sort.Slice(roots, func(i, j int) bool { return roots[i].ID < roots[j].ID })
	return roots
}

func addDictionarySegmentPage(stats map[uint32]*segmentPageStats, tableID uint32, ref dataPageRef, extentSize uint32) {
	stat := stats[tableID]
	if stat == nil {
		stat = &segmentPageStats{files: make(map[dataFileKey]map[uint32]bool)}
		stats[tableID] = stat
	}
	pages := stat.files[ref.key]
	if pages == nil {
		pages = make(map[uint32]bool)
		stat.files[ref.key] = pages
	}
	extentStart := ref.pageNo
	if extentSize > 0 {
		extentStart = (ref.pageNo / extentSize) * extentSize
	}
	pages[extentStart] = true
}

func dictionarySegmentsFromStats(stats map[uint32]*segmentPageStats, pageSize uint32, extentSize uint32) map[uint32]tableSegment {
	result := make(map[uint32]tableSegment)
	for tableID, stat := range stats {
		if segment, ok := stat.bestSegment(pageSize, extentSize); ok {
			result[tableID] = segment
		}
	}
	return result
}

func dictionaryAssistIDsByTable(tableSet map[uint32]bool, tables map[uint32]dictionaryObject, indexObjects map[uint32]dictionaryObject, indexes map[uint32]indexDef, partitionsByTable map[uint32][]PartitionInfo) map[uint32][]uint32 {
	result := make(map[uint32][]uint32)
	assistByParentID := assistIndexesByParentID(tables, indexObjects, indexes)
	for tableID := range tableSet {
		addDictionaryAssistIDs(result, tableID, tableID, assistByParentID, indexObjects)
		for _, part := range partitionsByTable[tableID] {
			addDictionaryAssistIDs(result, tableID, part.PartTableID, assistByParentID, indexObjects)
		}
	}
	return result
}

func addDictionaryAssistIDs(result map[uint32][]uint32, baseTableID uint32, physicalTableID uint32, assistByParentID map[uint32][]indexDef, indexObjects map[uint32]dictionaryObject) {
	seen := make(map[uint32]bool)
	add := func(assistID uint32) {
		if assistID == 0 || seen[assistID] {
			return
		}
		result[assistID] = append(result[assistID], baseTableID)
		seen[assistID] = true
	}
	add(tableDataAssistID(physicalTableID))
	for _, storage := range assistByParentID[physicalTableID] {
		add(storage.ID)
	}
	for indexID, obj := range indexObjects {
		if uint32(obj.ParentID) == physicalTableID && isAutoHiddenIndexObject(obj) {
			add(indexID)
		}
	}
}

func isProbableSegmentAssistPage(page []byte, pageSize uint32) bool {
	if len(page) < int(pageSize) || len(page) < dataPageAssistIndexOff+4 {
		return false
	}
	assistID := binary.LittleEndian.Uint32(page[dataPageAssistIndexOff:])
	if assistID == 0 {
		return false
	}
	nSlot := binary.LittleEndian.Uint16(page[dataPageSlotCountOff:])
	freeEnd := binary.LittleEndian.Uint16(page[dataPageFreeEndOff:])
	nRec := binary.LittleEndian.Uint16(page[dataPageRecordCountOff:])
	if nSlot >= 2048 || nRec > nSlot {
		return false
	}
	return freeEnd >= 0x52 && uint32(freeEnd) <= pageSize
}

func (s *segmentPageStats) bestSegment(pageSize uint32, extentSize uint32) (tableSegment, bool) {
	if s == nil || len(s.files) == 0 {
		return tableSegment{}, false
	}
	var bestKey dataFileKey
	var bestPages map[uint32]bool
	for key, pages := range s.files {
		if len(pages) == 0 {
			continue
		}
		if bestPages == nil || len(pages) > len(bestPages) || (len(pages) == len(bestPages) && lessDataFileKey(key, bestKey)) {
			bestKey = key
			bestPages = pages
		}
	}
	if len(bestPages) == 0 {
		return tableSegment{}, false
	}
	extentStarts := segmentExtentStarts(bestPages, extentSize)
	if len(extentStarts) == 0 {
		return tableSegment{}, false
	}
	sort.Slice(extentStarts, func(i, j int) bool { return extentStarts[i] < extentStarts[j] })
	blocksPerExtent := extentSize
	if blocksPerExtent == 0 {
		blocksPerExtent = uint32(len(bestPages))
	}
	blocks := uint32(len(extentStarts)) * blocksPerExtent
	return tableSegment{
		fileID:       bestKey.fileID,
		headerPage:   extentStarts[0],
		blocks:       blocks,
		extents:      uint32(len(extentStarts)),
		bytes:        uint64(blocks) * uint64(pageSize),
		tablespaceID: bestKey.groupID,
	}, true
}

func segmentExtentStarts(pages map[uint32]bool, extentSize uint32) []uint32 {
	seen := make(map[uint32]bool)
	var starts []uint32
	for pageNo := range pages {
		start := pageNo
		if extentSize > 0 {
			start = (pageNo / extentSize) * extentSize
		}
		if seen[start] {
			continue
		}
		seen[start] = true
		starts = append(starts, start)
	}
	return starts
}

func lessDataFileKey(left dataFileKey, right dataFileKey) bool {
	if left.groupID != right.groupID {
		return left.groupID < right.groupID
	}
	return left.fileID < right.fileID
}
