package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestTwitchWebSocketReceiverPersistsRedeliveryAndStatus(t *testing.T) {
	f, s, p, event := inboundFixture(t)
	twitch := newTwitchFake(t)
	scriptDone := make(chan struct{})
	twitch.socket = func(socket *websocket.Conn, session, path string) {
		defer close(scriptDone)
		select {
		case <-twitch.subscribed:
		case <-time.After(3 * time.Second):
			return
		}
		socket.WriteJSON(fakeNotification(session, event))
		socket.WriteJSON(fakeNotification(session, event))
		for {
			if _, _, err := socket.ReadMessage(); err != nil {
				return
			}
		}
	}
	s.twitch = &twitchRuntime{client: twitch.client}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); s.receiveTwitch(ctx, p) }()
	defer func() { cancel(); <-done }()
	waitForTwitch(t, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, n := range f.nodes {
			if n.Type == "message" && n.Content != nil {
				return true
			}
		}
		return false
	})
	status(t, request(t, s, "GET", "/api/i1/twitch/channels", "key.secret", ""), 200)
	state, code, dropped := s.twitch.status(p.id)
	if state != "connected" || code != "" || dropped != 0 {
		t.Fatalf("state=%s code=%s dropped=%d", state, code, dropped)
	}
	cancel()
	<-done
	select {
	case <-scriptDone:
	case <-time.After(time.Second):
		t.Fatal("socket did not shut down")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	messages := 0
	for _, n := range f.nodes {
		if n.Type == "message" {
			messages++
		}
	}
	if messages != 1 {
		t.Fatalf("messages=%d", messages)
	}
}

func TestTwitchWebSocketProtocolRejectsMismatchedEventsAndRevocation(t *testing.T) {
	for _, kind := range []string{"welcome-type", "welcome-session", "welcome-status", "welcome-timeout", "welcome-timestamp", "notification-id", "notification-version", "notification-condition", "notification-session", "event-broadcaster", "event-user", "event-text", "event-reply", "event-shared", "bad-timestamp", "unknown-frame", "revocation", "oversize"} {
		t.Run(kind, func(t *testing.T) {
			_, _, p, event := inboundFixture(t)
			f := newTwitchFake(t)
			f.welcome = func(e twitchEnvelope) twitchEnvelope {
				switch kind {
				case "welcome-type":
					e.Metadata.MessageType = "notification"
				case "welcome-session":
					e.Payload.Session.ID = ""
				case "welcome-status":
					e.Payload.Session.Status = "reconnecting"
				case "welcome-timeout":
					e.Payload.Session.KeepaliveTimeoutSeconds = 0
				case "welcome-timestamp":
					e.Metadata.MessageTimestamp = "bad"
				}
				return e
			}
			f.socket = func(socket *websocket.Conn, session, path string) {
				if strings.HasPrefix(kind, "welcome-") {
					return
				}
				select {
				case <-f.subscribed:
				case <-time.After(3 * time.Second):
					return
				}
				switch kind {
				case "event-broadcaster":
					event.BroadcasterUserID = "999"
				case "event-user":
					event.ChatterUserID = ""
				case "event-text":
					event.Message.Text = ""
				case "event-reply":
					event.Reply = &struct {
						ParentMessageID string `json:"parent_message_id"`
					}{ParentMessageID: "../bad"}
				case "event-shared":
					event.SourceMessageID = "without-source-room"
				}
				e := fakeNotification(session, event)
				switch kind {
				case "notification-id":
					e.Payload.Subscription.ID = "other"
				case "notification-version":
					e.Metadata.SubscriptionVersion = "2"
				case "notification-condition":
					e.Payload.Subscription.Condition.UserID = "999"
				case "notification-session":
					e.Payload.Subscription.Transport.SessionID = "other"
				case "bad-timestamp":
					e.Metadata.MessageTimestamp = "bad"
				case "unknown-frame":
					e.Metadata.MessageType = "unknown"
				case "revocation":
					e.Metadata.MessageType = "revocation"
					e.Payload.Subscription.Status = "authorization_revoked"
				case "oversize":
					socket.WriteMessage(websocket.TextMessage, []byte(strings.Repeat("x", (1<<20)+1)))
					return
				}
				socket.WriteJSON(e)
				for {
					if _, _, err := socket.ReadMessage(); err != nil {
						return
					}
				}
			}
			queue := make(chan twitchEvent, 256)
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancel()
			err := f.client.connection(ctx, p, queue, func() {})
			if err == nil || len(queue) != 0 || errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("err=%v queued=%d", err, len(queue))
			}
			if kind == "revocation" && err != errTwitchForbidden {
				t.Fatal(err)
			}
		})
	}
}

func TestTwitchWebSocketKeepaliveExtendsLivenessButPingDoesNot(t *testing.T) {
	for _, kind := range []string{"keepalive", "ping-only", "silent"} {
		t.Run(kind, func(t *testing.T) {
			_, _, p, _ := inboundFixture(t)
			f := newTwitchFake(t)
			pong := make(chan struct{}, 1)
			f.socket = func(socket *websocket.Conn, session, path string) {
				select {
				case <-f.subscribed:
				case <-time.After(3 * time.Second):
					return
				}
				socket.SetPongHandler(func(string) error {
					select {
					case pong <- struct{}{}:
					default:
					}
					return nil
				})
				go func() {
					for {
						if _, _, err := socket.ReadMessage(); err != nil {
							return
						}
					}
				}()
				ticker := time.NewTicker(200 * time.Millisecond)
				defer ticker.Stop()
				for i := 0; i < 12; i++ {
					<-ticker.C
					if kind == "keepalive" {
						if socket.WriteJSON(fakeEnvelope("session_keepalive")) != nil {
							return
						}
					}
					if kind != "silent" {
						if socket.WriteControl(websocket.PingMessage, []byte("ping"), time.Now().Add(time.Second)) != nil {
							return
						}
					}
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 1800*time.Millisecond)
			defer cancel()
			start := time.Now()
			err := f.client.connection(ctx, p, make(chan twitchEvent, 256), func() {})
			elapsed := time.Since(start)
			if kind == "keepalive" {
				if !errors.Is(err, context.DeadlineExceeded) || elapsed < 1500*time.Millisecond {
					t.Fatalf("err=%v elapsed=%s", err, elapsed)
				}
			} else if err != errTwitchTransport || elapsed > 1600*time.Millisecond {
				t.Fatalf("err=%v elapsed=%s", err, elapsed)
			}
			if kind != "silent" {
				select {
				case <-pong:
				default:
					t.Fatal("no pong observed")
				}
			}
		})
	}
}

func TestTwitchReconnectTransfersSubscriptionAndIgnoresReplayUntilWelcome(t *testing.T) {
	_, _, p, event := inboundFixture(t)
	f := newTwitchFake(t)
	allowWelcome := make(chan struct{})
	oldEventSent := make(chan struct{})
	f.welcome = func(e twitchEnvelope) twitchEnvelope {
		if e.Payload.Session.ID == "session-2" {
			select {
			case <-allowWelcome:
			case <-time.After(3 * time.Second):
			}
		}
		return e
	}
	f.socket = func(socket *websocket.Conn, session, path string) {
		if session == "session-1" {
			select {
			case <-f.subscribed:
			case <-time.After(3 * time.Second):
				return
			}
			reconnect := fakeEnvelope("session_reconnect")
			reconnect.Payload.Session.ID = session
			reconnect.Payload.Session.Status = "reconnecting"
			reconnect.Payload.Session.ReconnectURL = strings.Replace(f.client.socketURL, "/ws", "/transfer?opaque=a%2Fb", 1)
			socket.WriteJSON(reconnect)
			socket.WriteJSON(reconnect)
			socket.WriteJSON(fakeNotification(session, event))
			close(oldEventSent)
		} else {
			if path != "/transfer?opaque=a%2Fb" {
				t.Error("reconnect URL altered")
			}
			event.MessageID = "after-transfer"
			socket.WriteJSON(fakeNotification(session, event))
		}
		for {
			if _, _, err := socket.ReadMessage(); err != nil {
				return
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	queue := make(chan twitchEvent, 256)
	done := make(chan error, 1)
	go func() { done <- f.client.connection(ctx, p, queue, func() {}) }()
	select {
	case <-oldEventSent:
	case <-ctx.Done():
		t.Fatal("no old event")
	}
	select {
	case e := <-queue:
		if e.MessageID != "chat-1" {
			t.Fatal("wrong old event")
		}
	case <-ctx.Done():
		t.Fatal("old connection stopped before replacement welcome")
	}
	close(allowWelcome)
	select {
	case e := <-queue:
		if e.MessageID != "after-transfer" {
			t.Fatal("wrong transferred event")
		}
	case <-ctx.Done():
		t.Fatal("no transferred event")
	}
	cancel()
	<-done
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.subscriptionCalls != 1 || f.socketCount != 2 {
		t.Fatalf("subscriptions=%d sockets=%d", f.subscriptionCalls, f.socketCount)
	}
}

func TestTwitchWebSocketQueueOverflowIsBounded(t *testing.T) {
	_, _, p, event := inboundFixture(t)
	f := newTwitchFake(t)
	f.socket = func(socket *websocket.Conn, session, path string) {
		select {
		case <-f.subscribed:
		case <-time.After(3 * time.Second):
			return
		}
		for i := 0; i < 257; i++ {
			if socket.WriteJSON(fakeNotification(session, event)) != nil {
				return
			}
		}
		for {
			if _, _, err := socket.ReadMessage(); err != nil {
				return
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	queue := make(chan twitchEvent, 256)
	err := f.client.connection(ctx, p, queue, func() {})
	if err != errTwitchOverflow || len(queue) != 256 {
		t.Fatalf("err=%v pending=%d", err, len(queue))
	}
}

func TestTwitchWebSocketUnexpectedLossResubscribes(t *testing.T) {
	f, s, p, event := inboundFixture(t)
	twitch := newTwitchFake(t)
	twitch.socket = func(socket *websocket.Conn, session, path string) {
		select {
		case <-twitch.subscribed:
		case <-time.After(3 * time.Second):
			return
		}
		if session == "session-1" {
			return
		}
		socket.WriteJSON(fakeNotification(session, event))
		for {
			if _, _, err := socket.ReadMessage(); err != nil {
				return
			}
		}
	}
	s.twitch = &twitchRuntime{client: twitch.client}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); s.receiveTwitch(ctx, p) }()
	waitForTwitch(t, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, n := range f.nodes {
			if n.Type == "message" && n.Content != nil {
				return true
			}
		}
		return false
	})
	cancel()
	<-done
	twitch.mu.Lock()
	defer twitch.mu.Unlock()
	if twitch.subscriptionCalls != 2 || twitch.socketCount != 2 {
		t.Fatalf("subscriptions=%d sockets=%d", twitch.subscriptionCalls, twitch.socketCount)
	}
}

func TestTwitchWebSocketSlowGraphDoesNotBlockPingAndKeepalive(t *testing.T) {
	f, s, p, event := inboundFixture(t)
	twitch := newTwitchFake(t)
	pong := make(chan struct{}, 1)
	twitch.socket = func(socket *websocket.Conn, session, path string) {
		select {
		case <-twitch.subscribed:
		case <-time.After(3 * time.Second):
			return
		}
		socket.SetPongHandler(func(string) error {
			select {
			case pong <- struct{}{}:
			default:
			}
			return nil
		})
		go func() {
			for {
				if _, _, err := socket.ReadMessage(); err != nil {
					return
				}
			}
		}()
		socket.WriteJSON(fakeNotification(session, event))
		for i := 0; i < 6; i++ {
			socket.WriteJSON(fakeEnvelope("session_keepalive"))
			socket.WriteControl(websocket.PingMessage, []byte("ping"), time.Now().Add(time.Second))
			time.Sleep(200 * time.Millisecond)
		}
	}
	s.twitch = &twitchRuntime{client: twitch.client}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	resourceLock.Lock()
	go func() { defer close(done); s.receiveTwitch(ctx, p) }()
	select {
	case <-pong:
	case <-time.After(2 * time.Second):
		resourceLock.Unlock()
		cancel()
		<-done
		t.Fatal("slow graph prevented pong")
	}
	f.mu.Lock()
	writes := f.writes
	f.mu.Unlock()
	resourceLock.Unlock()
	if writes != 0 {
		cancel()
		<-done
		t.Fatal("writer gate bypassed")
	}
	waitForTwitch(t, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, n := range f.nodes {
			if n.Type == "message" && n.Content != nil {
				return true
			}
		}
		return false
	})
	cancel()
	<-done
}

func TestTwitchIngestWriterGateHonorsCancellation(t *testing.T) {
	f, s, p, event := inboundFixture(t)
	resourceLock.Lock()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := s.ingestTwitch(ctx, p, event)
	resourceLock.Unlock()
	if err != context.DeadlineExceeded || f.writes != 0 {
		t.Fatalf("err=%v writes=%d", err, f.writes)
	}
}

func TestTwitchShutdownDrainsQueuedMessagesWithinBudget(t *testing.T) {
	f, s, p, event := inboundFixture(t)
	twitch := newTwitchFake(t)
	pong := make(chan struct{}, 1)
	twitch.socket = func(socket *websocket.Conn, session, path string) {
		select {
		case <-twitch.subscribed:
		case <-time.After(3 * time.Second):
			return
		}
		socket.SetPongHandler(func(string) error {
			select {
			case pong <- struct{}{}:
			default:
			}
			return nil
		})
		socket.WriteJSON(fakeNotification(session, event))
		event.MessageID = "queued-second"
		socket.WriteJSON(fakeNotification(session, event))
		socket.WriteControl(websocket.PingMessage, []byte("ping"), time.Now().Add(time.Second))
		for {
			if _, _, err := socket.ReadMessage(); err != nil {
				return
			}
		}
	}
	s.twitch = &twitchRuntime{client: twitch.client}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	resourceLock.Lock()
	go func() { defer close(done); s.receiveTwitch(ctx, p) }()
	select {
	case <-pong:
	case <-time.After(3 * time.Second):
		resourceLock.Unlock()
		cancel()
		<-done
		t.Fatal("queued messages not read")
	}
	cancel()
	resourceLock.Unlock()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown did not drain")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	messages := 0
	for _, n := range f.nodes {
		if n.Type == "message" && n.Content != nil {
			messages++
		}
	}
	if messages != 2 {
		t.Fatalf("flushed=%d", messages)
	}
	_, _, dropped := s.twitch.status(p.id)
	if dropped != 0 {
		t.Fatalf("dropped=%d", dropped)
	}
}

func TestTwitchRateLimitSurvivesSocketRetryWithoutEarlyNetwork(t *testing.T) {
	_, _, p, _ := inboundFixture(t)
	twitch := newTwitchFake(t)
	twitch.client.rateMu.Lock()
	twitch.client.retryAt = time.Now().Add(time.Minute)
	twitch.client.rateMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	err := twitch.client.connection(ctx, p, make(chan twitchEvent, 256), func() {})
	if err != context.DeadlineExceeded {
		t.Fatal(err)
	}
	twitch.mu.Lock()
	defer twitch.mu.Unlock()
	if twitch.validationCalls != 0 || twitch.subscriptionCalls != 0 || twitch.socketCount != 0 {
		t.Fatal("rate-limit wait made an early network request")
	}
}

func TestTwitchSubscribeHonorsWelcomeAdvertisedDeadline(t *testing.T) {
	_, _, p, _ := inboundFixture(t)
	f := newTwitchFake(t)
	f.welcome = func(e twitchEnvelope) twitchEnvelope { e.Payload.Session.KeepaliveTimeoutSeconds = 1; return e }
	f.subscribe = func(r *http.Request, sub twitchSubscription) (int, any) {
		time.Sleep(1300 * time.Millisecond)
		return 202, map[string]any{"data": []twitchSubscription{fakeSubscription(sub.Transport.SessionID)}}
	}
	var connected atomic.Bool
	started := time.Now()
	err := f.client.connection(context.Background(), p, make(chan twitchEvent, 256), func() { connected.Store(true) })
	if err == nil || connected.Load() {
		t.Fatalf("err=%v connected=%v elapsed=%s", err, connected.Load(), time.Since(started))
	}
}

func TestTwitchPeerEOFBeforeWelcomeRetriesReceiver(t *testing.T) {
	f, s, p, event := inboundFixture(t)
	twitch := newTwitchFake(t)
	var sockets atomic.Int64
	twitch.beforeWelcome = func(socket *websocket.Conn, path string) bool { return sockets.Add(1) == 1 }
	twitch.socket = func(socket *websocket.Conn, session, path string) {
		select {
		case <-twitch.subscribed:
		case <-time.After(3 * time.Second):
			return
		}
		socket.WriteJSON(fakeNotification(session, event))
		for {
			if _, _, err := socket.ReadMessage(); err != nil {
				return
			}
		}
	}
	s.twitch = &twitchRuntime{client: twitch.client}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); s.receiveTwitch(ctx, p) }()
	waitForTwitch(t, func() bool { state, _, _ := s.twitch.status(p.id); return state == "connected" && sockets.Load() >= 2 })
	waitForTwitch(t, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, n := range f.nodes {
			if n.Type == "message" && n.Content != nil {
				return true
			}
		}
		return false
	})
	cancel()
	<-done
	twitch.mu.Lock()
	defer twitch.mu.Unlock()
	if twitch.subscriptionCalls != 1 || twitch.socketCount != 1 || sockets.Load() < 2 {
		t.Fatalf("subscription=%d socket=%d dials=%d", twitch.subscriptionCalls, twitch.socketCount, sockets.Load())
	}
}
