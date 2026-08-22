package dm

import (
	"encoding/binary"
	"testing"
)

func TestScanRawTriggerTextsFindsMultipleTriggers(t *testing.T) {
	raw := []byte("xxCREATE OR REPLACE TRIGGER APP.TRG_A\nBEFORE INSERT ON APP.T\nBEGIN\n    IF 1 = 1 THEN\n        NULL;\n    END IF;\nEND;\x00yyCREATE OR REPLACE TRIGGER APP.TRG_B\nBEFORE UPDATE ON APP.T\nBEGIN\n    NULL;\nEND;\xD7zz")

	got := scanRawTriggerTexts(raw, textDecoder{preferred: "utf-8"})
	if !containsText(got["APP.TRG_A"], "END IF;") || !containsText(got["APP.TRG_A"], "TRG_A") {
		t.Fatalf("TRG_A text not recovered correctly: %q", got["APP.TRG_A"])
	}
	if !containsText(got["APP.TRG_B"], "TRG_B") {
		t.Fatalf("TRG_B text not recovered correctly: %q", got["APP.TRG_B"])
	}
}

func TestScanRawRoutineTextsFindsFunctionProcedureAndPackage(t *testing.T) {
	raw := []byte(
		"aaCREATE OR REPLACE FUNCTION APP.F_AMOUNT RETURN INT AS\nBEGIN\n    IF 1 = 1 THEN\n        RETURN 1;\n    END IF;\n    RETURN 0;\nEND;\x00" +
			"bbCREATE OR REPLACE PROCEDURE APP.P_ADD AS\nBEGIN\n    NULL;\nEND;\x00" +
			"ccCREATE OR REPLACE PACKAGE APP.PKG_TEST AS\n    PROCEDURE RUN;\nEND PKG_TEST;\x00" +
			"ddCREATE OR REPLACE PACKAGE BODY APP.PKG_TEST AS\n    PROCEDURE RUN AS BEGIN NULL; END;\nEND PKG_TEST;\x00")

	got := scanRawRoutineTexts(raw, textDecoder{preferred: "utf-8"})
	if !containsText(got[routineKey("APP", "F_AMOUNT", "FUNCTION")], "END IF;") {
		t.Fatalf("function text not recovered correctly: %q", got[routineKey("APP", "F_AMOUNT", "FUNCTION")])
	}
	if !containsText(got[routineKey("APP", "P_ADD", "PROCEDURE")], "PROCEDURE APP.P_ADD") {
		t.Fatalf("procedure text not recovered correctly: %q", got[routineKey("APP", "P_ADD", "PROCEDURE")])
	}
	if !containsText(got[routineKey("APP", "PKG_TEST", "PACKAGE")], "PACKAGE APP.PKG_TEST") {
		t.Fatalf("package spec text not recovered correctly: %q", got[routineKey("APP", "PKG_TEST", "PACKAGE")])
	}
	if !containsText(got[routineKey("APP", "PKG_TEST", "PACKAGE BODY")], "PACKAGE BODY APP.PKG_TEST") {
		t.Fatalf("package body text not recovered correctly: %q", got[routineKey("APP", "PKG_TEST", "PACKAGE BODY")])
	}
}

func TestScanRawRoutineTextsIgnoresEndSemicolonInsideString(t *testing.T) {
	raw := []byte("CREATE OR REPLACE FUNCTION APP.F_TEXT RETURN VARCHAR2 AS\nBEGIN\n    RETURN 'END;候诊时间为:以后';\nEND;\x00CREATE OR REPLACE FUNCTION APP.F_NEXT RETURN INT AS BEGIN RETURN 1; END;\x00")

	got := scanRawRoutineTexts(raw, textDecoder{preferred: "utf-8"})
	text := got[routineKey("APP", "F_TEXT", "FUNCTION")]
	if !containsText(text, "候诊时间为") || !containsText(text, "END;") {
		t.Fatalf("function string literal should be preserved, got %q", text)
	}
	if containsText(text, "F_NEXT") {
		t.Fatalf("function text should not include the next routine, got %q", text)
	}
}

func TestScanDictionaryRoutinesRepairsPackageSpecMisreadAsBody(t *testing.T) {
	const objectID = uint32(77)
	spec := "CREATE OR REPLACE PACKAGE APP.PKG_TEST AS\n    PROCEDURE RUN;\nEND;"
	body := "CREATE OR REPLACE PACKAGE BODY APP.PKG_TEST AS\n    PROCEDURE RUN AS BEGIN NULL; END;\nEND;"
	objects := map[uint32]dictionaryObject{
		objectID: {ID: objectID, Type: "SCHOBJ", Subtype: "PKG", Owner: "APP", Name: "PKG_TEST", Valid: "Y"},
	}
	// A protected dictionary row can make sequence zero point at the longer
	// package body. Raw text recovery still carries the correctly typed spec.
	texts := map[uint32]map[uint32]string{
		objectID: {0: body, 1: body},
	}
	rawTexts := map[string]string{
		routineKey("APP", "PKG_TEST", "PACKAGE"):      spec,
		routineKey("APP", "PKG_TEST", "PACKAGE BODY"): body,
	}

	routines := scanDictionaryRoutines(objects, texts, rawTexts, newOwnerMatcher("all"))
	if len(routines) != 2 {
		t.Fatalf("routine count = %d, want 2: %+v", len(routines), routines)
	}
	if routines[0].ObjectType != "PACKAGE" || routines[0].SQL != spec {
		t.Fatalf("package spec was not repaired: %+v", routines[0])
	}
	if routines[1].ObjectType != "PACKAGE BODY" || routines[1].SQL != body {
		t.Fatalf("package body changed unexpectedly: %+v", routines[1])
	}
}

func TestSequenceIsPrivilegeTarget(t *testing.T) {
	obj := dictionaryObject{Type: "SCHOBJ", Subtype: "SEQ", Owner: "APP", Name: "SEQ_T"}
	if !isTabPrivilegeTarget(obj) {
		t.Fatalf("sequence should be accepted as an object privilege target")
	}
	if got := dictionaryPrivilegeObjectType(obj); got != "SEQUENCE" {
		t.Fatalf("unexpected sequence object type: %q", got)
	}
}

func TestKnownGeneratedSYSDBARoutineIsFiltered(t *testing.T) {
	if !isKnownGeneratedSYSDBARoutine("SYSDBA", "SP_DB_BAKSET_REMOVE_BATCH") {
		t.Fatalf("known generated SYSDBA routine should be filtered")
	}
	if isKnownGeneratedSYSDBARoutine("SYSDBA", "P_ADD_SALES_LIST") {
		t.Fatalf("user-created SYSDBA procedure should not be filtered by name")
	}
	if isKnownGeneratedSYSDBARoutine("APP", "SP_DB_BAKSET_REMOVE_BATCH") {
		t.Fatalf("same name under a normal owner should not be filtered")
	}
}

func TestVectorMaintenanceTriggerNamesAreFiltered(t *testing.T) {
	for _, name := range []string{"TRIG_HNSW$IDX_VECTOR", "TRIG_IVFFLAT$IDX_VECTOR"} {
		if !isVectorGeneratedTriggerName(name) {
			t.Fatalf("%s should be recognized as a generated vector trigger", name)
		}
	}
	if isVectorGeneratedTriggerName("TRG_VECTOR_AUDIT") {
		t.Fatal("ordinary user trigger was classified as generated")
	}
}

func TestParseSequencePayload(t *testing.T) {
	payload := []byte{
		0xFF, 0x0F, 0xA5, 0xD4, 0xE8, 0x00, 0x00, 0x00,
		0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x02, 0x00, 0x00, 0x00, 0x02, 0x00,
		0x14, 0x00, 0x00, 0x00,
	}
	got := parseSequencePayload(payload)
	if got.maxValue != 999999999999 || got.minValue != 1 || !got.hasBounds || got.cacheSize != 20 {
		t.Fatalf("unexpected sequence payload parse: %+v", got)
	}
	if !got.hasRuntimeLocator || got.runtimeFile != 0 || got.runtimePage != 2 || got.runtimeSlot != 2 {
		t.Fatalf("unexpected sequence runtime locator: %+v", got)
	}
}

func TestParseSequencePayloadUsesSignedBounds(t *testing.T) {
	payload := make([]byte, 28)
	binary.LittleEndian.PutUint64(payload[0:], uint64(int64(0)))
	binary.LittleEndian.PutUint64(payload[8:], ^uint64(99)) // -100 as int64
	got := parseSequencePayload(payload)
	if !got.hasBounds || got.minValue != -100 || got.maxValue != 0 {
		t.Fatalf("signed sequence bounds were not preserved: %+v", got)
	}
}

func TestParseSequenceRuntimeValue(t *testing.T) {
	page := make([]byte, 8192)
	binary.LittleEndian.PutUint32(page[4:], 2)
	binary.LittleEndian.PutUint16(page[sequenceRuntimeCountOffset:], 4)
	tests := []struct {
		slot      uint16
		state     uint8
		stored    int64
		increment int64
		want      int64
	}{
		{slot: 0, state: 0x11, stored: 10, increment: 3, want: 10},
		{slot: 1, state: 0x01, stored: 22, increment: 3, want: 25},
		{slot: 2, state: 0x01, stored: 88, increment: -3, want: 85},
		{slot: 3, state: 0x11, stored: 0, increment: 1, want: 0},
	}
	for _, tt := range tests {
		off := sequenceRuntimeRecordBase + int(tt.slot)*sequenceRuntimeRecordSize
		page[off] = tt.state
		binary.LittleEndian.PutUint64(page[off+1:], uint64(tt.stored))
		got, state, ok := parseSequenceRuntimeValue(page, 2, tt.slot, tt.increment)
		if !ok || got != tt.want || state != tt.state {
			t.Fatalf("slot %d runtime value = %d state=0x%02X ok=%v, want %d state=0x%02X", tt.slot, got, state, ok, tt.want, tt.state)
		}
	}
	sparseSlot := uint16(7)
	off := sequenceRuntimeRecordBase + int(sparseSlot)*sequenceRuntimeRecordSize
	page[off] = 0x01
	binary.LittleEndian.PutUint64(page[off+1:], 40)
	if got, _, ok := parseSequenceRuntimeValue(page, 2, sparseSlot, 2); !ok || got != 42 {
		t.Fatalf("valid sparse sequence slot was not accepted: got=%d ok=%v", got, ok)
	}
	invalidSlot := uint16((len(page)-sequenceRuntimeRecordBase)/sequenceRuntimeRecordSize + 1)
	if _, _, ok := parseSequenceRuntimeValue(page, 2, invalidSlot, 1); ok {
		t.Fatal("slot beyond the runtime page capacity was accepted")
	}
}

func containsText(value string, part string) bool {
	return len(value) >= len(part) && indexASCIIInsensitive([]byte(value), []byte(part)) >= 0
}
