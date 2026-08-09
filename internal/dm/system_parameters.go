package dm

import (
	"encoding/binary"
	"fmt"
	"strings"
	"unicode"
)

const (
	// SYSINDEXOPENHISTORY is the stable DM8 index used by SYS.SYSOPENHISTORY.
	sysOpenHistoryIndexObjectID = uint32(147)
	sysOpenHistoryAssistID      = uint32(0x02000000) | sysOpenHistoryIndexObjectID
)

type systemInstanceCandidate struct {
	name     string
	openTime string
	absolute uint64
}

func detectSystemInstanceNameFromFile(path string, pageSize uint32) (string, string, bool) {
	if pageSize == 0 {
		return "", "", false
	}
	stream, err := openSystemPageStream(path)
	if err != nil {
		return "", "", false
	}
	defer stream.close()
	return detectSystemInstanceNameFromStream(stream)
}

func detectSystemInstanceNameFromStream(stream *systemPageStream) (string, string, bool) {
	if stream == nil || stream.pageSize == 0 {
		return "", "", false
	}
	preferredCharset := "utf-8"
	if charset, ok := stream.charset(); ok && charset.DecoderName != "" {
		preferredCharset = charset.DecoderName
	}
	decoder := textDecoder{preferred: preferredCharset}
	columns := sysOpenHistoryColumns()
	var best systemInstanceCandidate
	found := false
	err := stream.forEachPage(func(page []byte, pageNo uint32) {
		if !isProbableDMDataPage(page, stream.pageSize) || len(page) < dataPageAssistIndexOff+4 {
			return
		}
		if binary.LittleEndian.Uint32(page[dataPageAssistIndexOff:]) != sysOpenHistoryAssistID {
			return
		}
		seenOffsets := make(map[uint16]bool)
		for _, located := range locateRowsInDataPage(page, stream.pageSize, -1) {
			if seenOffsets[located.offset] {
				continue
			}
			seenOffsets[located.offset] = true
			start := int(located.offset)
			end := start + int(located.length)
			if start < 0 || end > len(page) || start >= end {
				continue
			}
			values, _, _, err := parseDataRowValues(page[start:end], columns, decoder, false, nil)
			if err != nil {
				continue
			}
			rguid := systemParameterString(values, 1)
			mode := systemParameterString(values, 3)
			primary := systemParameterString(values, 4)
			current := systemParameterString(values, 5)
			if !validSystemOpenHistoryInstance(rguid, mode, primary, current) {
				continue
			}
			candidate := systemInstanceCandidate{
				name:     current,
				openTime: systemParameterString(values, 2),
				absolute: uint64(pageNo)*uint64(stream.pageSize) + uint64(located.offset),
			}
			if !found || candidate.openTime > best.openTime ||
				(candidate.openTime == best.openTime && candidate.absolute > best.absolute) {
				best = candidate
				found = true
			}
		}
	})
	if err != nil || !found {
		return "", "", false
	}
	return best.name, "SYSTEM.DBF SYS.SYSOPENHISTORY.CUR_INST_NAME", true
}

func sysOpenHistoryColumns() []columnDef {
	columns := []columnDef{
		{ColID: 1, Name: "RGUID", DataType: "VARCHAR", Nullable: "N"},
		{ColID: 2, Name: "OPEN_TIME", DataType: "DATETIME", Nullable: "N"},
		{ColID: 3, Name: "SYS_MODE", DataType: "VARCHAR", Nullable: "N"},
		{ColID: 4, Name: "PRIMARY_INST_NAME", DataType: "VARCHAR", Nullable: "N"},
		{ColID: 5, Name: "CUR_INST_NAME", DataType: "VARCHAR", Nullable: "N"},
		{ColID: 6, Name: "PRIMARY_DB_MAGIC", DataType: "BIGINT", Nullable: "N"},
		{ColID: 7, Name: "CUR_DB_MAGIC", DataType: "BIGINT", Nullable: "N"},
		{ColID: 8, Name: "N_EP", DataType: "SMALLINT", Nullable: "N"},
	}
	for id := uint16(9); id <= 40; id++ {
		columns = append(columns, columnDef{ColID: id, Name: fmt.Sprintf("HISTORY_%d", id), DataType: "BIGINT", Nullable: "Y"})
	}
	return columns
}

func systemParameterString(values map[uint16]dataValue, columnID uint16) string {
	value, ok := values[columnID]
	if !ok || value.value == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprintf("%v", value.value))
}

func validSystemOpenHistoryInstance(rguid string, mode string, primary string, current string) bool {
	if !validSystemInstanceName(primary) || !validSystemInstanceName(current) || !validSystemMode(mode) {
		return false
	}
	prefix := strings.ToUpper(primary) + "_"
	return strings.HasPrefix(strings.ToUpper(strings.TrimSpace(rguid)), prefix)
}

func validSystemInstanceName(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || len([]byte(value)) > 128 {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return false
		}
	}
	return true
}

func validSystemMode(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 32 {
		return false
	}
	for _, r := range value {
		if !unicode.IsLetter(r) && r != '_' {
			return false
		}
	}
	return true
}
