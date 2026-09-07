package dm

import (
	"fmt"
	"sort"
	"strings"
)

func formatStoragePlanFailure(info dataTableInfo, storageID uint32, reason string) string {
	if strings.TrimSpace(reason) == "" {
		reason = "storage root did not produce a complete leaf chain"
	}
	return fmt.Sprintf("%s.%s storage unit %d storage_id=%d: %s; scanning the same group by storage_id", info.table.Owner, info.table.Name, info.storageUnitID, storageID, reason)
}

func buildDirectDataPageCandidates(assistByID map[uint32][]dataTableInfo) (map[dataPageRef][]dataTableInfo, map[dataPageRef]bool, map[uint32]bool) {
	pages := make(map[dataPageRef][]dataTableInfo)
	refs := make(map[dataPageRef]bool)
	units := make(map[uint32]bool)
	for _, candidates := range assistByID {
		for _, candidate := range candidates {
			if !candidate.pagePlanKnown || len(candidate.pagePlan) == 0 {
				continue
			}
			units[candidate.storageUnitID] = true
			for ref := range candidate.pagePlan {
				refs[ref] = true
				pages[ref] = appendUniqueDataCandidate(pages[ref], candidate)
			}
		}
	}
	return pages, refs, units
}

func sortedDataPageRefs(refs map[dataPageRef]bool) []dataPageRef {
	result := make([]dataPageRef, 0, len(refs))
	for ref := range refs {
		result = append(result, ref)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].key.groupID != result[j].key.groupID {
			return result[i].key.groupID < result[j].key.groupID
		}
		if result[i].key.fileID != result[j].key.fileID {
			return result[i].key.fileID < result[j].key.fileID
		}
		return result[i].pageNo < result[j].pageNo
	})
	return result
}

func finalizeDataRecoverySources(items map[dataRecoverySourceKey]*DataRecoverySource) []DataRecoverySource {
	result := make([]DataRecoverySource, 0, len(items))
	for _, source := range items {
		if source != nil {
			result = append(result, *source)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Owner != result[j].Owner {
			return result[i].Owner < result[j].Owner
		}
		if result[i].Name != result[j].Name {
			return result[i].Name < result[j].Name
		}
		if result[i].GroupID != result[j].GroupID {
			return result[i].GroupID < result[j].GroupID
		}
		if result[i].FileID != result[j].FileID {
			return result[i].FileID < result[j].FileID
		}
		if result[i].StorageID != result[j].StorageID {
			return result[i].StorageID < result[j].StorageID
		}
		return !result[i].Heuristic && result[j].Heuristic
	})
	return result
}

func sortedSegmentPageRefs(pages map[dataPageRef][]dataTableInfo) []dataPageRef {
	refs := make(map[dataPageRef]bool, len(pages))
	for ref := range pages {
		refs[ref] = true
	}
	return sortedDataPageRefs(refs)
}

func appendUniqueDataCandidate(items []dataTableInfo, candidate dataTableInfo) []dataTableInfo {
	for _, existing := range items {
		if existing.table.ID == candidate.table.ID && existing.storageUnitID == candidate.storageUnitID && existing.storage.ID == candidate.storage.ID && existing.pagePlanKnown == candidate.pagePlanKnown && existing.scanGroupOnly == candidate.scanGroupOnly && existing.historicalRows == candidate.historicalRows && existing.orphanRecovery == candidate.orphanRecovery {
			return items
		}
	}
	return append(items, candidate)
}

func markFailedPlanUnits(failed map[uint32]bool, candidates []dataTableInfo) {
	for _, candidate := range candidates {
		if candidate.storageUnitID != 0 {
			failed[candidate.storageUnitID] = true
		}
	}
}

func plannedDataPageMatches(page []byte, ref dataPageRef, candidates []dataTableInfo) bool {
	if !pageHeaderMatchesRef(page, ref) || dataPageKind(page) != dmPageKindRowData {
		return false
	}
	storageID := dataPageStorageID(page)
	for _, candidate := range candidates {
		if candidate.pagePlanKnown && candidate.pagePlan[ref] && candidate.storage.ID == storageID {
			return true
		}
	}
	return false
}

func formatDirectPageFailure(ref dataPageRef, err error) string {
	return fmt.Sprintf("planned page group=%d file=%d page=%d: %v; enabling storage fallback", ref.key.groupID, ref.key.fileID, ref.pageNo, err)
}

func fallbackGroupForInfo(info dataTableInfo) uint32 {
	if info.storageKnown {
		return uint32(info.storage.GroupID)
	}
	if info.recoveryGroupID != 0 {
		return info.recoveryGroupID
	}
	return info.segment.tablespaceID
}

func buildStorageFallbackCandidates(assistByID map[uint32][]dataTableInfo, plannedUnits map[uint32]bool, failedPlanUnits map[uint32]bool) (map[uint32][]dataTableInfo, map[uint32]bool, map[uint32]bool) {
	result := make(map[uint32][]dataTableInfo)
	groups := make(map[uint32]bool)
	units := make(map[uint32]bool)
	for assistID, candidates := range assistByID {
		for _, candidate := range candidates {
			needsFallback := !plannedUnits[candidate.storageUnitID] || failedPlanUnits[candidate.storageUnitID]
			if !needsFallback || candidate.pagePlanKnown || isLooseHistoricalCandidate(candidate) {
				continue
			}
			candidate.scanGroupOnly = true
			candidate.scanGroupID = fallbackGroupForInfo(candidate)
			result[assistID] = appendUniqueDataCandidate(result[assistID], candidate)
			groups[candidate.scanGroupID] = true
			units[candidate.storageUnitID] = true
		}
	}
	return result, groups, units
}

func unresolvedStorageUnits(storageUnits map[uint32]dataTableInfo, plannedUnits map[uint32]bool, failedPlanUnits map[uint32]bool, fallbackUnits map[uint32]bool, storagePagesFound map[uint32]bool) map[uint32]bool {
	result := make(map[uint32]bool)
	for unitID := range storageUnits {
		if plannedUnits[unitID] && !failedPlanUnits[unitID] {
			continue
		}
		if fallbackUnits[unitID] && storagePagesFound[unitID] {
			continue
		}
		result[unitID] = true
	}
	return result
}

func buildSegmentFallbackPages(storageUnits map[uint32]dataTableInfo, unresolved map[uint32]bool) map[dataPageRef][]dataTableInfo {
	result := make(map[dataPageRef][]dataTableInfo)
	for unitID := range unresolved {
		info, ok := storageUnits[unitID]
		if !ok || !info.segmentKnown || info.segment.blocks == 0 {
			continue
		}
		info.pagePlan = nil
		info.pagePlanKnown = false
		info.scanGroupOnly = false
		info.historicalRows = false
		groupID := info.segment.tablespaceID
		if groupID == 0 {
			groupID = info.recoveryGroupID
		}
		end := uint64(info.segment.headerPage) + uint64(info.segment.blocks)
		if end > uint64(^uint32(0)) {
			end = uint64(^uint32(0))
		}
		for pageNo := uint64(info.segment.headerPage); pageNo < end; pageNo++ {
			ref := dataPageRef{key: dataFileKey{groupID: groupID, fileID: info.segment.fileID}, pageNo: uint32(pageNo)}
			result[ref] = appendUniqueDataCandidate(result[ref], info)
			if info.dataStorageID != 0 {
				historical := info
				historical.historicalRows = true
				result[ref] = appendUniqueDataCandidate(result[ref], historical)
			}
		}
	}
	return result
}

func dataFileRefForKey(files []dataFileRef, key dataFileKey) dataFileRef {
	for _, file := range files {
		if file.key == key {
			return file
		}
	}
	return dataFileRef{key: key}
}

func addKnownDataAssistID(assistByID map[uint32][]dataTableInfo, info dataTableInfo, assistID uint32, storage indexDef, pagePlan map[dataPageRef]bool) {
	if storage.RootFile < 0 {
		return
	}
	info.storage = storage
	info.storageKnown = true
	allowHistoricalRows := shouldAllowHistoricalRows(info, storage.ID)
	if len(pagePlan) > 0 {
		exactInfo := info
		exactInfo.historicalRows = allowHistoricalRows
		exactInfo.pagePlan = pagePlan
		exactInfo.pagePlanKnown = true
		addDataAssistCandidate(assistByID, assistID, exactInfo)
	}
	info.pagePlan = nil
	info.pagePlanKnown = false
	info.historicalRows = allowHistoricalRows
	addDataAssistCandidate(assistByID, assistID, info)
}

func addUnknownDataAssistID(assistByID map[uint32][]dataTableInfo, info dataTableInfo, assistID uint32) bool {
	info.storageKnown = false
	before := len(assistByID[assistID])
	addDataAssistCandidate(assistByID, assistID, info)
	return len(assistByID[assistID]) > before
}

func addRecoveryDataAssistID(assistByID map[uint32][]dataTableInfo, info dataTableInfo, assistID uint32) bool {
	info.recoveryMode = true
	info.historicalRows = shouldAllowHistoricalRows(info, assistID)
	info.pagePlan = nil
	info.pagePlanKnown = false
	before := len(assistByID[assistID])
	addDataAssistCandidate(assistByID, assistID, info)
	return len(assistByID[assistID]) > before
}

func addHistoricalDataAssistID(assistByID map[uint32][]dataTableInfo, info dataTableInfo, assistID uint32) bool {
	if info.dataStorageID == 0 {
		return false
	}
	info.historicalRows = shouldAllowHistoricalRows(info, assistID)
	info.pagePlan = nil
	info.pagePlanKnown = false
	info.storageKnown = false
	before := len(assistByID[assistID])
	addDataAssistCandidate(assistByID, assistID, info)
	return len(assistByID[assistID]) > before
}

func addDataAssistCandidate(assistByID map[uint32][]dataTableInfo, assistID uint32, info dataTableInfo) {
	if assistID == 0 || info.table.ID == 0 {
		return
	}
	for _, existing := range assistByID[assistID] {
		if existing.table.ID == info.table.ID && existing.storageKnown == info.storageKnown && existing.storage.ID == info.storage.ID && existing.pagePlanKnown == info.pagePlanKnown && existing.recoveryMode == info.recoveryMode && existing.historicalRows == info.historicalRows && existing.orphanRecovery == info.orphanRecovery {
			return
		}
	}
	assistByID[assistID] = append(assistByID[assistID], info)
}

func addHiddenIndexObjectAssistIDs(assistByID map[uint32][]dataTableInfo, info dataTableInfo, tableID uint32, indexObjects map[uint32]dictionaryObject, indexes map[uint32]indexDef) bool {
	added := false
	for indexID, obj := range indexObjects {
		if uint32(obj.ParentID) != tableID || !isAutoHiddenIndexObject(obj) {
			continue
		}
		if _, ok := indexes[indexID]; ok {
			continue
		}
		if addUnknownDataAssistID(assistByID, info, indexID) {
			added = true
		}
	}
	return added
}

func isAutoHiddenIndexObject(obj dictionaryObject) bool {
	if obj.Type != "TABOBJ" || obj.Subtype != "INDEX" {
		return false
	}
	return strings.EqualFold(obj.Name, fmt.Sprintf("INDEX%d", obj.ID))
}

func tableDataAssistID(tableID uint32) uint32 {
	if tableID == 0 {
		return 0
	}
	return 0x02000000 | (tableID & 0x00FFFFFF)
}

// buildOrphanRecoveryCandidates creates one heuristic candidate for pages
// whose storage id no live object owns. Orphan pages cannot prove ownership,
// so broad recovery is disabled when more than one target table is selected.
func buildOrphanRecoveryCandidates(storageUnits map[uint32]dataTableInfo) ([]dataTableInfo, string) {
	byTable := make(map[uint32]dataTableInfo)
	for _, info := range storageUnits {
		if info.table.ID == 0 {
			continue
		}
		existing, ok := byTable[info.table.ID]
		if !ok || info.storageUnitID == info.table.ID || (existing.recoveryGroupID == 0 && info.recoveryGroupID != 0) {
			byTable[info.table.ID] = info
		}
	}
	if len(byTable) == 0 {
		return nil, ""
	}
	if len(byTable) != 1 {
		return nil, fmt.Sprintf("orphan storage recovery disabled for %d target tables; use recover table owner.table to avoid ambiguous attribution", len(byTable))
	}
	for _, info := range byTable {
		info.recoveryMode = true
		info.orphanRecovery = true
		info.historicalRows = info.dataStorageID != 0
		info.pagePlan = nil
		info.pagePlanKnown = false
		info.storage = indexDef{}
		info.storageKnown = false
		info.storageUnitID = info.table.ID
		return []dataTableInfo{info}, ""
	}
	return nil, ""
}

// secondaryIndexStorageIDSet collects storage ids that belong to secondary
// indexes. Table data storages carry Flag&1 == 1 with no key columns (see
// tableStorageByID); everything else with key columns stores index entries
// whose layout does not match the owning table's rows.
func secondaryIndexStorageIDSet(indexObjects map[uint32]dictionaryObject, indexes map[uint32]indexDef) map[uint32]bool {
	result := make(map[uint32]bool)
	for indexID, idx := range indexes {
		if _, ok := indexObjects[indexID]; !ok {
			continue
		}
		if idx.Flag&1 == 1 && idx.KeyNum == 0 {
			continue
		}
		result[indexID] = true
	}
	return result
}

func assistIndexesByParentID(tables map[uint32]dictionaryObject, indexObjects map[uint32]dictionaryObject, indexes map[uint32]indexDef) map[uint32][]indexDef {
	result := make(map[uint32][]indexDef)
	for indexID, obj := range indexObjects {
		idx, ok := indexes[indexID]
		if !ok {
			continue
		}
		parentID := uint32(obj.ParentID)
		table, ok := tables[parentID]
		if !ok {
			continue
		}
		if !isCandidateDataIndex(table, idx) || idx.RootFile < 0 {
			continue
		}
		result[parentID] = append(result[parentID], idx)
	}
	for parentID := range result {
		sort.Slice(result[parentID], func(i, j int) bool {
			return result[parentID][i].ID < result[parentID][j].ID
		})
	}
	return result
}

func mergeDictionaryStorageRoots(result map[uint32][]indexDef, tables map[uint32]DictionaryTable) {
	for tableID, table := range tables {
		if table.StorageID == 0 || table.RootFile < 0 || table.RootPage == 0 {
			continue
		}
		duplicate := false
		for _, storage := range result[tableID] {
			if storage.ID == table.StorageID {
				duplicate = true
				break
			}
		}
		if duplicate {
			continue
		}
		result[tableID] = append(result[tableID], indexDef{
			ID:       table.StorageID,
			GroupID:  uint16(table.GroupID),
			RootFile: table.RootFile,
			RootPage: int32(table.RootPage),
			Flag:     1,
		})
		sort.Slice(result[tableID], func(i, j int) bool {
			return result[tableID][i].ID < result[tableID][j].ID
		})
	}
}

func isCandidateDataIndex(table dictionaryObject, idx indexDef) bool {
	if idx.Flag&1 != 0 && idx.KeyNum == 0 {
		return true
	}
	return table.isIOTTable() && idx.Flag&0x4 != 0
}

func selectDataPageCandidate(candidates []dataTableInfo, file dataFileRef, pageNo uint32, page []byte, pageSize uint32, rows []locatedDataRow, decoder textDecoder) (dataTableInfo, bool) {
	if len(rows) == 0 {
		return dataTableInfo{}, false
	}
	pageKind := dataPageKind(page)
	pageStorageID := dataPageStorageID(page)
	ordered := append([]dataTableInfo(nil), candidates...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return dataCandidateRank(ordered[i]) < dataCandidateRank(ordered[j])
	})
	for _, candidate := range ordered {
		if isTableDataAssistHeaderCandidate(candidate, pageStorageID, pageKind) {
			continue
		}
		if isLooseHistoricalCandidate(candidate) {
			continue
		}
		if !candidateMatchesFile(candidate, file, pageNo) {
			continue
		}
		if candidate.orphanRecovery {
			if orphanRecoveryCandidateMatchesRows(candidate, page, pageSize, rows, decoder) {
				return candidate, true
			}
			continue
		}
		limit := len(rows)
		if limit > 3 {
			limit = 3
		}
		for i := 0; i < limit; i++ {
			row := rows[i]
			rowStart := int(row.offset)
			rowEnd := rowStart + int(row.length)
			if rowStart < 0 || rowEnd > int(pageSize) || rowEnd > len(page) {
				continue
			}
			if _, _, _, err := parseDataRowValues(page[rowStart:rowEnd], candidate.columns, decoder, candidate.historicalRows, candidate.lobReader); err == nil {
				return candidate, true
			}
		}
	}
	return dataTableInfo{}, false
}

// orphanRecoveryCandidateMatchesRows raises the confidence threshold for an
// unowned storage page. Ownership is still heuristic, but one coincidentally
// parseable row is no longer enough to attribute a whole page to the target.
func orphanRecoveryCandidateMatchesRows(candidate dataTableInfo, page []byte, pageSize uint32, rows []locatedDataRow, decoder textDecoder) bool {
	limit := len(rows)
	if limit > 16 {
		limit = 16
	}
	matched := 0
	for i := 0; i < limit; i++ {
		row := rows[i]
		rowStart := int(row.offset)
		rowEnd := rowStart + int(row.length)
		if rowStart < 0 || rowEnd > int(pageSize) || rowEnd > len(page) {
			continue
		}
		if _, _, _, err := parseDataRowValues(page[rowStart:rowEnd], candidate.columns, decoder, candidate.historicalRows, candidate.lobReader); err == nil {
			matched++
		}
	}
	if limit == 0 {
		return false
	}
	required := limit
	if limit >= 4 {
		required = (limit*3 + 3) / 4
	}
	return matched >= required
}

func isTableDataAssistHeaderCandidate(info dataTableInfo, pageStorageID uint32, pageKind uint32) bool {
	if info.recoveryMode || info.dataStorageID == 0 || pageKind != dmPageKindRowData {
		return false
	}
	tableAssistID := tableDataAssistID(info.table.ID)
	return pageStorageID == tableAssistID && pageStorageID != info.dataStorageID
}

func isLooseHistoricalCandidate(info dataTableInfo) bool {
	return info.historicalRows && !info.recoveryMode && !info.pagePlanKnown && !info.storageKnown
}

func dataCandidateRank(info dataTableInfo) int {
	switch {
	case info.pagePlanKnown:
		return 0
	case info.recoveryMode:
		return 1
	case info.segmentKnown:
		return 2
	case info.storageKnown:
		return 3
	default:
		return 4
	}
}

func candidateMatchesFile(info dataTableInfo, file dataFileRef, pageNo uint32) bool {
	if info.pagePlanKnown {
		if len(info.pagePlan) == 0 || !info.pagePlan[dataPageRef{key: file.key, pageNo: pageNo}] {
			return false
		}
		// The exact physical reference is authoritative. Segment metadata may be
		// stale after extent movement and remains an auxiliary fallback only.
		return true
	}
	if info.recoveryMode {
		return candidateMatchesRecoveryFile(info, file)
	}
	if info.scanGroupOnly {
		return file.key.groupID == info.scanGroupID
	}
	if info.segmentKnown {
		if !candidateMatchesSegmentIdentity(info, file) {
			return false
		}
		if info.segment.blocks > 0 && info.segment.extents <= 1 {
			return pageNo >= info.segment.headerPage && pageNo < info.segment.headerPage+info.segment.blocks
		}
		if info.segment.headerPage > 0 && info.segment.extents <= 1 {
			return pageNo >= info.segment.headerPage
		}
		return true
	}
	if !info.storageKnown {
		return true
	}
	return uint32(info.storage.GroupID) == file.key.groupID && info.storage.RootFile == file.key.fileID
}

func candidateMatchesRecoveryFile(info dataTableInfo, file dataFileRef) bool {
	groupID := info.recoveryGroupID
	if groupID == 0 && info.segmentKnown {
		groupID = info.segment.tablespaceID
	}
	if groupID == 0 && info.storageKnown {
		groupID = uint32(info.storage.GroupID)
	}
	if groupID != 0 && file.key.groupID != groupID {
		return false
	}
	return true
}

func candidateMatchesSegmentIdentity(info dataTableInfo, file dataFileRef) bool {
	if !info.segmentKnown {
		return true
	}
	if uint32(info.segment.fileID) != uint32(file.key.fileID) {
		return false
	}
	if info.segment.tablespaceID != 0 && info.segment.tablespaceID != file.key.groupID {
		return false
	}
	return true
}

func segmentByTableID(dict *DictionaryInfo, tableID uint32) tableSegment {
	if dict == nil {
		return tableSegment{}
	}
	for _, table := range dict.Tables {
		if table.ID != tableID || !dictionaryTableHasSegment(table) {
			continue
		}
		return tableSegment{
			fileID:       table.HeaderFile,
			headerPage:   table.HeaderBlock,
			blocks:       table.Blocks,
			extents:      table.Extents,
			bytes:        table.Bytes,
			tablespace:   table.Tablespace,
			tablespaceID: table.GroupID,
		}
	}
	return tableSegment{}
}

func hasSegmentRange(dict *DictionaryInfo, tableID uint32) bool {
	if dict == nil {
		return false
	}
	for _, table := range dict.Tables {
		if table.ID == tableID {
			return dictionaryTableHasSegment(table)
		}
	}
	return false
}

func dictionaryTableHasSegment(table DictionaryTable) bool {
	return table.HeaderBlock > 0 && table.Blocks > 0
}

func dictionaryTableGroupID(tables map[uint32]DictionaryTable, tableID uint32) uint32 {
	table, ok := tables[tableID]
	if !ok {
		return 0
	}
	return table.GroupID
}

func dataStorageIDForTable(dictionaryTables map[uint32]DictionaryTable, dataStorageByTable map[uint32]indexDef, tableID uint32) uint32 {
	if table, ok := dictionaryTables[tableID]; ok && table.StorageID != 0 {
		return table.StorageID
	}
	if storage, ok := dataStorageByTable[tableID]; ok {
		return storage.ID
	}
	return 0
}

// shouldAllowHistoricalRows reports whether an assist storage may carry
// historical (pre-ALTER) row versions of the table. The table's own primary
// storage never counts: its pages are deduplicated page-wise across the
// direct, storage-scan and segment phases, so rows from it cannot be
// revisited, and flagging it forces row-coverage tracking (O(rows) strings)
// on every table.
func shouldAllowHistoricalRows(info dataTableInfo, assistID uint32) bool {
	return info.dataStorageID != 0 && assistID != 0 && assistID != info.dataStorageID
}

func dictionaryDataAssistIDs(tables map[uint32]DictionaryTable, tableID uint32) []uint32 {
	table, ok := tables[tableID]
	if !ok {
		return nil
	}
	seen := make(map[uint32]bool)
	var result []uint32
	add := func(id uint32) {
		if id == 0 || seen[id] {
			return
		}
		seen[id] = true
		result = append(result, id)
	}
	add(table.StorageID)
	for _, id := range table.AssistIDs {
		add(id)
	}
	// The 0x02000000|table_id guess can collide with unrelated live storages
	// and forces coverage tracking on every table, so it only participates
	// when the dictionary carries no real storage id at all.
	if table.StorageID == 0 && len(table.AssistIDs) == 0 {
		add(tableDataAssistID(tableID))
	}
	return result
}
