package dm

import (
	"encoding/binary"
	"sort"
)

func (s *systemPageStream) dictionaryObjects(decoder textDecoder) (map[uint32]dictionaryObject, error) {
	objects := make(map[uint32]dictionaryObject)
	err := s.forEachDictionaryRow(func(page []byte, pageNo uint32, slotNo uint16, slotOff uint16) {
		obj, ok := parseDDLObjectRow(page, int(slotOff), pageNo, slotNo, slotOff, s.pageSize, decoder)
		if !ok {
			return
		}
		if _, exists := objects[obj.ID]; !exists {
			objects[obj.ID] = obj
		}
	})
	return objects, err
}

func (s *systemPageStream) dictionaryTexts(decoder textDecoder) (map[uint32]map[uint32]string, error) {
	result := make(map[uint32]map[uint32]string)
	err := s.forEachDictionaryRow(func(page []byte, pageNo uint32, slotNo uint16, slotOff uint16) {
		row, ok := parseDDLTextRow(page, int(slotOff), pageNo, slotNo, slotOff, s.pageSize, decoder)
		if !ok {
			return
		}
		seqs := result[row.ID]
		if seqs == nil {
			seqs = make(map[uint32]string)
			result[row.ID] = seqs
		}
		if existing := seqs[row.SeqNo]; len(row.Text) > len(existing) {
			seqs[row.SeqNo] = row.Text
		}
	})
	return result, err
}

func (s *systemPageStream) partitionsByTable(decoder textDecoder, tables map[uint32]dictionaryObject, matcher ownerMatcher) (map[uint32][]PartitionInfo, error) {
	result := make(map[uint32][]PartitionInfo)
	err := s.forEachDictionarySlotRange(func(page []byte, pageNo uint32, slotNo uint16, slotOff uint16, nextOff uint16) {
		for rowOff := int(slotOff); rowOff+sysHPartTypeOffset+2 < int(nextOff); rowOff++ {
			part, ok := parseDDLPartitionRowAt(page, rowOff, pageNo, slotNo, slotOff, s.pageSize, decoder)
			if !ok {
				continue
			}
			table, ok := tables[part.BaseTableID]
			if !ok || !matcher.allowed(table.Owner) {
				continue
			}
			result[part.BaseTableID] = append(result[part.BaseTableID], PartitionInfo{
				BaseTableID:      part.BaseTableID,
				PartTableID:      part.PartTableID,
				Type:             part.Type,
				Name:             part.Name,
				HighValue:        append([]byte(nil), part.HighValue...),
				HighValuePreview: partitionHighValuePreview(part.HighValue),
				HighValueHex:     partitionHighValueHex(part.HighValue, 64),
				PageNo:           part.Location.PageNo,
				SlotNo:           part.Location.SlotNo,
				SlotOffset:       part.Location.SlotOffset,
				RowOffset:        part.Location.RowOffset,
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
	return result, err
}

func (s *systemPageStream) partitionKeysByTable(decoder textDecoder, tables map[uint32]dictionaryObject, matcher ownerMatcher) (map[uint32][]uint16, error) {
	result := make(map[uint32][]uint16)
	err := s.forEachDictionaryRow(func(page []byte, _ uint32, _ uint16, slotOff uint16) {
		tableID, colIDs, ok := parseTabPartInfoRow(page, int(slotOff), s.pageSize, decoder)
		if !ok {
			return
		}
		table, ok := tables[tableID]
		if ok && matcher.allowed(table.Owner) && len(colIDs) > 0 {
			result[tableID] = colIDs
		}
	})
	return result, err
}

func (s *systemPageStream) tabPrivileges(objects map[uint32]dictionaryObject, users map[uint32]dictionaryObject, roles map[uint32]dictionaryObject, columns map[uint32][]columnDef, matcher ownerMatcher, tableMatcher tableNameMatcher) ([]DictionaryTabPrivilege, error) {
	granteeNames := make(map[uint32]string)
	for id, user := range users {
		granteeNames[id] = user.Name
	}
	for id, role := range roles {
		granteeNames[id] = role.Name
	}
	seen := make(map[string]bool)
	var privileges []DictionaryTabPrivilege
	err := s.forEachDictionaryRow(func(page []byte, pageNo uint32, slotNo uint16, slotOff uint16) {
		grant, ok := parseDDLObjectPrivilegeRow(page, int(slotOff), pageNo, slotNo, slotOff, s.pageSize)
		if !ok {
			return
		}
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
	sortDictionaryTabPrivileges(privileges)
	return privileges, err
}
