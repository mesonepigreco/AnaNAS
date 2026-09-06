package hash

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
)

func TestStreamRejectsInvalidSizesBeforeRead(t *testing.T) {
	for _, size := range []int64{0, -1, MaxBlockSize + 1, 1 << 62} {
		if err := Stream(context.Background(), panicReader{}, size, func(Block) error { return nil }); err == nil {
			t.Fatalf("accepted %d", size)
		}
	}
}

type panicReader struct{}

func (panicReader) Read([]byte) (int, error) { panic("must not read") }
func TestStreamCancellationAndCallbackFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Stream(ctx, panicReader{}, 16, func(Block) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	n := 0
	err := Stream(ctx, bytes.NewReader(make([]byte, 100)), 16, func(Block) error { n++; cancel(); return nil })
	if !errors.Is(err, context.Canceled) || n != 1 {
		t.Fatalf("blocks=%d err=%v", n, err)
	}
	want := errors.New("sink failed")
	err = Stream(context.Background(), bytes.NewReader(make([]byte, 100)), 16, func(Block) error { return want })
	if !errors.Is(err, want) {
		t.Fatal(err)
	}
}
func TestStreamReadErrorDoesNotEmitPartialBlock(t *testing.T) {
	n := 0
	want := errors.New("read failed")
	err := Stream(context.Background(), errorReader{want}, 16, func(Block) error { n++; return nil })
	if n != 0 || !errors.Is(err, want) {
		t.Fatalf("blocks=%d err=%v", n, err)
	}
}

type errorReader struct{ err error }

func (r errorReader) Read(p []byte) (int, error) { p[0] = 1; return 1, r.err }
func BenchmarkStream(b *testing.B) {
	data := make([]byte, 16<<20)
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	for b.Loop() {
		if err := Stream(context.Background(), bytes.NewReader(data), DefaultBlockSize, func(Block) error { return nil }); err != nil {
			b.Fatal(err)
		}
	}
}
func BenchmarkBlockDigests(b *testing.B) {
	data := make([]byte, 16<<20)
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := BlockDigests(bytes.NewReader(data), DefaultBlockSize); err != nil {
			b.Fatal(err)
		}
	}
}
func TestStreamShortFinalBlock(t *testing.T) {
	var n int64
	if err := Stream(context.Background(), io.LimitReader(zeroReader{}, 1025), 1024, func(b Block) error { n += b.Size; return nil }); err != nil {
		t.Fatal(err)
	}
	if n != 1025 {
		t.Fatal(n)
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }

type stalledReader struct{}

func (stalledReader) Read([]byte) (int, error) { return 0, nil }
func TestStreamNoProgress(t *testing.T) {
	if err := Stream(context.Background(), stalledReader{}, 16, func(Block) error { return nil }); !errors.Is(err, io.ErrNoProgress) {
		t.Fatal(err)
	}
}
