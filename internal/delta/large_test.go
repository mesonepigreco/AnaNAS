package delta

import (
	"bytes"
	"context"
	"io"
	"os"
	"testing"
)

type zeroSource struct{}

func (zeroSource) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// Opt in because this really hashes and transfers over three GiB through a
// pipe. No whole-file allocation or multi-gigabyte fixture is needed.
func TestMultiGiBStreamingRoundTrip(t *testing.T) {
	if os.Getenv("ANANAS_LARGE_FILE_TEST") != "1" {
		t.Skip("set ANANAS_LARGE_FILE_TEST=1 for the multi-GiB streaming test")
	}
	const size int64 = 3<<30 + 17
	ctx := context.Background()
	base, err := Build(ctx, bytes.NewReader(nil), 0, 65536)
	if err != nil {
		t.Fatal(err)
	}
	reader, writer := io.Pipe()
	defer reader.Close()
	done := make(chan error, 1)
	var sent Stats
	go func() {
		var err error
		sent, err = Encode(ctx, io.LimitReader(zeroSource{}, size), size, base, writer)
		writer.CloseWithError(err)
		done <- err
	}()
	got, err := Apply(ctx, reader, bytes.NewReader(nil), 0, base.Digest, io.Discard)
	reader.CloseWithError(err)
	encodeErr := <-done
	if err != nil || encodeErr != nil {
		t.Fatal(err, encodeErr)
	}
	if got.LiteralBytes != size || sent.LiteralBytes != size || got.Digest != sent.Digest {
		t.Fatal("multi-GiB size or digest mismatch", got, sent)
	}
}
