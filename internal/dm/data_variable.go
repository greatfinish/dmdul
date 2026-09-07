package dm

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strings"
)

func readVariableDataValue(col columnDef, row []byte, pos int, decoder textDecoder, lobReader *dmLOBReader) (any, int, error) {
	if isJSONDataType(col.DataType) {
		return readJSONDataValue(col, row, pos, decoder, lobReader)
	}
	if isVectorDataType(col.DataType) {
		raw, next, err := readShortDataBytes(row, pos)
		if err != nil {
			return nil, pos, fmt.Errorf("%s: %w", col.Name, err)
		}
		value, err := decodeDMVector(raw, col)
		if err != nil {
			return nil, pos, fmt.Errorf("%s: %w", col.Name, err)
		}
		return value, next, nil
	}
	if isNumberDataType(col.DataType) {
		value, next, err := readDMNumber(row, pos)
		if err != nil {
			return nil, pos, fmt.Errorf("%s: %w", col.Name, err)
		}
		return value, next, nil
	}
	if isBinaryDataType(col.DataType) {
		value, next, err := readShortDataBytes(row, pos)
		if err != nil {
			return nil, pos, fmt.Errorf("%s: %w", col.Name, err)
		}
		if payload, ok := unwrapInlineLOBPayload(value); ok {
			value = payload
		} else if locator, ok := parseDMLOBLocatorRaw(value); ok {
			if lobReader == nil {
				return nil, pos, fmt.Errorf("%s: out-of-line binary LOB locator cannot be resolved without data files", col.Name)
			}
			lazy, lazyErr := lobReader.lazyLOBValue(locator, dmPageKindLOBData, false, decoder)
			if lazyErr == nil {
				return lazy, next, nil
			}
			payload, longErr := lobReader.readLongRowPayload(locator)
			if longErr != nil {
				return nil, pos, fmt.Errorf("%s: %w", col.Name, lazyErr)
			}
			return dmBinary(payload), next, nil
		}
		return dmBinary(value), next, nil
	}
	if isCharacterLOBDataType(col.DataType) {
		value, next, err := readInlineTextLOB(row, pos, decoder, lobReader)
		if err != nil {
			return nil, pos, fmt.Errorf("%s: %w", col.Name, err)
		}
		return value, next, nil
	}
	raw, next, marker, err := readShortDataBytesWithMarker(row, pos)
	if err != nil {
		return nil, pos, fmt.Errorf("%s: %w", col.Name, err)
	}
	if locator, ok := parseDMLOBLocatorRaw(raw); ok {
		if lobReader == nil {
			return nil, pos, fmt.Errorf("%s: out-of-line long row locator cannot be resolved without data files", col.Name)
		}
		// An overflow VARCHAR/CHAR column can live in either a long-row (0x22)
		// page or a regular LOB (0x20) page: DM keeps sub-page overflow in
		// long-row pages but spills columns at/over ~page/2 into LOB pages
		// (observed at the 4000-byte boundary on 8K pages). Try both.
		raw, err = lobReader.readTextLOBOrLongRowPayload(locator)
		if err != nil {
			return nil, pos, fmt.Errorf("%s: %w", col.Name, err)
		}
	}
	value, ok := decoder.decode(raw)
	if !ok {
		return nil, pos, fmt.Errorf("%s: cannot decode varchar marker=0x%02X raw=%s", col.Name, marker, strings.ToUpper(hex.EncodeToString(raw)))
	}
	if strings.ContainsRune(value, '\uFFFD') || containsBadControl(value) {
		return nil, pos, fmt.Errorf("%s: decoded varchar contains invalid characters marker=0x%02X raw=%s", col.Name, marker, strings.ToUpper(hex.EncodeToString(raw)))
	}
	return value, next, nil
}

func readShortDataVarchar(row []byte, pos int, decoder textDecoder) (string, int, error) {
	raw, next, marker, err := readShortDataBytesWithMarker(row, pos)
	if err != nil {
		return "", pos, fmt.Errorf("%s", strings.Replace(err.Error(), "raw value", "varchar", 1))
	}
	value, ok := decoder.decode(raw)
	if !ok {
		return "", pos, fmt.Errorf("cannot decode varchar marker=0x%02X pos=%d raw=%s", marker, pos, strings.ToUpper(hex.EncodeToString(raw)))
	}
	if strings.ContainsRune(value, '\uFFFD') || containsBadControl(value) {
		return "", pos, fmt.Errorf("decoded varchar contains invalid characters marker=0x%02X pos=%d raw=%s", marker, pos, strings.ToUpper(hex.EncodeToString(raw)))
	}
	return value, next, nil
}

func readShortDataBytes(row []byte, pos int) ([]byte, int, error) {
	raw, next, _, err := readShortDataBytesWithMarker(row, pos)
	return raw, next, err
}

func readShortDataBytesWithMarker(row []byte, pos int) ([]byte, int, byte, error) {
	if pos >= len(row) {
		return nil, pos, 0, fmt.Errorf("raw value marker out of range")
	}
	marker := row[pos]
	if marker == 0x80 {
		return []byte{}, pos + 1, marker, nil
	}
	if marker < 0x80 {
		if pos+2 > len(row) {
			return nil, pos, marker, fmt.Errorf("raw value extended length out of range")
		}
		n := int(binary.BigEndian.Uint16(row[pos:]))
		if n <= 0 {
			return nil, pos, marker, fmt.Errorf("unsupported raw value marker 0x%02X at %d", marker, pos)
		}
		start := pos + 2
		end := start + n
		if end > len(row) {
			return nil, pos, marker, fmt.Errorf("raw value content out of range")
		}
		return append([]byte(nil), row[start:end]...), end, marker, nil
	}
	if marker < 0x81 || marker > 0xFE {
		return nil, pos, marker, fmt.Errorf("unsupported raw value marker 0x%02X at %d", marker, pos)
	}
	n := int(marker - 0x80)
	start := pos + 1
	end := start + n
	if end > len(row) {
		return nil, pos, marker, fmt.Errorf("raw value content out of range")
	}
	return append([]byte(nil), row[start:end]...), end, marker, nil
}
