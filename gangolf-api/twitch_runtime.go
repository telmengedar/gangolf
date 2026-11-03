package main

import (
	"context"
	"errors"
	"math/rand/v2"
	"slices"
	"sync"
	"time"
)

type twitchRuntime struct {
	client   *twitchClient
	mu       sync.Mutex
	profiles map[int64]twitchReceiverStatus
}

func (r *twitchRuntime) status(id int64) (string, string, uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	status, ok := r.profiles[id]
	if !ok {
		return "disabled", "", 0
	}
	return status.state, status.code, status.dropped
}

func (r *twitchRuntime) set(id int64, state, code string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.profiles == nil {
		r.profiles = map[int64]twitchReceiverStatus{}
	}
	status := r.profiles[id]
	status.state, status.code = state, code
	r.profiles[id] = status
}

func (r *twitchRuntime) drop(id int64, count uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.profiles == nil {
		r.profiles = map[int64]twitchReceiverStatus{}
	}
	status := r.profiles[id]
	status.dropped += count
	r.profiles[id] = status
}

func twitchGrantKey(p twitchProfile) string { return p.ClientID + ":" + p.BotUserID }

func credentialFingerprint(client *twitchClient, p twitchProfile) [32]byte {
	credential, err := readTwitchCredential(client.directory, p.CredentialRef)
	if err != nil {
		return [32]byte{}
	}
	return credential.fingerprint()
}

func (s *server) runTwitch(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	s.reconcileTwitch(ctx, ticker.C)
}

func (s *server) reconcileTwitch(ctx context.Context, ticks <-chan time.Time) {
	runtime := s.twitch
	workers := map[string]*twitchWorker{}
	defer func() {
		for _, worker := range workers {
			worker.cancel()
		}
		for _, worker := range workers {
			<-worker.done
			for _, p := range worker.profiles {
				runtime.set(p.id, "disabled", "")
			}
		}
	}()
	for {
		check, finish := inboundDeadline(ctx)
		var cfg *authConfig
		err := lockResources(check)
		if err == nil {
			cfg, err = s.graph.loadConfig(check)
			resourceLock.Unlock()
		}
		finish()
		desired := map[string][]twitchProfile{}
		if err == nil {
			for _, p := range cfg.twitch {
				if _, ok := eligibleTwitch(cfg, s.graph.config, p.id); ok {
					key := twitchGrantKey(p)
					desired[key] = append(desired[key], p)
				}
			}
		}
		for key, worker := range workers {
			profiles := desired[key]
			restart := false
			select {
			case <-worker.done:
				for _, p := range worker.profiles {
					state, code, _ := runtime.status(p.id)
					restart = restart || state == "degraded" && code == "storage_unavailable"
				}
			default:
			}
			if sameTwitchProfiles(worker.profiles, profiles) && !worker.credentialChanged(credentialFingerprint(runtime.client, profiles[0])) && !restart {
				continue
			}
			worker.cancel()
			<-worker.done
			for _, p := range worker.profiles {
				state, code := "disabled", ""
				if err != nil {
					state, code = "degraded", "configuration_unavailable"
				}
				runtime.set(p.id, state, code)
			}
			delete(workers, key)
		}
		for key, profiles := range desired {
			if _, ok := workers[key]; ok {
				continue
			}
			if len(profiles) > 300 {
				for _, p := range profiles {
					runtime.set(p.id, "degraded", "subscription_capacity_exceeded")
				}
				continue
			}
			workerCtx, cancel := context.WithCancel(ctx)
			worker := &twitchWorker{profiles: profiles, cancel: cancel, done: make(chan struct{}), fingerprint: credentialFingerprint(runtime.client, profiles[0])}
			workers[key] = worker
			go func() { defer close(worker.done); s.receiveTwitch(workerCtx, worker) }()
		}
		select {
		case <-ctx.Done():
			return
		case <-ticks:
		}
	}
}

func sameTwitchProfiles(a, b []twitchProfile) bool {
	if len(a) != len(b) {
		return false
	}
	for _, p := range a {
		found := false
		for _, q := range b {
			if sameReceiver(p, q) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func (s *server) receiveTwitch(ctx context.Context, worker *twitchWorker) {
	profiles := worker.profiles
	runtime := s.twitch
	base := runtime.client
	client := &twitchClient{http: base.http, validateURL: base.validateURL, tokenURL: base.tokenURL, subscriptionURL: base.subscriptionURL, socketURL: base.socketURL, directory: base.directory}
	active := profiles
	blocked := map[int64]bool{}
	set := func(state, code string) {
		for _, p := range active {
			runtime.set(p.id, state, code)
		}
	}
	queue := make(chan twitchEvent, 256)
	failure := make(chan error, 1)
	writerCtx, stopWriter := context.WithCancel(context.Background())
	defer stopWriter()
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		failed := false
		for e := range queue {
			var p twitchProfile
			for _, candidate := range profiles {
				if candidate.BroadcasterUserID == e.BroadcasterUserID {
					p = candidate
					break
				}
			}
			if failed {
				runtime.drop(p.id, 1)
				continue
			}
			writeCtx, cancel := inboundDeadline(writerCtx)
			_, err := s.ingestTwitch(writeCtx, p, e)
			cancel()
			if err != nil {
				failed = true
				runtime.drop(p.id, 1)
				failure <- err
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
		set("connecting", "")
		connectionCtx, cancel := context.WithCancel(ctx)
		finished := make(chan error, 1)
		go func() {
			finished <- client.connection(connectionCtx, active, queue, func(p twitchProfile, state, code string, fingerprint [32]byte) {
				if state == "connected" {
					worker.rememberCredential(fingerprint)
				}
				if state == "auth-required" {
					blocked[p.id] = true
				}
				if code == "inbound_queue_full" {
					runtime.drop(p.id, 1)
				}
				runtime.set(p.id, state, code)
			})
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
			set("degraded", "storage_unavailable")
			return
		case err = <-finished:
			cancel()
		}
		if ctx.Err() != nil {
			return
		}
		active = slices.DeleteFunc(append([]twitchProfile(nil), active...), func(p twitchProfile) bool { return blocked[p.id] })
		if len(active) == 0 {
			return
		}
		switch {
		case errors.Is(err, errTwitchAuth), errors.Is(err, errTwitchCredential), errors.Is(err, errTwitchForbidden):
			set("auth-required", err.Error())
			return
		case errors.Is(err, errTwitchProtocol):
			set("degraded", "invalid_twitch_protocol")
			return
		case errors.Is(err, errTwitchOverflow):
			set("degraded", "inbound_queue_full")
		default:
			set("degraded", "twitch_unavailable")
		}
		if waitTwitch(ctx, delay+time.Duration(rand.Int64N(int64(delay/4)))) != nil {
			return
		}
		delay = min(delay*2, 60*time.Second)
	}
}
