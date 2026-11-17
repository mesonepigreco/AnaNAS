package transferapi

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestDownloadWaitDoesNotTimeOutLocalProcessing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		body := downloadBody{io.NopCloser(strings.NewReader("ab")), downloadWait{cancel, time.Minute}}
		for range 2 {
			var b [1]byte
			if n, err := body.Read(b[:]); n != 1 || err != nil {
				t.Fatal(n, err)
			}
			// Simulate slow local reconstruction/pacing between network reads.
			time.Sleep(2 * time.Minute)
			if ctx.Err() != nil {
				t.Fatal("local work incorrectly timed out", ctx.Err())
			}
		}
	})
}

func TestDownloadWaitPreservesCancellationAndReadErrors(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		wait := downloadWait{cancel, time.Minute}
		finish := wait.start()
		cancel()
		if err := finish(ctx.Err()); !errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("pause/shutdown was relabelled as a stall", err)
		}
		finish = wait.start()
		if err := finish(io.ErrUnexpectedEOF); err != io.ErrUnexpectedEOF {
			t.Fatal("underlying read error changed", err)
		}
	})
}
