package main

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestTwitchReconcileDisableAndInvalidStorageStopPublicationAndRecover(t *testing.T) {
	for _, kind := range []string{"disable", "tenant-disabled", "storage", "two-enabled"} {
		t.Run(kind, func(t *testing.T) {
			f, s, p, _ := inboundFixture(t)
			twitch := newTwitchFake(t)
			twitch.socket = func(socket *websocket.Conn, session, path string) {
				select {
				case <-twitch.subscribed:
				case <-time.After(3 * time.Second):
					return
				}
				go func() {
					for {
						if _, _, err := socket.ReadMessage(); err != nil {
							return
						}
					}
				}()
				ticker := time.NewTicker(100 * time.Millisecond)
				defer ticker.Stop()
				for range ticker.C {
					if socket.WriteJSON(fakeEnvelope("session_keepalive")) != nil {
						return
					}
				}
			}
			s.twitch = &twitchRuntime{client: twitch.client}
			ctx, cancel := context.WithCancel(context.Background())
			ticks := make(chan time.Time)
			done := make(chan struct{})
			go func() { defer close(done); s.reconcileTwitch(ctx, ticks) }()
			defer func() { cancel(); <-done }()
			waitForTwitch(t, func() bool { state, _, _ := s.twitch.status(p.id); return state == "connected" })
			f.mu.Lock()
			switch kind {
			case "disable":
				changeProfile(f, 30, func(p *twitchProfile) { p.Enabled = new(false) })
			case "tenant-disabled":
				f.legacy.mu.Lock()
				grant := f.legacy.cfg.Users["7"]
				grant.Enabled = false
				f.legacy.cfg.Users["7"] = grant
				f.legacy.mu.Unlock()
			case "storage":
				f.fail = "list"
			case "two-enabled":
				provisionTwitch(t, f, 40, "7", "102")
				changeProfile(f, 40, func(p *twitchProfile) { p.Enabled = new(true) })
			}
			f.mu.Unlock()
			select {
			case ticks <- time.Now():
			case <-time.After(3 * time.Second):
				t.Fatal("reconcile did not receive tick")
			}
			expected := "disabled"
			if kind == "storage" || kind == "two-enabled" {
				expected = "degraded"
			}
			waitForTwitch(t, func() bool { state, _, _ := s.twitch.status(p.id); return state == expected })
			if kind == "storage" {
				f.mu.Lock()
				f.fail = ""
				f.mu.Unlock()
				select {
				case ticks <- time.Now():
				case <-time.After(3 * time.Second):
					t.Fatal("recovery did not receive tick")
				}
				waitForTwitch(t, func() bool { state, _, _ := s.twitch.status(p.id); return state == "connected" })
				twitch.mu.Lock()
				calls := twitch.subscriptionCalls
				twitch.mu.Unlock()
				if calls != 2 {
					t.Fatalf("subscriptions=%d", calls)
				}
			}
		})
	}
}

func TestTwitchReconcileMissingCredentialWaitsForProvisionedFileWithoutAuthLoop(t *testing.T) {
	_, s, p, _ := inboundFixture(t)
	twitch := newTwitchFake(t)
	path := filepath.Join(twitch.client.directory, "test-grant")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	s.twitch = &twitchRuntime{client: twitch.client}
	ctx, cancel := context.WithCancel(context.Background())
	ticks := make(chan time.Time)
	done := make(chan struct{})
	go func() { defer close(done); s.reconcileTwitch(ctx, ticks) }()
	defer func() { cancel(); <-done }()
	waitForTwitch(t, func() bool { state, _, _ := s.twitch.status(p.id); return state == "auth-required" })
	select {
	case ticks <- time.Now():
	case <-time.After(time.Second):
		t.Fatal("no reconcile")
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case ticks <- time.Now():
	case <-time.After(time.Second):
		t.Fatal("no credential reconciliation")
	}
	waitForTwitch(t, func() bool { state, _, _ := s.twitch.status(p.id); return state == "connected" })
	twitch.mu.Lock()
	validations := twitch.validationCalls
	twitch.mu.Unlock()
	if validations != 1 {
		t.Fatalf("validations=%d", validations)
	}
}

func TestTwitchReconcileInvalidTokenDoesNotReconnectLoop(t *testing.T) {
	_, s, p, _ := inboundFixture(t)
	twitch := newTwitchFake(t)
	var validateCalls atomic.Int64
	twitch.validate = func(*http.Request) (int, any) { validateCalls.Add(1); return 401, map[string]any{} }
	twitch.refresh = func(*http.Request) (int, any) { return 400, map[string]any{} }
	s.twitch = &twitchRuntime{client: twitch.client}
	ctx, cancel := context.WithCancel(context.Background())
	ticks := make(chan time.Time)
	done := make(chan struct{})
	go func() { defer close(done); s.reconcileTwitch(ctx, ticks) }()
	defer func() { cancel(); <-done }()
	waitForTwitch(t, func() bool { state, _, _ := s.twitch.status(p.id); return state == "auth-required" })
	for i := 0; i < 3; i++ {
		select {
		case ticks <- time.Now():
		case <-time.After(time.Second):
			t.Fatal("reconcile stalled")
		}
	}
	cancel()
	<-done
	if validateCalls.Load() != 1 {
		t.Fatalf("invalid token retried %d times", validateCalls.Load())
	}
}
