package dm

import (
	"encoding/binary"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

type dictionaryTextDef struct {
	ID       uint32
	SeqNo    uint32
	Text     string
	Location ddlLocation
}

type dictionaryObjectPrivilegeDef struct {
	GranteeID uint32
	ObjectID  uint32
	PrivID    int32
	Privilege string
	Grantable string
	Location  ddlLocation
}

func parseDDLSynonymTarget(page []byte, start int, decoder textDecoder) (string, string) {
	limit := start + 128
	if limit > len(page) {
		limit = len(page)
	}
	for pos := start; pos+4 < limit; pos++ {
		ownerLen := int(binary.LittleEndian.Uint16(page[pos:]))
		if ownerLen <= 0 || ownerLen > 128 {
			continue
		}
		ownerStart := pos + 2
		ownerEnd := ownerStart + ownerLen
		if ownerEnd+2 > limit {
			continue
		}
		nameLen := int(binary.LittleEndian.Uint16(page[ownerEnd:]))
		if nameLen <= 0 || nameLen > 256 {
			continue
		}
		nameStart := ownerEnd + 2
		nameEnd := nameStart + nameLen
		if nameEnd > len(page) {
			continue
		}
		owner, ok := decoder.decode(page[ownerStart:ownerEnd])
		if !ok || !isSafeShortText(owner) {
			continue
		}
		name, ok := decoder.decode(page[nameStart:nameEnd])
		if !ok || !isSafeShortText(name) {
			continue
		}
		if !looksLikeIdentifierText(owner) || !looksLikeIdentifierText(name) {
			continue
		}
		return owner, name
	}
	return "", ""
}

func looksLikeIdentifierText(value string) bool {
	if strings.TrimSpace(value) == "" {
		return false
	}
	for _, ch := range value {
		if ch == '_' || ch == '$' || ch == '#' || ch == '"' || ch == '.' {
			continue
		}
		if ch >= '0' && ch <= '9' {
			continue
		}
		if ch >= 'A' && ch <= 'Z' {
			continue
		}
		if ch >= 'a' && ch <= 'z' {
			continue
		}
		if ch > 127 {
			continue
		}
		return false
	}
	return true
}

func parseDDLTextRow(page []byte, slotOff int, pageNo uint32, slotNo uint16, rawSlotOff uint16, pageSize uint32, decoder textDecoder) (dictionaryTextDef, bool) {
	for delta := 0; delta < 4; delta++ {
		base := slotOff + delta
		if row, ok := parseStandardDDLTextRow(page, base, pageNo, slotNo, rawSlotOff, pageSize, decoder); ok {
			return row, true
		}
		if base+32 > int(pageSize) || base+32 > len(page) {
			continue
		}
		rowLen := int(binary.LittleEndian.Uint16(page[base:]))
		if rowLen < 32 || rowLen > int(pageSize)-base {
			continue
		}
		id := binary.LittleEndian.Uint32(page[base+2:])
		seqNo := binary.LittleEndian.Uint32(page[base+6:])
		if id == 0 || seqNo > 32 {
			continue
		}
		textLen := int(binary.LittleEndian.Uint32(page[base+21:]))
		textStart := base + 25
		textEnd := textStart + textLen
		if textLen <= 0 || textLen > rowLen || textEnd > len(page) || textEnd > base+rowLen {
			continue
		}
		text, ok := decoder.decode(page[textStart:textEnd])
		if !ok || strings.TrimSpace(text) == "" {
			continue
		}
		rowAbs := uint64(pageNo)*uint64(pageSize) + uint64(base)
		return dictionaryTextDef{
			ID:       id,
			SeqNo:    seqNo,
			Text:     text,
			Location: ddlLocation{PageNo: pageNo, SlotNo: slotNo, SlotOffset: rawSlotOff, RowOffset: rowAbs},
		}, true
	}
	return dictionaryTextDef{}, false
}

func parseStandardDDLTextRow(page []byte, base int, pageNo uint32, slotNo uint16, rawSlotOff uint16, pageSize uint32, decoder textDecoder) (dictionaryTextDef, bool) {
	const fixedStart = 3 // two-byte row header plus one byte of 2-bit metadata
	if base < 0 || base+fixedStart+8 > int(pageSize) || base+fixedStart+8 > len(page) {
		return dictionaryTextDef{}, false
	}
	lengthWord := binary.BigEndian.Uint16(page[base:])
	if lengthWord&dataRowDeletedMask != 0 {
		return dictionaryTextDef{}, false
	}
	rowLen := int(lengthWord &^ dataRowDeletedMask)
	if rowLen < fixedStart+8+1 || base+rowLen > int(pageSize) || base+rowLen > len(page) {
		return dictionaryTextDef{}, false
	}
	row := page[base : base+rowLen]
	states := decodeRowColumnStates(row[2:3], 3)
	if states[0] != 0 || states[1] != 0 || states[2] == 0x02 || states[2] == 0x03 {
		return dictionaryTextDef{}, false
	}
	id := binary.LittleEndian.Uint32(row[fixedStart:])
	seqNo := binary.LittleEndian.Uint32(row[fixedStart+4:])
	if id == 0 || seqNo > 1<<20 {
		return dictionaryTextDef{}, false
	}
	raw, _, err := readShortDataBytes(row, fixedStart+8)
	if err != nil {
		return dictionaryTextDef{}, false
	}
	payload, ok := unwrapDictionaryTextPayload(raw)
	if !ok {
		return dictionaryTextDef{}, false
	}
	raw = payload
	text, ok := decoder.decode(raw)
	if !ok || strings.TrimSpace(text) == "" || containsBadControl(text) {
		return dictionaryTextDef{}, false
	}
	rowAbs := uint64(pageNo)*uint64(pageSize) + uint64(base)
	return dictionaryTextDef{
		ID:       id,
		SeqNo:    seqNo,
		Text:     text,
		Location: ddlLocation{PageNo: pageNo, SlotNo: slotNo, SlotOffset: rawSlotOff, RowOffset: rowAbs},
	}, true
}

func unwrapDictionaryTextPayload(raw []byte) ([]byte, bool) {
	if len(raw) < 13 || raw[0] != 0x01 || (raw[2] != 0x02 && raw[2] != 0x04) {
		return nil, false
	}
	payloadLen := int(binary.LittleEndian.Uint32(raw[9:13]))
	if payloadLen < 0 || payloadLen != len(raw)-13 {
		return nil, false
	}
	return raw[13:], true
}

func scanDictionaryViews(objects map[uint32]dictionaryObject, texts map[uint32]map[uint32]string, matcher ownerMatcher) []DictionaryView {
	var views []DictionaryView
	for _, obj := range objects {
		if obj.Type != "SCHOBJ" || obj.Subtype != "VIEW" || obj.Valid == "N" || !matcher.allowed(obj.Owner) {
			continue
		}
		seqs := texts[obj.ID]
		view := DictionaryView{
			ID:       obj.ID,
			Owner:    obj.Owner,
			Name:     obj.Name,
			Valid:    obj.Valid,
			SQL:      seqs[0],
			QuerySQL: seqs[1],
		}
		views = append(views, view)
	}
	sortDictionaryViews(views)
	return views
}

func scanDictionarySequences(objects map[uint32]dictionaryObject, texts map[uint32]map[uint32]string, matcher ownerMatcher) []DictionarySequence {
	var sequences []DictionarySequence
	for _, obj := range objects {
		if obj.Type != "SCHOBJ" || obj.Subtype != "SEQ" || obj.Valid == "N" || !matcher.allowed(obj.Owner) {
			continue
		}
		seqInfo := parseSequencePayload(obj.Payload)
		sequences = append(sequences, DictionarySequence{
			ID:                obj.ID,
			Owner:             obj.Owner,
			Name:              obj.Name,
			Valid:             obj.Valid,
			StartWith:         int64(obj.Info3),
			HasStartWith:      true,
			MinValue:          seqInfo.minValue,
			HasMinValue:       seqInfo.hasBounds,
			MaxValue:          seqInfo.maxValue,
			HasMaxValue:       seqInfo.hasBounds,
			IncrementBy:       obj.Info4,
			CycleFlag:         boolFlag(obj.Info1&0x01 != 0),
			OrderFlag:         boolFlag(obj.Info1&0xFF00 == 0x100),
			CacheSize:         seqInfo.cacheSize,
			RuntimeFile:       seqInfo.runtimeFile,
			RuntimePage:       seqInfo.runtimePage,
			RuntimeSlot:       seqInfo.runtimeSlot,
			HasRuntimeLocator: seqInfo.hasRuntimeLocator,
			SQL:               sequenceTextSQL(texts[obj.ID]),
		})
	}
	sortDictionarySequences(sequences)
	return sequences
}

func sequenceTextSQL(seqs map[uint32]string) string {
	for _, seqNo := range []uint32{0, 1} {
		if sql := strings.TrimSpace(seqs[seqNo]); strings.HasPrefix(strings.ToUpper(sql), "CREATE") {
			return sql
		}
	}
	return ""
}

type sequencePayloadInfo struct {
	minValue          int64
	maxValue          int64
	cacheSize         uint32
	runtimeFile       uint16
	runtimePage       uint32
	runtimeSlot       uint16
	hasRuntimeLocator bool
	hasBounds         bool
}

func parseSequencePayload(payload []byte) sequencePayloadInfo {
	var result sequencePayloadInfo
	if len(payload) >= 16 {
		result.maxValue = int64(binary.LittleEndian.Uint64(payload[0:]))
		result.minValue = int64(binary.LittleEndian.Uint64(payload[8:]))
		result.hasBounds = true
	}
	if len(payload) >= 24 {
		result.runtimeFile = binary.LittleEndian.Uint16(payload[16:])
		result.runtimePage = binary.LittleEndian.Uint32(payload[18:])
		result.runtimeSlot = binary.LittleEndian.Uint16(payload[22:])
		result.hasRuntimeLocator = result.runtimePage != 0
	}
	if len(payload) >= 28 {
		cache := binary.LittleEndian.Uint32(payload[24:])
		if cache < 1_000_000 {
			result.cacheSize = cache
		}
	}
	return result
}

const (
	sequenceRuntimeCountOffset = 0x52
	sequenceRuntimeRecordBase  = 0x54
	sequenceRuntimeRecordSize  = 9
	sequenceRuntimeUnusedFlag  = 0x10
)

// enrichSequenceRuntimeValues follows the locator embedded in SYSOBJECTS.INFO5
// to the compact sequence-state slot in SYSTEM.DBF. For an allocated slot DM
// stores the last reserved value, while DBA_SEQUENCES.LAST_NUMBER exposes the
// next safe value (stored value + increment).
func enrichSequenceRuntimeValues(stream *systemPageStream, sequences []DictionarySequence) {
	if stream == nil {
		return
	}
	pages := make(map[uint32][]byte)
	for i := range sequences {
		seq := &sequences[i]
		if seq.HasLastNumber || !seq.HasRuntimeLocator || seq.RuntimeFile != 0 {
			continue
		}
		page, ok := pages[seq.RuntimePage]
		if !ok {
			var err error
			page, err = stream.readPage(seq.RuntimePage)
			if err != nil {
				continue
			}
			pages[seq.RuntimePage] = page
		}
		last, state, ok := parseSequenceRuntimeValue(page, seq.RuntimePage, seq.RuntimeSlot, seq.IncrementBy)
		if !ok {
			continue
		}
		seq.LastNumber = last
		seq.HasLastNumber = true
		seq.RuntimeState = state
	}
}

func mergeSequenceRuntimeMetadata(preferred []DictionarySequence, recovered []DictionarySequence) {
	byID := make(map[uint32]DictionarySequence, len(recovered))
	byName := make(map[string]DictionarySequence, len(recovered))
	for _, seq := range recovered {
		byID[seq.ID] = seq
		byName[strings.ToUpper(seq.Owner)+"\x00"+strings.ToUpper(seq.Name)] = seq
	}
	for i := range preferred {
		seq := &preferred[i]
		recoveredSeq, ok := byID[seq.ID]
		if !ok {
			recoveredSeq, ok = byName[strings.ToUpper(seq.Owner)+"\x00"+strings.ToUpper(seq.Name)]
		}
		if !ok {
			continue
		}
		if !seq.HasStartWith && seq.StartWith == 0 {
			seq.StartWith = recoveredSeq.StartWith
			seq.HasStartWith = recoveredSeq.HasStartWith
		}
		if !seq.HasRuntimeLocator {
			seq.RuntimeFile = recoveredSeq.RuntimeFile
			seq.RuntimePage = recoveredSeq.RuntimePage
			seq.RuntimeSlot = recoveredSeq.RuntimeSlot
			seq.HasRuntimeLocator = recoveredSeq.HasRuntimeLocator
		}
		if !seq.HasLastNumber && recoveredSeq.HasLastNumber {
			seq.LastNumber = recoveredSeq.LastNumber
			seq.HasLastNumber = true
			seq.RuntimeState = recoveredSeq.RuntimeState
		}
	}
}

func sequenceRuntimeRecoveryStats(sequences []DictionarySequence) (int, int) {
	recovered := 0
	pages := make(map[uint64]bool)
	for _, seq := range sequences {
		if !seq.HasLastNumber {
			continue
		}
		recovered++
		if seq.HasRuntimeLocator {
			key := uint64(seq.RuntimeFile)<<32 | uint64(seq.RuntimePage)
			pages[key] = true
		}
	}
	return recovered, len(pages)
}

func parseSequenceRuntimeValue(page []byte, pageNo uint32, slot uint16, increment int64) (int64, uint8, bool) {
	if len(page) < sequenceRuntimeRecordBase || len(page) < 8 || binary.LittleEndian.Uint32(page[4:]) != pageNo {
		return 0, 0, false
	}
	count := binary.LittleEndian.Uint16(page[sequenceRuntimeCountOffset:])
	capacity := (len(page) - sequenceRuntimeRecordBase) / sequenceRuntimeRecordSize
	// The header field is an active-record count, not the highest slot plus
	// one. Dropped sequences leave holes, so a valid locator may point beyond
	// count while still remaining inside the fixed record array.
	if count == 0 || int(count) > capacity || int(slot) >= capacity {
		return 0, 0, false
	}
	off := sequenceRuntimeRecordBase + int(slot)*sequenceRuntimeRecordSize
	if off < 0 || off+sequenceRuntimeRecordSize > len(page) {
		return 0, 0, false
	}
	state := page[off]
	if state&0x01 == 0 || state&0xE0 != 0 {
		return 0, 0, false
	}
	stored := int64(binary.LittleEndian.Uint64(page[off+1:]))
	if state&sequenceRuntimeUnusedFlag != 0 {
		return stored, state, true
	}
	last, ok := checkedAddInt64(stored, increment)
	return last, state, ok
}

func checkedAddInt64(left int64, right int64) (int64, bool) {
	const maxInt64 = int64(^uint64(0) >> 1)
	const minInt64 = -maxInt64 - 1
	if (right > 0 && left > maxInt64-right) || (right < 0 && left < minInt64-right) {
		return 0, false
	}
	return left + right, true
}

func scanDictionaryRoutines(objects map[uint32]dictionaryObject, texts map[uint32]map[uint32]string, rawTexts map[string]string, matcher ownerMatcher) []DictionaryRoutine {
	var routines []DictionaryRoutine
	for _, obj := range objects {
		if obj.Type != "SCHOBJ" || obj.Valid == "N" || !matcher.allowed(obj.Owner) {
			continue
		}
		if isSystemCatalogOwner(obj.Owner) || strings.HasPrefix(obj.Name, "##") {
			continue
		}
		if isKnownGeneratedSYSDBARoutine(obj.Owner, obj.Name) {
			continue
		}
		switch obj.Subtype {
		case "PROC":
			sql := routineTextSQL(texts[obj.ID], 0)
			objectType := routineTypeFromSQL(sql)
			if objectType == "" {
				objectType = "PROCEDURE"
			}
			if raw, rawType := bestRawRoutineText(rawTexts, obj.Owner, obj.Name, objectType, "FUNCTION", "PROCEDURE"); len(raw) > len(sql) {
				sql = raw
				objectType = rawType
			}
			routines = append(routines, DictionaryRoutine{
				ID:         obj.ID,
				Owner:      obj.Owner,
				Name:       obj.Name,
				ObjectType: objectType,
				SeqNo:      0,
				Valid:      obj.Valid,
				SQL:        sql,
			})
		case "PKG":
			specSQL := routineTextSQL(texts[obj.ID], 0)
			if raw, _ := bestRawRoutineText(rawTexts, obj.Owner, obj.Name, "PACKAGE"); len(raw) > len(specSQL) {
				specSQL = raw
			}
			routines = append(routines, DictionaryRoutine{
				ID:         obj.ID,
				Owner:      obj.Owner,
				Name:       obj.Name,
				ObjectType: "PACKAGE",
				SeqNo:      0,
				Valid:      obj.Valid,
				SQL:        specSQL,
			})
			bodySQL := routineTextSQL(texts[obj.ID], 1)
			if raw, _ := bestRawRoutineText(rawTexts, obj.Owner, obj.Name, "PACKAGE BODY"); len(raw) > len(bodySQL) {
				bodySQL = raw
			}
			if bodySQL != "" {
				routines = append(routines, DictionaryRoutine{
					ID:         obj.ID,
					Owner:      obj.Owner,
					Name:       obj.Name,
					ObjectType: "PACKAGE BODY",
					SeqNo:      1,
					Valid:      obj.Valid,
					SQL:        bodySQL,
				})
			}
		}
	}
	sortDictionaryRoutines(routines)
	return routines
}

func routineTextSQL(seqs map[uint32]string, seqNo uint32) string {
	return strings.TrimSpace(seqs[seqNo])
}

func routineTypeFromSQL(sql string) string {
	objectType, _, _ := parseCreateRoutineName(sql)
	return objectType
}

func bestRawRoutineText(rawTexts map[string]string, owner string, name string, preferredTypes ...string) (string, string) {
	for _, objectType := range preferredTypes {
		objectType = normalizeRoutineObjectType(objectType)
		if objectType == "" {
			continue
		}
		if sql := rawTexts[routineKey(owner, name, objectType)]; sql != "" {
			return sql, objectType
		}
	}
	for _, objectType := range []string{"PACKAGE", "PACKAGE BODY", "FUNCTION", "PROCEDURE"} {
		if sql := rawTexts[routineKey(owner, name, objectType)]; sql != "" {
			return sql, objectType
		}
	}
	return "", ""
}

func scanDictionaryTriggers(objects map[uint32]dictionaryObject, texts map[uint32]map[uint32]string, rawTexts map[string]string, matcher ownerMatcher) []DictionaryTrigger {
	var triggers []DictionaryTrigger
	for _, obj := range objects {
		if obj.Type != "SCHOBJ" || obj.Subtype != "TRIG" || obj.Valid == "N" || !matcher.allowed(obj.Owner) {
			continue
		}
		sql := triggerTextSQL(texts[obj.ID])
		if raw := rawTexts[qualifiedObjectKey(obj.Owner, obj.Name)]; len(raw) > len(sql) {
			sql = raw
		}
		tableOwner, tableName := triggerTargetFromParent(objects, obj)
		if tableOwner == "" || tableName == "" {
			tableOwner, tableName = parseTriggerTargetTable(sql)
		}
		triggers = append(triggers, DictionaryTrigger{
			ID:         obj.ID,
			Owner:      obj.Owner,
			Name:       obj.Name,
			TableOwner: tableOwner,
			TableName:  tableName,
			Valid:      obj.Valid,
			SQL:        sql,
		})
	}
	sortDictionaryTriggers(triggers)
	return triggers
}

func triggerTextSQL(seqs map[uint32]string) string {
	for _, seqNo := range []uint32{0, 1} {
		if sql := strings.TrimSpace(seqs[seqNo]); strings.Contains(strings.ToUpper(sql), "TRIGGER") {
			return sql
		}
	}
	return ""
}

func triggerTargetFromParent(objects map[uint32]dictionaryObject, trigger dictionaryObject) (string, string) {
	if trigger.ParentID <= 0 {
		return "", ""
	}
	table, ok := objects[uint32(trigger.ParentID)]
	if !ok || table.Type != "SCHOBJ" || table.Subtype != "UTAB" {
		return "", ""
	}
	return table.Owner, table.Name
}

func scanDictionarySynonyms(objects map[uint32]dictionaryObject, matcher ownerMatcher) []DictionarySynonym {
	var synonyms []DictionarySynonym
	for _, obj := range objects {
		if obj.Subtype != "SYNOM" && obj.Type != "DSYNOM" {
			continue
		}
		if obj.TargetOwner == "" || obj.TargetName == "" {
			continue
		}
		if strings.HasPrefix(obj.Name, "##") || strings.HasPrefix(obj.TargetName, "##") {
			continue
		}
		owner := obj.Owner
		public := false
		if obj.Type == "DSYNOM" || owner == "" {
			owner = "PUBLIC"
			public = true
		}
		if owner == "SYS" || (public && strings.EqualFold(obj.TargetOwner, "SYS")) {
			continue
		}
		target, targetOK := dictionaryObjectByOwnerName(objects, obj.TargetOwner, obj.TargetName)
		if targetOK && !isTabPrivilegeTarget(target) {
			continue
		}
		if public && !targetOK {
			continue
		}
		if !matcher.allowed(owner) && !matcher.allowed(obj.TargetOwner) {
			continue
		}
		synonyms = append(synonyms, DictionarySynonym{
			ID:         obj.ID,
			Owner:      owner,
			Name:       obj.Name,
			TableOwner: obj.TargetOwner,
			TableName:  obj.TargetName,
			Public:     public,
		})
	}
	sortDictionarySynonyms(synonyms)
	return synonyms
}

func dictionaryObjectByOwnerName(objects map[uint32]dictionaryObject, owner string, name string) (dictionaryObject, bool) {
	for _, obj := range objects {
		if strings.EqualFold(obj.Owner, owner) && strings.EqualFold(obj.Name, name) {
			return obj, true
		}
	}
	return dictionaryObject{}, false
}

func isSystemCatalogOwner(owner string) bool {
	switch strings.ToUpper(strings.TrimSpace(owner)) {
	case "SYS", "CTISYS", "SYSAUDITOR", "SYSSSO", "SYSJOB":
		return true
	default:
		return false
	}
}

func isKnownGeneratedSYSDBARoutine(owner string, name string) bool {
	if !strings.EqualFold(strings.TrimSpace(owner), "SYSDBA") {
		return false
	}
	switch strings.ToUpper(strings.TrimSpace(name)) {
	case "SP_ARCH_BAKSET_REMOVE_BATCH",
		"SP_DB_BAKSET_REMOVE_BATCH",
		"SP_DROP_CONS_AND_OBJ_REFS_WHEN_DROP_SCHEMA",
		"SP_TAB_BAKSET_REMOVE_BATCH",
		"SP_TS_BAKSET_REMOVE_BATCH",
		"SP_UPDATE_SYSHPARTTABLEINFO_RESVD1":
		return true
	default:
		return false
	}
}

func parseDDLObjectPrivilegeRow(page []byte, slotOff int, pageNo uint32, slotNo uint16, rawSlotOff uint16, pageSize uint32) (dictionaryObjectPrivilegeDef, bool) {
	for delta := 0; delta < 4; delta++ {
		base := slotOff + delta
		if base+44 > int(pageSize) || base+44 > len(page) {
			continue
		}
		rowLen := binary.LittleEndian.Uint16(page[base+1:])
		if rowLen != 44 {
			continue
		}
		colID := int32(binary.LittleEndian.Uint32(page[base+12:]))
		privID := int32(binary.LittleEndian.Uint32(page[base+16:]))
		grantor := int32(binary.LittleEndian.Uint32(page[base+20:]))
		grantable := page[base+24]
		if colID != -1 || privID == -1 || grantor == -1 {
			continue
		}
		if grantable != 'Y' && grantable != 'N' {
			continue
		}
		privilege := objectPrivilegeName(privID)
		if privilege == "" {
			continue
		}
		granteeID := binary.LittleEndian.Uint32(page[base+4:])
		objectID := binary.LittleEndian.Uint32(page[base+8:])
		if granteeID == 0 || objectID == 0 {
			continue
		}
		rowAbs := uint64(pageNo)*uint64(pageSize) + uint64(base)
		return dictionaryObjectPrivilegeDef{
			GranteeID: granteeID,
			ObjectID:  objectID,
			PrivID:    privID,
			Privilege: privilege,
			Grantable: string([]byte{grantable}),
			Location:  ddlLocation{PageNo: pageNo, SlotNo: slotNo, SlotOffset: rawSlotOff, RowOffset: rowAbs},
		}, true
	}
	return dictionaryObjectPrivilegeDef{}, false
}

func objectPrivilegeName(privID int32) string {
	switch privID {
	case 8192:
		return "SELECT"
	case 8193:
		return "INSERT"
	case 8194:
		return "DELETE"
	case 8195:
		return "UPDATE"
	default:
		return ""
	}
}

func isTabPrivilegeTarget(obj dictionaryObject) bool {
	if obj.Type != "SCHOBJ" {
		return false
	}
	if strings.HasPrefix(obj.Name, "##") {
		return false
	}
	switch obj.Subtype {
	case "UTAB", "VIEW", "SEQ":
		return obj.Owner != "" && obj.Name != ""
	default:
		return false
	}
}

func dictionaryPrivilegeObjectType(obj dictionaryObject) string {
	switch obj.Subtype {
	case "VIEW":
		return "VIEW"
	case "SEQ":
		return "SEQUENCE"
	default:
		return "TABLE"
	}
}

func sortDictionaryViews(views []DictionaryView) {
	sort.Slice(views, func(i, j int) bool {
		if views[i].Owner != views[j].Owner {
			return views[i].Owner < views[j].Owner
		}
		if views[i].Name != views[j].Name {
			return views[i].Name < views[j].Name
		}
		return views[i].ID < views[j].ID
	})
}

func sortDictionarySequences(sequences []DictionarySequence) {
	sort.Slice(sequences, func(i, j int) bool {
		if sequences[i].Owner != sequences[j].Owner {
			return sequences[i].Owner < sequences[j].Owner
		}
		if sequences[i].Name != sequences[j].Name {
			return sequences[i].Name < sequences[j].Name
		}
		return sequences[i].ID < sequences[j].ID
	})
}

func sortDictionaryRoutines(routines []DictionaryRoutine) {
	sort.Slice(routines, func(i, j int) bool {
		if routines[i].Owner != routines[j].Owner {
			return routines[i].Owner < routines[j].Owner
		}
		if routines[i].Name != routines[j].Name {
			return routines[i].Name < routines[j].Name
		}
		if routines[i].ID != routines[j].ID {
			return routines[i].ID < routines[j].ID
		}
		if routines[i].SeqNo != routines[j].SeqNo {
			return routines[i].SeqNo < routines[j].SeqNo
		}
		return routineTypeOrder(routines[i].ObjectType) < routineTypeOrder(routines[j].ObjectType)
	})
}

func routineTypeOrder(objectType string) int {
	switch normalizeRoutineObjectType(objectType) {
	case "PACKAGE":
		return 1
	case "PACKAGE BODY":
		return 2
	case "PROCEDURE":
		return 3
	case "FUNCTION":
		return 4
	default:
		return 9
	}
}

func sortDictionaryTriggers(triggers []DictionaryTrigger) {
	sort.Slice(triggers, func(i, j int) bool {
		if triggers[i].Owner != triggers[j].Owner {
			return triggers[i].Owner < triggers[j].Owner
		}
		if triggers[i].Name != triggers[j].Name {
			return triggers[i].Name < triggers[j].Name
		}
		return triggers[i].ID < triggers[j].ID
	})
}

func sortDictionarySynonyms(synonyms []DictionarySynonym) {
	sort.Slice(synonyms, func(i, j int) bool {
		if synonyms[i].Owner != synonyms[j].Owner {
			return synonyms[i].Owner < synonyms[j].Owner
		}
		if synonyms[i].Name != synonyms[j].Name {
			return synonyms[i].Name < synonyms[j].Name
		}
		return synonyms[i].ID < synonyms[j].ID
	})
}

func sortDictionaryTabPrivileges(privileges []DictionaryTabPrivilege) {
	sort.Slice(privileges, func(i, j int) bool {
		if privileges[i].Grantee != privileges[j].Grantee {
			return privileges[i].Grantee < privileges[j].Grantee
		}
		if privileges[i].Owner != privileges[j].Owner {
			return privileges[i].Owner < privileges[j].Owner
		}
		if privileges[i].ObjectName != privileges[j].ObjectName {
			return privileges[i].ObjectName < privileges[j].ObjectName
		}
		if privileges[i].Privilege != privileges[j].Privilege {
			return privileges[i].Privilege < privileges[j].Privilege
		}
		return privileges[i].Grantable < privileges[j].Grantable
	})
}

func dictionarySequencesForDDL(dict *DictionaryInfo, matcher ownerMatcher) ([]DictionarySequence, bool) {
	if dict == nil || len(dict.Sequences) == 0 {
		return nil, false
	}
	sequences := make([]DictionarySequence, 0, len(dict.Sequences))
	for _, seq := range dict.Sequences {
		if strings.TrimSpace(seq.Owner) == "" || strings.TrimSpace(seq.Name) == "" || !matcher.allowed(seq.Owner) {
			continue
		}
		sequences = append(sequences, seq)
	}
	sortDictionarySequences(sequences)
	return sequences, true
}

func dictionaryRoutinesForDDL(dict *DictionaryInfo, matcher ownerMatcher) ([]DictionaryRoutine, bool) {
	if dict == nil || len(dict.Routines) == 0 {
		return nil, false
	}
	routines := make([]DictionaryRoutine, 0, len(dict.Routines))
	for _, routine := range dict.Routines {
		if strings.TrimSpace(routine.Owner) == "" || strings.TrimSpace(routine.Name) == "" || !matcher.allowed(routine.Owner) {
			continue
		}
		if isSystemCatalogOwner(routine.Owner) || strings.HasPrefix(routine.Name, "##") {
			continue
		}
		if isKnownGeneratedSYSDBARoutine(routine.Owner, routine.Name) {
			continue
		}
		objectType := normalizeRoutineObjectType(routine.ObjectType)
		if objectType == "" {
			continue
		}
		routine.ObjectType = objectType
		routines = append(routines, routine)
	}
	sortDictionaryRoutines(routines)
	return routines, true
}

func dictionaryTriggersForDDL(dict *DictionaryInfo, matcher ownerMatcher, tableMatcher tableNameMatcher) ([]DictionaryTrigger, bool) {
	if dict == nil || len(dict.Triggers) == 0 {
		return nil, false
	}
	triggers := make([]DictionaryTrigger, 0, len(dict.Triggers))
	for _, trigger := range dict.Triggers {
		if strings.TrimSpace(trigger.Owner) == "" || strings.TrimSpace(trigger.Name) == "" {
			continue
		}
		if !matcher.allowed(trigger.Owner) && !matcher.allowed(trigger.TableOwner) {
			continue
		}
		if tableMatcher.hasRules && !tableMatcher.all && !tableMatcher.allowed(trigger.TableOwner, trigger.TableName) {
			continue
		}
		triggers = append(triggers, trigger)
	}
	sortDictionaryTriggers(triggers)
	return triggers, true
}

func dictionaryViewsForDDL(dict *DictionaryInfo, matcher ownerMatcher) ([]DictionaryView, bool) {
	if dict == nil || len(dict.Views) == 0 {
		return nil, false
	}
	views := make([]DictionaryView, 0, len(dict.Views))
	for _, view := range dict.Views {
		if strings.TrimSpace(view.Owner) == "" || strings.TrimSpace(view.Name) == "" || !matcher.allowed(view.Owner) {
			continue
		}
		views = append(views, view)
	}
	sortDictionaryViews(views)
	return views, true
}

func dictionarySynonymsForDDL(dict *DictionaryInfo, matcher ownerMatcher) ([]DictionarySynonym, bool) {
	if dict == nil || len(dict.Synonyms) == 0 {
		return nil, false
	}
	synonyms := make([]DictionarySynonym, 0, len(dict.Synonyms))
	for _, syn := range dict.Synonyms {
		if strings.TrimSpace(syn.Owner) == "" || strings.TrimSpace(syn.Name) == "" || strings.TrimSpace(syn.TableOwner) == "" || strings.TrimSpace(syn.TableName) == "" {
			continue
		}
		if !matcher.allowed(syn.Owner) && !matcher.allowed(syn.TableOwner) {
			continue
		}
		synonyms = append(synonyms, syn)
	}
	sortDictionarySynonyms(synonyms)
	return synonyms, true
}

func dictionaryTabPrivilegesForDDL(dict *DictionaryInfo, matcher ownerMatcher, tableMatcher tableNameMatcher) ([]DictionaryTabPrivilege, bool) {
	if dict == nil || len(dict.TabPrivileges) == 0 {
		return nil, false
	}
	privileges := make([]DictionaryTabPrivilege, 0, len(dict.TabPrivileges))
	for _, priv := range dict.TabPrivileges {
		if strings.TrimSpace(priv.Grantee) == "" || strings.TrimSpace(priv.Owner) == "" || strings.TrimSpace(priv.ObjectName) == "" || strings.TrimSpace(priv.Privilege) == "" {
			continue
		}
		if !matcher.allowed(priv.Owner) && !matcher.allowed(priv.Grantee) {
			continue
		}
		if !tableMatcher.allowed(priv.Owner, priv.ObjectName) && !matcher.allowed(priv.Grantee) {
			continue
		}
		privileges = append(privileges, priv)
	}
	sortDictionaryTabPrivileges(privileges)
	return privileges, true
}

func boolFlag(value bool) string {
	if value {
		return "Y"
	}
	return "N"
}

func qualifiedObjectKey(owner string, name string) string {
	return strings.ToUpper(strings.TrimSpace(owner)) + "." + strings.ToUpper(strings.TrimSpace(name))
}

func routineKey(owner string, name string, objectType string) string {
	return qualifiedObjectKey(owner, name) + "\x00" + normalizeRoutineObjectType(objectType)
}

var createTriggerNamePattern = regexp.MustCompile(`(?is)CREATE\s+OR\s+REPLACE\s+TRIGGER\s+((?:"[^"]+"|[A-Za-z_][A-Za-z0-9_$#]*)\.)?("[^"]+"|[A-Za-z_][A-Za-z0-9_$#]*)`)
var triggerOnTablePattern = regexp.MustCompile(`(?is)\bON\s+((?:"[^"]+"|[A-Za-z_][A-Za-z0-9_$#]*)\.)?("[^"]+"|[A-Za-z_][A-Za-z0-9_$#]*)`)
var createRoutineNamePattern = regexp.MustCompile(`(?is)CREATE\s+(?:OR\s+REPLACE\s+)?(PACKAGE\s+BODY|PACKAGE|PROCEDURE|FUNCTION)\s+((?:"[^"]+"|[A-Za-z_][A-Za-z0-9_$#]*)\.)?("[^"]+"|[A-Za-z_][A-Za-z0-9_$#]*)`)

func scanRawRoutineTexts(data []byte, decoder textDecoder) map[string]string {
	result := make(map[string]string)
	for _, keyword := range [][]byte{
		[]byte("CREATE OR REPLACE FUNCTION"),
		[]byte("CREATE OR REPLACE PROCEDURE"),
		[]byte("CREATE OR REPLACE PACKAGE"),
	} {
		for searchFrom := 0; searchFrom < len(data); {
			rel := indexASCIIInsensitive(data[searchFrom:], keyword)
			if rel < 0 {
				break
			}
			start := searchFrom + rel
			end := rawRoutineEnd(data, start)
			if end <= start {
				searchFrom = start + len(keyword)
				continue
			}
			sql, ok := decoder.decode(data[start:end])
			if !ok {
				searchFrom = end
				continue
			}
			sql = cleanRecoveredSQLText(sql)
			objectType, owner, name := parseCreateRoutineName(sql)
			if owner != "" && name != "" && objectType != "" {
				key := routineKey(owner, name, objectType)
				if len(sql) > len(result[key]) {
					result[key] = sql
				}
			}
			searchFrom = end
		}
	}
	return result
}

func scanRawTriggerTexts(data []byte, decoder textDecoder) map[string]string {
	result := make(map[string]string)
	keyword := []byte("CREATE OR REPLACE TRIGGER")
	for searchFrom := 0; searchFrom < len(data); {
		rel := indexASCIIInsensitive(data[searchFrom:], keyword)
		if rel < 0 {
			break
		}
		start := searchFrom + rel
		end := rawTriggerEnd(data, start)
		if end <= start {
			searchFrom = start + len(keyword)
			continue
		}
		sql, ok := decoder.decode(data[start:end])
		if !ok {
			searchFrom = end
			continue
		}
		sql = cleanRecoveredSQLText(sql)
		owner, name := parseCreateTriggerName(sql)
		if owner != "" && name != "" {
			key := qualifiedObjectKey(owner, name)
			if len(sql) > len(result[key]) {
				result[key] = sql
			}
		}
		searchFrom = end
	}
	return result
}

func rawRoutineEnd(data []byte, start int) int {
	maxEnd := start + 512*1024
	if maxEnd > len(data) {
		maxEnd = len(data)
	}
	window := data[start:maxEnd]
	tokens := scanRawSQLTokens(window)
	for i := 0; i < len(tokens); i++ {
		if tokens[i].text != "END" {
			continue
		}
		semicolonIndex := i + 1
		if semicolonIndex < len(tokens) && tokens[semicolonIndex].text != ";" {
			if isNestedRoutineEndTail(tokens[semicolonIndex].text) {
				continue
			}
			semicolonIndex++
		}
		if semicolonIndex >= len(tokens) || tokens[semicolonIndex].text != ";" {
			continue
		}
		end := tokens[semicolonIndex].end
		if rawSQLBoundary(window, end) {
			return start + end
		}
	}
	return 0
}

type rawSQLToken struct {
	text string
	pos  int
	end  int
}

func scanRawSQLTokens(data []byte) []rawSQLToken {
	var tokens []rawSQLToken
	for i := 0; i < len(data); {
		b := data[i]
		switch {
		case b == '\'':
			i = skipRawSQLSingleQuoted(data, i)
		case b == '"':
			i = skipRawSQLDoubleQuoted(data, i)
		case b == '-' && i+1 < len(data) && data[i+1] == '-':
			i += 2
			for i < len(data) && data[i] != '\r' && data[i] != '\n' {
				i++
			}
		case b == '/' && i+1 < len(data) && data[i+1] == '*':
			i += 2
			for i+1 < len(data) && !(data[i] == '*' && data[i+1] == '/') {
				i++
			}
			if i+1 < len(data) {
				i += 2
			}
		case b == ';':
			tokens = append(tokens, rawSQLToken{text: ";", pos: i, end: i + 1})
			i++
		case isRawSQLWordStart(b):
			start := i
			i++
			for i < len(data) && isRawSQLWordPart(data[i]) {
				i++
			}
			tokens = append(tokens, rawSQLToken{text: strings.ToUpper(string(data[start:i])), pos: start, end: i})
		default:
			i++
		}
	}
	return tokens
}

func skipRawSQLSingleQuoted(data []byte, start int) int {
	for i := start + 1; i < len(data); i++ {
		if data[i] != '\'' {
			continue
		}
		if i+1 < len(data) && data[i+1] == '\'' {
			i++
			continue
		}
		return i + 1
	}
	return len(data)
}

func skipRawSQLDoubleQuoted(data []byte, start int) int {
	for i := start + 1; i < len(data); i++ {
		if data[i] != '"' {
			continue
		}
		if i+1 < len(data) && data[i+1] == '"' {
			i++
			continue
		}
		return i + 1
	}
	return len(data)
}

func isRawSQLWordStart(b byte) bool {
	return (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') || b == '_'
}

func isRawSQLWordPart(b byte) bool {
	return isRawSQLWordStart(b) || (b >= '0' && b <= '9') || b == '$' || b == '#'
}

func isNestedRoutineEndTail(tail string) bool {
	tail = strings.TrimSpace(strings.ToUpper(tail))
	switch tail {
	case "IF", "LOOP", "CASE", "WHILE", "FOR":
		return true
	default:
		return false
	}
}

func indexASCIIInsensitive(data []byte, needle []byte) int {
	if len(needle) == 0 {
		return 0
	}
	if len(data) < len(needle) {
		return -1
	}
	first := toUpperASCII(needle[0])
	limit := len(data) - len(needle)
	for i := 0; i <= limit; i++ {
		if toUpperASCII(data[i]) != first {
			continue
		}
		matched := true
		for j := 1; j < len(needle); j++ {
			if toUpperASCII(data[i+j]) != toUpperASCII(needle[j]) {
				matched = false
				break
			}
		}
		if matched {
			return i
		}
	}
	return -1
}

func toUpperASCII(b byte) byte {
	if b >= 'a' && b <= 'z' {
		return b - ('a' - 'A')
	}
	return b
}

func rawTriggerEnd(data []byte, start int) int {
	maxEnd := start + 65536
	if maxEnd > len(data) {
		maxEnd = len(data)
	}
	window := data[start:maxEnd]
	tokens := scanRawSQLTokens(window)
	for i := 0; i < len(tokens); i++ {
		if tokens[i].text != "END" {
			continue
		}
		semicolonIndex := i + 1
		if semicolonIndex < len(tokens) && tokens[semicolonIndex].text != ";" {
			if isNestedRoutineEndTail(tokens[semicolonIndex].text) {
				continue
			}
			semicolonIndex++
		}
		if semicolonIndex >= len(tokens) || tokens[semicolonIndex].text != ";" {
			continue
		}
		end := tokens[semicolonIndex].end
		if rawSQLBoundary(window, end) {
			return start + end
		}
	}
	return 0
}

func rawSQLBoundary(window []byte, end int) bool {
	for i := end; i < len(window) && i < end+64; i++ {
		b := window[i]
		if b == 0 {
			return true
		}
		if b == '/' || b == '\r' || b == '\n' || b == '\t' || b == ' ' {
			continue
		}
		if b < 32 || b >= 0x80 {
			return true
		}
		return false
	}
	return true
}

func parseCreateTriggerName(sql string) (string, string) {
	matches := createTriggerNamePattern.FindStringSubmatch(sql)
	if len(matches) == 0 {
		return "", ""
	}
	owner := strings.TrimSuffix(matches[1], ".")
	name := matches[2]
	return unquoteIdentifier(owner), unquoteIdentifier(name)
}

func parseTriggerTargetTable(sql string) (string, string) {
	matches := triggerOnTablePattern.FindStringSubmatch(sql)
	if len(matches) == 0 {
		return "", ""
	}
	owner := strings.TrimSuffix(matches[1], ".")
	name := matches[2]
	return unquoteIdentifier(owner), unquoteIdentifier(name)
}

func parseCreateRoutineName(sql string) (string, string, string) {
	matches := createRoutineNamePattern.FindStringSubmatch(sql)
	if len(matches) == 0 {
		return "", "", ""
	}
	objectType := normalizeRoutineObjectType(matches[1])
	owner := strings.TrimSuffix(matches[2], ".")
	name := matches[3]
	return objectType, unquoteIdentifier(owner), unquoteIdentifier(name)
}

func normalizeRoutineObjectType(objectType string) string {
	objectType = strings.Join(strings.Fields(strings.ToUpper(strings.TrimSpace(objectType))), " ")
	switch objectType {
	case "PROCEDURE", "FUNCTION", "PACKAGE", "PACKAGE BODY":
		return objectType
	default:
		return ""
	}
}

func unquoteIdentifier(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 2 && strings.HasPrefix(value, `"`) && strings.HasSuffix(value, `"`) {
		return strings.ReplaceAll(value[1:len(value)-1], `""`, `"`)
	}
	return value
}

func formatInt64Field(value int64) string {
	if value == 0 {
		return ""
	}
	return strconv.FormatInt(value, 10)
}

func parseInt64Field(value string) int64 {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0
	}
	return parsed
}
