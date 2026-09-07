package dm

import (
	"encoding/binary"
	"fmt"
	"math"
	"strconv"
	"strings"
)

func decodeDMVector(raw []byte, col columnDef) (dmVectorValue, error) {
	// The two format/version bytes after 0x01 differ between DM9 builds and
	// between rows created before/after an upgrade (observed 0xF4,0x03 and
	// 0x13,0x04 in one database). The length, dimension and element fields are
	// stable, so validate those structural fields instead of pinning a build id.
	if len(raw) < 17 || raw[0] != 0x01 {
		return dmVectorValue{}, fmt.Errorf("invalid VECTOR header")
	}
	payloadLen := int(binary.LittleEndian.Uint32(raw[9:13]))
	if payloadLen != len(raw)-13 || payloadLen < 4 {
		return dmVectorValue{}, fmt.Errorf("invalid VECTOR payload length %d/%d", payloadLen, len(raw)-13)
	}
	payload := raw[13:]
	dimension := int(binary.LittleEndian.Uint16(payload[0:2]))
	if dimension <= 0 {
		return dmVectorValue{}, fmt.Errorf("invalid VECTOR dimension %d", dimension)
	}
	if col.Length > 0 && dimension != int(col.Length) {
		return dmVectorValue{}, fmt.Errorf("VECTOR dimension %d, expected %d", dimension, col.Length)
	}
	elementKind := payload[2]
	sparse := payload[3] != 0
	data := payload[4:]

	var text string
	var err error
	if sparse {
		text, err = decodeDMSparseVector(dimension, elementKind, data)
	} else {
		text, err = decodeDMDenseVector(dimension, elementKind, data)
	}
	if err != nil {
		return dmVectorValue{}, err
	}
	return dmVectorValue{text: text, raw: append(dmBinary(nil), raw...)}, nil
}

func decodeDMDenseVector(dimension int, elementKind byte, raw []byte) (string, error) {
	values := make([]string, dimension)
	switch elementKind {
	case 0x01:
		if len(raw) != dimension*4 {
			return "", fmt.Errorf("FLOAT32 VECTOR payload length %d, expected %d", len(raw), dimension*4)
		}
		for i := range values {
			bits := binary.LittleEndian.Uint32(raw[i*4:])
			values[i] = strconv.FormatFloat(float64(math.Float32frombits(bits)), 'g', -1, 32)
		}
	case 0x02:
		if len(raw) != dimension*8 {
			return "", fmt.Errorf("FLOAT64 VECTOR payload length %d, expected %d", len(raw), dimension*8)
		}
		for i := range values {
			bits := binary.LittleEndian.Uint64(raw[i*8:])
			values[i] = strconv.FormatFloat(math.Float64frombits(bits), 'g', -1, 64)
		}
	case 0x03:
		if len(raw) != dimension {
			return "", fmt.Errorf("INT8 VECTOR payload length %d, expected %d", len(raw), dimension)
		}
		for i := range values {
			values[i] = strconv.FormatInt(int64(int8(raw[i])), 10)
		}
	case 0x04:
		expected := (dimension + 7) / 8
		if len(raw) != expected {
			return "", fmt.Errorf("BINARY VECTOR payload length %d, expected %d", len(raw), expected)
		}
		values = values[:len(raw)]
		for i := range values {
			values[i] = strconv.FormatUint(uint64(raw[i]), 10)
		}
	default:
		return "", fmt.Errorf("unsupported VECTOR element kind 0x%02X", elementKind)
	}
	return "[" + strings.Join(values, ",") + "]", nil
}

func decodeDMSparseVector(dimension int, elementKind byte, raw []byte) (string, error) {
	if elementKind != 0x01 {
		return "", fmt.Errorf("unsupported sparse VECTOR element kind 0x%02X", elementKind)
	}
	if len(raw) < 2 {
		return "", fmt.Errorf("sparse VECTOR count is missing")
	}
	count := int(binary.LittleEndian.Uint16(raw[0:2]))
	valuesStart := 2 + count*2
	if count > dimension || len(raw) != valuesStart+count*4 {
		return "", fmt.Errorf("invalid sparse VECTOR count or payload length: count=%d length=%d", count, len(raw))
	}
	indexes := make([]string, count)
	values := make([]string, count)
	for i := 0; i < count; i++ {
		index := int(binary.LittleEndian.Uint16(raw[2+i*2:]))
		if index >= dimension {
			return "", fmt.Errorf("sparse VECTOR index %d exceeds dimension %d", index, dimension)
		}
		indexes[i] = strconv.Itoa(index)
		bits := binary.LittleEndian.Uint32(raw[valuesStart+i*4:])
		values[i] = strconv.FormatFloat(float64(math.Float32frombits(bits)), 'g', -1, 32)
	}
	return fmt.Sprintf("[%d,[%s],[%s]]", dimension, strings.Join(indexes, ","), strings.Join(values, ",")), nil
}
