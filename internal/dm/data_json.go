package dm

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"strings"
)

func readJSONDataValue(col columnDef, row []byte, pos int, decoder textDecoder, lobReader *dmLOBReader) (any, int, error) {
	raw, next, err := readShortDataBytes(row, pos)
	if err != nil {
		return nil, pos, fmt.Errorf("%s: %w", col.Name, err)
	}
	var value any = dmBinary(raw)
	if payload, ok := unwrapInlineLOBPayload(raw); ok {
		value = dmBinary(payload)
	} else if locator, ok := parseDMLOBLocatorRaw(raw); ok {
		if lobReader == nil {
			return nil, pos, fmt.Errorf("%s: out-of-line JSON locator cannot be resolved without data files", col.Name)
		}
		lazy, lazyErr := lobReader.lazyLOBValue(locator, dmPageKindLOBData, false, decoder)
		if lazyErr != nil {
			return nil, pos, fmt.Errorf("%s: %w", col.Name, lazyErr)
		}
		value = lazy
	}
	return dmJSONValue{value: value, binary: normalizeDataType(col.DataType) == "JSONB", decoder: decoder}, next, nil
}

type dmJSONBDescriptor struct {
	kind    byte
	length  int
	special bool
}

func decodeDMJSONB(raw []byte, decoder textDecoder) (dmJSON, error) {
	if len(raw) < 20 {
		return "", fmt.Errorf("JSONB payload is too short: %d", len(raw))
	}
	value, err := decodeDMJSONBContainer(raw[16:], decoder)
	if err != nil {
		return "", err
	}
	if !json.Valid([]byte(value)) {
		return "", fmt.Errorf("decoded JSONB value is not valid JSON")
	}
	return dmJSON(value), nil
}

func decodeDMJSONBContainer(raw []byte, decoder textDecoder) (string, error) {
	if len(raw) < 4 {
		return "", fmt.Errorf("JSONB container header is truncated")
	}
	header := binary.LittleEndian.Uint32(raw[:4])
	kind := byte(header >> 28)
	count := int(header & 0x0FFFFFFF)
	if count < 0 || count > 1_000_000 {
		return "", fmt.Errorf("invalid JSONB item count %d", count)
	}
	descriptorCount := 0
	switch kind {
	case 1:
		descriptorCount = count
	case 2:
		descriptorCount = count * 2
	case 4:
		descriptorCount = count
	default:
		return "", fmt.Errorf("unsupported JSONB container kind 0x%X", kind)
	}
	descriptorEnd := 4 + descriptorCount*4
	if descriptorEnd > len(raw) {
		return "", fmt.Errorf("JSONB descriptor array is truncated")
	}
	if descriptorCount == 0 {
		if kind == 2 {
			return "{}", nil
		}
		if kind == 4 {
			return "[]", nil
		}
		return "null", nil
	}

	descriptors := make([]dmJSONBDescriptor, descriptorCount)
	knownPayload := 0
	specialIndex := -1
	for i := 0; i < descriptorCount; i++ {
		desc := binary.LittleEndian.Uint32(raw[4+i*4:])
		descKind := byte(desc >> 28)
		special := descKind >= 8
		if special {
			descKind -= 8
			if specialIndex >= 0 {
				return "", fmt.Errorf("multiple JSONB special descriptors")
			}
			specialIndex = i
		}
		length := int(desc & 0x0FFFFFFF)
		if descKind >= 4 && descKind <= 6 {
			length = 0
		}
		descriptors[i] = dmJSONBDescriptor{kind: descKind, length: length, special: special}
		if !special {
			knownPayload += length
		}
	}
	payloadLength := len(raw) - descriptorEnd
	if specialIndex >= 0 {
		length := payloadLength - knownPayload
		if length < 0 {
			return "", fmt.Errorf("invalid JSONB special descriptor length")
		}
		descriptors[specialIndex].length = length
	} else if knownPayload != payloadLength {
		return "", fmt.Errorf("JSONB payload length mismatch: descriptors=%d payload=%d", knownPayload, payloadLength)
	}

	values := make([]string, descriptorCount)
	pos := descriptorEnd
	for i, desc := range descriptors {
		if desc.length < 0 || pos+desc.length > len(raw) {
			return "", fmt.Errorf("JSONB item %d is out of range", i)
		}
		text, err := decodeDMJSONBItem(desc.kind, raw[pos:pos+desc.length], decoder)
		if err != nil {
			return "", fmt.Errorf("JSONB item %d: %w", i, err)
		}
		values[i] = text
		pos += desc.length
	}
	if pos != len(raw) {
		return "", fmt.Errorf("JSONB payload has %d trailing bytes", len(raw)-pos)
	}

	switch kind {
	case 1:
		if len(values) != 1 {
			return "", fmt.Errorf("JSONB scalar wrapper contains %d values", len(values))
		}
		return values[0], nil
	case 2:
		pairs := make([]string, 0, count)
		for i := 0; i < len(values); i += 2 {
			pairs = append(pairs, values[i]+":"+values[i+1])
		}
		return "{" + strings.Join(pairs, ",") + "}", nil
	case 4:
		return "[" + strings.Join(values, ",") + "]", nil
	default:
		return "", fmt.Errorf("unsupported JSONB container kind 0x%X", kind)
	}
}

func decodeDMJSONBItem(kind byte, raw []byte, decoder textDecoder) (string, error) {
	switch kind {
	case 0:
		return decodeDMJSONBContainer(raw, decoder)
	case 1:
		var value int64
		switch len(raw) {
		case 1:
			value = int64(int8(raw[0]))
		case 2:
			value = int64(int16(binary.LittleEndian.Uint16(raw)))
		case 4:
			value = int64(int32(binary.LittleEndian.Uint32(raw)))
		case 8:
			value = int64(binary.LittleEndian.Uint64(raw))
		default:
			return "", fmt.Errorf("unsupported integer width %d", len(raw))
		}
		return fmt.Sprintf("%d", value), nil
	case 2:
		value := string(raw)
		if !json.Valid([]byte(value)) {
			return "", fmt.Errorf("invalid numeric text %q", value)
		}
		return value, nil
	case 3:
		value, ok := decoder.decode(raw)
		if !ok || strings.ContainsRune(value, '\uFFFD') || containsBadControl(value) {
			return "", fmt.Errorf("cannot decode JSONB string")
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return "", err
		}
		return string(encoded), nil
	case 4:
		return "false", nil
	case 5:
		return "true", nil
	case 6:
		return "null", nil
	default:
		return "", fmt.Errorf("unsupported JSONB item kind 0x%X", kind)
	}
}
