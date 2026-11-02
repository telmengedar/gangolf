package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"sync"
	"time"
)

type twitchRuntime struct {
	client    *twitchClient
	mu        sync.Mutex
	profileID int64
	state     string
	code      string
	dropped   uint64
}

func (r *twitchRuntime) status(id int64) (string, string, uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.profileID != id {
		return "disabled", "", 0
	}
	return r.state, r.code, r.dropped
}

func (r *twitchRuntime) set(id int64, state, code string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.profileID, r.state, r.code = id, state, code
}

func (r *twitchRuntime) drop(count uint64) { r.mu.Lock(); r.dropped += count; r.mu.Unlock() }

func credentialFingerprint(client *twitchClient, p twitchProfile) [32]byte {
	credential, err := readTwitchCredential(client.directory, p.CredentialRef)
	if err != nil {
		return [32]byte{}
	}
	data, _ := json.Marshal(credential)
	return sha256.Sum256(data)
}

func (s *server) runTwitch(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	s.reconcileTwitch(ctx, ticker.C)
}

func (s *server) reconcileTwitch(ctx context.Context, ticks <-chan time.Time) {
	runtime := s.twitch
	var active twitchProfile
	var cancel context.CancelFunc
	var done chan struct{}
	var fingerprint [32]byte
	stop := func() {
		if cancel != nil {
			cancel()
			<-done
			cancel = nil
		}
	}
	defer stop()
	for {
		check, finish := inboundDeadline(ctx)
		var cfg *authConfig
		err := lockResources(check)
		if err == nil {
			cfg, err = s.graph.loadConfig(check)
			resourceLock.Unlock()
		}
		finish()
		var desired twitchProfile
		enabled := false
		if err == nil {
			desired, enabled = eligibleTwitch(cfg, s.graph.config)
		}
		if err != nil || !enabled {
			stop()
			if active.id != 0 {
				state, code := "disabled", ""
				if err != nil {
					state, code = "degraded", "configuration_unavailable"
				}
				runtime.set(active.id, state, code)
			}
			active = twitchProfile{}
		} else {
			currentFingerprint := credentialFingerprint(runtime.client, desired)
			restart := false
			if done != nil {
				select {
				case <-done:
					state, code, _ := runtime.status(active.id)
					restart = state == "degraded" && code == "storage_unavailable"
				default:
				}
			}
			if cancel == nil || restart || !sameReceiver(active, desired) || fingerprint != currentFingerprint {
				stop()
				active = desired
				fingerprint = currentFingerprint
				workerCtx, stopWorker := context.WithCancel(ctx)
				cancel = stopWorker
				done = make(chan struct{})
				go func(p twitchProfile, finished chan struct{}) { defer close(finished); s.receiveTwitch(workerCtx, p) }(desired, done)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticks:
		}
	}
}

func (s *server) receiveTwitch(ctx context.Context, p twitchProfile) {
	runtime := s.twitch
	queue := make(chan twitchEvent, 256)
	failure := make(chan error, 1)
	writerCtx, stopWriter := context.WithCancel(context.Background())
	defer stopWriter()
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for e := range queue {
			writeCtx, cancel := inboundDeadline(writerCtx)
			_, err := s.ingestTwitch(writeCtx, p, e)
			cancel()
			if err != nil {
				runtime.drop(1 + uint64(len(queue)))
				select {
				case failure <- err:
				default:
				}
				return
			}
		}
	}()
	defer func() {
		close(queue)
		timer := time.NewTimer(5 * time.Second)
		defer timer.Stop()
		select {
		case <-writerDone:
		case <-timer.C:
			stopWriter()
			<-writerDone
		}
	}()
	delay := time.Second
	for {
		runtime.set(p.id, "connecting", "")
		connectionCtx, cancel := context.WithCancel(ctx)
		finished := make(chan error, 1)
		go func() {
			finished <- s.twitch.client.connection(connectionCtx, p, queue, func() { runtime.set(p.id, "connected", "") })
		}()
		var err error
		select {
		case <-ctx.Done():
			cancel()
			<-finished
			return
		case err = <-failure:
			cancel()
			<-finished
			runtime.set(p.id, "degraded", "storage_unavailable")
			return
		case err = <-finished:
			cancel()
		}
		if ctx.Err() != nil {
			return
		}
		switch {
		case errors.Is(err, errTwitchAuth), errors.Is(err, errTwitchCredential), errors.Is(err, errTwitchForbidden):
			runtime.set(p.id, "auth-required", err.Error())
			return
		case errors.Is(err, errTwitchProtocol):
			runtime.set(p.id, "degraded", "invalid_twitch_protocol")
			return
		case errors.Is(err, errTwitchOverflow):
			runtime.drop(1)
			runtime.set(p.id, "degraded", "inbound_queue_full")
		default:
			runtime.set(p.id, "degraded", "twitch_unavailable")
		}
		if waitTwitch(ctx, delay+time.Duration(rand.Int64N(int64(delay/4)))) != nil {
			return
		}
		delay = min(delay*2, 60*time.Second)
	}
}
