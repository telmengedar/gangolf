package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type twitchFake struct {
	mu                sync.Mutex
	host              *httptest.Server
	client            *twitchClient
	subscribed        chan string
	sockets           []*websocket.Conn
	socketCount       int
	subscriptionCalls int
	validationCalls   int
	refreshCalls      int
	validate          func(*http.Request) (int, any)
	refresh           func(*http.Request) (int, any)
	subscribe         func(*http.Request, twitchSubscription) (int, any)
	welcome           func(twitchEnvelope) twitchEnvelope
	socket            func(*websocket.Conn, string, string)
}

func newTwitchFake(t *testing.T) *twitchFake {
	t.Helper()
	f := &twitchFake{subscribed: make(chan string, 20)}
	f.host = httptest.NewServer(http.HandlerFunc(f.serve))
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(twitchCredential{AccessToken: "local-access", RefreshToken: "local-refresh", ClientSecret: "local-client"})
	if err := os.WriteFile(filepath.Join(directory, "test-grant"), data, 0600); err != nil {
		t.Fatal(err)
	}
	f.client = productionTwitchClient(directory)
	f.client.validateURL = f.host.URL + "/validate"
	f.client.tokenURL = f.host.URL + "/token"
	f.client.subscriptionURL = f.host.URL + "/subscriptions"
	f.client.socketURL = "ws" + strings.TrimPrefix(f.host.URL, "http") + "/ws"
	t.Cleanup(func() {
		f.mu.Lock()
		sockets := append([]*websocket.Conn(nil), f.sockets...)
		f.mu.Unlock()
		for _, socket := range sockets {
			socket.Close()
		}
		f.host.Close()
	})
	return f
}

func fakeEnvelope(kind string) twitchEnvelope {
	var e twitchEnvelope
	e.Metadata.MessageID = "envelope-1"
	e.Metadata.MessageType = kind
	e.Metadata.MessageTimestamp = "2026-11-02T12:00:00Z"
	return e
}

func fakeSubscription(session string) twitchSubscription {
	var sub twitchSubscription
	sub.ID = "subscription-1"
	sub.Status = "enabled"
	sub.Type = "channel.chat.message"
	sub.Version = "1"
	sub.Condition.BroadcasterUserID = "101"
	sub.Condition.UserID = "900"
	sub.Transport.Method = "websocket"
	sub.Transport.SessionID = session
	return sub
}

func fakeNotification(session string, event twitchEvent) twitchEnvelope {
	e := fakeEnvelope("notification")
	e.Metadata.SubscriptionType = "channel.chat.message"
	e.Metadata.SubscriptionVersion = "1"
	e.Payload.Subscription = fakeSubscription(session)
	e.Payload.Event, _ = json.Marshal(event)
	return e
}

func (f *twitchFake) serve(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/validate":
		f.mu.Lock()
		f.validationCalls++
		f.mu.Unlock()
		if f.validate != nil {
			code, body := f.validate(r)
			respond(w, code, body)
			return
		}
		respond(w, 200, map[string]any{"client_id": "testclient", "user_id": "900", "scopes": []string{"user:read:chat"}, "expires_in": 3600})
	case "/token":
		f.mu.Lock()
		f.refreshCalls++
		f.mu.Unlock()
		if f.refresh != nil {
			code, body := f.refresh(r)
			respond(w, code, body)
			return
		}
		respond(w, 200, map[string]any{"access_token": "rotated-access", "refresh_token": "rotated-refresh"})
	case "/subscriptions":
		f.mu.Lock()
		f.subscriptionCalls++
		f.mu.Unlock()
		var sub twitchSubscription
		if json.NewDecoder(r.Body).Decode(&sub) != nil {
			w.WriteHeader(400)
			return
		}
		if sub.Type != "channel.chat.message" || sub.Version != "1" || sub.Condition.UserID != "900" || sub.Condition.BroadcasterUserID != "101" || sub.Transport.Method != "websocket" || sub.Transport.SessionID == "" || r.Header.Get("Client-Id") != "testclient" || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			w.WriteHeader(400)
			return
		}
		if f.subscribe != nil {
			code, body := f.subscribe(r, sub)
			if code != 202 {
				w.Header().Set("Retry-After", "1")
				respond(w, code, body)
				return
			}
			respond(w, code, body)
		} else {
			respond(w, 202, map[string]any{"data": []twitchSubscription{fakeSubscription(sub.Transport.SessionID)}})
		}
		f.subscribed <- sub.Transport.SessionID
	case "/ws", "/transfer":
		if r.Header.Get("Authorization") != "" || r.Header.Get("Client-Id") != "" {
			w.WriteHeader(400)
			return
		}
		upgrader := websocket.Upgrader{}
		socket, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer socket.Close()
		f.mu.Lock()
		f.socketCount++
		session := "session-" + strconv.Itoa(f.socketCount)
		f.sockets = append(f.sockets, socket)
		f.mu.Unlock()
		welcome := fakeEnvelope("session_welcome")
		welcome.Payload.Session.ID = session
		welcome.Payload.Session.Status = "connected"
		welcome.Payload.Session.KeepaliveTimeoutSeconds = 1
		if f.welcome != nil {
			welcome = f.welcome(welcome)
		}
		if socket.WriteJSON(welcome) != nil {
			return
		}
		if f.socket != nil {
			f.socket(socket, session, r.URL.RequestURI())
			return
		}
		for {
			if _, _, err := socket.ReadMessage(); err != nil {
				return
			}
		}
	default:
		w.WriteHeader(404)
	}
}

func waitForTwitch(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		if condition() {
			return
		}
		select {
		case <-deadline.C:
			t.Fatal("timed out waiting for local Twitch state")
		case <-ticker.C:
		}
	}
}
