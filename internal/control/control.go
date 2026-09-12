// Package control owns durable user intent independently of LAN/capability gates.
// It does not start network work or authorize automatic writes.
package control

import (
	"sync"
	"sync/atomic"
)

type Store interface {
	Paused() (bool, error)
	SetPaused(bool) error
}

type Controller struct {
	store   Store
	mu      sync.Mutex
	paused  atomic.Bool
	changes chan struct{}
}

func New(store Store) (*Controller, error) {
	p, err := store.Paused()
	if err != nil {
		return nil, err
	}
	c := &Controller{store: store, changes: make(chan struct{}, 1)}
	c.paused.Store(p)
	return c, nil
}

func (c *Controller) Paused() bool             { return c.paused.Load() }
func (c *Controller) Changes() <-chan struct{} { return c.changes }

func (c *Controller) SetPaused(p bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.paused.Load() == p {
		return nil
	}
	if err := c.store.SetPaused(p); err != nil {
		return err
	}
	c.paused.Store(p)
	select {
	case c.changes <- struct{}{}:
	default:
	}
	return nil
}
