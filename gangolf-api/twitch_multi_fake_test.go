package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type twitchMultiFake struct {
	mu            sync.Mutex
	twitch        *twitchFake
	subscriptions map[string]twitchSubscription
	commands      map[string]chan *twitchEnvelope
	denied        map[string]bool
}

func newTwitchMultiFake(t *testing.T) *twitchMultiFake {
	t.Helper()
	f := &twitchMultiFake{twitch: newTwitchFake(t), subscriptions: map[string]twitchSubscription{}, commands: map[string]chan *twitchEnvelope{}, denied: map[string]bool{}}
	data, _ := json.Marshal(twitchCredential{AccessToken: "other-access", RefreshToken: "other-refresh"})
	if err := os.WriteFile(filepath.Join(f.twitch.client.directory, "other-grant"), data, 0600); err != nil {
		t.Fatal(err)
	}
	f.twitch.validate = func(r *http.Request) (int, any) {
		bot := "900"
		if r.Header.Get("Authorization") == "OAuth other-access" {
			bot = "901"
		}
		return 200, map[string]any{"client_id": "testclient", "user_id": bot, "scopes": []string{"user:read:chat"}, "expires_in": 3600}
	}
	f.twitch.subscribe = func(r *http.Request, sub twitchSubscription) (int, any) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.denied[sub.Condition.BroadcasterUserID] {
			return 403, map[string]any{}
		}
		sub.ID = "subscription-" + sub.Condition.BroadcasterUserID
		sub.Status = "enabled"
		f.subscriptions[sub.Condition.BroadcasterUserID] = sub
		return 202, map[string]any{"data": []twitchSubscription{sub}}
	}
	f.twitch.socket = func(socket *websocket.Conn, session, path string) {
		commands := make(chan *twitchEnvelope)
		f.mu.Lock()
		f.commands[session] = commands
		f.mu.Unlock()
		closed := make(chan struct{})
		go func() {
			defer close(closed)
			for {
				if _, _, err := socket.ReadMessage(); err != nil {
					return
				}
			}
		}()
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-closed:
				return
			case e := <-commands:
				if e == nil {
					return
				}
				if socket.WriteJSON(e) != nil {
					return
				}
			case <-ticker.C:
				if socket.WriteJSON(fakeEnvelope("session_keepalive")) != nil {
					return
				}
			}
		}
	}
	return f
}

func (f *twitchMultiFake) subscription(t *testing.T, broadcaster string) twitchSubscription {
	t.Helper()
	var sub twitchSubscription
	waitForTwitch(t, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		sub = f.subscriptions[broadcaster]
		return sub.ID != "" && f.commands[sub.Transport.SessionID] != nil
	})
	return sub
}

func (f *twitchMultiFake) send(t *testing.T, broadcaster string, e *twitchEnvelope) {
	t.Helper()
	sub := f.subscription(t, broadcaster)
	f.mu.Lock()
	commands := f.commands[sub.Transport.SessionID]
	f.mu.Unlock()
	select {
	case commands <- e:
	case <-time.After(3 * time.Second):
		t.Fatal("socket script stalled")
	}
}

func (f *twitchMultiFake) publish(t *testing.T, broadcaster, id string) {
	t.Helper()
	sub := f.subscription(t, broadcaster)
	e := fakeEnvelope("notification")
	e.Metadata.SubscriptionType = "channel.chat.message"
	e.Metadata.SubscriptionVersion = "1"
	e.Payload.Subscription = sub
	event := twitchEvent{BroadcasterUserID: broadcaster, BroadcasterUserLogin: "room-" + broadcaster, ChatterUserID: "700", ChatterUserLogin: "speaker", ChatterUserName: "Speaker", MessageID: id}
	event.Message.Text = "message for " + broadcaster
	e.Payload.Event, _ = json.Marshal(event)
	f.send(t, broadcaster, &e)
}
