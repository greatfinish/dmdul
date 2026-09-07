package dm

import (
	"encoding/binary"
	"fmt"
	"sort"
	"strings"
)

const (
	bootstrapSYSIndexesRootOffset = 0x7C
	bootstrapSYSObjectsRootOffset = 0x80
)

var bootstrapStage2Tables = []string{
	"SYSCOLUMNS",
	"SYSTEXTS",
	"SYSGRANTS",
	"SYSOBJINFOS",
	"SYSHPARTTABLEINFO",
}

type standardBootstrapCatalog struct {
	stream      *systemPageStream
	decoder     textDecoder
	cache       *dataFilePageCache
	objects     map[uint32]dictionaryObject
	indexes     map[uint32]indexDef
	plans       map[string]map[dataPageRef]bool
	diagnostics []BootstrapDiagnostic
	emit        func(BootstrapDiagnostic)
	fallback    bool
}

func loadStandardBootstrapCatalog(stream *systemPageStream, decoder textDecoder, emit func(BootstrapDiagnostic)) (*standardBootstrapCatalog, string) {
	catalog := &standardBootstrapCatalog{
		stream:  stream,
		decoder: decoder,
		cache: newSystemDictionaryPageCache([]dataFileRef{{
			key:    dataFileKey{groupID: 0, fileID: 0},
			path:   stream.path,
			reader: sizedReaderAt(stream.file, stream.fileSize),
		}}, stream.pageSize),
		plans: make(map[string]map[dataPageRef]bool),
		emit:  emit,
	}

	objects, objectPlan, objectDiag, ok := catalog.scanAnchorObjects()
	catalog.addDiagnostic(objectDiag)
	if !ok {
		return catalog, defaultIfEmpty(objectDiag.Reason, "SYSOBJECTS anchor is unavailable")
	}
	catalog.objects = objects
	catalog.plans["SYSOBJECTS"] = objectPlan

	indexes, indexPlan, indexDiag, ok := catalog.scanAnchorIndexes()
	catalog.addDiagnostic(indexDiag)
	if !ok {
		return catalog, defaultIfEmpty(indexDiag.Reason, "SYSINDEXES anchor is unavailable")
	}
	catalog.indexes = indexes
	catalog.plans["SYSINDEXES"] = indexPlan

	if reason := validateStandardBootstrapStage1(objects, indexes); reason != "" {
		catalog.addDiagnostic(BootstrapDiagnostic{Stage: 1, Phase: "validate", Name: "core-catalog", Mode: "root-chain", Status: "ERROR", RootFile: -1, Reason: reason})
		return catalog, reason
	}
	catalog.addDiagnostic(BootstrapDiagnostic{
		Stage: 1, Phase: "validate", Name: "core-catalog", Mode: "root-chain", Status: "OK", RootFile: -1,
		Rows: len(objects) + len(indexes),
	})

	catalog.buildStage2Plans()
	return catalog, ""
}

func (c *standardBootstrapCatalog) addDiagnostic(diag BootstrapDiagnostic) {
	if diag.RootFile == 0 && diag.RootPage == 0 && diag.StorageID == 0 {
		diag.RootFile = -1
	}
	c.diagnostics = append(c.diagnostics, diag)
	if diag.Status == "FALLBACK" || diag.Status == "ERROR" || diag.Status == "WARNING" {
		c.fallback = true
	}
	if c.emit != nil {
		c.emit(diag)
	}
}

func (c *standardBootstrapCatalog) scanAnchorObjects() (map[uint32]dictionaryObject, map[dataPageRef]bool, BootstrapDiagnostic, bool) {
	plan, diag, ok := c.anchorPlan("SYSOBJECTS", bootstrapSYSObjectsRootOffset)
	if !ok {
		return nil, nil, diag, false
	}
	objects := make(map[uint32]dictionaryObject)
	err := c.forEachPlanRow(plan, func(page []byte, pageNo uint32, slotNo uint16, slotOff uint16) {
		obj, parsed := parseDDLObjectRow(page, int(slotOff), pageNo, slotNo, slotOff, c.stream.pageSize, c.decoder)
		if !parsed {
			return
		}
		if existing, exists := objects[obj.ID]; !exists || obj.Location.RowOffset < existing.Location.RowOffset {
			objects[obj.ID] = obj
		}
	})
	if err != nil {
		diag.Status, diag.Reason = "ERROR", err.Error()
		return nil, nil, diag, false
	}
	diag.Rows = len(objects)
	if len(objects) == 0 {
		diag.Status, diag.Reason = "ERROR", "no SYSOBJECTS rows parsed from leaf chain"
		return nil, nil, diag, false
	}
	return objects, plan, diag, true
}

func (c *standardBootstrapCatalog) scanAnchorIndexes() (map[uint32]indexDef, map[dataPageRef]bool, BootstrapDiagnostic, bool) {
	plan, diag, ok := c.anchorPlan("SYSINDEXES", bootstrapSYSIndexesRootOffset)
	if !ok {
		return nil, nil, diag, false
	}
	indexes := make(map[uint32]indexDef)
	err := c.forEachPlanRow(plan, func(page []byte, _ uint32, _ uint16, slotOff uint16) {
		if idx, parsed := parseDDLIndexRow(page, int(slotOff), c.stream.pageSize); parsed {
			indexes[idx.ID] = idx
		}
	})
	if err != nil {
		diag.Status, diag.Reason = "ERROR", err.Error()
		return nil, nil, diag, false
	}
	diag.Rows = len(indexes)
	if len(indexes) == 0 {
		diag.Status, diag.Reason = "ERROR", "no SYSINDEXES rows parsed from leaf chain"
		return nil, nil, diag, false
	}
	return indexes, plan, diag, true
}

func (c *standardBootstrapCatalog) anchorPlan(name string, offset int) (map[dataPageRef]bool, BootstrapDiagnostic, bool) {
	diag := BootstrapDiagnostic{Stage: 1, Phase: "anchor", Name: name, Mode: "root-chain", Status: "OK", RootFile: 0}
	if len(c.stream.header) < offset+4 {
		diag.Status, diag.Reason = "ERROR", fmt.Sprintf("SYSTEM.DBF header does not contain offset 0x%X", offset)
		return nil, diag, false
	}
	rootPage := binary.LittleEndian.Uint32(c.stream.header[offset:])
	diag.RootPage = rootPage
	rootRef := dataPageRef{key: dataFileKey{groupID: 0, fileID: 0}, pageNo: rootPage}
	root, ok := c.cache.readPage(rootRef)
	if !ok {
		diag.Status, diag.Reason = "ERROR", "root page is unreadable"
		return nil, diag, false
	}
	if !pageHeaderMatchesRef(root, rootRef) {
		diag.Status, diag.Reason = "ERROR", "root page header does not match group/file/page"
		return nil, diag, false
	}
	diag.StorageID = dataPageStorageID(root)
	storage := indexDef{ID: diag.StorageID, GroupID: 0, RootFile: 0, RootPage: int32(rootPage)}
	plan := buildStoragePagePlan(storage, c.cache)
	if len(plan) == 0 {
		diag.Status, diag.Reason = "ERROR", "root/internal/leaf traversal returned no leaf pages"
		return nil, diag, false
	}
	diag.Pages = len(plan)
	return plan, diag, true
}

func validateStandardBootstrapStage1(objects map[uint32]dictionaryObject, indexes map[uint32]indexDef) string {
	for id, name := range map[uint32]string{0: "SYSOBJECTS", 2: "SYSCOLUMNS", 5: "SYSTEXTS", 6: "SYSGRANTS", 19: "SYSHPARTTABLEINFO"} {
		obj, ok := objects[id]
		if !ok || !strings.EqualFold(obj.Name, name) {
			return fmt.Sprintf("required object %s(id=%d) is missing", name, id)
		}
	}
	if len(indexes) < 2 {
		return "SYSINDEXES contains too few parsed rows"
	}
	return ""
}

func (c *standardBootstrapCatalog) buildStage2Plans() {
	systemTables := make(map[uint32]dictionaryObject)
	indexObjects := make(map[uint32]dictionaryObject)
	for id, obj := range c.objects {
		switch {
		case obj.Type == "SCHOBJ" && obj.Subtype == "STAB":
			systemTables[id] = obj
		case obj.Type == "TABOBJ" && obj.Subtype == "INDEX":
			indexObjects[id] = obj
		}
	}
	for _, name := range bootstrapStage2Tables {
		var table dictionaryObject
		for _, obj := range systemTables {
			if strings.EqualFold(obj.Name, name) {
				table = obj
				break
			}
		}
		diag := BootstrapDiagnostic{Stage: 2, Phase: "dictionary", Name: name, Mode: "root-chain", Status: "OK", RootFile: -1}
		if table.ID == 0 && !strings.EqualFold(name, "SYSOBJECTS") {
			diag.Status, diag.Reason = "FALLBACK", "table object is missing from SYSOBJECTS"
			c.addDiagnostic(diag)
			continue
		}
		storage, ok := chooseSystemTableStorage(table.ID, indexObjects, c.indexes)
		if !ok {
			diag.Status, diag.Reason = "FALLBACK", "storage root is missing from SYSINDEXES"
			c.addDiagnostic(diag)
			continue
		}
		diag.RootFile, diag.RootPage, diag.StorageID = storage.RootFile, uint32(storage.RootPage), storage.ID
		plan := buildStoragePagePlan(storage, c.cache)
		if len(plan) == 0 {
			diag.Status, diag.Reason = "FALLBACK", "root/internal/leaf traversal returned no leaf pages"
			c.addDiagnostic(diag)
			continue
		}
		diag.Pages = len(plan)
		c.plans[strings.ToUpper(name)] = plan
		c.addDiagnostic(diag)
	}
}

func chooseSystemTableStorage(tableID uint32, indexObjects map[uint32]dictionaryObject, indexes map[uint32]indexDef) (indexDef, bool) {
	var best indexDef
	bestScore := -1
	for indexID, obj := range indexObjects {
		if uint32(obj.ParentID) != tableID {
			continue
		}
		idx, ok := indexes[indexID]
		if !ok || idx.GroupID != 0 || idx.RootFile != 0 || idx.RootPage < 0 || idx.Type != "BT" {
			continue
		}
		score := 0
		if idx.Flag&4 != 0 {
			score += 100
		}
		if idx.KeyNum == 0 {
			score += 20
		}
		if idx.Flag&1 != 0 {
			score += 10
		}
		if score > bestScore || (score == bestScore && idx.ID < best.ID) {
			best, bestScore = idx, score
		}
	}
	return best, bestScore >= 0
}

func (c *standardBootstrapCatalog) forEachPlanRow(plan map[dataPageRef]bool, visit func(page []byte, pageNo uint32, slotNo uint16, slotOff uint16)) error {
	refs := sortedBootstrapPageRefs(plan)
	for _, ref := range refs {
		if ref.key.groupID != 0 || ref.key.fileID != 0 {
			return fmt.Errorf("unexpected SYSTEM dictionary page reference group=%d file=%d page=%d", ref.key.groupID, ref.key.fileID, ref.pageNo)
		}
		page, err := c.stream.readPage(ref.pageNo)
		if err != nil {
			return err
		}
		if !pageHeaderMatchesRef(page, ref) {
			return fmt.Errorf("SYSTEM dictionary page header mismatch at page %d", ref.pageNo)
		}
		iterDictionaryRowsInPage(page, c.stream.pageSize, ref.pageNo, visit)
	}
	return nil
}

func (c *standardBootstrapCatalog) forEachTableRow(name string, visit func(page []byte, pageNo uint32, slotNo uint16, slotOff uint16)) (bool, error) {
	plan := c.plans[strings.ToUpper(name)]
	if len(plan) == 0 {
		return false, nil
	}
	return true, c.forEachPlanRow(plan, visit)
}

func (c *standardBootstrapCatalog) forEachTableSlotRange(name string, visit func(page []byte, pageNo uint32, slotNo uint16, slotOff uint16, nextOff uint16)) (bool, error) {
	plan := c.plans[strings.ToUpper(name)]
	if len(plan) == 0 {
		return false, nil
	}
	for _, ref := range sortedBootstrapPageRefs(plan) {
		page, err := c.stream.readPage(ref.pageNo)
		if err != nil {
			return true, err
		}
		if !pageHeaderMatchesRef(page, ref) {
			return true, fmt.Errorf("SYSTEM dictionary page header mismatch at page %d", ref.pageNo)
		}
		iterDictionarySlotRangesInPage(page, c.stream.pageSize, ref.pageNo, visit)
	}
	return true, nil
}

func (c *standardBootstrapCatalog) recordTableRows(name string, rows int, used bool, reason string) {
	mode, status := "root-chain", "OK"
	if !used {
		mode, status = "stream-scan-fallback", "FALLBACK"
	}
	diag := BootstrapDiagnostic{Stage: 2, Phase: "extract", Name: name, Mode: mode, Status: status, RootFile: -1, Rows: rows, Reason: reason}
	if plan := c.plans[strings.ToUpper(name)]; len(plan) > 0 {
		diag.Pages = len(plan)
	}
	c.addDiagnostic(diag)
}

func (c *standardBootstrapCatalog) dictionaryTexts() (map[uint32]map[uint32]string, bool, error) {
	result := make(map[uint32]map[uint32]string)
	parsed := 0
	used, err := c.forEachTableRow("SYSTEXTS", func(page []byte, pageNo uint32, slotNo uint16, slotOff uint16) {
		row, ok := parseDDLTextRow(page, int(slotOff), pageNo, slotNo, slotOff, c.stream.pageSize, c.decoder)
		if !ok {
			return
		}
		parsed++
		seqs := result[row.ID]
		if seqs == nil {
			seqs = make(map[uint32]string)
			result[row.ID] = seqs
		}
		if existing := seqs[row.SeqNo]; len(row.Text) > len(existing) {
			seqs[row.SeqNo] = row.Text
		}
	})
	if used {
		c.recordTableRows("SYSTEXTS", parsed, true, "")
	}
	return result, used, err
}

func (c *standardBootstrapCatalog) tabPrivileges(objects map[uint32]dictionaryObject, users map[uint32]dictionaryObject, roles map[uint32]dictionaryObject, columns map[uint32][]columnDef, matcher ownerMatcher, tableMatcher tableNameMatcher) ([]DictionaryTabPrivilege, bool, error) {
	granteeNames := make(map[uint32]string)
	for id, user := range users {
		granteeNames[id] = user.Name
	}
	for id, role := range roles {
		granteeNames[id] = role.Name
	}
	seen := make(map[string]bool)
	var privileges []DictionaryTabPrivilege
	parsed := 0
	used, err := c.forEachTableRow("SYSGRANTS", func(page []byte, pageNo uint32, slotNo uint16, slotOff uint16) {
		grant, ok := parseDDLObjectPrivilegeRow(page, int(slotOff), pageNo, slotNo, slotOff, c.stream.pageSize)
		if !ok {
			return
		}
		parsed++
		target, ok := objects[grant.ObjectID]
		if !ok || !isTabPrivilegeTarget(target) || isSystemCatalogOwner(target.Owner) {
			return
		}
		grantee := granteeNames[grant.GranteeID]
		if grantee == "" || (!matcher.allowed(target.Owner) && !matcher.allowed(grantee)) {
			return
		}
		if !tableMatcher.allowed(target.Owner, target.Name) && !matcher.allowed(grantee) {
			return
		}
		item := DictionaryTabPrivilege{
			Grantee: grantee, Owner: target.Owner, ObjectName: target.Name,
			ObjectType: dictionaryPrivilegeObjectType(target), Privilege: grant.Privilege, Grantable: grant.Grantable,
		}
		enrichColumnPrivilege(&item, grant, columns, granteeNames)
		key := dictionaryPrivilegeKey(item)
		if !seen[key] {
			seen[key] = true
			privileges = append(privileges, item)
		}
	})
	if used {
		c.recordTableRows("SYSGRANTS", parsed, true, "")
	}
	sortDictionaryTabPrivileges(privileges)
	return privileges, used, err
}

func (c *standardBootstrapCatalog) partitionsByTable(tables map[uint32]dictionaryObject, matcher ownerMatcher) (map[uint32][]PartitionInfo, bool, error) {
	result := make(map[uint32][]PartitionInfo)
	parsed := 0
	used, err := c.forEachTableSlotRange("SYSHPARTTABLEINFO", func(page []byte, pageNo uint32, slotNo uint16, slotOff uint16, nextOff uint16) {
		for rowOff := int(slotOff); rowOff+sysHPartTypeOffset+2 < int(nextOff); rowOff++ {
			part, ok := parseDDLPartitionRowAt(page, rowOff, pageNo, slotNo, slotOff, c.stream.pageSize, c.decoder)
			if !ok {
				continue
			}
			parsed++
			table, ok := tables[part.BaseTableID]
			if !ok || !matcher.allowed(table.Owner) {
				continue
			}
			result[part.BaseTableID] = append(result[part.BaseTableID], PartitionInfo{
				BaseTableID: part.BaseTableID, PartTableID: part.PartTableID, Type: part.Type, Name: part.Name,
				HighValue: append([]byte(nil), part.HighValue...), HighValuePreview: partitionHighValuePreview(part.HighValue),
				HighValueHex: partitionHighValueHex(part.HighValue, 64), PageNo: part.Location.PageNo,
				SlotNo: part.Location.SlotNo, SlotOffset: part.Location.SlotOffset, RowOffset: part.Location.RowOffset,
			})
			rowLen := int(binary.LittleEndian.Uint16(page[rowOff+0x01:]))
			if rowLen > 0 {
				rowOff += rowLen - 1
			}
		}
	})
	for tableID, parts := range result {
		sort.Slice(parts, func(i, j int) bool {
			if parts[i].PartTableID == parts[j].PartTableID {
				return parts[i].Name < parts[j].Name
			}
			return parts[i].PartTableID < parts[j].PartTableID
		})
		result[tableID] = parts
	}
	if used {
		c.recordTableRows("SYSHPARTTABLEINFO", parsed, true, "")
	}
	return result, used, err
}

func (c *standardBootstrapCatalog) partitionKeysByTable(tables map[uint32]dictionaryObject, matcher ownerMatcher) (map[uint32][]uint16, bool, error) {
	result := make(map[uint32][]uint16)
	parsed := 0
	used, err := c.forEachTableRow("SYSOBJINFOS", func(page []byte, _ uint32, _ uint16, slotOff uint16) {
		tableID, colIDs, ok := parseTabPartInfoRow(page, int(slotOff), c.stream.pageSize, c.decoder)
		if !ok {
			return
		}
		parsed++
		table, ok := tables[tableID]
		if !ok || !matcher.allowed(table.Owner) || len(colIDs) == 0 {
			return
		}
		result[tableID] = append([]uint16(nil), colIDs...)
	})
	if used {
		c.recordTableRows("SYSOBJINFOS", parsed, true, "TYPE$='TABPART'")
	}
	return result, used, err
}

func sortedBootstrapPageRefs(plan map[dataPageRef]bool) []dataPageRef {
	refs := make([]dataPageRef, 0, len(plan))
	for ref := range plan {
		refs = append(refs, ref)
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].key.groupID != refs[j].key.groupID {
			return refs[i].key.groupID < refs[j].key.groupID
		}
		if refs[i].key.fileID != refs[j].key.fileID {
			return refs[i].key.fileID < refs[j].key.fileID
		}
		return refs[i].pageNo < refs[j].pageNo
	})
	return refs
}

func countDictionaryTextRows(texts map[uint32]map[uint32]string) int {
	count := 0
	for _, seqs := range texts {
		count += len(seqs)
	}
	return count
}
