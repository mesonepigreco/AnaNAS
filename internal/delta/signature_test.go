package delta

import (
	"bytes"
	"context"
	"encoding/binary"
	"reflect"
	"testing"

	"nas-sync/internal/hash"
)

func TestSignatureRoundTrip(t *testing.T) {
	for _, size := range []int{0, 1, 1023, 1024, 1025, 16384} {
		data := randomBytes(size)
		s, err := Build(context.Background(), bytes.NewReader(data), int64(size), 1024)
		if err != nil {
			t.Fatal(err)
		}
		if s.Digest != hash.SumBytes(data) {
			t.Fatal("wrong whole digest")
		}
		wire, err := s.MarshalBinary()
		if err != nil {
			t.Fatal(err)
		}
		got, err := ParseSignature(wire)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(s, got) {
			t.Fatal("signature roundtrip differs")
		}
	}
}

func TestSignatureInvalidBounds(t *testing.T) {
	s, err := Build(context.Background(), bytes.NewReader(randomBytes(1024)), 1024, 1024)
	if err != nil {
		t.Fatal(err)
	}
	wire, _ := s.MarshalBinary()
	for _, mutate := range []func([]byte){
		func(p []byte) { p[0] ^= 1 },
		func(p []byte) { binary.BigEndian.PutUint32(p[8:12], 0) },
		func(p []byte) { binary.BigEndian.PutUint32(p[8:12], 1025) },
		func(p []byte) { binary.BigEndian.PutUint32(p[12:16], 0xffffffff) },
		func(p []byte) { binary.BigEndian.PutUint64(p[16:24], 1<<63) },
		func(p []byte) { binary.BigEndian.PutUint64(p[16:24], 0) },
	} {
		p := bytes.Clone(wire)
		mutate(p)
		if _, err := ParseSignature(p); err == nil {
			t.Fatal("accepted invalid signature")
		}
	}
	for n := 0; n < len(wire); n++ {
		if _, err := ParseSignature(wire[:n]); err == nil {
			t.Fatalf("accepted truncated signature at %d", n)
		}
	}
	if _, err := ParseSignature(append(wire, 0)); err == nil {
		t.Fatal("accepted trailing signature bytes")
	}
}

func FuzzParseSignature(f *testing.F) {
	s, _ := Build(context.Background(), bytes.NewReader(randomBytes(1024)), 1024, 1024)
	wire, _ := s.MarshalBinary()
	f.Add(wire)
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, p []byte) {
		s, err := ParseSignature(p)
		if err != nil {
			return
		}
		got, err := s.MarshalBinary()
		if err != nil || !bytes.Equal(got, p) {
			t.Fatal("accepted non-canonical signature")
		}
	})
}
