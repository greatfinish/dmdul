package dm

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func FuzzPageGeometry(f *testing.F) {
	seed := make([]byte, 4*8192)
	binary.LittleEndian.PutUint32(seed[0x84:], 8192)
	f.Add(seed)
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > 2<<20 {
			t.Skip()
		}
		size, _, err := ProbePageSize(bytes.NewReader(raw), int64(len(raw)))
		if err == nil && (!validPageSize(size) || int(size) > len(raw)) {
			t.Fatalf("invalid geometry %d", size)
		}
	})
}

func FuzzDataPageSlots(f *testing.F) {
	seed := make([]byte, 8192)
	putTestIntDataPage(seed, 4, 0, 16, 1042, 42)
	f.Add(seed, uint32(8192))
	f.Add([]byte{}, uint32(0))
	f.Fuzz(func(t *testing.T, raw []byte, ps uint32) {
		if len(raw) > 32768 {
			t.Skip()
		}
		for _, residual := range []bool{false, true} {
			rows := locateRowsInDataPageMode(raw, ps, 0, residual)
			for _, row := range rows {
				if int(row.offset)+int(row.length) > len(raw) || row.length < 3 {
					t.Fatal("row out of bounds")
				}
				if !residual && (!row.fromSlot || row.deleted) {
					t.Fatal("normal scan recovered physical hole or deleted row")
				}
			}
		}
	})
}

func FuzzDataRowMetadata(f *testing.F) {
	f.Add([]byte{0, 7, 0, 1, 0, 0, 0})
	f.Add([]byte{0, 3, 0})
	undo := make([]byte, 8192)
	binary.LittleEndian.PutUint32(undo[20:], 0x1d)
	binary.LittleEndian.PutUint16(undo[43:], 55)
	binary.LittleEndian.PutUint16(undo[45:], 100)
	binary.LittleEndian.PutUint16(undo[55:], 45)
	undo[57], undo[58] = 3, 2
	binary.LittleEndian.PutUint16(undo[98:], 55)
	f.Add(undo)
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > 32768 {
			t.Skip()
		}
		cols := []columnDef{{ColID: 0, Name: "ID", DataType: "INT", Length: 4, Nullable: "Y"}, {ColID: 1, Name: "TXT", DataType: "VARCHAR", Length: 1024, Nullable: "Y"}}
		_, start, end, err := parseDataRowValuesWithMetadata(raw, cols, textDecoder{preferred: "utf-8"}, nil)
		if err == nil && (start < 0 || end < start || end > len(raw)) {
			t.Fatalf("bad range %d..%d", start, end)
		}
		_, _ = decodeDataRowControlTail(raw)
		_, _ = decodeUndoRecordEvidence(raw, 55)
	})
}

func FuzzDMASMMetadata(f *testing.F) {
	seed := make([]byte, 512)
	copy(seed[4:], "+DATA/SYSTEM.DBF")
	f.Add(seed)
	mirror := make([]byte, 512)
	mirror[0] = 1
	copy(mirror[3:], "+DATA/SYSTEM.DBF")
	f.Add(mirror)
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > 4096 {
			t.Skip()
		}
		_, _ = parseRawASMInode(raw, 0, 0, 0)
		group := RawASMGroup{groupID: 1, auSize: 1024 * 1024}
		_, _ = group.parseMirrorInode(raw, 0, 0, 0)
		_, _ = parseRawASMMirrorDAddr(raw)
		desc, ok := parseRawASMMirrorDescriptor(raw)
		if ok && len(desc.mirrors) > 2 {
			t.Fatal("too many mirror copies")
		}
	})
}

func FuzzDMPContainer(f *testing.F) {
	path := filepath.Join(f.TempDir(), "fuzz.dmp")
	w, err := NewDMPDataWriter(DMPDataOptions{OutputPath: path, Schema: "APP", Table: "T", ColumnCount: 1})
	if err != nil {
		f.Fatal(err)
	}
	if err = w.WriteRow([]DMPField{DMPShortField([]byte("data"))}); err != nil {
		f.Fatal(err)
	}
	if _, err = w.Close(); err != nil {
		f.Fatal(err)
	}
	seed, err := os.ReadFile(path)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(seed)
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > 1<<20 {
			t.Skip()
		}
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		_, _ = InspectDMP(path)
	})
}
