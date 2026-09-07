package dm

import (
	"strconv"
	"strings"
)

func enrichColumnPrivilege(item *DictionaryTabPrivilege, grant dictionaryObjectPrivilegeDef, columns map[uint32][]columnDef, names map[uint32]string) {
	item.Grantor = names[grant.GrantorID]
	if grant.ColumnID < 0 {
		return
	}
	id := uint16(grant.ColumnID)
	item.ColumnID = &id
	for _, column := range columns[grant.ObjectID] {
		if column.ColID == id {
			item.ColumnName = column.Name
			return
		}
	}
}

func dictionaryPrivilegeKey(item DictionaryTabPrivilege) string {
	colID := ""
	if item.ColumnID != nil {
		colID = strconv.Itoa(int(*item.ColumnID))
	}
	return strings.Join([]string{item.Grantee, item.Owner, item.ObjectName, item.Privilege,
		item.Grantable, colID, item.ColumnName, item.Grantor}, "\x00")
}
