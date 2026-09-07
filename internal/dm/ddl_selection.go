package dm

import (
	"sort"
	"strings"
)

func loadTablespaceNames(controlPath string, controlDULPath string) map[uint32]string {
	result := defaultTablespaceNames()
	mergeControlDULTablespaceNames(result, controlDULPath)
	if controlPath == "" {
		return result
	}
	ctl, err := InspectControlFile(controlPath)
	if err != nil {
		return result
	}
	for _, entry := range ctl.Entries {
		result[entry.ID] = entry.Name
	}
	return result
}

func defaultTablespaceNames() map[uint32]string {
	return map[uint32]string{
		0: "SYSTEM",
		1: "ROLL",
		3: "TEMP",
		4: "MAIN",
	}
}

func tableStorageByID(tables map[uint32]dictionaryObject, indexObjects map[uint32]dictionaryObject, indexes map[uint32]indexDef, tablespaces map[uint32]string) map[uint32]indexDef {
	result := make(map[uint32]indexDef)
	for indexID, obj := range indexObjects {
		tableID := uint32(obj.ParentID)
		if _, ok := tables[tableID]; !ok {
			continue
		}
		idx, ok := indexes[indexID]
		if !ok {
			continue
		}
		if idx.Flag&1 == 0 || idx.KeyNum != 0 {
			continue
		}
		// SYSOBJECTS/SYSINDEXES can retain older table-data storage objects after
		// TRUNCATE or a storage rebuild. DM9 also no longer guarantees that the
		// active storage id equals 0x02000000|table_id. Object ids are allocated
		// monotonically, so the greatest candidate id is the current storage;
		// older candidates remain available through assist ids for recover mode.
		if current, exists := result[tableID]; !exists || idx.ID > current.ID {
			result[tableID] = idx
		}
	}
	// HUGE main and transaction auxiliary tables can have SYSINDEXES storage
	// rows without a matching TABOBJ/INDEX object. Their table-data storage id
	// follows the regular 0x02000000|table_id rule, so retain that exact catalog
	// row instead of guessing a root or scanning a data file.
	for tableID, table := range tables {
		if !table.isHugeTable() {
			continue
		}
		if idx, ok := indexes[tableDataAssistID(tableID)]; ok && idx.Flag&1 != 0 && idx.KeyNum == 0 {
			result[tableID] = idx
		}
	}
	return result
}

func tableIDByOwnerName(tables map[uint32]dictionaryObject, columnsByTable map[uint32][]columnDef, matcher ownerMatcher, owner string, name string) (uint32, bool) {
	var candidates []uint32
	for id, obj := range tables {
		if obj.Owner == owner && obj.Name == name && matcher.allowed(obj.Owner) {
			candidates = append(candidates, id)
		}
	}
	if len(candidates) == 0 {
		return 0, false
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i] < candidates[j]
	})
	for _, id := range candidates {
		if len(columnsByTable[id]) > 0 {
			return id, true
		}
	}
	return candidates[0], true
}

func countAllowedTables(tables map[uint32]dictionaryObject, columnsByTable map[uint32][]columnDef, matcher ownerMatcher, tableMatcher tableNameMatcher) int {
	count := 0
	for id, table := range tables {
		if matcher.allowed(table.Owner) && tableMatcher.allowed(table.Owner, table.Name) && len(columnsByTable[id]) > 0 {
			count++
		}
	}
	return count
}

func countDDLIndexes(tables map[uint32]dictionaryObject, indexObjects map[uint32]dictionaryObject, indexes map[uint32]indexDef, matcher ownerMatcher, tableMatcher tableNameMatcher) int {
	count := 0
	for indexID, obj := range indexObjects {
		table, ok := tables[uint32(obj.ParentID)]
		if !ok || !matcher.allowed(table.Owner) || !tableMatcher.allowed(table.Owner, table.Name) {
			continue
		}
		idx, ok := indexes[indexID]
		if !ok || idx.Flag&1 != 0 || idx.KeyNum == 0 || !isRenderableUserIndexType(idx.Type) {
			continue
		}
		count++
	}
	return count
}

func isRenderableUserIndexType(indexType string) bool {
	switch strings.ToUpper(strings.TrimSpace(indexType)) {
	case "BT", "HS", "IF":
		return true
	default:
		return false
	}
}

func countExportedPartitionedTables(partitionsByTable map[uint32][]PartitionInfo, columnsByTable map[uint32][]columnDef) int {
	count := 0
	for tableID, parts := range partitionsByTable {
		if len(parts) > 0 && len(columnsByTable[tableID]) > 0 {
			count++
		}
	}
	return count
}

func countExportedPartitions(partitionsByTable map[uint32][]PartitionInfo, columnsByTable map[uint32][]columnDef) int {
	count := 0
	for tableID, parts := range partitionsByTable {
		if len(columnsByTable[tableID]) > 0 {
			count += len(parts)
		}
	}
	return count
}

func countExportedRoleGrants(roleGrants []roleGrantDef, users map[uint32]dictionaryObject, roles map[uint32]dictionaryObject, userIDs []uint32, roleIDs []uint32) int {
	return len(renderRoleGrantLines(roleGrants, users, roles, userIDs, roleIDs))
}

func exportedUserIDs(users map[uint32]dictionaryObject, matcher ownerMatcher) []uint32 {
	var ids []uint32
	for id, user := range users {
		if isBuiltInUserName(user.Name) || !matcher.allowed(user.Name) {
			continue
		}
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		a := users[ids[i]]
		b := users[ids[j]]
		if a.Name == b.Name {
			return ids[i] < ids[j]
		}
		return a.Name < b.Name
	})
	return ids
}

func exportedRoleIDs(roles map[uint32]dictionaryObject, roleGrants []roleGrantDef, users map[uint32]dictionaryObject, userIDs []uint32) []uint32 {
	userSet := idSet(userIDs)
	wanted := make(map[uint32]bool)
	for _, grant := range roleGrants {
		if !userSet[grant.GranteeID] {
			continue
		}
		role, ok := roles[grant.RoleID]
		if !ok || isBuiltInRoleName(role.Name) {
			continue
		}
		wanted[grant.RoleID] = true
	}
	var ids []uint32
	for id := range wanted {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		a := roles[ids[i]]
		b := roles[ids[j]]
		if a.Name == b.Name {
			return ids[i] < ids[j]
		}
		return a.Name < b.Name
	})
	return ids
}

func idSet(ids []uint32) map[uint32]bool {
	result := make(map[uint32]bool, len(ids))
	for _, id := range ids {
		result[id] = true
	}
	return result
}

func isRealURObject(obj dictionaryObject) bool {
	return obj.SchemaID == 0 && obj.Valid != "N" && isSafeShortText(obj.Name)
}

func isBuiltInUserName(name string) bool {
	return builtInUserNames[strings.ToUpper(strings.TrimSpace(name))]
}

func isBuiltInRoleName(name string) bool {
	return builtInRoleNames[strings.ToUpper(strings.TrimSpace(name))]
}

func filterDDLTriggersByTable(triggers []DictionaryTrigger, matcher tableNameMatcher) []DictionaryTrigger {
	if !matcher.hasRules || matcher.all {
		return triggers
	}
	filtered := make([]DictionaryTrigger, 0, len(triggers))
	for _, trigger := range triggers {
		if matcher.allowed(trigger.TableOwner, trigger.TableName) {
			filtered = append(filtered, trigger)
		}
	}
	return filtered
}

func filterDDLPrivilegesByTable(privileges []DictionaryTabPrivilege, matcher tableNameMatcher) []DictionaryTabPrivilege {
	if !matcher.hasRules || matcher.all {
		return privileges
	}
	filtered := make([]DictionaryTabPrivilege, 0, len(privileges))
	for _, privilege := range privileges {
		if matcher.allowed(privilege.Owner, privilege.ObjectName) {
			filtered = append(filtered, privilege)
		}
	}
	return filtered
}

func filterDDLPrivilegesToTables(privileges []DictionaryTabPrivilege, tables map[uint32]dictionaryObject, ownerMatcher ownerMatcher, tableMatcher tableNameMatcher) []DictionaryTabPrivilege {
	tableKeys := make(map[string]bool)
	for _, table := range tables {
		if ownerMatcher.allowed(table.Owner) && tableMatcher.allowed(table.Owner, table.Name) {
			tableKeys[qualifiedObjectKey(table.Owner, table.Name)] = true
		}
	}
	filtered := make([]DictionaryTabPrivilege, 0, len(privileges))
	for _, privilege := range privileges {
		if tableKeys[qualifiedObjectKey(privilege.Owner, privilege.ObjectName)] {
			filtered = append(filtered, privilege)
		}
	}
	return filtered
}

func userDefaultTablespaceName(user dictionaryObject, tablespaces map[uint32]string) string {
	groupID := uint32(user.Info3 & 0xFFFF)
	if groupID == 0 {
		groupID = 0
	}
	return tablespaces[groupID]
}
