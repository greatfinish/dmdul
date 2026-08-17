package dm

import (
	"sort"
	"strconv"
	"strings"
)

type pageImpactKey struct {
	tableKey string
	typeName PageObjectType
	storage  uint32
}

type pageImpactAggregate struct {
	object  PageAffectedObject
	methods map[PageAttributionMethod]bool
}

type pageTableImpactMeta struct {
	bytes uint64
}

// pageImpactAccumulator keeps exact counts for every bad page while the
// retained console detail remains bounded. Its memory use grows with affected
// objects, not with the number of corrupt pages.
type pageImpactAccumulator struct {
	attributed       int
	unattributed     int
	reasons          map[string]int
	objects          map[pageImpactKey]*pageImpactAggregate
	tableMeta        map[string]pageTableImpactMeta
	affectedTables   map[string]bool
	totalTables      int
	totalTableBytes  uint64
	affectedTableSum uint64
}

func newPageImpactAccumulator(dict *DictionaryInfo) *pageImpactAccumulator {
	a := &pageImpactAccumulator{
		reasons:        make(map[string]int),
		objects:        make(map[pageImpactKey]*pageImpactAggregate),
		tableMeta:      make(map[string]pageTableImpactMeta),
		affectedTables: make(map[string]bool),
	}
	if dict == nil {
		return a
	}
	for _, table := range dict.Tables {
		key := pageTableKey(table.ID, table.Owner, table.Name)
		a.tableMeta[key] = pageTableImpactMeta{bytes: table.Bytes}
		a.totalTables++
		a.totalTableBytes += table.Bytes
	}
	return a
}

func (a *pageImpactAccumulator) add(bad BadPage) {
	if bad.Owner == "" || bad.Table == "" || bad.ObjectType == PageObjectUnattributed {
		a.unattributed++
		reason := bad.UnattributedReason
		if reason == "" {
			reason = "unmapped"
		}
		a.reasons[reason]++
		return
	}

	a.attributed++
	tableKey := pageTableKey(bad.TableID, bad.Owner, bad.Table)
	if !a.affectedTables[tableKey] {
		a.affectedTables[tableKey] = true
		if meta, ok := a.tableMeta[tableKey]; ok {
			a.affectedTableSum += meta.bytes
		}
	}
	key := pageImpactKey{
		tableKey: tableKey,
		typeName: bad.ObjectType,
		storage:  bad.ObjectStorageID,
	}
	agg := a.objects[key]
	if agg == nil {
		agg = &pageImpactAggregate{
			object: PageAffectedObject{
				Owner:                 bad.Owner,
				Table:                 bad.Table,
				TableID:               bad.TableID,
				ObjectType:            bad.ObjectType,
				StorageID:             bad.ObjectStorageID,
				GroupID:               bad.ObjectGroupID,
				Tablespace:            bad.Tablespace,
				HeaderFile:            bad.ObjectHeaderFile,
				HeaderBlock:           bad.ObjectHeaderBlock,
				AttributionConfidence: bad.AttributionConfidence,
				SegmentBytes:          bad.SegmentBytes,
			},
			methods: make(map[PageAttributionMethod]bool),
		}
		a.objects[key] = agg
	}
	agg.methods[bad.Attribution] = true
	agg.object.AttributionConfidence = weakerAttributionConfidence(
		agg.object.AttributionConfidence, bad.AttributionConfidence)
	agg.object.BadPages++
	if bad.ObjectType == PageObjectTable && bad.ObjectHeaderFile >= 0 &&
		bad.FileID == bad.ObjectHeaderFile && bad.PageNo == bad.ObjectHeaderBlock {
		agg.object.SegmentHeaderBadPages++
	}
	switch bad.Kind {
	case PageCorruptionHeader:
		agg.object.HeaderInvalid++
	case PageCorruptionChecksum:
		agg.object.ChecksumFail++
	case PageCorruptionStructure:
		agg.object.StructureInvalid++
	}
}

func (a *pageImpactAccumulator) apply(result *PageCheckResult) {
	result.AttributedBadPages = a.attributed
	result.UnattributedBadPages = a.unattributed
	result.UnattributedReasons = make(map[string]int, len(a.reasons))
	for reason, count := range a.reasons {
		result.UnattributedReasons[reason] = count
	}
	result.AffectedTables = len(a.affectedTables)
	result.TotalTables = a.totalTables
	result.AffectedTableBytes = a.affectedTableSum
	result.TotalTableBytes = a.totalTableBytes
	result.AffectedObjects = make([]PageAffectedObject, 0, len(a.objects))
	for _, agg := range a.objects {
		methods := make([]string, 0, len(agg.methods))
		for method := range agg.methods {
			methods = append(methods, string(method))
		}
		sort.Strings(methods)
		agg.object.Attribution = strings.Join(methods, ",")
		result.AffectedObjects = append(result.AffectedObjects, agg.object)
	}
	sort.Slice(result.AffectedObjects, func(i, j int) bool {
		a := &result.AffectedObjects[i]
		b := &result.AffectedObjects[j]
		if a.Owner != b.Owner {
			return a.Owner < b.Owner
		}
		if a.Table != b.Table {
			return a.Table < b.Table
		}
		if a.ObjectType != b.ObjectType {
			return a.ObjectType < b.ObjectType
		}
		return a.StorageID < b.StorageID
	})
}

func pageTableKey(tableID uint32, owner, table string) string {
	return strconv.FormatUint(uint64(tableID), 10) + "\x00" + owner + "\x00" + table
}

func weakerAttributionConfidence(a, b PageAttributionConfidence) PageAttributionConfidence {
	if a == PageAttributionNo || b == PageAttributionNo {
		return PageAttributionNo
	}
	if a == PageAttributionMedium || b == PageAttributionMedium {
		return PageAttributionMedium
	}
	return PageAttributionHigh
}
