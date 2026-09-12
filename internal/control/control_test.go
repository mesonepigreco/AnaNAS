package control

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

type memoryStore struct {
	paused bool
	fail   bool
	writes atomic.Uint64
}

func (s *memoryStore) Paused() (bool, error) { return s.paused, nil }
func (s *memoryStore) SetPaused(p bool) error {
	if s.fail {
		return errors.New("disk full")
	}
	s.paused = p
	s.writes.Add(1)
	return nil
}

func TestFailedPersistenceDoesNotChangeVisibleIntent(t *testing.T) {
	s := &memoryStore{paused: true, fail: true}
	c, err := New(s)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SetPaused(false); err == nil {
		t.Fatal("storage failure ignored")
	}
	if !c.Paused() {
		t.Fatal("reported resumed despite persistence failure")
	}
	select {
	case <-c.Changes():
		t.Fatal("failed persistence woke sync work")
	default:
	}
	s.fail = false
	if err := c.SetPaused(false); err != nil {
		t.Fatal(err)
	}
	if c.Paused() {
		t.Fatal("resume not reflected")
	}
	select {
	case <-c.Changes():
	default:
		t.Fatal("durable resume did not notify worker")
	}
	if err := c.SetPaused(false); err != nil {
		t.Fatal(err)
	}
	if s.writes.Load() != 1 {
		t.Fatal("repeated control caused redundant writes")
	}
	select {
	case <-c.Changes():
		t.Fatal("unchanged control woke sync work")
	default:
	}
}

func TestConcurrentControlsSerializeStorageAndSnapshot(t *testing.T) {
	s := &memoryStore{}
	c, err := New(s)
	if err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	for i := 0; i < 20; i++ {
		group.Add(1)
		go func(p bool) {
			defer group.Done()
			if err := c.SetPaused(p); err != nil {
				t.Error(err)
			}
			_ = c.Paused()
		}(i%2 == 0)
	}
	group.Wait()
	if c.Paused() != s.paused {
		t.Fatal("visible intent and durable intent differ")
	}
}
