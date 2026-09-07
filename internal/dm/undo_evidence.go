package dm

import (
	"encoding/binary"
	"fmt"
	"strings"
)

// These fields have been checked against DM8 INSERT/UPDATE/DELETE/ROLLBACK
// captures. They are evidence, NOT a transaction visibility decision. In
// particular a reused ROLL page cannot prove that an absent transaction committed.
type undoRecordEvidence struct {
	operation           string
	sequence            uint32
	previousTransaction uint64
	object1, object2    uint32
	previous            dataRowControlTail
	payload             []byte
}

func decodeUndoRecordEvidence(page []byte, offset uint16) (undoRecordEvidence, error) {
	if len(page) < 55 || dataPageKind(page) != 0x1d {
		return undoRecordEvidence{}, fmt.Errorf("not an observed 0x1D ROLL record page")
	}
	start := int(binary.LittleEndian.Uint16(page[43:45]))
	end := int(binary.LittleEndian.Uint16(page[45:47]))
	pos := int(offset)
	if start < 55 || end < start || end > len(page) || pos < start || pos+45 > end {
		return undoRecordEvidence{}, fmt.Errorf("ROLL record outside used region")
	}
	length := int(binary.LittleEndian.Uint16(page[pos:]))
	if length < 45 || pos+length > end {
		return undoRecordEvidence{}, fmt.Errorf("invalid ROLL record length")
	}
	record := page[pos : pos+length]
	if binary.LittleEndian.Uint16(record[length-2:]) != offset {
		return undoRecordEvidence{}, fmt.Errorf("ROLL record self offset mismatch")
	}
	var op string
	switch record[2] {
	case 1:
		op = "INSERT"
	case 2:
		op = "UPDATE"
	case 3:
		op = "DELETE"
	default:
		return undoRecordEvidence{}, fmt.Errorf("unsupported undo opcode 0x%X", record[2])
	}
	if record[3] != 2 && record[3] != 3 {
		return undoRecordEvidence{}, fmt.Errorf("unsupported undo flags 0x%X", record[3])
	}
	r := undoRecordEvidence{
		operation: op, sequence: binary.LittleEndian.Uint32(record[4:8]), previousTransaction: decodeUint48LE(record[8:14]),
		object1: binary.LittleEndian.Uint32(record[20:24]), object2: binary.LittleEndian.Uint32(record[24:28]),
		payload: append([]byte(nil), record[43:length-2]...),
	}
	r.previous = dataRowControlTail{clusterRowID: decodeUint48LE(record[35:41]), rollFile: record[28], rollPage: binary.LittleEndian.Uint32(record[29:33]), rollOffset: binary.LittleEndian.Uint16(record[33:35]), transactionID: r.previousTransaction}
	return r, nil
}

func traceRowUndoEvidence(row []byte, cache *dataFilePageCache) string {
	tail, ok := decodeDataRowControlTail(row)
	if !ok || len(row) < 22 {
		return "NO_CONTROL_TAIL"
	}
	if !tail.hasRollbackAddress() {
		return "NO_ROLL_ADDRESS (visibility unknown)"
	}
	seen := make(map[dataRowControlTail]bool)
	var evidence []string
	var object1, object2 uint32
	stop := func(reason string) string { return strings.Join(append(evidence, reason), " -> ") }
	for depth := 0; depth < 128; depth++ {
		if !tail.hasRollbackAddress() {
			return stop("END (visibility unknown)")
		}
		if seen[tail] {
			return stop("STOP_CYCLE")
		}
		seen[tail] = true
		ref := dataPageRef{key: dataFileKey{groupID: 1, fileID: int16(tail.rollFile)}, pageNo: tail.rollPage}
		page, ok := cache.readPage(ref)
		if !ok {
			return stop("STOP_MISSING_ROLL_PAGE")
		}
		if !pageHeaderMatchesRef(page, ref) || dataPageKind(page) != 0x1d {
			return stop("STOP_ROLL_IDENTITY")
		}
		if decodeUint48LE(page[36:42]) != tail.transactionID {
			return stop("STOP_ROLL_REUSED_OR_TRX_MISMATCH")
		}
		record, err := decodeUndoRecordEvidence(page, tail.rollOffset)
		if err != nil {
			return stop("STOP: " + err.Error())
		}
		if record.previous.clusterRowID != tail.clusterRowID {
			return stop("STOP_ROW_ID_MISMATCH")
		}
		if depth != 0 && (record.object1 != object1 || record.object2 != object2) {
			return stop("STOP_OBJECT_MISMATCH")
		}
		object1, object2 = record.object1, record.object2
		evidence = append(evidence, fmt.Sprintf("%d:%d:%d %s seq=%d previous_trx=%d", tail.rollFile, tail.rollPage, tail.rollOffset, record.operation, record.sequence, record.previousTransaction))
		tail = record.previous
	}
	return stop("STOP_DEPTH_LIMIT")
}
