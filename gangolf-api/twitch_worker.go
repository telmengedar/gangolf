package main

import (
	"context"
	"sync"
)

type twitchWorker struct {
	mu          sync.Mutex
	profiles    []twitchProfile
	cancel      context.CancelFunc
	done        chan struct{}
	fingerprint [32]byte
}

func (w *twitchWorker) credentialChanged(current [32]byte) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.fingerprint != current
}

func (w *twitchWorker) rememberCredential(current [32]byte) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.fingerprint = current
}
