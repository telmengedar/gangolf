package main

import (
	"context"
	"sync"
)

type resourceMutex struct {
	mu       sync.Mutex
	waitMu   sync.Mutex
	released chan struct{}
}

func (m *resourceMutex) Lock() { m.mu.Lock() }

func (m *resourceMutex) TryLock() bool { return m.mu.TryLock() }

func (m *resourceMutex) Unlock() {
	m.waitMu.Lock()
	m.mu.Unlock()
	if m.released != nil {
		close(m.released)
		m.released = nil
	}
	m.waitMu.Unlock()
}

func lockResources(ctx context.Context) error {
	return lockMutex(ctx, &resourceLock)
}

func lockMutex(ctx context.Context, m *resourceMutex) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		m.waitMu.Lock()
		if err := ctx.Err(); err != nil {
			m.waitMu.Unlock()
			return err
		}
		if m.mu.TryLock() {
			m.waitMu.Unlock()
			if err := ctx.Err(); err != nil {
				m.Unlock()
				return err
			}
			return nil
		}
		if m.released == nil {
			m.released = make(chan struct{})
		}
		released := m.released
		m.waitMu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-released:
		}
	}
}
