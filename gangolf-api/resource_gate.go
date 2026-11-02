package main

import (
	"context"
	"time"
)

func lockResources(ctx context.Context) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if resourceLock.TryLock() {
		return nil
	}
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if resourceLock.TryLock() {
				return nil
			}
		}
	}
}
