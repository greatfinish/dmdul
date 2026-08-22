package dm

import (
	"strings"
	"testing"
)

func TestDMPMetadataInlinesPrimaryKeyInCreateTable(t *testing.T) {
	const (
		tableID      = uint32(1001)
		indexID      = uint32(2001)
		constraintID = uint32(3001)
	)
	tables := map[uint32]dictionaryObject{
		tableID: {ID: tableID, Owner: "APP", Name: "PARENT"},
	}
	columnsByTable := map[uint32][]columnDef{
		tableID: {{TableID: tableID, ColID: 0, Name: "ID", DataType: "INT", Nullable: "N"}},
	}
	columns := map[tableColKey]columnDef{
		{tableID: tableID, colID: 0}: columnsByTable[tableID][0],
	}
	indexes := map[uint32]indexDef{
		indexID: {ID: indexID, Keys: []indexKey{{ColID: 0}}},
	}
	constraintObjects := map[uint32]dictionaryObject{
		constraintID: {ID: constraintID, Name: "PARENT_PK"},
	}
	constraints := []constraintDef{{
		ID: constraintID, TableID: tableID, Type: "P", Valid: "Y", IndexID: indexID,
	}}

	sql := dmpCreateTableSQL(
		tableID, tables, columnsByTable, columns, nil, nil, nil,
		constraintObjects, indexes, constraints, nil,
	)
	if !strings.Contains(sql, "CONSTRAINT PARENT_PK PRIMARY KEY (ID)") {
		t.Fatalf("DMP CREATE TABLE did not inline the primary key:\n%s", sql)
	}
	records := dmpConstraintMetadata(tableID, nil, tables, columns, constraintObjects, indexes, constraints)
	if len(records) != 0 {
		t.Fatalf("primary key should not be emitted as deferred DMP metadata: %+v", records)
	}
}
