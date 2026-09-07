package dm

import (
	"encoding/binary"
	"fmt"
	"strings"
	"unicode/utf8"
)

func iterDictionaryRowsInPage(page []byte, pageSize uint32, pageNo uint32, visit func(page []byte, pageNo uint32, slotNo uint16, slotOff uint16)) {
	if len(page) < int(pageSize) || len(page) < sysObjectsSlotCountOff+2 {
		return
	}
	slotCount := binary.LittleEndian.Uint16(page[sysObjectsSlotCountOff:])
	if slotCount == 0 || slotCount >= 2048 {
		return
	}
	slotArrayStart := int(pageSize) - pageSlotTrailerLenForPage(page) - int(slotCount)*2
	if slotArrayStart < 0x40 || slotArrayStart >= int(pageSize) {
		return
	}
	for slotNo := uint16(0); slotNo < slotCount; slotNo++ {
		pos := slotArrayStart + int(slotNo)*2
		slotOff := binary.LittleEndian.Uint16(page[pos:])
		if slotOff == 0 || int(slotOff) >= int(pageSize) {
			continue
		}
		visit(page, pageNo, slotNo, slotOff)
	}
}

func parseDDLObjectRow(page []byte, rowOff int, pageNo uint32, slotNo uint16, slotOff uint16, pageSize uint32, decoder textDecoder) (dictionaryObject, bool) {
	if rowOff+0x50 >= int(pageSize) {
		return dictionaryObject{}, false
	}
	name, next, ok := readDDLShortString(page, rowOff+0x40, decoder, false)
	if !ok {
		return dictionaryObject{}, false
	}
	objType, next, ok := readDDLShortString(page, next, decoder, false)
	if !ok {
		return dictionaryObject{}, false
	}
	subtype, subtypeNext, ok := readOptionalDDLObjectSubtype(page, next, decoder, objType)
	if !ok {
		return dictionaryObject{}, false
	}
	if !isLikelyDictionaryType(objType, subtype) {
		return dictionaryObject{}, false
	}
	targetOwner, targetName := "", ""
	if objType == "DSYNOM" || subtype == "SYNOM" {
		targetOwner, targetName = parseDDLSynonymTarget(page, subtypeNext, decoder)
	}
	payload := parseDDLObjectPayload(page, subtypeNext)
	valid := ""
	if b := page[rowOff+0x3F]; b == 'Y' || b == 'N' {
		valid = string([]byte{b})
	}
	rowAbs := uint64(pageNo)*uint64(pageSize) + uint64(rowOff)
	schemaID := binary.LittleEndian.Uint32(page[rowOff+0x0B:])
	return dictionaryObject{
		ID:          binary.LittleEndian.Uint32(page[rowOff+0x07:]),
		SchemaID:    schemaID,
		Owner:       schemaName(schemaID),
		ParentID:    int32(binary.LittleEndian.Uint32(page[rowOff+0x0F:])),
		Info1:       binary.LittleEndian.Uint32(page[rowOff+sysObjectsInfo1Offset:]),
		Info2:       binary.LittleEndian.Uint32(page[rowOff+0x23:]),
		Info3:       binary.LittleEndian.Uint64(page[rowOff+sysObjectsInfo3Offset:]),
		Info4:       int64(binary.LittleEndian.Uint64(page[rowOff+0x2F:])),
		Payload:     payload,
		Valid:       valid,
		Name:        name,
		Type:        objType,
		Subtype:     subtype,
		TargetOwner: targetOwner,
		TargetName:  targetName,
		Location:    ddlLocation{PageNo: pageNo, SlotNo: slotNo, SlotOffset: slotOff, RowOffset: rowAbs},
	}, true
}

func isLikelyDictionaryType(objType string, subtype string) bool {
	switch objType {
	case "UR", "SCH", "DIR", "PROFILE", "SCHOBJ", "DMNOBJ", "TABOBJ", "DSYNOM":
	default:
		return false
	}
	if subtype == "" {
		return objType == "SCH" || objType == "DIR" || objType == "PROFILE" || objType == "DSYNOM"
	}
	if len(subtype) > 16 {
		return false
	}
	for _, ch := range subtype {
		if ch < 32 || ch > 126 {
			return false
		}
	}
	return true
}

func readOptionalDDLObjectSubtype(page []byte, markerOff int, decoder textDecoder, objType string) (string, int, bool) {
	if markerOff < len(page) {
		marker := page[markerOff]
		if marker >= 0x80 && marker <= 0xBF {
			value, _, ok := readDDLShortString(page, markerOff, decoder, false)
			if ok {
				_, next, _ := readDDLShortString(page, markerOff, decoder, false)
				return value, next, true
			}
		}
	}
	switch objType {
	case "SCH", "DIR", "PROFILE", "DSYNOM":
		return "", markerOff, true
	default:
		return "", markerOff, false
	}
}

func parseDDLObjectPayload(page []byte, pos int) []byte {
	if pos >= len(page) {
		return nil
	}
	marker := page[pos]
	if marker < 0x80 || marker > 0xBF {
		return nil
	}
	length := int(marker - 0x80)
	start := pos + 1
	end := start + length
	if length <= 0 || end > len(page) {
		return nil
	}
	return append([]byte(nil), page[start:end]...)
}

func parseDDLColumnRow(page []byte, rowOff int, pageNo uint32, slotNo uint16, slotOff uint16, pageSize uint32, decoder textDecoder) (columnDef, bool) {
	if rowOff+0x30 >= int(pageSize) {
		return columnDef{}, false
	}
	rowLen := binary.LittleEndian.Uint16(page[rowOff+0x01:])
	if rowLen < 0x20 || rowLen > 0x300 {
		return columnDef{}, false
	}
	nullable := ""
	if b := page[rowOff+0x11]; b == 'Y' || b == 'N' {
		nullable = string([]byte{b})
	}
	name, next, ok := readDDLShortString(page, rowOff+0x16, decoder, false)
	if !ok {
		return columnDef{}, false
	}
	typeMarkerOff := next
	dataType, next, ok := readDDLShortString(page, next, decoder, false)
	if !ok {
		return columnDef{}, false
	}
	if typeMarkerOff < len(page) && page[typeMarkerOff] >= 0x80 && page[typeMarkerOff] <= 0xBF {
		rawLength := int(page[typeMarkerOff] - 0x80)
		rawStart := typeMarkerOff + 1
		if rawStart+rawLength <= len(page) {
			dataType = repairCatalogDataType(page[rawStart:rawStart+rawLength], dataType)
		}
	}
	if !isSafeShortText(name) || !isSafeShortText(dataType) {
		return columnDef{}, false
	}
	scale := int16(binary.LittleEndian.Uint16(page[rowOff+0x0F:]))
	dataType = normalizeCatalogColumnType(dataType, scale)
	rowAbs := uint64(pageNo)*uint64(pageSize) + uint64(rowOff)
	return columnDef{
		TableID:  binary.LittleEndian.Uint32(page[rowOff+0x05:]),
		ColID:    binary.LittleEndian.Uint16(page[rowOff+0x09:]),
		Name:     name,
		DataType: dataType,
		Length:   binary.LittleEndian.Uint32(page[rowOff+0x0B:]),
		Scale:    scale,
		Nullable: nullable,
		Default:  parseDDLDefault(page, next, decoder),
		Location: ddlLocation{PageNo: pageNo, SlotNo: slotNo, SlotOffset: slotOff, RowOffset: rowAbs},
	}, true
}

func parseDDLIndexRow(page []byte, slotOff int, pageSize uint32) (indexDef, bool) {
	for delta := 0; delta < 16; delta++ {
		base := slotOff + delta
		if base+40 >= int(pageSize) {
			continue
		}
		isUnique := page[base+4]
		if isUnique != 'Y' && isUnique != 'N' {
			continue
		}
		idxType := string(page[base+13 : base+15])
		switch idxType {
		case "BT", "BM", "AR", "IF", "HS", "MP":
		default:
			continue
		}
		keyNum := binary.LittleEndian.Uint16(page[base+23:])
		if keyNum == 0 {
			// KEYINFO is nullable. DM can omit the trailing NULL value entirely
			// for a keyless table-data storage row, so there is no short-string
			// marker at base+31. Requiring 0x80 here used to drop ordinary heap
			// and HUGE $RAUX storage roots and force full-file fallbacks.
			return indexDef{
				ID:          binary.LittleEndian.Uint32(page[base:]),
				IsUnique:    string([]byte{isUnique}),
				GroupID:     binary.LittleEndian.Uint16(page[base+5:]),
				RootFile:    int16(binary.LittleEndian.Uint16(page[base+7:])),
				RootPage:    int32(binary.LittleEndian.Uint32(page[base+9:])),
				Type:        idxType,
				XType:       binary.LittleEndian.Uint32(page[base+15:]),
				Flag:        binary.LittleEndian.Uint32(page[base+19:]),
				KeyNum:      keyNum,
				InitExtents: binary.LittleEndian.Uint16(page[base+25:]),
				BatchAlloc:  binary.LittleEndian.Uint16(page[base+27:]),
				MinExtents:  binary.LittleEndian.Uint16(page[base+29:]),
			}, true
		}
		keyMarker := page[base+31]
		if keyMarker < 0x80 || keyMarker > 0xBF {
			continue
		}
		keyLen := int(keyMarker - 0x80)
		keyStart := base + 32
		keyEnd := keyStart + keyLen
		if keyEnd > int(pageSize) {
			continue
		}
		if keyNum*3 != uint16(keyLen) {
			continue
		}
		keyInfo := append([]byte(nil), page[keyStart:keyEnd]...)
		return indexDef{
			ID:          binary.LittleEndian.Uint32(page[base:]),
			IsUnique:    string([]byte{isUnique}),
			GroupID:     binary.LittleEndian.Uint16(page[base+5:]),
			RootFile:    int16(binary.LittleEndian.Uint16(page[base+7:])),
			RootPage:    int32(binary.LittleEndian.Uint32(page[base+9:])),
			Type:        idxType,
			XType:       binary.LittleEndian.Uint32(page[base+15:]),
			Flag:        binary.LittleEndian.Uint32(page[base+19:]),
			KeyNum:      keyNum,
			InitExtents: binary.LittleEndian.Uint16(page[base+25:]),
			BatchAlloc:  binary.LittleEndian.Uint16(page[base+27:]),
			MinExtents:  binary.LittleEndian.Uint16(page[base+29:]),
			KeyInfo:     keyInfo,
			Keys:        parseDDLKeyInfo(keyInfo),
		}, true
	}
	return indexDef{}, false
}

func parseDDLConstraintRow(page []byte, slotOff int, pageNo uint32, slotNo uint16, rawSlotOff uint16, pageSize uint32, decoder textDecoder) (constraintDef, bool) {
	for delta := 0; delta < 16; delta++ {
		base := slotOff + delta
		if base+0x40 >= int(pageSize) {
			continue
		}
		typ := page[base+0x0A]
		valid := page[base+0x0B]
		if !strings.ContainsRune("PCUFR", rune(typ)) {
			continue
		}
		if valid != 'Y' && valid != 'N' {
			continue
		}
		tableID := binary.LittleEndian.Uint32(page[base+0x04:])
		if tableID == 0 {
			continue
		}
		rowAbs := uint64(pageNo)*uint64(pageSize) + uint64(slotOff)
		cons := constraintDef{
			ID:        binary.LittleEndian.Uint32(page[base:]),
			TableID:   tableID,
			ColID:     int16(binary.LittleEndian.Uint16(page[base+0x08:])),
			Type:      string([]byte{typ}),
			Valid:     string([]byte{valid}),
			IndexID:   binary.LittleEndian.Uint32(page[base+0x0C:]),
			CheckInfo: parseDDLVarcharAt(page, base+0x1A, decoder),
			Location:  ddlLocation{PageNo: pageNo, SlotNo: slotNo, SlotOffset: rawSlotOff, RowOffset: rowAbs},
		}
		if typ == 'F' || typ == 'R' {
			cons.FIndexID = binary.LittleEndian.Uint32(page[base+0x10:])
			cons.FAction = strings.TrimSpace(string(page[base+0x14 : base+0x16]))
			cons.TriggerID = int32(binary.LittleEndian.Uint32(page[base+0x16:]))
		}
		return cons, true
	}
	return constraintDef{}, false
}

func parseDDLRoleGrantRow(page []byte, slotOff int, pageNo uint32, slotNo uint16, rawSlotOff uint16, pageSize uint32) (roleGrantDef, bool) {
	for delta := 0; delta < 4; delta++ {
		base := slotOff + delta
		if base+44 > int(pageSize) {
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
		if colID != -1 || privID != -1 || grantor != -1 {
			continue
		}
		if grantable != 'Y' && grantable != 'N' {
			continue
		}
		granteeID := binary.LittleEndian.Uint32(page[base+4:])
		roleID := binary.LittleEndian.Uint32(page[base+8:])
		if granteeID == 0 || roleID == 0 || granteeID == roleID {
			continue
		}
		rowAbs := uint64(pageNo)*uint64(pageSize) + uint64(base)
		return roleGrantDef{
			GranteeID:   granteeID,
			RoleID:      roleID,
			AdminOption: string([]byte{grantable}),
			Location:    ddlLocation{PageNo: pageNo, SlotNo: slotNo, SlotOffset: rawSlotOff, RowOffset: rowAbs},
		}, true
	}
	return roleGrantDef{}, false
}

func parseDDLTableCommentRow(page []byte, slotOff int, pageSize uint32, decoder textDecoder) (tableComment, bool) {
	for deltaBase := 0; deltaBase < 32; deltaBase++ {
		base := slotOff + deltaBase
		if base+16 >= int(pageSize) {
			continue
		}
		rowLen := binary.LittleEndian.Uint16(page[base+0x01:])
		if rowLen < 0x10 || rowLen > 0x1000 {
			continue
		}
		for _, delta := range []int{3, 4} {
			pos := base + delta
			values := make([]string, 0, 4)
			ok := true
			for i := 0; i < 4; i++ {
				var value string
				value, pos, ok = readDDLShortString(page, pos, decoder, true)
				if !ok {
					break
				}
				values = append(values, value)
			}
			if !ok || len(values) != 4 {
				continue
			}
			if values[2] != "TABLE" && values[2] != "VIEW" {
				continue
			}
			if values[0] == "" || values[1] == "" {
				continue
			}
			return tableComment{Owner: values[0], TableName: values[1], TableType: values[2], Comment: values[3]}, true
		}
	}
	return tableComment{}, false
}

func parseDDLColumnCommentRow(page []byte, slotOff int, pageSize uint32, decoder textDecoder) (columnComment, bool) {
	for deltaBase := 0; deltaBase < 32; deltaBase++ {
		base := slotOff + deltaBase
		if base+16 >= int(pageSize) {
			continue
		}
		rowLen := binary.LittleEndian.Uint16(page[base+0x01:])
		if rowLen < 0x10 || rowLen > 0x1000 {
			continue
		}
		for _, delta := range []int{4, 3} {
			pos := base + delta
			values := make([]string, 0, 5)
			ok := true
			for i := 0; i < 5; i++ {
				var value string
				value, pos, ok = readDDLShortString(page, pos, decoder, true)
				if !ok {
					break
				}
				values = append(values, value)
			}
			if !ok || len(values) != 5 {
				continue
			}
			if values[3] != "TABLE" && values[3] != "VIEW" {
				continue
			}
			if values[0] == "" || values[1] == "" || values[2] == "" {
				continue
			}
			return columnComment{Owner: values[0], TableName: values[1], ColumnName: values[2], TableType: values[3], Comment: values[4]}, true
		}
	}
	return columnComment{}, false
}

func readDDLShortString(page []byte, pos int, decoder textDecoder, allowEmpty bool) (string, int, bool) {
	if pos >= len(page) {
		return "", pos, false
	}
	marker := page[pos]
	if marker == 0x80 {
		return "", pos + 1, allowEmpty
	}
	if marker < 0x81 || marker > 0xBF {
		return "", pos, false
	}
	n := int(marker - 0x80)
	start := pos + 1
	end := start + n
	if end > len(page) {
		return "", pos, false
	}
	value, ok := decoder.decode(page[start:end])
	if !ok {
		return "", pos, false
	}
	return value, end, true
}

func parseDDLVarcharAt(page []byte, pos int, decoder textDecoder) string {
	if pos >= len(page) {
		return ""
	}
	marker := page[pos]
	if marker == 0x80 {
		return ""
	}
	if marker < 0x81 || marker > 0xBF {
		return ""
	}
	n := int(marker - 0x80)
	start := pos + 1
	end := start + n
	if end > len(page) {
		return ""
	}
	value, ok := decoder.decode(page[start:end])
	if !ok {
		return ""
	}
	return value
}

func parseDDLDefault(page []byte, pos int, decoder textDecoder) string {
	value := strings.TrimSpace(parseDDLVarcharAt(page, pos, decoder))
	if !isSafeDefault(value) {
		return ""
	}
	return value
}

func isSafeDefault(value string) bool {
	if value == "" || strings.EqualFold(value, "NULL") {
		return false
	}
	if len(value) > 256 {
		return false
	}
	for _, ch := range value {
		if ch == utf8.RuneError || (ch < 32 && ch != '\t') {
			return false
		}
	}
	return true
}

func parseDDLKeyInfo(keyInfo []byte) []indexKey {
	var result []indexKey
	for i := 0; i+3 <= len(keyInfo); i += 3 {
		order := fmt.Sprintf("FLAG_0x%02X", keyInfo[i+2])
		switch keyInfo[i+2] {
		case 0x41:
			order = "ASC"
		case 0x44:
			order = "DESC"
		}
		result = append(result, indexKey{
			ColID: binary.LittleEndian.Uint16(keyInfo[i:]),
			Order: order,
		})
	}
	return result
}
