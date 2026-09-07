package dm

import (
	"encoding/binary"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

type DictionarySystemPrivilege struct {
	Grantee     string
	PrivilegeID int32
	Privilege   string
	AdminOption string
}

func parseSystemPrivilegeRow(page []byte, offset int, pageSize uint32, names map[uint32]string) (DictionarySystemPrivilege, bool) {
	for delta := 0; delta < 4; delta++ {
		start := offset + delta
		if start < 0 || start+44 > len(page) || uint64(start+44) > uint64(pageSize) {
			continue
		}
		row := page[start : start+44]
		if row[0]&0x80 != 0 || binary.LittleEndian.Uint16(row[1:]) != 44 || binary.LittleEndian.Uint32(row[8:]) != ^uint32(0) || binary.LittleEndian.Uint32(row[12:]) != ^uint32(0) {
			continue
		}
		id := int32(binary.LittleEndian.Uint32(row[16:]))
		grantee := names[binary.LittleEndian.Uint32(row[4:])]
		if grantee == "" || id < 4096 || id >= 8192 || (row[24] != 'Y' && row[24] != 'N') {
			continue
		}
		return DictionarySystemPrivilege{Grantee: grantee, PrivilegeID: id, Privilege: systemPrivilegeNames[id], AdminOption: string(row[24:25])}, true
	}
	return DictionarySystemPrivilege{}, false
}

func scanSystemPrivileges(stream *systemPageStream, catalog *standardBootstrapCatalog, users, roles map[uint32]dictionaryObject) ([]DictionarySystemPrivilege, error) {
	names := make(map[uint32]string)
	for id, user := range users {
		names[id] = user.Name
	}
	for id, role := range roles {
		names[id] = role.Name
	}
	result := make([]DictionarySystemPrivilege, 0)
	seen := make(map[string]bool)
	visit := func(page []byte, _ uint32, _ uint16, off uint16) {
		priv, ok := parseSystemPrivilegeRow(page, int(off), stream.pageSize, names)
		if !ok {
			return
		}
		key := priv.Grantee + "\x00" + strconv.Itoa(int(priv.PrivilegeID)) + "\x00" + priv.AdminOption
		if !seen[key] {
			seen[key] = true
			result = append(result, priv)
		}
	}
	used := false
	var err error
	if catalog != nil {
		used, err = catalog.forEachTableRow("SYSGRANTS", visit)
	}
	if err != nil {
		return nil, err
	}
	if !used {
		err = stream.forEachDictionaryRow(visit)
	}
	sortSystemPrivileges(result)
	return result, err
}

func sortSystemPrivileges(items []DictionarySystemPrivilege) {
	sort.Slice(items, func(i, j int) bool {
		if items[i].Grantee != items[j].Grantee {
			return items[i].Grantee < items[j].Grantee
		}
		if items[i].PrivilegeID != items[j].PrivilegeID {
			return items[i].PrivilegeID < items[j].PrivilegeID
		}
		return items[i].AdminOption < items[j].AdminOption
	})
}

func systemPrivilegeSQL(priv DictionarySystemPrivilege) (string, bool) {
	// TSV names are evidence, not arbitrary SQL fragments. Unknown/mismatched
	// IDs are retained but must never be replaced by a broader privilege.
	name := systemPrivilegeNames[priv.PrivilegeID]
	if name == "" || priv.Privilege != name || priv.Grantee == "" || (priv.AdminOption != "Y" && priv.AdminOption != "N") {
		return "", false
	}
	sql := fmt.Sprintf("GRANT %s TO %s", name, quoteIdent(priv.Grantee))
	if priv.AdminOption == "Y" {
		sql += " WITH ADMIN OPTION"
	}
	return sql + ";", true
}

func renderSystemPrivileges(out *strings.Builder, privileges []DictionarySystemPrivilege) {
	if len(privileges) == 0 {
		return
	}
	out.WriteString("-- System privileges recovered from SYSGRANTS\n")
	for _, priv := range privileges {
		if sql, ok := systemPrivilegeSQL(priv); ok {
			out.WriteString(sql + "\n")
		} else {
			fmt.Fprintf(out, "-- WARNING: unresolved system privilege id=%d grantee=%s; no GRANT generated.\n", priv.PrivilegeID, strconv.Quote(priv.Grantee))
		}
	}
	out.WriteByte('\n')
}

func exportedRoleIDsForScope(roles map[uint32]dictionaryObject, grants []roleGrantDef, users map[uint32]dictionaryObject, userIDs []uint32, matcher ownerMatcher) []uint32 {
	if !matcher.allUser {
		return exportedRoleIDs(roles, grants, users, userIDs)
	}
	ids := make([]uint32, 0, len(roles))
	for id, role := range roles {
		if !isBuiltInRoleName(role.Name) {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool {
		if roles[ids[i]].Name == roles[ids[j]].Name {
			return ids[i] < ids[j]
		}
		return roles[ids[i]].Name < roles[ids[j]].Name
	})
	return ids
}

func selectSystemPrivileges(privileges []DictionarySystemPrivilege, users, roles map[uint32]dictionaryObject, grants []roleGrantDef, matcher ownerMatcher) []DictionarySystemPrivilege {
	allowed := make(map[string]bool)
	for _, user := range users {
		if matcher.allowed(user.Name) {
			allowed[user.Name] = true
		}
	}
	for _, id := range exportedRoleIDsForScope(roles, grants, users, exportedUserIDs(users, matcher), matcher) {
		allowed[roles[id].Name] = true
	}
	result := make([]DictionarySystemPrivilege, 0)
	for _, priv := range privileges {
		// Built-in accounts/roles keep the target instance's own security policy.
		if allowed[priv.Grantee] && !isBuiltInUserName(priv.Grantee) && !isBuiltInRoleName(priv.Grantee) {
			result = append(result, priv)
		}
	}
	return result
}

func writeDictionarySystemPrivileges(path string, privileges []DictionarySystemPrivilege) error {
	rows := make([][]string, 0, len(privileges))
	for _, p := range privileges {
		rows = append(rows, []string{p.Grantee, strconv.Itoa(int(p.PrivilegeID)), p.Privilege, p.AdminOption})
	}
	return writeTSV(path, []string{"grantee", "privilege_id", "privilege", "admin_option"}, rows)
}

func readDictionarySystemPrivileges(path string) ([]DictionarySystemPrivilege, error) {
	rows, err := readTSV(path)
	if err != nil {
		return nil, err
	}
	result := make([]DictionarySystemPrivilege, 0, len(rows))
	for i, row := range rows {
		if i == 0 && len(row) > 0 && row[0] == "grantee" {
			continue
		}
		if len(row) != 4 || row[0] == "" || (row[3] != "Y" && row[3] != "N") {
			return nil, fmt.Errorf("sys_privs.tsv row %d: invalid fields", i+1)
		}
		id, err := strconv.ParseInt(row[1], 10, 32)
		if err != nil || id < 4096 || id >= 8192 {
			return nil, fmt.Errorf("sys_privs.tsv row %d: invalid privilege_id", i+1)
		}
		result = append(result, DictionarySystemPrivilege{Grantee: row[0], PrivilegeID: int32(id), Privilege: row[2], AdminOption: row[3]})
	}
	sortSystemPrivileges(result)
	return result, nil
}
