package main

import (
	"context"
	"sync"
	"time"
)

func lockResources(ctx context.Context) error {
	return lockMutex(ctx, &resourceLock)
}

func lockMutex(ctx context.Context, mu *sync.Mutex) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if mu.TryLock() {
		return nil
	}
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if mu.TryLock() {
				return nil
			}
		}
	}
}
