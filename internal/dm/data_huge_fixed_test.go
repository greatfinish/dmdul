package dm

import (
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHugeFixedPresenceBitmap(t *testing.T) {
	for _, typ := range []string{"INT", "BIGINT", "SMALLINT", "DOUBLE", "DATE"} {
		t.Run(typ, func(t *testing.T) {
			column := columnDef{Name: "V", DataType: typ, Nullable: "Y"}
			meta := hugeColumnSection{offset: 4096, count: 9, nlen: 4096, nulls: 3, nullsKnown: true}
			width, _, err := hugeColumnSectionLayout(column, meta)
			if err != nil {
				t.Fatal(err)
			}
			data := make([]byte, 8192)
			section := data[4096:]
			copy(section, hugeHFSSectionMagic)
			binary.LittleEndian.PutUint32(section[4:], 4096)
			binary.LittleEndian.PutUint16(section[24:], hugeFixedTypeID(column))
			// NULL rows 3, 6 and 9, including the first bit of a second byte.
			section[4094] = 0xDB
			section[4095] = 0
			for i := 0; i < 9; i++ {
				b := section[128+i*width : 128+(i+1)*width]
				switch typ {
				case "BIGINT":
					binary.LittleEndian.PutUint64(b, uint64(i+1)*1000000000)
				case "DOUBLE":
					binary.LittleEndian.PutUint64(b, math.Float64bits(float64(i)+0.125))
				case "DATE":
					binary.LittleEndian.PutUint16(b, 2026)
					b[2] = 9
					b[3] = byte(i + 1)
					binary.LittleEndian.PutUint16(b[10:], 1000)
				default:
					binary.LittleEndian.PutUint32(b, uint32(i+1))
				}
			}
			dir := t.TempDir()
			path := filepath.Join(dir, "COL0000_0000000000.dta")
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			reader, _, err := openHugeColumnSection(dir, column, meta, textDecoder{})
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 9; i++ {
				got, err := reader.next()
				if err != nil {
					t.Fatal(err)
				}
				if (got == nil) != (i%3 == 2) {
					t.Fatalf("row %d value=%v", i+1, got)
				}
				if i == 0 {
					wants := map[string]any{"INT": int32(1), "SMALLINT": int16(1), "BIGINT": int64(1000000000), "DOUBLE": float64(0.125), "DATE": "2026-09-01"}
					if got != wants[typ] {
						t.Fatalf("value=%v want=%v", got, wants[typ])
					}
				}
			}
			closeHugeColumnReaders([]*hugeColumnSectionReader{reader})
			meta.nulls = 2
			if _, _, err := openHugeColumnSection(dir, column, meta, textDecoder{}); err == nil || !strings.Contains(err.Error(), "N_NULL") {
				t.Fatalf("corrupted count: %v", err)
			}
			meta.nulls = 3
			binary.LittleEndian.PutUint16(data[4096+24:], 999)
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			if _, _, err := openHugeColumnSection(dir, column, meta, textDecoder{}); err == nil || !strings.Contains(err.Error(), "type mismatch") {
				t.Fatalf("wrong type: %v", err)
			}
		})
	}
}

func TestHugeFixedValuesRejectUnknownLayouts(t *testing.T) {
	raw := make([]byte, 4)
	binary.LittleEndian.PutUint32(raw, 32768)
	if _, err := decodeHugeFixedValue(columnDef{DataType: "SMALLINT"}, raw); err == nil {
		t.Fatal("accepted out of range SMALLINT")
	}
	date := []byte{0xEA, 7, 2, 30, 0, 0, 0, 0, 0, 0, 0xE8, 3, 0}
	if _, err := decodeHugeFixedValue(columnDef{DataType: "DATE"}, date); err == nil {
		t.Fatal("accepted February 30")
	}
	date[3] = 20
	date[4] = 1
	if _, err := decodeHugeFixedValue(columnDef{DataType: "DATE"}, date); err == nil {
		t.Fatal("accepted unknown time payload")
	}
	meta := hugeColumnSection{offset: 4096, count: 9, nlen: 130}
	dir := t.TempDir()
	writeSyntheticHugeSection(t, dir, 0, 0, []byte{0, 0}, nil)
	if _, _, err := openHugeColumnSection(dir, columnDef{DataType: "INT", Nullable: "Y"}, meta, textDecoder{}); err == nil {
		t.Fatal("accepted overlapping payload and bitmap")
	}
}

func TestHugeNullableIntDM8Fixture(t *testing.T) {
	raw, err := os.ReadFile("testdata/dm8_hfs_nullable_int.bin")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "COL0001_0000000000.dta"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	meta := hugeColumnSection{colID: 1, offset: 4096, count: 1024, nlen: 8192, nulls: 341, nullsKnown: true}
	reader, _, err := openHugeColumnSection(dir, columnDef{ColID: 1, Name: "NI", DataType: "INT", Nullable: "Y"}, meta, textDecoder{})
	if err != nil {
		t.Fatal(err)
	}
	defer closeHugeColumnReaders([]*hugeColumnSectionReader{reader})
	for i := int32(1); i <= 1024; i++ {
		got, err := reader.next()
		if err != nil {
			t.Fatal(err)
		}
		var want any = i
		if i%3 == 0 {
			want = nil
		}
		if got != want {
			t.Fatalf("row %d got=%v want=%v", i, got, want)
		}
	}
}
