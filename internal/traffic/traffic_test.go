package traffic

import (
	"path/filepath"
	"testing"
	"time"
)

func TestDurabilityRestartRollingAndPending(t *testing.T) {
	path := filepath.Join(t.TempDir(), "traffic.db")
	r, e := Open(path)
	if e != nil {
		t.Fatal(e)
	}
	now := time.Now()
	r.addAt(now.Add(-25*time.Hour), 100, 100)
	r.addAt(now, 12, 34)
	s := r.Summary(now)
	if s.Session.Upload != 112 || s.Last24Hours.Upload != 12 || s.Last24Hours.Download != 34 {
		t.Fatal(s)
	}
	since := s.RecordingSince
	if e = r.Close(); e != nil {
		t.Fatal(e)
	}
	r, e = Open(path)
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	s = r.Summary(now)
	if s.Session.Upload != 0 || s.Last24Hours.Upload != 12 || !s.RecordingSince.Equal(since) {
		t.Fatal(s)
	}
	r.addAt(now, 1, 2)
	if s = r.Summary(now); s.Last24Hours.Upload != 13 || s.Last24Hours.Download != 36 {
		t.Fatal(s)
	}
	if s = r.Summary(now.Add(25 * time.Hour)); s.Last24Hours.Upload != 0 {
		t.Fatal(s)
	}
}
