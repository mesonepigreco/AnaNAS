package manifest

import (
	"encoding/binary"
	"reflect"
	"strings"
	"testing"

	"nas-sync/internal/diff"
	"nas-sync/internal/hash"
)

func fixture() *Manifest {
	return &Manifest{BlockSize: 1024, Content: diff.Manifest{ID: strings.Repeat("a", 64), Size: 1027,
		Blocks: []diff.Block{{Digest: hash.SumBytes(make([]byte, 1024)), Size: 1024}, {Digest: hash.SumBytes([]byte("end")), Size: 3}},
	}}
}

func TestRoundTrip(t *testing.T) {
	for _, m := range []*Manifest{fixture(), {BlockSize: 65536, Content: diff.Manifest{ID: strings.Repeat("b", 64)}},
		{BlockSize: 65536, Content: diff.Manifest{ID: strings.Repeat("c", 64), Tombstone: true}},
		{BlockSize: 65536, Content: diff.Manifest{ID: strings.Repeat("d", 64), Directory: true}}} {
		encoded, err := Encode(m)
		if err != nil {
			t.Fatal(err)
		}
		got, err := Decode(encoded)
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Content.Blocks) == 0 {
			got.Content.Blocks = nil
		}
		if !reflect.DeepEqual(got, m) {
			t.Fatalf("roundtrip mismatch: %+v", got)
		}
	}
}

func TestRejectCorruptEncoding(t *testing.T) {
	valid, err := Encode(fixture())
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]func([]byte) []byte{
		"version":           func(b []byte) []byte { b[5] = 2; return b },
		"algorithm":         func(b []byte) []byte { b[7] = 2; return b },
		"flags":             func(b []byte) []byte { b[8] = 4; return b },
		"both types":        func(b []byte) []byte { b[8] = 3; return b },
		"directory payload": func(b []byte) []byte { b[8] = 2; return b },
		"reserved":          func(b []byte) []byte { b[28] = 1; return b },
		"oversize count":    func(b []byte) []byte { binary.BigEndian.PutUint32(b[24:], MaxBlocks+1); return b },
		"size overflow":     func(b []byte) []byte { b[16] = 255; return b },
		"bad id":            func(b []byte) []byte { b[32] = '/'; return b },
		"block layout":      func(b []byte) []byte { binary.BigEndian.PutUint32(b[128:], 1023); return b },
		"tombstone payload": func(b []byte) []byte { b[8] = 1; return b },
		"truncated":         func(b []byte) []byte { return b[:len(b)-1] },
		"trailing":          func(b []byte) []byte { return append(b, 0) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Decode(mutate(append([]byte(nil), valid...))); err == nil {
				t.Fatal("accepted malformed manifest")
			}
		})
	}
}

func FuzzDecode(f *testing.F) {
	b, _ := Encode(fixture())
	f.Add(b)
	f.Add([]byte("bad header"))
	f.Fuzz(func(t *testing.T, b []byte) {
		m, err := Decode(b)
		if err != nil {
			return
		}
		roundtrip, err := Encode(m)
		if err != nil || string(roundtrip) != string(b) {
			t.Fatal("noncanonical accepted encoding")
		}
	})
}
