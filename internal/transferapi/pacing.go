package transferapi

import (
	"context"
	"io"
	"net/http"
	"time"
)

// A request shares one budget for incoming wire and native content reads.
// Waiting is coalesced over 64 KiB instead of creating a timer per small delta
// frame. With reads capped at 64 KiB, initial/unpaced work is below 128 KiB.
// The handler uses it serially; there is no idle ticker or background worker.
type pacer struct {
	ctx         context.Context
	rate, bytes int64
	start       time.Time
}

func (p *pacer) before() error {
	if err := p.ctx.Err(); err != nil {
		return err
	}
	if p.bytes < 64*1024 {
		return nil
	}
	wait := time.Until(p.start.Add(time.Duration(p.bytes * int64(time.Second) / p.rate)))
	if wait > 0 {
		t := time.NewTimer(wait)
		defer t.Stop()
		select {
		case <-p.ctx.Done():
			return p.ctx.Err()
		case <-t.C:
		}
	}
	p.bytes = 0
	p.start = time.Now()
	return nil
}

type paceKey struct{}

func requestPacer(r *http.Request) *pacer { return r.Context().Value(paceKey{}).(*pacer) }

type pacedReader struct {
	r    io.Reader
	pace *pacer
}

func (r *pacedReader) Read(p []byte) (int, error) {
	if err := r.pace.before(); err != nil {
		return 0, err
	}
	p = p[:min(len(p), 64*1024)]
	n, err := r.r.Read(p)
	r.pace.bytes += int64(n)
	return n, err
}

type pacedBody struct {
	io.ReadCloser
	pace *pacer
}

func (r *pacedBody) Read(p []byte) (int, error) {
	return (&pacedReader{r: r.ReadCloser, pace: r.pace}).Read(p)
}

type pacedReaderAt struct {
	r    io.ReaderAt
	pace *pacer
}

func (r *pacedReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if err := r.pace.before(); err != nil {
		return 0, err
	}
	if len(p) > 64*1024 {
		return 0, io.ErrShortBuffer
	}
	n, err := r.r.ReadAt(p, off)
	r.pace.bytes += int64(n)
	return n, err
}
