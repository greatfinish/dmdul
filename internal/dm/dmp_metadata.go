package dm

import (
	"fmt"
	"sort"
	"strings"
)

const (
	dmpRecordSchema          uint16 = 1
	dmpRecordIndex           uint16 = 3
	dmpRecordTrigger         uint16 = 4
	dmpRecordRoutine         uint16 = 5
	dmpRecordView            uint16 = 6
	dmpRecordSequence        uint16 = 7
	dmpRecordSynonym         uint16 = 8
	dmpRecordUser            uint16 = 9
	dmpRecordRole            uint16 = 10
	dmpRecordSystemPrivilege uint16 = 11
	dmpRecordRoleGrant       uint16 = 12
	dmpRecordTable           uint16 = 13
	dmpRecordRowMarker       uint16 = 14
	dmpRecordConstraint      uint16 = 15
	dmpRecordSchemaGrant     uint16 = 16
	dmpRecordColumnGrant     uint16 = 17
	dmpRecordPackage         uint16 = 18
	dmpRecordObjectGrant     uint16 = 20
	dmpRecordPackageBody     uint16 = 23
	dmpRecordUnique          uint16 = 30
	dmpRecordTableComment    uint16 = 31
	dmpRecordColumnComment   uint16 = 32
	dmpRecordBuiltinGrant    uint16 = 37
)

// DMPMetadataCatalog is the logical object directory written ahead of table
// data phases. It intentionally contains recovered SQL rather than opaque
// native catalog rows, so every record can also be audited in the companion
// SQL output before dimp is used.
type DMPMetadataCatalog struct {
	Mode          DMPExportMode
	GlobalRecords []DMPMetadataRecord
	Schemas       []DMPSchemaMetadata
}

type DMPMetadataCounts struct {
	Users            int
	Roles            int
	RoleGrants       int
	Tables           int
	Indexes          int
	Constraints      int
	Views            int
	Sequences        int
	Routines         int
	Triggers         int
	Synonyms         int
	Privileges       int
	SystemPrivileges int
	TableComments    int
	ColumnComments   int
}

func (catalog *DMPMetadataCatalog) Counts() DMPMetadataCounts {
	var counts DMPMetadataCounts
	if catalog == nil {
		return counts
	}
	visit := func(record DMPMetadataRecord) {
		switch record.RecordType {
		case dmpRecordUser:
			counts.Users++
		case dmpRecordRole:
			counts.Roles++
		case dmpRecordSystemPrivilege:
			counts.SystemPrivileges++
		case dmpRecordRoleGrant, dmpRecordBuiltinGrant:
			counts.RoleGrants++
		case dmpRecordTable:
			counts.Tables++
		case dmpRecordIndex:
			counts.Indexes++
		case dmpRecordConstraint, dmpRecordUnique:
			counts.Constraints++
		case dmpRecordView:
			counts.Views++
		case dmpRecordSequence:
			counts.Sequences++
		case dmpRecordRoutine, dmpRecordPackage, dmpRecordPackageBody:
			counts.Routines++
		case dmpRecordTrigger:
			counts.Triggers++
		case dmpRecordSynonym:
			counts.Synonyms++
		case dmpRecordSchemaGrant, dmpRecordObjectGrant, dmpRecordColumnGrant:
			counts.Privileges++
		case dmpRecordTableComment:
			counts.TableComments++
		case dmpRecordColumnComment:
			counts.ColumnComments++
		}
	}
	for _, record := range catalog.GlobalRecords {
		visit(record)
	}
	for _, schema := range catalog.Schemas {
		for _, record := range schema.Records {
			visit(record)
		}
		for _, table := range schema.Tables {
			for _, record := range table.Records {
				visit(record)
			}
		}
	}
	return counts
}

type DMPSchemaMetadata struct {
	Name    string
	Owner   string
	Records []DMPMetadataRecord
	Tables  []DMPTableMetadata
}

type DMPTableMetadata struct {
	ID          uint32
	Schema      string
	Owner       string
	Name        string
	ColumnCount uint16
	Records     []DMPMetadataRecord
}

type DMPMetadataRecord struct {
	RecordType uint16
	Name       string
	SQL        string
	TableName  string
	ExtraValue uint32
	Grant      *DMPObjectGrant
}

type DMPObjectGrant struct {
	Grantor    string
	Grantee    string
	Privilege  string
	Owner      string
	ObjectName string
	ObjectType string
	Grantable  string
	ColumnName string
}

func buildDMPMetadataCatalog(
	mode DMPExportMode,
	dict *DictionaryInfo,
	objects map[uint32]dictionaryObject,
	users map[uint32]dictionaryObject,
	roles map[uint32]dictionaryObject,
	roleGrants []roleGrantDef,
	tables map[uint32]dictionaryObject,
	columnsByTable map[uint32][]columnDef,
	columnsByTableColID map[tableColKey]columnDef,
	indexObjects map[uint32]dictionaryObject,
	indexes map[uint32]indexDef,
	tableStorage map[uint32]indexDef,
	partitionsByTable map[uint32][]PartitionInfo,
	partitionKeysByTable map[uint32][]uint16,
	constraintObjects map[uint32]dictionaryObject,
	constraints []constraintDef,
	tableComments map[ownerTableKey]tableComment,
	columnComments map[ownerTableColumnKey]columnComment,
	views []DictionaryView,
	sequences []DictionarySequence,
	routines []DictionaryRoutine,
	triggers []DictionaryTrigger,
	synonyms []DictionarySynonym,
	privileges []DictionaryTabPrivilege,
	systemPrivileges []DictionarySystemPrivilege,
	matcher ownerMatcher,
	tableMatcher tableNameMatcher,
	tablespaces map[uint32]string,
) (*DMPMetadataCatalog, error) {
	if mode != DMPModeFull && mode != DMPModeOwner && mode != DMPModeSchemas && mode != DMPModeTables {
		return nil, fmt.Errorf("unsupported dmp export mode %s", mode)
	}
	catalog := &DMPMetadataCatalog{Mode: mode}

	schemaOwners := dmpSchemaOwnerMap(objects, users, dict)
	schemaNames := dmpSchemaNameMap(objects, dict)
	schemas := make(map[string]*DMPSchemaMetadata)
	ensureSchema := func(name string) *DMPSchemaMetadata {
		name = strings.TrimSpace(name)
		key := strings.ToUpper(name)
		if existing := schemas[key]; existing != nil {
			return existing
		}
		owner := schemaOwners[key]
		if owner == "" {
			owner = name
		}
		schema := &DMPSchemaMetadata{Name: name, Owner: owner}
		schemas[key] = schema
		return schema
	}

	if mode != DMPModeTables {
		for key := range schemaOwners {
			name := defaultIfEmpty(schemaNames[key], key)
			if matcher.allowed(name) {
				ensureSchema(name)
			}
		}
	}

	if mode == DMPModeFull || mode == DMPModeOwner {
		userIDs := exportedUserIDs(users, matcher)
		grantSQL := make(map[string]bool)
		for _, userID := range userIDs {
			user := users[userID]
			if isBuiltInUserName(user.Name) {
				continue
			}
			catalog.GlobalRecords = append(catalog.GlobalRecords,
				DMPMetadataRecord{RecordType: dmpRecordUser, Name: user.Name, SQL: renderCreateUser(user, tablespaces)},
				DMPMetadataRecord{RecordType: dmpRecordBuiltinGrant, Name: "PUBLIC", SQL: fmt.Sprintf("GRANT %s TO %s;", quoteIdent("PUBLIC"), quoteIdent(user.Name))},
			)
			grantSQL[strings.ToUpper(strings.TrimSpace(fmt.Sprintf("GRANT %s TO %s;", quoteIdent("PUBLIC"), quoteIdent(user.Name))))] = true
			ensureSchema(user.Name)
		}
		roleIDs := exportedRoleIDsForScope(roles, roleGrants, users, userIDs, matcher)
		for _, roleID := range roleIDs {
			role := roles[roleID]
			if isBuiltInRoleName(role.Name) {
				continue
			}
			catalog.GlobalRecords = append(catalog.GlobalRecords, DMPMetadataRecord{
				RecordType: dmpRecordRole, Name: role.Name,
				SQL: fmt.Sprintf("CREATE ROLE %s;", quoteIdent(role.Name)),
			})
		}
		for _, priv := range systemPrivileges {
			if sql, ok := systemPrivilegeSQL(priv); ok {
				catalog.GlobalRecords = append(catalog.GlobalRecords, DMPMetadataRecord{RecordType: dmpRecordSystemPrivilege, Name: priv.Privilege, SQL: sql})
			} else {
				return nil, fmt.Errorf("unresolved system privilege id=%d grantee=%s; inspect sys_privs.tsv before DMP export", priv.PrivilegeID, priv.Grantee)
			}
		}
		for _, line := range renderRoleGrantLines(roleGrants, users, roles, userIDs, roleIDs) {
			key := strings.ToUpper(strings.TrimSpace(line))
			if grantSQL[key] {
				continue
			}
			grantSQL[key] = true
			name := dmpGrantedRoleName(line)
			recordType := uint16(dmpRecordRoleGrant)
			if isBuiltInRoleName(name) {
				recordType = dmpRecordBuiltinGrant
			}
			catalog.GlobalRecords = append(catalog.GlobalRecords, DMPMetadataRecord{
				RecordType: recordType, Name: name, SQL: line,
			})
		}
	}

	for _, view := range views {
		sql := strings.TrimSpace(view.SQL)
		if sql == "" && strings.TrimSpace(view.QuerySQL) != "" {
			sql = fmt.Sprintf("CREATE OR REPLACE VIEW %s.%s AS\n%s", quoteIdent(view.Owner), quoteIdent(view.Name), strings.TrimSpace(view.QuerySQL))
		}
		if sql == "" {
			continue
		}
		ensureSchema(view.Owner).Records = append(ensureSchema(view.Owner).Records, DMPMetadataRecord{
			RecordType: dmpRecordView, Name: view.Name, SQL: ensureSQLTerminator(sql),
		})
	}
	for _, seq := range sequences {
		sql := strings.TrimSpace(seq.SQL)
		if sql == "" {
			sql = renderRecoveredSequenceSQL(seq)
		}
		if sql == "" {
			continue
		}
		ensureSchema(seq.Owner).Records = append(ensureSchema(seq.Owner).Records, DMPMetadataRecord{
			RecordType: dmpRecordSequence, Name: seq.Name, SQL: ensureSQLTerminator(sql),
		})
	}
	for _, routine := range routines {
		sql := strings.TrimSpace(routine.SQL)
		if sql == "" {
			continue
		}
		recordType := dmpRecordRoutine
		extra := uint32(0)
		switch normalizeRoutineObjectType(routine.ObjectType) {
		case "FUNCTION":
			extra = 1
		case "PACKAGE":
			recordType = dmpRecordPackage
		case "PACKAGE BODY":
			recordType = dmpRecordPackageBody
		}
		ensureSchema(routine.Owner).Records = append(ensureSchema(routine.Owner).Records, DMPMetadataRecord{
			RecordType: recordType, Name: routine.Name, SQL: ensureSQLTerminator(sql), ExtraValue: extra,
		})
	}
	for _, synonym := range synonyms {
		// OWNER/SCHEMAS modes export objects owned by the selected schemas.
		// Dictionary DDL recovery may also retain inbound synonyms whose target
		// is selected; those belong to a different logical export unit.
		if !matcher.allowed(synonym.Owner) {
			continue
		}
		var sql string
		if synonym.Public || strings.EqualFold(synonym.Owner, "PUBLIC") {
			sql = fmt.Sprintf("CREATE OR REPLACE PUBLIC SYNONYM %s FOR %s.%s;", quoteIdent(synonym.Name), quoteIdent(synonym.TableOwner), quoteIdent(synonym.TableName))
		} else {
			sql = fmt.Sprintf("CREATE OR REPLACE SYNONYM %s.%s FOR %s.%s;", quoteIdent(synonym.Owner), quoteIdent(synonym.Name), quoteIdent(synonym.TableOwner), quoteIdent(synonym.TableName))
		}
		record := DMPMetadataRecord{
			RecordType: dmpRecordSynonym, Name: synonym.Name, SQL: sql,
		}
		if synonym.Public || strings.EqualFold(synonym.Owner, "PUBLIC") {
			catalog.GlobalRecords = append(catalog.GlobalRecords, record)
			continue
		}
		ensureSchema(synonym.Owner).Records = append(ensureSchema(synonym.Owner).Records, record)
	}

	selectedTables := make(map[string]*DMPTableMetadata)
	for _, tableID := range sortedTableIDs(tables) {
		table := tables[tableID]
		columns := columnsByTable[tableID]
		if !matcher.allowed(table.Owner) || !tableMatcher.allowed(table.Owner, table.Name) || len(columns) == 0 {
			continue
		}
		if len(columns) > int(^uint16(0)) {
			return nil, fmt.Errorf("table %s.%s has %d columns; dmp supports at most %d", table.Owner, table.Name, len(columns), ^uint16(0))
		}
		metadata := DMPTableMetadata{
			ID: table.ID, Schema: table.Owner, Owner: schemaOwners[strings.ToUpper(table.Owner)],
			Name: table.Name, ColumnCount: uint16(len(columns)),
		}
		if metadata.Owner == "" {
			metadata.Owner = table.Owner
		}
		createSQL := dmpCreateTableSQL(tableID, tables, columnsByTable, columnsByTableColID, tableStorage, partitionsByTable, partitionKeysByTable, constraintObjects, indexes, constraints, tablespaces)
		if createSQL == "" {
			return nil, fmt.Errorf("cannot render dmp table metadata for %s.%s", table.Owner, table.Name)
		}
		metadata.Records = append(metadata.Records, DMPMetadataRecord{RecordType: dmpRecordTable, Name: table.Name, SQL: createSQL})
		metadata.Records = append(metadata.Records, dmpIndexMetadata(tableID, tables, columnsByTableColID, indexObjects, indexes, tablespaces)...)
		metadata.Records = append(metadata.Records, dmpConstraintMetadata(tableID, objects, tables, columnsByTableColID, constraintObjects, indexes, constraints)...)
		metadata.Records = append(metadata.Records, dmpCommentMetadata(table, columns, tableComments, columnComments)...)
		selectedTables[dmpObjectKey(table.Owner, table.Name)] = &metadata
		ensureSchema(table.Owner).Tables = append(ensureSchema(table.Owner).Tables, metadata)
	}

	// Attach table triggers after table records so dimp creates the table first.
	for _, trigger := range triggers {
		record := DMPMetadataRecord{
			RecordType: dmpRecordTrigger, Name: trigger.Name,
			SQL: ensureSQLTerminator(trigger.SQL), TableName: trigger.TableName,
		}
		if strings.TrimSpace(trigger.SQL) == "" {
			continue
		}
		if table := selectedTables[dmpObjectKey(trigger.TableOwner, trigger.TableName)]; table != nil {
			appendDMPTableRecord(schemas, table.Schema, table.ID, record)
		} else {
			ensureSchema(trigger.Owner).Records = append(ensureSchema(trigger.Owner).Records, record)
		}
	}
	for _, privilege := range privileges {
		record := dmpObjectGrantRecord(privilege)
		if table := selectedTables[dmpObjectKey(privilege.Owner, privilege.ObjectName)]; table != nil {
			appendDMPTableRecord(schemas, table.Schema, table.ID, record)
		} else if matcher.allowed(privilege.Owner) {
			ensureSchema(privilege.Owner).Records = append(ensureSchema(privilege.Owner).Records, record)
		}
	}

	for _, schema := range schemas {
		sort.SliceStable(schema.Records, func(i, j int) bool {
			if schema.Records[i].RecordType != schema.Records[j].RecordType {
				return schema.Records[i].RecordType < schema.Records[j].RecordType
			}
			return schema.Records[i].Name < schema.Records[j].Name
		})
		sort.Slice(schema.Tables, func(i, j int) bool {
			if schema.Tables[i].Name != schema.Tables[j].Name {
				return schema.Tables[i].Name < schema.Tables[j].Name
			}
			return schema.Tables[i].ID < schema.Tables[j].ID
		})
		catalog.Schemas = append(catalog.Schemas, *schema)
	}
	sort.Slice(catalog.Schemas, func(i, j int) bool {
		if catalog.Schemas[i].Name != catalog.Schemas[j].Name {
			return catalog.Schemas[i].Name < catalog.Schemas[j].Name
		}
		return catalog.Schemas[i].Owner < catalog.Schemas[j].Owner
	})
	return catalog, nil
}

func dmpSchemaOwnerMap(objects map[uint32]dictionaryObject, users map[uint32]dictionaryObject, dict *DictionaryInfo) map[string]string {
	result := make(map[string]string)
	for _, schema := range dictSchemas(dict) {
		if schema.Name != "" {
			result[strings.ToUpper(schema.Name)] = defaultIfEmpty(schema.Owner, schema.Name)
		}
	}
	for _, obj := range objects {
		if obj.Type != "SCH" || obj.Valid == "N" || obj.ParentID <= 0 || obj.Name == "" {
			continue
		}
		if user, ok := users[uint32(obj.ParentID)]; ok && user.Name != "" {
			result[strings.ToUpper(obj.Name)] = user.Name
		}
	}
	return result
}

func dmpSchemaNameMap(objects map[uint32]dictionaryObject, dict *DictionaryInfo) map[string]string {
	result := make(map[string]string)
	for _, schema := range dictSchemas(dict) {
		if schema.Name != "" {
			result[strings.ToUpper(schema.Name)] = schema.Name
		}
	}
	for _, obj := range objects {
		if obj.Type == "SCH" && obj.Valid != "N" && obj.Name != "" {
			result[strings.ToUpper(obj.Name)] = obj.Name
		}
	}
	return result
}

func dictSchemas(dict *DictionaryInfo) []DictionarySchema {
	if dict == nil {
		return nil
	}
	return dict.Schemas
}

func dmpCreateTableSQL(tableID uint32, tables map[uint32]dictionaryObject, columnsByTable map[uint32][]columnDef, columnsByTableColID map[tableColKey]columnDef, tableStorage map[uint32]indexDef, partitionsByTable map[uint32][]PartitionInfo, partitionKeysByTable map[uint32][]uint16, constraintObjects map[uint32]dictionaryObject, indexes map[uint32]indexDef, constraints []constraintDef, tablespaces map[uint32]string) string {
	table := tables[tableID]
	matcher := newOwnerMatcher(table.Owner)
	tableMatcher := newTableNameMatcher(table.Owner + "." + table.Name)
	var out strings.Builder
	renderCreateTables(&out, tables, columnsByTable, columnsByTableColID, tableStorage, partitionsByTable, partitionKeysByTable, matcher, tableMatcher, tablespaces)
	sql := strings.TrimSpace(strings.TrimPrefix(out.String(), "-- Tables\n"))
	primaryKey := dmpInlinePrimaryKeyClause(tableID, columnsByTableColID, constraintObjects, indexes, constraints)
	if primaryKey == "" {
		return sql
	}
	closePos := strings.Index(sql, "\n)")
	if closePos < 0 {
		return sql
	}
	return sql[:closePos] + ",\n    " + primaryKey + sql[closePos:]
}

func dmpInlinePrimaryKeyClause(tableID uint32, columns map[tableColKey]columnDef, constraintObjects map[uint32]dictionaryObject, indexes map[uint32]indexDef, constraints []constraintDef) string {
	for _, cons := range constraints {
		if cons.TableID != tableID || cons.Type != "P" || cons.Valid != "Y" {
			continue
		}
		cols := ddlColumns(columnsFromIndex(cons.IndexID, cons.TableID, indexes, columns), false)
		if cols == "" {
			continue
		}
		name := ""
		if obj, ok := constraintObjects[cons.ID]; ok {
			name = recoveredConstraintNameClause(obj.Name)
		}
		return fmt.Sprintf("%sPRIMARY KEY (%s)", name, cols)
	}
	return ""
}

func dmpIndexMetadata(tableID uint32, tables map[uint32]dictionaryObject, columns map[tableColKey]columnDef, indexObjects map[uint32]dictionaryObject, indexes map[uint32]indexDef, tablespaces map[uint32]string) []DMPMetadataRecord {
	var records []DMPMetadataRecord
	for indexID, obj := range indexObjects {
		if uint32(obj.ParentID) != tableID {
			continue
		}
		idx, ok := indexes[indexID]
		if !ok || idx.Flag&1 != 0 || idx.KeyNum == 0 || !isRenderableUserIndexType(idx.Type) {
			continue
		}
		cols := ddlColumns(columnsFromIndex(indexID, tableID, indexes, columns), true)
		if cols == "" {
			continue
		}
		table := tables[tableID]
		sql, ok := renderCreateIndexSQL(obj, table, idx, cols, tablespaces)
		if !ok {
			continue
		}
		records = append(records, DMPMetadataRecord{RecordType: dmpRecordIndex, Name: obj.Name, SQL: sql})
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Name < records[j].Name })
	return records
}

func dmpConstraintMetadata(tableID uint32, objects map[uint32]dictionaryObject, tables map[uint32]dictionaryObject, columns map[tableColKey]columnDef, constraintObjects map[uint32]dictionaryObject, indexes map[uint32]indexDef, constraints []constraintDef) []DMPMetadataRecord {
	var records []DMPMetadataRecord
	for _, cons := range constraints {
		if cons.TableID != tableID || cons.Valid != "Y" {
			continue
		}
		table, ok := tables[tableID]
		if !ok {
			continue
		}
		obj, ok := constraintObjects[cons.ID]
		if !ok {
			continue
		}
		prefix := fmt.Sprintf("ALTER TABLE %s.%s ADD %s", quoteIdent(table.Owner), quoteIdent(table.Name), recoveredConstraintNameClause(obj.Name))
		recordType := dmpRecordConstraint
		var sql string
		switch cons.Type {
		case "P":
			// Native dexp places primary keys inside CREATE TABLE. Besides matching
			// that layout, this guarantees referenced keys exist before dimp applies
			// deferred foreign-key records from other tables.
			continue
		case "U":
			cols := ddlColumns(columnsFromIndex(cons.IndexID, cons.TableID, indexes, columns), false)
			if cols == "" {
				continue
			}
			recordType = dmpRecordUnique
			sql = fmt.Sprintf("%sUNIQUE (%s);", prefix, cols)
		case "C":
			if cons.CheckInfo == "" {
				continue
			}
			sql = fmt.Sprintf("%sCHECK (%s);", prefix, cons.CheckInfo)
		case "F":
			childCols := ddlColumns(columnsFromIndex(cons.IndexID, cons.TableID, indexes, columns), false)
			parentIndexObj, ok := objects[cons.FIndexID]
			if !ok || childCols == "" {
				continue
			}
			parentTable, ok := tables[uint32(parentIndexObj.ParentID)]
			if !ok {
				continue
			}
			parentCols := ddlColumns(columnsFromIndex(cons.FIndexID, uint32(parentIndexObj.ParentID), indexes, columns), false)
			if parentCols == "" {
				continue
			}
			sql = fmt.Sprintf("%sFOREIGN KEY (%s) REFERENCES %s.%s (%s);", prefix, childCols, quoteIdent(parentTable.Owner), quoteIdent(parentTable.Name), parentCols)
		default:
			continue
		}
		records = append(records, DMPMetadataRecord{RecordType: recordType, Name: obj.Name, SQL: sql})
	}
	sort.SliceStable(records, func(i, j int) bool {
		if records[i].RecordType != records[j].RecordType {
			return records[i].RecordType < records[j].RecordType
		}
		return records[i].Name < records[j].Name
	})
	return records
}

func dmpCommentMetadata(table dictionaryObject, columns []columnDef, tableComments map[ownerTableKey]tableComment, columnComments map[ownerTableColumnKey]columnComment) []DMPMetadataRecord {
	var records []DMPMetadataRecord
	if comment, ok := tableComments[ownerTableKey{owner: table.Owner, table: table.Name}]; ok {
		records = append(records, DMPMetadataRecord{
			RecordType: dmpRecordTableComment, Name: table.Name,
			SQL: fmt.Sprintf("COMMENT ON TABLE %s.%s IS %s;", quoteIdent(comment.Owner), quoteIdent(comment.TableName), sqlLiteral(comment.Comment)),
		})
	}
	for _, column := range columns {
		comment, ok := columnComments[ownerTableColumnKey{owner: table.Owner, table: table.Name, column: column.Name}]
		if !ok {
			continue
		}
		records = append(records, DMPMetadataRecord{
			RecordType: dmpRecordColumnComment, Name: table.Name,
			SQL: fmt.Sprintf("COMMENT ON COLUMN %s.%s.%s IS %s;", quoteIdent(comment.Owner), quoteIdent(comment.TableName), quoteIdent(comment.ColumnName), sqlLiteral(comment.Comment)),
		})
	}
	return records
}

func dmpObjectGrantRecord(privilege DictionaryTabPrivilege) DMPMetadataRecord {
	grantable := "N"
	if strings.EqualFold(privilege.Grantable, "Y") || strings.EqualFold(privilege.Grantable, "YES") {
		grantable = "Y"
	}
	recordType := uint16(dmpRecordObjectGrant)
	objectType := strings.ToUpper(strings.TrimSpace(privilege.ObjectType))
	if objectType != "" && objectType != "TABLE" {
		recordType = dmpRecordSchemaGrant
		switch objectType {
		case "SEQUENCE":
			objectType = "SEQ"
		case "FUNCTION", "PROCEDURE":
			objectType = "PROC"
		case "PACKAGE", "PACKAGE BODY":
			objectType = "PKG"
		}
	}
	if privilege.ColumnID != nil || privilege.ColumnName != "" {
		recordType = dmpRecordColumnGrant
	}
	grantor := privilege.Grantor
	if grantor == "" {
		grantor = "SYSDBA"
	}
	return DMPMetadataRecord{
		RecordType: recordType,
		Name:       privilege.ObjectName,
		Grant: &DMPObjectGrant{
			Grantor: grantor, Grantee: privilege.Grantee,
			Privilege: strings.ToUpper(strings.TrimSpace(privilege.Privilege)),
			Owner:     privilege.Owner, ObjectName: privilege.ObjectName,
			ObjectType: objectType, Grantable: grantable,
			ColumnName: privilege.ColumnName,
		},
	}
}

func appendDMPTableRecord(schemas map[string]*DMPSchemaMetadata, schemaName string, tableID uint32, record DMPMetadataRecord) {
	schema := schemas[strings.ToUpper(schemaName)]
	if schema == nil {
		return
	}
	for i := range schema.Tables {
		if schema.Tables[i].ID == tableID {
			schema.Tables[i].Records = append(schema.Tables[i].Records, record)
			return
		}
	}
}

func dmpObjectKey(owner string, name string) string {
	return strings.ToUpper(strings.TrimSpace(owner)) + "\x00" + strings.ToUpper(strings.TrimSpace(name))
}

func dmpGrantedRoleName(sql string) string {
	fields := strings.Fields(sql)
	if len(fields) < 2 {
		return "ROLE"
	}
	return strings.Trim(fields[1], `"`)
}
