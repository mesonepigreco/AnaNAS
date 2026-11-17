package transferapi

import (
	"context"
	"fmt"
	"io"
	"time"
)

// Bound each wait for the download peer independently of the much larger
// whole-operation budget. Arm only while requesting/reading network data:
// local reconstruction, pacing and disk writes may legitimately take longer.
// Quiet notification streams use a different request and are not affected.
type downloadWait struct {
	cancel context.CancelFunc
	idle   time.Duration
}

func (w downloadWait) start() func(error) error {
	fired := make(chan struct{})
	timer := time.AfterFunc(w.idle, func() {
		w.cancel() // HTTP cancellation unblocks both headers and response reads.
		close(fired)
	})
	return func(err error) error {
		if timer.Stop() {
			return err
		}
		// Join the callback so it cannot cancel a subsequent read after return.
		<-fired
		return fmt.Errorf("no response data from the NAS for %s; the download was stopped so it can be retried: %w", w.idle, context.DeadlineExceeded)
	}
}

type downloadBody struct {
	io.ReadCloser
	wait downloadWait
}

func (b downloadBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	finish := b.wait.start()
	n, err := b.ReadCloser.Read(p)
	return n, finish(err)
}
