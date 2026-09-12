package delta

import (
	"bytes"
	"context"
	"math/rand"
	"testing"
)

func TestWireBoundCoversEmptyLiteralAndMixedEncodings(t *testing.T) {
	for _, size := range []int{0, 1, 1023, 1024, 65535, 65536, 65537, 512 * 1024} {
		base := make([]byte, size)
		rand.New(rand.NewSource(int64(size))).Read(base)
		sig, err := Build(context.Background(), bytes.NewReader(base), int64(size), 65536)
		if err != nil {
			t.Fatal(err)
		}
		for _, target := range [][]byte{nil, base, append([]byte{17}, base...), bytes.Repeat([]byte{29}, size)} {
			var wire bytes.Buffer
			if _, err := Encode(context.Background(), bytes.NewReader(target), int64(len(target)), sig, &wire); err != nil {
				t.Fatal(err)
			}
			bound, err := WireBound(int64(len(target)))
			if err != nil || int64(wire.Len()) > bound {
				t.Fatal("encoded content exceeds reservation", size, len(target), wire.Len(), bound, err)
			}
		}
	}
	for _, size := range []int64{-1, (8 << 30) + 1} {
		if _, err := WireBound(size); err == nil {
			t.Fatal("invalid size accepted", size)
		}
	}
	if bound, err := WireBound(8 << 30); err != nil || bound <= 8<<30 {
		t.Fatal("large target bound overflowed", bound, err)
	}
	if bound, err := WireBound(1); err != nil || bound > 256 {
		t.Fatal("tiny target reserves global frame allowance", bound, err)
	}
}
