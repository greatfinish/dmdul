package dm

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

func TestPageGeometryRecoversDamagedHeader(t *testing.T) {
	for _, size := range []uint32{4096, 8192, 16384, 32768} {
		raw := make([]byte, 32*size)
		for n := uint32(16); n < 22; n++ {
			buildHealthyRowPage(raw[n*size:(n+1)*size], size, 4, 0, n, 1042, 1)
		}
		// Neither the header field nor the file length discriminates candidates.
		binary.LittleEndian.PutUint32(raw[systemPageSizeOffset:], 65536)
		got, source, err := ProbePageSize(bytes.NewReader(raw), int64(len(raw)))
		if err != nil || got != size || !strings.Contains(source, "multi-page") {
			t.Fatalf("size=%d got=%d source=%s err=%v", size, got, source, err)
		}
	}
}

func TestPageGeometryRejectsMissingOrAmbiguousEvidence(t *testing.T) {
	raw := make([]byte, 64*32768)
	if _, _, err := ProbePageSize(bytes.NewReader(raw), int64(len(raw))); err == nil {
		t.Fatal("blank aligned file guessed")
	}
	for n := uint32(2); n < 5; n++ {
		buildHealthyRowPage(raw[n*8192:(n+1)*8192], 8192, 4, 0, n, 1042, 1)
	}
	for n := uint32(16); n < 19; n++ {
		buildHealthyRowPage(raw[n*32768:(n+1)*32768], 32768, 4, 0, n, 1042, 1)
	}
	if _, _, err := ProbePageSize(bytes.NewReader(raw), int64(len(raw))); err == nil {
		t.Fatal("ambiguous geometry accepted")
	}
}

func TestPageGeometryHeaderSurvivesTruncation(t *testing.T) {
	raw := make([]byte, 3*32768+123)
	binary.LittleEndian.PutUint32(raw[systemPageSizeOffset:], 32768)
	got, _, err := ProbePageSize(bytes.NewReader(raw), int64(len(raw)))
	if err != nil || got != 32768 {
		t.Fatalf("got=%d err=%v", got, err)
	}
	if validPageSize(65536) {
		t.Fatal("unverified 64 KiB accepted")
	}
}

func TestPageGeometryRejectsPlausibleButContradictoryHeader(t *testing.T) {
	raw := make([]byte, 32*32768)
	binary.LittleEndian.PutUint32(raw[systemPageSizeOffset:], 8192)
	for n := uint32(16); n < 22; n++ {
		buildHealthyRowPage(raw[n*32768:(n+1)*32768], 32768, 4, 0, n, 1042, 1)
	}
	if _, _, err := ProbePageSize(bytes.NewReader(raw), int64(len(raw))); err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("%v", err)
	}
}
