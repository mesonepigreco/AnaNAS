package hash

import (
	"bytes"
	"testing"
)

func TestSumBytesStable(t *testing.T) {
	a := SumBytes([]byte("hello world"))
	b := SumBytes([]byte("hello world"))
	c := SumBytes([]byte("hello worlD"))
	if a != b {
		t.Fatalf("digest not deterministic: %v != %v", a, b)
	}
	if a == c {
		t.Fatalf("digest collision on different input")
	}
	if len(a.Hex()) != 64 {
		t.Fatalf("hex digest must be 64 chars, got %d", len(a.Hex()))
	}
}

func TestSumReaderMatchesBytes(t *testing.T) {
	data := bytes.Repeat([]byte("0123456789abcdef"), 4096)
	fromReader, err := Sum(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if fromReader != SumBytes(data) {
		t.Fatalf("reader and bytes digests differ")
	}
}

func TestBlocksSplitsExactly(t *testing.T) {
	blockSize := int64(1024)
	cases := []struct {
		size int64
		want int
	}{
		{0, 0},
		{1, 1},
		{1024, 1},
		{1025, 2},
		{5 * 1024, 5},
		{5*1024 + 1, 6},
	}
	for _, tc := range cases {
		data := bytes.Repeat([]byte{'x'}, int(tc.size))
		blocks, err := Blocks(bytes.NewReader(data), blockSize)
		if err != nil {
			t.Fatal(err)
		}
		if len(blocks) != tc.want {
			t.Fatalf("size=%d: got %d blocks, want %d", tc.size, len(blocks), tc.want)
		}
		var total int64
		for _, b := range blocks {
			if b.Size <= 0 || b.Size > blockSize {
				t.Fatalf("block %d has illegal size %d", b.Index, b.Size)
			}
			total += b.Size
		}
		if total != tc.size {
			t.Fatalf("block sizes sum to %d, want %d", total, tc.size)
		}
		// First full block digests must agree with hashing that slice alone.
		if len(blocks) > 0 {
			full := blocks[0]
			if got := SumBytes(data[:full.Size]); got != full.Digest {
				t.Fatalf("block %d digest mismatch", full.Index)
			}
		}
	}
}

func TestBlocksLastShort(t *testing.T) {
	blockSize := int64(100)
	data := bytes.Repeat([]byte("ab"), 55) // 110 bytes
	blocks, err := Blocks(bytes.NewReader(data), blockSize)
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 2 {
		t.Fatalf("want 2 blocks, got %d", len(blocks))
	}
	if blocks[0].Size != 100 || blocks[1].Size != 10 {
		t.Fatalf("unexpected sizes %d,%d", blocks[0].Size, blocks[1].Size)
	}
}

func TestBlockDigestsMatchesBlocks(t *testing.T) {
	data := bytes.Repeat([]byte("payload-"), 5000)
	bs, err := Blocks(bytes.NewReader(data), 2048)
	if err != nil {
		t.Fatal(err)
	}
	ds, err := BlockDigests(bytes.NewReader(data), 2048)
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != len(bs) {
		t.Fatalf("digest count %d != block count %d", len(ds), len(bs))
	}
	for i := range bs {
		if ds[i] != bs[i].Digest {
			t.Fatalf("digest %d differs", i)
		}
	}
}
