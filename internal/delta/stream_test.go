package delta

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math/rand"
	"testing"

	"nas-sync/internal/hash"
)

func randomBytes(n int) []byte {
	p := make([]byte, n)
	rand.New(rand.NewSource(int64(n))).Read(p)
	return p
}

func encoded(t testing.TB, base, target []byte, blockSize int) (*Signature, []byte, Stats) {
	t.Helper()
	s, err := Build(context.Background(), bytes.NewReader(base), int64(len(base)), blockSize)
	if err != nil {
		t.Fatal(err)
	}
	var wire bytes.Buffer
	stats, err := Encode(context.Background(), bytes.NewReader(target), int64(len(target)), s, &wire)
	if err != nil {
		t.Fatal(err)
	}
	return s, wire.Bytes(), stats
}

func roundTrip(t testing.TB, base, target []byte, blockSize int) (Stats, int) {
	t.Helper()
	s, wire, sent := encoded(t, base, target, blockSize)
	var out bytes.Buffer
	got, err := Apply(context.Background(), bytes.NewReader(wire), bytes.NewReader(base), s.Size, s.Digest, &out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), target) {
		t.Fatal("reconstructed bytes differ")
	}
	if sent.Digest != hash.SumBytes(target) || got.Digest != sent.Digest || got.LiteralBytes != sent.LiteralBytes || got.ReusedBytes != sent.ReusedBytes || got.Operations != sent.Operations {
		t.Fatalf("inconsistent transfer statistics: sent=%+v received=%+v", sent, got)
	}
	return sent, len(wire)
}

func TestRoundTripReuse(t *testing.T) {
	const block = 1024
	base := randomBytes(12*block + 137)
	edit := bytes.Clone(base)
	copy(edit[4*block:5*block], randomBytes(block))
	insert := append(bytes.Clone(base[:17]), append([]byte{99}, base[17:]...)...)
	deleted := append(bytes.Clone(base[:17]), base[18:]...)
	for _, tc := range []struct {
		name         string
		base, target []byte
		maxLiteral   int64
	}{
		{"empty", nil, nil, 0},
		{"new", nil, base, int64(len(base))},
		{"truncate-empty", base, nil, 0},
		{"identical", base, base, 0},
		{"short-identical", base[:137], base[:137], 0},
		{"overwrite-block", base, edit, block},
		{"insert-byte", base, insert, block + 1},
		{"delete-byte", base, deleted, block - 1},
		{"append", base, append(bytes.Clone(base), randomBytes(71)...), 137 + 71},
		{"truncate", base, base[:5*block+19], 19},
		{"duplicates", bytes.Repeat(base[:block], 8), bytes.Repeat(base[:block], 9), 0},
		{"prefix-shift", base, append(randomBytes(2*block+3), base...), 2*block + 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stats, _ := roundTrip(t, tc.base, tc.target, block)
			if stats.LiteralBytes > tc.maxLiteral {
				t.Fatalf("literal bytes %d exceed %d", stats.LiteralBytes, tc.maxLiteral)
			}
		})
	}
}

func TestRollingAndBoundaryEdits(t *testing.T) {
	const block = 1024
	base := randomBytes(4*block + 131)
	for _, at := range []int{0, 1, block - 1, block, block + 1, 2*block - 1, len(base) - 1, len(base)} {
		for _, n := range []int{1, 131, block - 1, block, block + 1} {
			target := append(bytes.Clone(base[:at]), randomBytes(n)...)
			target = append(target, base[at:]...)
			roundTrip(t, base, target, block)
		}
	}
	data := randomBytes(8 * block)
	sum := checksum(data[:block])
	for i := block; i < len(data); i++ {
		sum.shift(data[i-block], data[i], block)
		if sum != checksum(data[i-block+1:i+1]) {
			t.Fatalf("rolling mismatch at %d", i)
		}
	}
}

func TestRejectCorruptStreams(t *testing.T) {
	base := randomBytes(2048)
	s, copyWire, _ := encoded(t, base, base, 1024)
	_, literalWire, _ := encoded(t, base, randomBytes(137), 1024)
	for _, tc := range []struct {
		name   string
		wire   []byte
		mutate func([]byte)
	}{
		{"magic", copyWire, func(p []byte) { p[0] ^= 1 }},
		{"block-size", copyWire, func(p []byte) { binary.BigEndian.PutUint32(p[8:12], 0xffffffff) }},
		{"size-overflow", copyWire, func(p []byte) { binary.BigEndian.PutUint64(p[20:28], 1<<63) }},
		{"wrong-base", copyWire, func(p []byte) { p[28] ^= 1 }},
		{"opcode", copyWire, func(p []byte) { p[streamHeader] = 9 }},
		{"copy-range-overflow", copyWire, func(p []byte) { binary.BigEndian.PutUint64(p[streamHeader+1:], ^uint64(0)) }},
		{"copy-unaligned", copyWire, func(p []byte) { binary.BigEndian.PutUint64(p[streamHeader+1:], 1) }},
		{"copy-zero", copyWire, func(p []byte) { binary.BigEndian.PutUint32(p[streamHeader+9:], 0) }},
		{"copy-digest", copyWire, func(p []byte) { p[streamHeader+13] ^= 1 }},
		{"literal-too-large", literalWire, func(p []byte) { binary.BigEndian.PutUint32(p[streamHeader+1:], 1025) }},
		{"literal-payload", literalWire, func(p []byte) { p[streamHeader+37] ^= 1 }},
		{"trailer-digest", copyWire, func(p []byte) { p[len(p)-1] ^= 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wire := bytes.Clone(tc.wire)
			tc.mutate(wire)
			if _, err := Apply(context.Background(), bytes.NewReader(wire), bytes.NewReader(base), s.Size, s.Digest, io.Discard); err == nil {
				t.Fatal("accepted corrupt stream")
			}
		})
	}
	for cut := 0; cut < len(literalWire); cut++ {
		if _, err := Apply(context.Background(), bytes.NewReader(literalWire[:cut]), bytes.NewReader(base), s.Size, s.Digest, io.Discard); err == nil {
			t.Fatalf("accepted truncation at %d", cut)
		}
	}
	if _, err := Apply(context.Background(), bytes.NewReader(append(bytes.Clone(copyWire), 0)), bytes.NewReader(base), s.Size, s.Digest, io.Discard); err == nil {
		t.Fatal("accepted trailing data")
	}
	changed := bytes.Clone(base)
	changed[0] ^= 1
	var out bytes.Buffer
	if _, err := Apply(context.Background(), bytes.NewReader(copyWire), bytes.NewReader(changed), s.Size, s.Digest, &out); err == nil || out.Len() != 0 {
		t.Fatal("changed base was written before verification")
	}
}

type stalledReader struct{}

func (stalledReader) Read([]byte) (int, error) { return 0, nil }

type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) { return len(p) / 2, nil }

func TestIOFailuresAndCancellation(t *testing.T) {
	base := randomBytes(2048)
	s, wire, _ := encoded(t, base, base, 1024)
	for _, size := range []int64{0, 2047, 2049} {
		if _, err := Build(context.Background(), bytes.NewReader(base), size, 1024); err == nil {
			t.Fatalf("Build accepted wrong size %d", size)
		}
		if _, err := Encode(context.Background(), bytes.NewReader(base), size, s, io.Discard); err == nil {
			t.Fatalf("Encode accepted wrong size %d", size)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Build(ctx, bytes.NewReader(base), s.Size, 1024); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := Encode(ctx, bytes.NewReader(base), s.Size, s, io.Discard); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := Apply(ctx, bytes.NewReader(wire), bytes.NewReader(base), s.Size, s.Digest, io.Discard); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := Build(context.Background(), stalledReader{}, 1024, 1024); !errors.Is(err, io.ErrNoProgress) {
		t.Fatal(err)
	}
	if _, err := Encode(context.Background(), stalledReader{}, s.Size, s, io.Discard); !errors.Is(err, io.ErrNoProgress) {
		t.Fatal(err)
	}
	if _, err := Apply(context.Background(), stalledReader{}, nil, s.Size, s.Digest, io.Discard); !errors.Is(err, io.ErrNoProgress) {
		t.Fatal(err)
	}
	if _, err := Encode(context.Background(), bytes.NewReader(base), s.Size, s, shortWriter{}); !errors.Is(err, io.ErrShortWrite) {
		t.Fatal(err)
	}
	if _, err := Apply(context.Background(), bytes.NewReader(wire), bytes.NewReader(base), s.Size, s.Digest, shortWriter{}); !errors.Is(err, io.ErrShortWrite) {
		t.Fatal(err)
	}
}

func TestVerificationBudget(t *testing.T) {
	// All-zero windows have the same weak checksum. An untrusted signature
	// claims that checksum with a different strong digest: fail boundedly.
	s := &Signature{BlockSize: 1024, Size: 1024, Blocks: []Block{{Strong: hash.SumBytes([]byte("different"))}}}
	stats, err := Encode(context.Background(), bytes.NewReader(make([]byte, 4096)), 4096, s, io.Discard)
	if !errors.Is(err, ErrVerificationBudget) {
		t.Fatal(err)
	}
	if stats.StrongChecks > 4*4+1025 {
		t.Fatalf("verification work unbounded: %d", stats.StrongChecks)
	}
}

func FuzzRoundTrip(f *testing.F) {
	f.Add([]byte("base"), []byte("target"))
	f.Add(randomBytes(2049), randomBytes(3100))
	f.Fuzz(func(t *testing.T, base, target []byte) {
		if len(base) > 32768 || len(target) > 32768 {
			t.Skip()
		}
		roundTrip(t, base, target, 1024)
	})
}

func FuzzApply(f *testing.F) {
	base := randomBytes(2048)
	s, wire, _ := encoded(f, base, append([]byte{99}, base...), 1024)
	f.Add(wire)
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 65536 {
			t.Skip()
		}
		var out bytes.Buffer
		stats, err := Apply(context.Background(), bytes.NewReader(data), bytes.NewReader(base), s.Size, s.Digest, &out)
		if err == nil && (stats.Digest != hash.SumBytes(out.Bytes()) || stats.LiteralBytes+stats.ReusedBytes != int64(out.Len())) {
			t.Fatal("accepted stream with inconsistent output")
		}
	})
}

func TestProductionBlockTraffic(t *testing.T) {
	const block = 64 * 1024
	base := randomBytes(4 * 1024 * 1024)
	target := append([]byte{99}, base...)
	stats, wireBytes := roundTrip(t, base, target, block)
	if stats.LiteralBytes != 1 || stats.ReusedBytes != int64(len(base)) {
		t.Fatalf("shift transferred unchanged payload: %+v", stats)
	}
	// Header + one literal frame/byte + 64 copy frames + whole-file trailer.
	if wireBytes != 60+37+1+64*45+33 {
		t.Fatalf("unexpected stream size %d", wireBytes)
	}
}

func BenchmarkEncode(b *testing.B) {
	const block = 64 * 1024
	base := randomBytes(4 * 1024 * 1024)
	for _, tc := range []struct {
		name   string
		target []byte
	}{
		{"identical", base},
		{"insert-byte", append([]byte{99}, base...)},
		{"all-new", randomBytes(len(base) + 1)},
	} {
		b.Run(tc.name, func(b *testing.B) {
			s, err := Build(context.Background(), bytes.NewReader(base), int64(len(base)), block)
			if err != nil {
				b.Fatal(err)
			}
			b.SetBytes(int64(len(tc.target)))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := Encode(context.Background(), bytes.NewReader(tc.target), int64(len(tc.target)), s, io.Discard); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
