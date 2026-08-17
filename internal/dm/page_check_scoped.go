package dm

// Storage-scoped page diagnosis: uses a recovered dictionary to attribute bad
// pages to owner.table, walk B-tree leaf chains for break/cycle detection, and
// check dictionary self-consistency. Requires a loaded dictionary; the plain
// whole-file page scan runs without one.

import (
	"fmt"
	"sort"
	"strings"
)

// pageAttribution maps a page to the table that owns it, first by the page's
// own storage id, then by falling back to the table segment page range for
// pages whose storage id was wiped (e.g. zeroed pages).
type pageAttribution struct {
	byStorage map[uint32][]pageObjectIdentity
	segments  []attributionSegment
}

type pageObjectIdentity struct {
	owner           string
	table           string
	tableID         uint32
	objectType      PageObjectType
	objectStorageID uint32
	groupID         uint32
	method          PageAttributionMethod
	confidence      PageAttributionConfidence
	tablespace      string
	headerFile      int16
	headerBlock     uint32
	segmentBytes    uint64
}

type attributionSegment struct {
	identity  pageObjectIdentity
	groupID   uint32
	fileID    int16
	startPage uint64
	endPage   uint64
}

func newPageAttribution(dict *DictionaryInfo) *pageAttribution {
	a := &pageAttribution{byStorage: make(map[uint32][]pageObjectIdentity)}
	for _, table := range dict.Tables {
		primary := pageObjectIdentity{
			owner:           table.Owner,
			table:           table.Name,
			tableID:         table.ID,
			objectType:      PageObjectTable,
			objectStorageID: table.StorageID,
			groupID:         table.GroupID,
			method:          PageAttributionStorageID,
			confidence:      PageAttributionHigh,
			tablespace:      table.Tablespace,
			headerFile:      table.HeaderFile,
			headerBlock:     table.HeaderBlock,
			segmentBytes:    table.Bytes,
		}
		if table.StorageID != 0 {
			a.addStorageIdentity(table.StorageID, primary)
		}
		for _, assist := range table.AssistIDs {
			if assist == 0 || assist == table.StorageID {
				continue
			}
			a.addStorageIdentity(assist, pageObjectIdentity{
				owner:           table.Owner,
				table:           table.Name,
				tableID:         table.ID,
				objectType:      PageObjectTableAssist,
				objectStorageID: assist,
				groupID:         table.GroupID,
				method:          PageAttributionAssistStorageID,
				confidence:      PageAttributionHigh,
				tablespace:      table.Tablespace,
				headerFile:      -1,
			})
		}
		if table.Blocks > 0 && table.HeaderFile >= 0 {
			segmentIdentity := primary
			segmentIdentity.method = PageAttributionSegmentRange
			segmentIdentity.confidence = PageAttributionMedium
			a.segments = append(a.segments, attributionSegment{
				identity:  segmentIdentity,
				groupID:   table.GroupID,
				fileID:    table.HeaderFile,
				startPage: uint64(table.HeaderBlock),
				endPage:   uint64(table.HeaderBlock) + uint64(table.Blocks),
			})
		}
	}
	return a
}

func (a *pageAttribution) addStorageIdentity(storageID uint32, identity pageObjectIdentity) {
	for _, existing := range a.byStorage[storageID] {
		if existing.tableID == identity.tableID && existing.objectType == identity.objectType &&
			existing.objectStorageID == identity.objectStorageID {
			return
		}
	}
	a.byStorage[storageID] = append(a.byStorage[storageID], identity)
}

func (a *pageAttribution) attribute(bad *BadPage) (owner string, table string) {
	if a == nil {
		return "", ""
	}
	copy := *bad
	a.apply(&copy)
	return copy.Owner, copy.Table
}

func (a *pageAttribution) apply(bad *BadPage) {
	if a == nil || bad == nil {
		return
	}
	bad.Owner = ""
	bad.Table = ""
	bad.TableID = 0
	bad.ObjectType = PageObjectUnattributed
	bad.ObjectStorageID = 0
	bad.ObjectGroupID = 0
	bad.ObjectHeaderFile = -1
	bad.ObjectHeaderBlock = 0
	bad.Attribution = PageAttributionNone
	bad.AttributionConfidence = PageAttributionNo
	bad.UnattributedReason = ""
	bad.SegmentBytes = 0
	if bad.StorageID != 0 {
		identities := a.byStorage[bad.StorageID]
		if len(identities) == 1 {
			applyPageObjectIdentity(bad, identities[0])
			return
		}
		if len(identities) > 1 {
			if identity, ok := a.uniqueSegmentIdentity(bad); ok {
				applyPageObjectIdentity(bad, identity)
				return
			}
			bad.UnattributedReason = "ambiguous_storage_id"
			return
		}
	}
	if identity, ok := a.uniqueSegmentIdentity(bad); ok {
		applyPageObjectIdentity(bad, identity)
		return
	}
	if a.segmentMatchCount(bad) > 1 {
		bad.UnattributedReason = "ambiguous_segment_range"
	} else if bad.StorageID != 0 {
		bad.UnattributedReason = "unknown_storage_id"
	} else {
		bad.UnattributedReason = "outside_known_segments"
	}
}

func (a *pageAttribution) uniqueSegmentIdentity(bad *BadPage) (pageObjectIdentity, bool) {
	var matched pageObjectIdentity
	count := 0
	for i := range a.segments {
		seg := &a.segments[i]
		if seg.groupID == bad.GroupID && seg.fileID == bad.FileID &&
			uint64(bad.PageNo) >= seg.startPage && uint64(bad.PageNo) < seg.endPage {
			matched = seg.identity
			count++
		}
	}
	return matched, count == 1
}

func (a *pageAttribution) segmentMatchCount(bad *BadPage) int {
	count := 0
	for i := range a.segments {
		seg := &a.segments[i]
		if seg.groupID == bad.GroupID && seg.fileID == bad.FileID &&
			uint64(bad.PageNo) >= seg.startPage && uint64(bad.PageNo) < seg.endPage {
			count++
		}
	}
	return count
}

func applyPageObjectIdentity(bad *BadPage, identity pageObjectIdentity) {
	bad.Owner = identity.owner
	bad.Table = identity.table
	bad.TableID = identity.tableID
	bad.ObjectType = identity.objectType
	bad.ObjectStorageID = identity.objectStorageID
	bad.ObjectGroupID = identity.groupID
	bad.ObjectHeaderFile = identity.headerFile
	bad.ObjectHeaderBlock = identity.headerBlock
	bad.Attribution = identity.method
	bad.AttributionConfidence = identity.confidence
	bad.UnattributedReason = ""
	bad.SegmentBytes = identity.segmentBytes
}

// checkLeafChains walks each table's B-tree leaf chain and reports breaks or
// cycles. buildStoragePagePlanDetailed already validates root identity, page
// kind, storage id, chain links and cycles, returning a reason on failure.
func checkLeafChains(dict *DictionaryInfo, files []dataFileRef, pageSize uint32) []ChainIssue {
	if dict == nil || len(files) == 0 {
		return nil
	}
	// Only tables whose storage root actually lives in a present data file can
	// be walked. Tables in tablespaces that were not provided (a common case
	// when checking one file) would otherwise report a spurious "cannot read
	// root" — a missing file is not corruption.
	available := make(map[dataFileKey]bool, len(files))
	for _, file := range files {
		available[file.key] = true
	}
	cache := newDataFilePageCache(files, pageSize)
	var issues []ChainIssue
	for _, table := range dict.Tables {
		if table.Temporary || table.StorageID == 0 || table.RootFile < 0 {
			continue
		}
		rootKey := dataFileKey{groupID: table.GroupID, fileID: table.RootFile}
		if !available[rootKey] {
			continue
		}
		storage := indexDef{
			ID:       table.StorageID,
			GroupID:  uint16(table.GroupID),
			RootFile: table.RootFile,
			RootPage: int32(table.RootPage),
		}
		plan, reason := buildStoragePagePlanDetailed(storage, cache)
		// Only report a broken/cyclic chain whose ROOT is valid — the walk got
		// past the root and then hit a bad leaf/internal page. A root-level
		// failure (unreadable root, root identity or storage_id mismatch) is far
		// more likely a stale dictionary root pointer than page corruption, and
		// the unload path already recovers those via storage/segment fallback;
		// reporting them here floods the output with dictionary-vs-data drift.
		if reason != "" && len(plan) == 0 && isLeafChainBreakReason(reason) {
			issues = append(issues, ChainIssue{
				Owner:     table.Owner,
				Table:     table.Name,
				StorageID: table.StorageID,
				RootFile:  table.RootFile,
				RootPage:  table.RootPage,
				Reason:    reason,
			})
		}
	}
	sort.Slice(issues, func(i, j int) bool {
		if issues[i].Owner != issues[j].Owner {
			return issues[i].Owner < issues[j].Owner
		}
		return issues[i].Table < issues[j].Table
	})
	return issues
}

// isLeafChainBreakReason reports whether a page-plan failure reason describes a
// break in the leaf/internal chain (root was valid) rather than a root-level
// problem (stale pointer, unreadable/mismatched root). Only chain breaks are
// high-confidence page corruption.
func isLeafChainBreakReason(reason string) bool {
	if reason == "" {
		return false
	}
	if strings.Contains(reason, "storage root metadata") ||
		strings.Contains(reason, "cannot read root page") ||
		strings.Contains(reason, "root page identity") ||
		strings.Contains(reason, "root page storage_id") ||
		strings.Contains(reason, "unsupported root page kind") {
		return false
	}
	return true
}

// checkDictionaryConsistency finds impossible catalog entries: duplicate table
// ids, columns whose table is absent, and tables whose owner is unknown. This
// serves the same goal as dmdbchk's object-id validity check (detecting
// corrupt catalog references) without the DM id-reserve page format.
func checkDictionaryConsistency(dict *DictionaryInfo) []DictIssue {
	if dict == nil {
		return nil
	}
	var issues []DictIssue

	tableIDs := make(map[uint32]string, len(dict.Tables))
	for _, table := range dict.Tables {
		label := table.Owner + "." + table.Name
		if prev, ok := tableIDs[table.ID]; ok {
			issues = append(issues, DictIssue{
				Category: "duplicate-table-id",
				Detail:   fmt.Sprintf("table id %d used by both %s and %s", table.ID, prev, label),
			})
			continue
		}
		tableIDs[table.ID] = label
	}

	owners := make(map[string]bool, len(dict.Users))
	for _, user := range dict.Users {
		owners[strings.ToUpper(user.Name)] = true
	}
	for _, schema := range dict.Schemas {
		owners[strings.ToUpper(schema.Name)] = true
	}
	if len(owners) > 0 {
		for _, table := range dict.Tables {
			if !owners[strings.ToUpper(table.Owner)] {
				issues = append(issues, DictIssue{
					Category: "orphan-table-owner",
					Detail:   fmt.Sprintf("table %s.%s (id %d) has no matching user/schema", table.Owner, table.Name, table.ID),
				})
			}
		}
	}

	// Columns whose table id is absent from the table set.
	missingTables := make(map[uint32]int)
	for _, col := range dict.Columns {
		if _, ok := tableIDs[col.TableID]; !ok {
			missingTables[col.TableID]++
		}
	}
	for tableID, count := range missingTables {
		issues = append(issues, DictIssue{
			Category: "dangling-columns",
			Detail:   fmt.Sprintf("%d column(s) reference missing table id %d", count, tableID),
		})
	}

	sort.Slice(issues, func(i, j int) bool {
		if issues[i].Category != issues[j].Category {
			return issues[i].Category < issues[j].Category
		}
		return issues[i].Detail < issues[j].Detail
	})
	return issues
}
