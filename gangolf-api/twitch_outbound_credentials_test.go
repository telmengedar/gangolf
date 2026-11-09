package main

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestAuthorizeSend_ProfileTokenScopesAndIdentity_BlocksBeforeIntent(t *testing.T) {
	for _, mode := range []string{"read-only", "write-only", "wrong-client", "wrong-bot", "expired-refresh-rejected", "refresh-malformed"} {
		t.Run(mode, func(t *testing.T) {
			f, s, tw := outboundFixture(t)
			tw.validate = func(r *http.Request) (int, any) {
				if strings.HasPrefix(mode, "expired-") || mode == "refresh-malformed" {
					return 401, map[string]any{}
				}
				client, bot := "testclient", "900"
				scopes := []string{"user:read:chat", "user:write:chat"}
				if mode == "read-only" {
					scopes = []string{"user:read:chat"}
				}
				if mode == "write-only" {
					scopes = []string{"user:write:chat"}
				}
				if mode == "wrong-client" {
					client = "other"
				}
				if mode == "wrong-bot" {
					bot = "other"
				}
				return 200, map[string]any{"client_id": client, "user_id": bot, "scopes": scopes, "expires_in": 3600}
			}
			tw.refresh = func(*http.Request) (int, any) {
				if mode == "refresh-malformed" {
					return 200, map[string]any{"access_token": "missing-refresh"}
				}
				return 400, map[string]any{}
			}
			w := outboundRequest(s, "key.secret", "scope", outboundBody)
			status(t, w, 503)
			if tw.validationCalls == 0 || f.writes != 0 || sends(tw) != 0 {
				t.Fatal("token rejection not reached before intent")
			}
			if mode == "read-only" {
				cfg, err := s.graph.loadConfig(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				if _, err := tw.client.authorize(context.Background(), cfg.twitch[0]); err != nil {
					t.Fatalf("read-only receiver authorization broken: %v", err)
				}
			}
		})
	}
}

func TestAuthorizeSend_ConcurrentReceiveRefresh_RotatesOnceAndUsesCurrentCredential(t *testing.T) {
	f, s, tw := outboundFixture(t)
	cfg, err := s.graph.loadConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.twitch[0]
	var oldValidations, newValidations, refreshes atomic.Int64
	tw.validate = func(r *http.Request) (int, any) {
		if r.Header.Get("Authorization") == "OAuth local-access" {
			oldValidations.Add(1)
			return 401, map[string]any{}
		}
		if r.Header.Get("Authorization") != "OAuth rotated-access" {
			t.Error("unknown validation token")
		}
		newValidations.Add(1)
		return 200, map[string]any{"client_id": "testclient", "user_id": "900", "scopes": []string{"user:read:chat", "user:write:chat"}, "expires_in": 3600}
	}
	tw.refresh = func(r *http.Request) (int, any) {
		refreshes.Add(1)
		if r.ParseForm() != nil || r.Form.Get("refresh_token") != "local-refresh" || r.Form.Get("client_id") != "testclient" {
			t.Error("stale or invalid refresh grant")
		}
		return 200, map[string]any{"access_token": "rotated-access", "refresh_token": "rotated-refresh"}
	}
	tw.send = func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer rotated-access" {
			t.Error("send used stale credential")
		}
		respond(w, 200, map[string]any{"data": []any{map[string]any{"is_sent": true, "message_id": "after-refresh"}}})
	}
	receiver := productionTwitchClient(tw.client.directory)
	receiver.validateURL, receiver.tokenURL, receiver.subscriptionURL, receiver.socketURL = tw.client.validateURL, tw.client.tokenURL, tw.client.subscriptionURL, tw.client.socketURL
	start := make(chan struct{})
	var work sync.WaitGroup
	work.Add(2)
	var responseStatus int
	var receiveErr error
	go func() {
		defer work.Done()
		<-start
		receiveCredential, err := receiver.authorize(context.Background(), p)
		receiveErr = err
		if err == nil && receiveCredential.AccessToken != "rotated-access" {
			t.Error("receiver got stale token")
		}
	}()
	go func() {
		defer work.Done()
		<-start
		responseStatus = outboundRequest(s, "key.secret", "refresh", outboundBody).Code
	}()
	close(start)
	work.Wait()
	if responseStatus != 201 || receiveErr != nil || refreshes.Load() != 1 || oldValidations.Load() != 1 || newValidations.Load() < 2 || sends(tw) != 1 {
		t.Fatalf("status=%d receive=%v refresh=%d old=%d new=%d sends=%d", responseStatus, receiveErr, refreshes.Load(), oldValidations.Load(), newValidations.Load(), sends(tw))
	}
	stored, err := readTwitchCredential(tw.client.directory, p.CredentialRef)
	if err != nil || stored.AccessToken != "rotated-access" || stored.RefreshToken != "rotated-refresh" {
		t.Fatal("rotation not persisted")
	}
	if f.writes == 0 {
		t.Fatal("API did not publish intent")
	}
}

func TestRefresh_StaleReceiverCredential_ReusesSenderRotation(t *testing.T) {
	_, s, tw := outboundFixture(t)
	cfg, err := s.graph.loadConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.twitch[0]
	old, err := readTwitchCredential(tw.client.directory, p.CredentialRef)
	if err != nil {
		t.Fatal(err)
	}
	updated := old
	updated.AccessToken = "rotated-access"
	updated.RefreshToken = "rotated-refresh"
	if err := writeTwitchCredential(tw.client.directory, p.CredentialRef, updated); err != nil {
		t.Fatal(err)
	}
	got, err := tw.client.refresh(context.Background(), p, old)
	if err != nil || got != updated || tw.refreshCalls != 0 {
		t.Fatalf("stale refresh was not reused: %v calls=%d", err, tw.refreshCalls)
	}
}
