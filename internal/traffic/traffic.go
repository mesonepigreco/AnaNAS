// Package traffic keeps bounded, durable UI accounting outside the sync journal.
package traffic

import (
	"encoding/binary"
	"fmt"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
)

var samples = []byte("seconds")
var metadata = []byte("metadata")

type Bytes struct {
	Upload   uint64 `json:"upload"`
	Download uint64 `json:"download"`
}
type Summary struct {
	Session        Bytes     `json:"session"`
	Last24Hours    Bytes     `json:"last24Hours"`
	StartedAt      time.Time `json:"startedAt"`
	RecordingSince time.Time `json:"recordingSince"`
	HistoryError   bool      `json:"historyError"`
}
type Recorder struct {
	mu               sync.Mutex
	db               *bolt.DB
	history          map[int64]Bytes
	pending          map[int64]Bytes
	session          Bytes
	started, since   time.Time
	wake, stop, done chan struct{}
	lastError        error
}

func Open(path string) (*Recorder, error) {
	db, err := bolt.Open(path, 0600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	r := &Recorder{db: db, history: map[int64]Bytes{}, pending: map[int64]Bytes{}, started: now, since: now, wake: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{})}
	err = db.Update(func(tx *bolt.Tx) error {
		b, e := tx.CreateBucketIfNotExists(samples)
		if e != nil {
			return e
		}
		m, e := tx.CreateBucketIfNotExists(metadata)
		if e != nil {
			return e
		}
		if raw := m.Get([]byte("since")); raw != nil {
			if e = r.since.UnmarshalText(raw); e != nil {
				return e
			}
		} else {
			raw, _ := now.MarshalText()
			if e = m.Put([]byte("since"), raw); e != nil {
				return e
			}
		}
		return b.ForEach(func(k, v []byte) error {
			if len(k) != 8 || len(v) != 16 {
				return fmt.Errorf("invalid traffic sample")
			}
			second := int64(binary.BigEndian.Uint64(k))
			if second >= now.Unix()-86400 {
				r.history[second] = Bytes{binary.BigEndian.Uint64(v), binary.BigEndian.Uint64(v[8:])}
			}
			return nil
		})
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	go r.run()
	return r, nil
}
func (r *Recorder) Add(upload, download uint64) { r.addAt(time.Now(), upload, download) }
func (r *Recorder) addAt(now time.Time, upload, download uint64) {
	if upload == 0 && download == 0 {
		return
	}
	r.mu.Lock()
	second := now.Unix()
	v := r.history[second]
	v.Upload += upload
	v.Download += download
	r.history[second] = v
	r.pending[second] = v
	r.session.Upload += upload
	r.session.Download += download
	r.mu.Unlock()
	select {
	case r.wake <- struct{}{}:
	default:
	}
}
func (r *Recorder) Summary(now time.Time) Summary {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := Summary{Session: r.session, StartedAt: r.started, RecordingSince: r.since, HistoryError: r.lastError != nil}
	for second, v := range r.history {
		if second >= now.Unix()-86400 && second <= now.Unix() {
			result.Last24Hours.Upload += v.Upload
			result.Last24Hours.Download += v.Download
		}
	}
	return result
}
func (r *Recorder) flush(now time.Time) error {
	r.mu.Lock()
	pending := r.pending
	r.pending = make(map[int64]Bytes)
	r.mu.Unlock()
	cutoff := now.Unix() - 86400
	err := r.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(samples)
		for second, v := range pending {
			var k [8]byte
			var raw [16]byte
			binary.BigEndian.PutUint64(k[:], uint64(second))
			binary.BigEndian.PutUint64(raw[:], v.Upload)
			binary.BigEndian.PutUint64(raw[8:], v.Download)
			if err := b.Put(k[:], raw[:]); err != nil {
				return err
			}
		}
		c := b.Cursor()
		for k, _ := c.First(); k != nil && int64(binary.BigEndian.Uint64(k)) < cutoff; k, _ = c.Next() {
			if err := c.Delete(); err != nil {
				return err
			}
		}
		return nil
	})
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastError = err
	if err == nil {
		for second := range r.history {
			if second < cutoff {
				delete(r.history, second)
			}
		}
	} else {
		// Samples are cumulative per second. Keep newer arrivals over failed writes.
		for second, v := range pending {
			if _, ok := r.pending[second]; !ok {
				r.pending[second] = v
			}
		}
	}
	return err
}
func (r *Recorder) run() {
	defer close(r.done)
	for {
		select {
		case <-r.stop:
			r.flush(time.Now())
			return
		case <-r.wake:
		}
		timer := time.NewTimer(5 * time.Second)
		select {
		case <-r.stop:
			timer.Stop()
			r.flush(time.Now())
			return
		case <-timer.C:
		}
		// Drain notifications already represented in this checkpoint.
		select {
		case <-r.wake:
		default:
		}
		r.flush(time.Now())
	}
}
func (r *Recorder) Close() error {
	close(r.stop)
	<-r.done
	err := r.db.Close()
	if r.lastError != nil {
		return r.lastError
	}
	return err
}
