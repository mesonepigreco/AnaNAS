package replica

import (
	"context"
	"io"
	"time"
)

// One active file shares a budget across signature reads, reused base reads and
// staged writes. Timers are coalesced after 64 KiB; there is no idle ticker.
type pacer struct {
	ctx         context.Context
	rate, bytes int64
	start       time.Time
}

func newPacer(ctx context.Context, rate int64) *pacer {
	return &pacer{ctx: ctx, rate: rate, start: time.Now()}
}
func (p *pacer) before() error {
	if err := p.ctx.Err(); err != nil {
		return err
	}
	if p.bytes < 64*1024 {
		return nil
	}
	if delay := time.Until(p.start.Add(time.Duration(p.bytes * int64(time.Second) / p.rate))); delay > 0 {
		t := time.NewTimer(delay)
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

type pacedReader struct {
	in   io.Reader
	pace *pacer
}

func (r *pacedReader) Read(b []byte) (int, error) {
	if err := r.pace.before(); err != nil {
		return 0, err
	}
	n, err := r.in.Read(b[:min(len(b), 64*1024)])
	r.pace.bytes += int64(n)
	return n, err
}

type pacedReaderAt struct {
	in   io.ReaderAt
	pace *pacer
}

func (r *pacedReaderAt) ReadAt(b []byte, offset int64) (int, error) {
	if len(b) > 64*1024 {
		return 0, io.ErrShortBuffer
	}
	if err := r.pace.before(); err != nil {
		return 0, err
	}
	n, err := r.in.ReadAt(b, offset)
	r.pace.bytes += int64(n)
	return n, err
}

type pacedWriter struct {
	out  io.Writer
	pace *pacer
}

func (w *pacedWriter) Write(b []byte) (int, error) {
	total := 0
	for len(b) > 0 {
		if err := w.pace.before(); err != nil {
			return total, err
		}
		chunk := b[:min(len(b), 64*1024)]
		n, err := w.out.Write(chunk)
		w.pace.bytes += int64(n)
		total += n
		if err == nil && n != len(chunk) {
			err = io.ErrShortWrite
		}
		if err != nil {
			return total, err
		}
		b = b[n:]
	}
	return total, nil
}
