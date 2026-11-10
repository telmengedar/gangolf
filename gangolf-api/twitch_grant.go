package main

import (
	"context"
	"sync"
)

var twitchGrantLocks sync.Map

func lockTwitchGrant(ctx context.Context, p twitchProfile) (func(), error) {
	value, _ := twitchGrantLocks.LoadOrStore(twitchGrantKey(p), make(chan struct{}, 1))
	gate := value.(chan struct{})
	select {
	case gate <- struct{}{}:
		if ctx.Err() != nil {
			<-gate
			return nil, ctx.Err()
		}
		return func() { <-gate }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
