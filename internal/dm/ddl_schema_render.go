package dm

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

func ensureSQLTerminator(sql string) string {
	sql = cleanRecoveredSQLText(sql)
	if strings.HasSuffix(sql, ";") {
		return sql
	}
	return sql + ";"
}

// ensurePLSQLBlockTerminator ends a PL/SQL object body (trigger, procedure,
// function, package) with a "/" batch terminator on its own line. In disql the
// trailing ";" belongs to the block body, so without "/" every statement that
// follows is silently appended to the block buffer instead of being executed.
// DMP metadata records are executed per record by dimp and must NOT carry "/".
func ensurePLSQLBlockTerminator(sql string) string {
	return ensureSQLTerminator(sql) + "\n/"
}

func cleanRecoveredSQLText(sql string) string {
	sql = strings.TrimSpace(sql)
	for i, ch := range sql {
		if ch == utf8.RuneError || (ch < 32 && ch != '\t' && ch != '\n' && ch != '\r') {
			sql = sql[:i]
			break
		}
	}
	return strings.TrimSpace(sql)
}

func renderIndexes(out *strings.Builder, tables map[uint32]dictionaryObject, columnsByTableColID map[tableColKey]columnDef, indexObjects map[uint32]dictionaryObject, indexes map[uint32]indexDef, matcher ownerMatcher, tableMatcher tableNameMatcher, tablespaces map[uint32]string) {
	out.WriteString("-- Indexes\n")
	var indexIDs []uint32
	for id := range indexObjects {
		indexIDs = append(indexIDs, id)
	}
	sort.Slice(indexIDs, func(i, j int) bool {
		a := indexObjects[indexIDs[i]]
		b := indexObjects[indexIDs[j]]
		if a.Owner == b.Owner {
			return a.Name < b.Name
		}
		return a.Owner < b.Owner
	})
	for _, indexID := range indexIDs {
		obj := indexObjects[indexID]
		table, ok := tables[uint32(obj.ParentID)]
		if !ok || !matcher.allowed(table.Owner) || !tableMatcher.allowed(table.Owner, table.Name) {
			continue
		}
		idx, ok := indexes[indexID]
		if !ok || idx.Flag&1 != 0 || idx.KeyNum == 0 || !isRenderableUserIndexType(idx.Type) {
			continue
		}
		cols := ddlColumns(columnsFromIndex(indexID, uint32(obj.ParentID), indexes, columnsByTableColID), true)
		if cols == "" {
			continue
		}
		if sql, ok := renderCreateIndexSQL(obj, table, idx, cols, tablespaces); ok {
			out.WriteString(sql)
			out.WriteByte('\n')
		}
	}
	out.WriteString("\n")
}

func renderCreateIndexSQL(obj dictionaryObject, table dictionaryObject, idx indexDef, columns string, tablespaces map[uint32]string) (string, bool) {
	switch strings.ToUpper(strings.TrimSpace(idx.Type)) {
	case "BT":
		unique := ""
		if idx.IsUnique == "Y" {
			unique = "UNIQUE "
		}
		return fmt.Sprintf("CREATE %sINDEX %s ON %s.%s (%s)%s;",
			unique, quoteIdent(obj.Name), quoteIdent(table.Owner), quoteIdent(table.Name), columns,
			storageClause(uint32(idx.GroupID), tablespaces, defaultStorageOrg)), true
	case "HS":
		return fmt.Sprintf("CREATE VECTOR INDEX %s ON %s.%s (%s) ORGANIZATION NEIGHBOR GRAPH;",
			quoteIdent(obj.Name), quoteIdent(table.Owner), quoteIdent(table.Name), columns), true
	case "IF":
		return fmt.Sprintf("CREATE VECTOR INDEX %s ON %s.%s (%s) ORGANIZATION NEIGHBOR PARTITIONS;",
			quoteIdent(obj.Name), quoteIdent(table.Owner), quoteIdent(table.Name), columns), true
	default:
		return "", false
	}
}

func renderConstraints(out *strings.Builder, objects map[uint32]dictionaryObject, tables map[uint32]dictionaryObject, columnsByTableColID map[tableColKey]columnDef, constraintObjects map[uint32]dictionaryObject, indexes map[uint32]indexDef, constraints []constraintDef, matcher ownerMatcher, tableMatcher tableNameMatcher) {
	out.WriteString("-- Constraints\n")
	ddlOrder := map[string]int{"P": 1, "U": 2, "C": 3, "F": 4, "R": 5}
	sort.Slice(constraints, func(i, j int) bool {
		oi := ddlOrder[constraints[i].Type]
		oj := ddlOrder[constraints[j].Type]
		if oi == oj {
			ti := tables[constraints[i].TableID]
			tj := tables[constraints[j].TableID]
			if ti.Owner != tj.Owner {
				return ti.Owner < tj.Owner
			}
			if ti.Name != tj.Name {
				return ti.Name < tj.Name
			}
			return constraints[i].ID < constraints[j].ID
		}
		return oi < oj
	})
	for _, cons := range constraints {
		if cons.Valid != "Y" {
			continue
		}
		table, ok := tables[cons.TableID]
		if !ok || !matcher.allowed(table.Owner) || !tableMatcher.allowed(table.Owner, table.Name) {
			continue
		}
		consObj, ok := constraintObjects[cons.ID]
		if !ok {
			continue
		}
		owner := quoteIdent(table.Owner)
		tableName := quoteIdent(table.Name)
		consName := recoveredConstraintNameClause(consObj.Name)
		switch cons.Type {
		case "P", "U":
			cols := ddlColumns(columnsFromIndex(cons.IndexID, cons.TableID, indexes, columnsByTableColID), false)
			if cols == "" {
				continue
			}
			kind := "PRIMARY KEY"
			if cons.Type == "U" {
				kind = "UNIQUE"
			}
			out.WriteString(fmt.Sprintf("ALTER TABLE %s.%s ADD %s%s (%s);\n", owner, tableName, consName, kind, cols))
		case "C":
			if cons.CheckInfo == "" {
				continue
			}
			out.WriteString(fmt.Sprintf("ALTER TABLE %s.%s ADD %sCHECK (%s);\n", owner, tableName, consName, cons.CheckInfo))
		case "F":
			childCols := ddlColumns(columnsFromIndex(cons.IndexID, cons.TableID, indexes, columnsByTableColID), false)
			parentIndexObj, ok := objects[cons.FIndexID]
			if !ok || childCols == "" {
				continue
			}
			parentTable, ok := tables[uint32(parentIndexObj.ParentID)]
			if !ok {
				continue
			}
			parentCols := ddlColumns(columnsFromIndex(cons.FIndexID, uint32(parentIndexObj.ParentID), indexes, columnsByTableColID), false)
			if parentCols == "" {
				continue
			}
			out.WriteString(fmt.Sprintf("ALTER TABLE %s.%s ADD %sFOREIGN KEY (%s) REFERENCES %s.%s (%s);\n",
				owner, tableName, consName, childCols, quoteIdent(parentTable.Owner), quoteIdent(parentTable.Name), parentCols))
		}
	}
	out.WriteString("\n")
}

func recoveredConstraintNameClause(name string) string {
	name = strings.TrimSpace(name)
	if name == "" || isDMGeneratedConstraintName(name) {
		return ""
	}
	return "CONSTRAINT " + quoteIdent(name) + " "
}

func isDMGeneratedConstraintName(name string) bool {
	upper := strings.ToUpper(strings.TrimSpace(name))
	if len(upper) <= len("CONS") || !strings.HasPrefix(upper, "CONS") {
		return false
	}
	for _, ch := range upper[len("CONS"):] {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return true
}

func renderComments(out *strings.Builder, tables map[uint32]dictionaryObject, columnsByTable map[uint32][]columnDef, tableComments map[ownerTableKey]tableComment, columnComments map[ownerTableColumnKey]columnComment, matcher ownerMatcher, tableMatcher tableNameMatcher) {
	out.WriteString("-- Comments\n")
	tableIDs := sortedTableIDs(tables)
	wroteTableComment := false
	for _, tableID := range tableIDs {
		table := tables[tableID]
		if !matcher.allowed(table.Owner) || !tableMatcher.allowed(table.Owner, table.Name) || len(columnsByTable[tableID]) == 0 {
			continue
		}
		key := ownerTableKey{owner: table.Owner, table: table.Name}
		comment, ok := tableComments[key]
		if !ok {
			continue
		}
		out.WriteString(fmt.Sprintf("COMMENT ON TABLE %s.%s IS %s;\n", quoteIdent(comment.Owner), quoteIdent(comment.TableName), sqlLiteral(comment.Comment)))
		wroteTableComment = true
	}

	if wroteTableComment && len(columnComments) > 0 {
		out.WriteString("\n")
	}

	for _, tableID := range tableIDs {
		table := tables[tableID]
		if !matcher.allowed(table.Owner) || !tableMatcher.allowed(table.Owner, table.Name) || len(columnsByTable[tableID]) == 0 {
			continue
		}
		for _, col := range columnsByTable[tableID] {
			key := ownerTableColumnKey{owner: table.Owner, table: table.Name, column: col.Name}
			comment, ok := columnComments[key]
			if !ok {
				continue
			}
			out.WriteString(fmt.Sprintf("COMMENT ON COLUMN %s.%s.%s IS %s;\n", quoteIdent(comment.Owner), quoteIdent(comment.TableName), quoteIdent(comment.ColumnName), sqlLiteral(comment.Comment)))
		}
	}
}

func sortedTableIDs(tables map[uint32]dictionaryObject) []uint32 {
	ids := make([]uint32, 0, len(tables))
	for id := range tables {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		a := tables[ids[i]]
		b := tables[ids[j]]
		if a.Owner == b.Owner {
			if a.Name == b.Name {
				return ids[i] < ids[j]
			}
			return a.Name < b.Name
		}
		return a.Owner < b.Owner
	})
	return ids
}

func columnsFromIndex(indexID uint32, tableID uint32, indexes map[uint32]indexDef, columns map[tableColKey]columnDef) []namedIndexKey {
	idx, ok := indexes[indexID]
	if !ok {
		return nil
	}
	var result []namedIndexKey
	for _, key := range idx.Keys {
		name := fmt.Sprintf("COLID_%d", key.ColID)
		if col, ok := columns[tableColKey{tableID: tableID, colID: key.ColID}]; ok {
			name = col.Name
		}
		result = append(result, namedIndexKey{Name: name, Order: key.Order})
	}
	return result
}

type namedIndexKey struct {
	Name  string
	Order string
}

func ddlColumns(keys []namedIndexKey, includeOrder bool) string {
	var parts []string
	for _, key := range keys {
		if key.Name == "" {
			continue
		}
		part := quoteIdent(key.Name)
		if includeOrder && key.Order != "" && key.Order != "ASC" {
			part += " /* " + key.Order + " */"
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, ", ")
}

func quoteIdent(name string) string {
	if regularIdentifierPattern.MatchString(name) && !reservedIdentifierNames[strings.ToUpper(name)] {
		return name
	}
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func quoteStorageName(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func sqlLiteral(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func passwordLiteral(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}

func storageClause(groupID uint32, tablespaces map[uint32]string, organization string) string {
	name := tablespaces[groupID]
	if name == "" {
		return ""
	}
	if organization == "" {
		return fmt.Sprintf("\nSTORAGE(ON %s)", quoteStorageName(name))
	}
	return fmt.Sprintf("\nSTORAGE(ON %s, %s)", quoteStorageName(name), organization)
}

func formatColumnType(dataType string, length uint32, scale int16) string {
	dt := strings.TrimSpace(dataType)
	upper := normalizeDataType(dt)
	charTypes := map[string]bool{
		"CHAR": true, "CHARACTER": true, "VARCHAR": true, "VARCHAR2": true,
		"NCHAR": true, "NVARCHAR": true, "NVARCHAR2": true, "VARCHARACTER": true,
		"CHARACTER VARYING": true, "NATIONAL CHAR": true, "NATIONAL CHARACTER": true,
		"NATIONAL CHAR VARYING": true, "NATIONAL CHARACTER VARYING": true, "NCHAR VARYING": true,
	}
	binaryTypes := map[string]bool{"BINARY": true, "VARBINARY": true, "RAW": true}
	noLengthTypes := map[string]bool{
		"INT": true, "INTEGER": true, "PLS_INTEGER": true, "BIGINT": true, "SMALLINT": true, "TINYINT": true, "BYTE": true,
		"DATE": true,
		"TEXT": true, "LONGVARCHAR": true, "LONGVARBINARY": true, "LONG RAW": true, "CLOB": true, "NCLOB": true, "BLOB": true, "IMAGE": true,
		"BFILE": true, "JSON": true, "JSONB": true,
		"XMLTYPE": true,
		"BIT":     true, "BOOLEAN": true, "BOOL": true,
		"REAL": true, "BINARY_FLOAT": true, "FLOAT": true, "DOUBLE": true, "DOUBLE PRECISION": true, "BINARY_DOUBLE": true,
		"ROWID": true,
	}
	timePrecisionTypes := map[string]bool{
		"DATETIME": true, "TIME": true, "TIMESTAMP": true,
		"DATETIME WITH TIME ZONE": true, "TIME WITH TIME ZONE": true,
		"TIMESTAMP WITH TIME ZONE": true, "TIMESTAMP WITH LOCAL TIME ZONE": true,
	}
	numberTypes := map[string]bool{"NUMBER": true, "NUMERIC": true, "DEC": true, "DECIMAL": true}
	switch {
	case upper == "VECTOR":
		return formatVectorColumnType(dt, length, scale)
	case isYearMonthIntervalDataType(upper) || isDayTimeIntervalDataType(upper):
		formatted, _ := formatIntervalColumnType(upper, scale)
		return formatted
	case charTypes[upper]:
		if length > 0 {
			return fmt.Sprintf("%s(%d)", dt, length)
		}
		return dt
	case binaryTypes[upper]:
		if length > 0 {
			return fmt.Sprintf("%s(%d)", dt, length)
		}
		return dt
	case noLengthTypes[upper]:
		return dt
	case timePrecisionTypes[upper]:
		precision := timeFractionalPrecision(scale)
		if precision > 0 && precision <= 9 {
			if idx := strings.Index(strings.ToUpper(dt), " WITH "); idx >= 0 {
				base := strings.TrimSpace(dt[:idx])
				suffix := strings.TrimSpace(dt[idx+6:])
				return fmt.Sprintf("%s(%d) WITH %s", base, precision, suffix)
			}
			return fmt.Sprintf("%s(%d)", dt, precision)
		}
		return dt
	case numberTypes[upper]:
		if length > 0 && !(length == 38 && scale == 0) {
			if scale > 0 {
				return fmt.Sprintf("%s(%d,%d)", dt, length, scale)
			}
			return fmt.Sprintf("%s(%d)", dt, length)
		}
		return dt
	default:
		if length > 0 && length != 4 && length != 8 {
			return fmt.Sprintf("%s(%d)", dt, length)
		}
		return dt
	}
}

func formatVectorColumnType(dataType string, dimension uint32, scale int16) string {
	if dimension == 0 {
		return dataType
	}
	flags := uint16(scale)
	element := ""
	switch flags & 0x0F00 {
	case 0x0100:
		element = "FLOAT32"
	case 0x0200:
		element = "FLOAT64"
	case 0x0300:
		element = "INT8"
	case 0x0400:
		element = "BINARY"
	}
	parts := []string{strconv.FormatUint(uint64(dimension), 10)}
	if element != "" {
		parts = append(parts, element)
	}
	if flags&0x1000 != 0 {
		if element == "" {
			parts = append(parts, "FLOAT32")
		}
		parts = append(parts, "SPARSE")
	}
	return dataType + "(" + strings.Join(parts, ", ") + ")"
}
