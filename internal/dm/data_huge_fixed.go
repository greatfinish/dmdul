package dm

import (
	"encoding/binary"
	"fmt"
	"time"
)

func hugeFixedTypeID(column columnDef) uint16 {
	switch normalizeDataType(column.DataType) {
	case "SMALLINT":
		return 6
	case "INT", "INTEGER", "PLS_INTEGER":
		return 7
	case "BIGINT":
		return 8
	case "DOUBLE", "DOUBLE PRECISION":
		return 11
	case "DATE":
		return 14
	}
	return 0
}

func (reader *hugeColumnSectionReader) initFixedNulls(length int64) error {
	if length == 0 {
		if reader.meta.nullsKnown && reader.meta.nulls != 0 {
			return fmt.Errorf("HFS non-nullable column %s has N_NULL=%d", reader.column.Name, reader.meta.nulls)
		}
		return nil
	}
	if length > 32*1024*1024 {
		return fmt.Errorf("HFS NULL bitmap exceeds 32 MiB limit")
	}
	reader.presentBits = make([]byte, int(length))
	start := reader.meta.offset + int64(reader.meta.nlen) - length
	if _, err := reader.file.ReadAt(reader.presentBits, start); err != nil {
		return fmt.Errorf("read HFS NULL bitmap: %w", err)
	}
	// Unlike row metadata, HFS fixed sections use an MSB-first presence bit:
	// one means a value, zero means NULL. The bitmap is at the section end.
	var nulls uint32
	for i := uint32(0); i < reader.meta.count; i++ {
		if reader.presentBits[i/8]&(0x80>>(i%8)) == 0 {
			nulls++
		}
	}
	if reader.meta.nullsKnown && nulls != reader.meta.nulls {
		return fmt.Errorf("HFS column %s NULL bitmap count=%d differs from AUX N_NULL=%d", reader.column.Name, nulls, reader.meta.nulls)
	}
	return nil
}

func decodeHugeFixedValue(column columnDef, raw []byte) (any, error) {
	switch normalizeDataType(column.DataType) {
	case "SMALLINT":
		if len(raw) != 4 {
			return nil, fmt.Errorf("HFS SMALLINT requires four bytes")
		}
		value := int32(binary.LittleEndian.Uint32(raw))
		if value < -32768 || value > 32767 {
			return nil, fmt.Errorf("HFS SMALLINT out of range: %d", value)
		}
		return int16(value), nil
	case "DATE":
		if len(raw) != 13 {
			return nil, fmt.Errorf("HFS DATE requires thirteen bytes")
		}
		// Only the observed, unpacked AD date form is accepted. Nonzero time
		// fields or another timezone marker require a separate layout probe.
		for _, b := range raw[4:10] {
			if b != 0 {
				return nil, fmt.Errorf("unverified HFS DATE time payload")
			}
		}
		if binary.LittleEndian.Uint16(raw[10:]) != 1000 || raw[12] != 0 {
			return nil, fmt.Errorf("unverified HFS DATE suffix")
		}
		year, month, day := int(binary.LittleEndian.Uint16(raw)), int(raw[2]), int(raw[3])
		date := time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.UTC)
		if year < 1 || year > 9999 || date.Year() != year || int(date.Month()) != month || date.Day() != day {
			return nil, fmt.Errorf("invalid HFS DATE %d-%d-%d", year, month, day)
		}
		return date.Format("2006-01-02"), nil
	}
	value, end, err := parseFixedDataValuePresent(column, raw, 0)
	if err != nil {
		return nil, err
	}
	if end != len(raw) {
		return nil, fmt.Errorf("fixed decoder consumed %d/%d bytes", end, len(raw))
	}
	return value, nil
}
