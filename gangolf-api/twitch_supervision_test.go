package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func multiTenantFixture(t *testing.T) (*resourceFixture, *server, *twitchMultiFake, string, string) {
	t.Helper()
	f, s, _, _ := inboundFixture(t)
	provisionTwitch(t, f, 40, "8", "102")
	changeProfile(f, 40, func(p *twitchProfile) { p.Enabled = new(true); p.BotUserID = "901"; p.CredentialRef = "other-grant" })
	provisionTwitch(t, f, 50, "7", "103")
	changeProfile(f, 50, func(p *twitchProfile) { p.Enabled = new(true) })
	f.legacy.cfg.Users["8"] = grant{Enabled: true, Permissions: []string{"host"}}
	a := addTestKey(f, s, "tenant-a", "7", "host")
	b := addTestKey(f, s, "tenant-b", "8", "host")
	twitch := newTwitchMultiFake(t)
	s.twitch = &twitchRuntime{client: twitch.twitch.client}
	return f, s, twitch, a, b
}

func startTwitchSupervisor(t *testing.T, s *server) (chan time.Time, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ticks := make(chan time.Time)
	done := make(chan struct{})
	go func() { defer close(done); s.reconcileTwitch(ctx, ticks) }()
	stop := func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("supervisor shutdown stalled")
		}
	}
	t.Cleanup(stop)
	return ticks, stop
}

func twitchTick(t *testing.T, ticks chan time.Time) {
	t.Helper()
	select {
	case ticks <- time.Now():
	case <-time.After(3 * time.Second):
		t.Fatal("supervisor tick stalled")
	}
}

func persistedTwitch(t *testing.T, f *resourceFixture, broadcaster, id string) int64 {
	t.Helper()
	var found int64
	waitForTwitch(t, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, n := range f.nodes {
			if n.Name == twitchMessageName(broadcaster, id) && n.Content != nil && *n.Content != "" {
				found = n.ID
				return true
			}
		}
		return false
	})
	return found
}

func TestReconcileTwitch_ConcurrentGrantsAndSharedGrant_PersistsAndIsolatesTenantsAcrossRestart(t *testing.T) {
	f, s, twitch, a, b := multiTenantFixture(t)
	_, stop := startTwitchSupervisor(t, s)
	for _, id := range []int64{30, 40, 50} {
		waitForTwitch(t, func() bool { state, _, _ := s.twitch.status(id); return state == "connected" })
	}
	first := twitch.subscription(t, "101")
	shared := twitch.subscription(t, "103")
	other := twitch.subscription(t, "102")
	if first.Transport.SessionID != shared.Transport.SessionID || first.Transport.SessionID == other.Transport.SessionID {
		t.Fatalf("sessions: %s %s %s", first.Transport.SessionID, shared.Transport.SessionID, other.Transport.SessionID)
	}
	twitch.twitch.mu.Lock()
	sockets, calls := twitch.twitch.socketCount, twitch.twitch.subscriptionCalls
	twitch.twitch.mu.Unlock()
	if sockets != 2 || calls != 3 {
		t.Fatalf("sockets=%d subscriptions=%d", sockets, calls)
	}
	ids := map[string]int64{}
	for _, room := range []string{"101", "102", "103"} {
		twitch.publish(t, room, "same-id")
		ids[room] = persistedTwitch(t, f, room, "same-id")
	}
	for _, tc := range []struct {
		token   string
		rooms   []string
		foreign string
	}{{a, []string{"101", "103"}, "102"}, {b, []string{"102"}, "101"}} {
		w := request(t, s, "GET", "/api/v3/messages", tc.token, "")
		status(t, w, 200)
		var page struct {
			Result []messageResource
			Total  int
		}
		if json.Unmarshal(w.Body.Bytes(), &page) != nil || page.Total != len(tc.rooms) || len(page.Result) != len(tc.rooms) {
			t.Fatal(w.Body.String())
		}
		for _, message := range page.Result {
			if message.Channel == tc.foreign {
				t.Fatal("foreign message listed")
			}
		}
		foreign := strconv.FormatInt(ids[tc.foreign], 10)
		for _, method := range []string{"GET", "PATCH", "DELETE"} {
			body := ""
			if method == "PATCH" {
				body = `[{"op":"replace","path":"/tts","value":true}]`
			}
			status(t, twitchRequest(t, s, method, "/api/v3/messages/"+foreign, tc.token, body), 404)
		}
		reply := strings.TrimSuffix(strings.ReplaceAll(v3Message, "twitch", "localhost"), "}") + `,"replyTo":"` + foreign + `"}`
		status(t, request(t, s, "POST", "/api/v3/messages", tc.token, reply), 404)
		channels := request(t, s, "GET", "/api/i1/twitch/channels", tc.token, "")
		status(t, channels, 200)
		if strings.Contains(channels.Body.String(), `"broadcasterUserId":"`+tc.foreign+`"`) {
			t.Fatal(channels.Body.String())
		}
	}
	stop()
	fresh := *s
	fresh.graph = &graph{base: s.graph.base, token: s.graph.token, root: 10, group: 11, config: 12, http: s.graph.http}
	fresh.twitch = &twitchRuntime{client: twitch.twitch.client}
	f.mu.Lock()
	before := f.writes
	f.queries = nil
	f.mu.Unlock()
	_, stopFresh := startTwitchSupervisor(t, &fresh)
	waitForTwitch(t, func() bool { state, _, _ := fresh.twitch.status(50); return state == "connected" })
	for _, room := range []string{"101", "102", "103"} {
		twitch.publish(t, room, "same-id")
	}
	waitForTwitch(t, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		seen := map[string]bool{}
		for _, q := range f.queries {
			for room := range ids {
				if q.Get("name") == twitchMessageName(room, "same-id") {
					seen[room] = true
				}
			}
		}
		return len(seen) == 3
	})
	stopFresh()
	f.mu.Lock()
	if f.writes != before {
		t.Fatalf("redelivery wrote: %d -> %d", before, f.writes)
	}
	f.mu.Unlock()
	_, stopAgain := startTwitchSupervisor(t, &fresh)
	waitForTwitch(t, func() bool { state, _, _ := fresh.twitch.status(50); return state == "connected" })
	for _, room := range []string{"101", "102", "103"} {
		twitch.publish(t, room, "after-restart")
		persistedTwitch(t, f, room, "after-restart")
	}
	stopAgain()
	f.mu.Lock()
	defer f.mu.Unlock()
	messages := 0
	for _, n := range f.nodes {
		if n.Type == "message" {
			messages++
		}
	}
	if messages != 6 || f.writes <= before {
		t.Fatalf("messages=%d writes=%d before=%d", messages, f.writes, before)
	}
	for room, id := range ids {
		n := f.nodes[id]
		if n.Name != twitchMessageName(room, "same-id") || n.Content == nil {
			t.Fatal("durable identity changed")
		}
	}
}

func TestReconcileTwitch_GrantOutageAndConfigChanges_LeavesOtherGrantConnectedAndRecovers(t *testing.T) {
	f, s, twitch, _, _ := multiTenantFixture(t)
	ticks, _ := startTwitchSupervisor(t, s)
	for _, room := range []string{"101", "102", "103"} {
		twitch.subscription(t, room)
	}
	healthy := twitch.subscription(t, "102")
	failed := twitch.subscription(t, "101")
	twitch.send(t, "101", nil)
	waitForTwitch(t, func() bool {
		state, code, _ := s.twitch.status(30)
		return state == "degraded" && code == "twitch_unavailable"
	})
	state, _, _ := s.twitch.status(40)
	if state != "connected" {
		t.Fatal(state)
	}
	twitch.publish(t, "102", "during-outage")
	persistedTwitch(t, f, "102", "during-outage")
	waitForTwitch(t, func() bool {
		twitch.mu.Lock()
		defer twitch.mu.Unlock()
		return twitch.subscriptions["101"].Transport.SessionID != failed.Transport.SessionID
	})
	twitch.publish(t, "101", "recovered")
	persistedTwitch(t, f, "101", "recovered")
	f.mu.Lock()
	changeProfile(f, 50, func(p *twitchProfile) { p.Enabled = new(false) })
	f.mu.Unlock()
	twitchTick(t, ticks)
	waitForTwitch(t, func() bool { state, _, _ := s.twitch.status(50); return state == "disabled" })
	waitForTwitch(t, func() bool { state, _, _ := s.twitch.status(30); return state == "connected" })
	if got := twitch.subscription(t, "102"); got.Transport.SessionID != healthy.Transport.SessionID {
		t.Fatal("healthy grant restarted")
	}
	twitch.publish(t, "101", "after-disable")
	persistedTwitch(t, f, "101", "after-disable")
	f.mu.Lock()
	changeProfile(f, 50, func(p *twitchProfile) { p.Enabled = new(true) })
	f.mu.Unlock()
	twitchTick(t, ticks)
	waitForTwitch(t, func() bool { state, _, _ := s.twitch.status(50); return state == "connected" })
	twitch.publish(t, "103", "reenabled")
	persistedTwitch(t, f, "103", "reenabled")
	if got := twitch.subscription(t, "102"); got.Transport.SessionID != healthy.Transport.SessionID {
		t.Fatal("healthy grant restarted after enable")
	}
}

func TestLoadTwitchProfiles_SharedGrantWithDivergentCredentialRefs_RejectsWithoutWrites(t *testing.T) {
	f, s, _, _, _ := multiTenantFixture(t)
	changeProfile(f, 50, func(p *twitchProfile) { p.CredentialRef = "different-grant" })
	if _, err := s.graph.loadConfig(context.Background()); err != errCorrupt {
		t.Fatalf("err=%v", err)
	}
	if f.writes != 0 {
		t.Fatal("invalid config wrote")
	}
}

func TestReconcileTwitch_SharedChannelRevocationOrForbidden_PreservesSiblingSubscription(t *testing.T) {
	for _, kind := range []string{"revocation", "forbidden"} {
		t.Run(kind, func(t *testing.T) {
			f, s, twitch, _, _ := multiTenantFixture(t)
			if kind == "forbidden" {
				twitch.denied["103"] = true
			}
			_, stop := startTwitchSupervisor(t, s)
			healthy := twitch.subscription(t, "101")
			twitch.subscription(t, "102")
			if kind == "revocation" {
				revoked := twitch.subscription(t, "103")
				e := fakeEnvelope("revocation")
				e.Payload.Subscription = revoked
				e.Payload.Subscription.Status = "authorization_revoked"
				twitch.send(t, "103", &e)
			}
			waitForTwitch(t, func() bool {
				state, code, _ := s.twitch.status(50)
				return state == "auth-required" && code == "subscription_forbidden"
			})
			state, _, _ := s.twitch.status(30)
			if state != "connected" {
				t.Fatal(state)
			}
			twitch.publish(t, "101", "sibling-healthy")
			persistedTwitch(t, f, "101", "sibling-healthy")
			w := request(t, s, "GET", "/api/i1/twitch/channels", "key.secret", "")
			status(t, w, 200)
			if !strings.Contains(w.Body.String(), `"subscription_forbidden"`) {
				t.Fatal(w.Body.String())
			}
			if got := twitch.subscription(t, "101"); got.Transport.SessionID != healthy.Transport.SessionID {
				t.Fatal("sibling restarted")
			}
			twitch.send(t, "101", nil)
			waitForTwitch(t, func() bool {
				twitch.mu.Lock()
				defer twitch.mu.Unlock()
				return twitch.subscriptions["101"].Transport.SessionID != healthy.Transport.SessionID
			})
			twitch.publish(t, "101", "sibling-reconnected")
			persistedTwitch(t, f, "101", "sibling-reconnected")
			state, _, _ = s.twitch.status(50)
			if state != "auth-required" {
				t.Fatal("blocked subscription reactivated: " + state)
			}
			twitch.twitch.mu.Lock()
			calls := twitch.twitch.subscriptionCalls
			twitch.twitch.mu.Unlock()
			if calls != 4 {
				t.Fatalf("subscriptions=%d", calls)
			}
			stop()
		})
	}
}

func TestReconcileTwitch_StorageOutage_StopsAllGrantsAndRecoversPersistedMessages(t *testing.T) {
	f, s, twitch, _, _ := multiTenantFixture(t)
	ticks, _ := startTwitchSupervisor(t, s)
	original := map[string]string{}
	for _, room := range []string{"101", "102", "103"} {
		original[room] = twitch.subscription(t, room).Transport.SessionID
		twitch.publish(t, room, "before-storage")
		persistedTwitch(t, f, room, "before-storage")
	}
	f.mu.Lock()
	f.fail = "list"
	before := f.writes
	f.mu.Unlock()
	twitchTick(t, ticks)
	for _, id := range []int64{30, 40, 50} {
		waitForTwitch(t, func() bool {
			state, code, _ := s.twitch.status(id)
			return state == "degraded" && code == "configuration_unavailable"
		})
	}
	f.mu.Lock()
	if f.writes != before {
		t.Fatal("storage outage wrote")
	}
	f.fail = ""
	f.mu.Unlock()
	twitchTick(t, ticks)
	for _, room := range []string{"101", "102", "103"} {
		waitForTwitch(t, func() bool {
			twitch.mu.Lock()
			defer twitch.mu.Unlock()
			return twitch.subscriptions[room].Transport.SessionID != original[room]
		})
		twitch.publish(t, room, "after-storage")
		persistedTwitch(t, f, room, "after-storage")
	}
	w := request(t, s, "GET", "/api/v3/messages", "key.secret", "")
	status(t, w, 200)
	var page struct{ Total int }
	if json.Unmarshal(w.Body.Bytes(), &page) != nil || page.Total != 6 {
		t.Fatal(w.Body.String())
	}
}

func TestReconcileTwitch_OneGrantCredentialUnavailable_RecoversWithoutRestartingOtherGrant(t *testing.T) {
	f, s, twitch, _, _ := multiTenantFixture(t)
	path := filepath.Join(twitch.twitch.client.directory, "other-grant")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	ticks, _ := startTwitchSupervisor(t, s)
	healthy := twitch.subscription(t, "101")
	waitForTwitch(t, func() bool {
		state, code, _ := s.twitch.status(40)
		return state == "auth-required" && code == "credential_unavailable"
	})
	twitch.publish(t, "101", "credential-outage")
	persistedTwitch(t, f, "101", "credential-outage")
	twitchTick(t, ticks)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	twitchTick(t, ticks)
	waitForTwitch(t, func() bool { state, _, _ := s.twitch.status(40); return state == "connected" })
	twitch.publish(t, "102", "credential-recovered")
	persistedTwitch(t, f, "102", "credential-recovered")
	if got := twitch.subscription(t, "101"); got.Transport.SessionID != healthy.Transport.SessionID {
		t.Fatal("credential failure restarted other grant")
	}
}

func TestReconcileTwitch_SharedGrantRefresh_RotatesOnceAndRetainsSocketAtReconcile(t *testing.T) {
	f, s, twitch, _, _ := multiTenantFixture(t)
	original := twitch.twitch.validate
	twitch.twitch.validate = func(r *http.Request) (int, any) {
		if r.Header.Get("Authorization") == "OAuth local-access" {
			return 401, map[string]any{}
		}
		return original(r)
	}
	ticks, _ := startTwitchSupervisor(t, s)
	for _, id := range []int64{30, 40, 50} {
		waitForTwitch(t, func() bool { state, _, _ := s.twitch.status(id); return state == "connected" })
	}
	before := twitch.subscription(t, "101")
	twitchTick(t, ticks)
	twitch.publish(t, "101", "after-refresh")
	persistedTwitch(t, f, "101", "after-refresh")
	twitch.publish(t, "103", "after-refresh")
	persistedTwitch(t, f, "103", "after-refresh")
	twitch.twitch.mu.Lock()
	refreshes, sockets, subscriptions := twitch.twitch.refreshCalls, twitch.twitch.socketCount, twitch.twitch.subscriptionCalls
	twitch.twitch.mu.Unlock()
	if refreshes != 1 || sockets != 2 || subscriptions != 3 {
		t.Fatalf("refreshes=%d sockets=%d subscriptions=%d", refreshes, sockets, subscriptions)
	}
	if after := twitch.subscription(t, "101"); after.Transport.SessionID != before.Transport.SessionID {
		t.Fatal("refresh restarted socket")
	}
}

func TestReconcileTwitch_ExternalCredentialRotationDuringSubscription_RestartsOnlyAffectedGrant(t *testing.T) {
	f, s, twitch, _, _ := multiTenantFixture(t)
	original := twitch.twitch.subscribe
	var rotated sync.Once
	twitch.twitch.subscribe = func(r *http.Request, sub twitchSubscription) (int, any) {
		if sub.Condition.BroadcasterUserID == "103" {
			rotated.Do(func() {
				if err := writeTwitchCredential(twitch.twitch.client.directory, "test-grant", twitchCredential{AccessToken: "operator-rotated-access", RefreshToken: "operator-rotated-refresh"}); err != nil {
					t.Error(err)
				}
			})
		}
		return original(r, sub)
	}
	ticks, _ := startTwitchSupervisor(t, s)
	for _, id := range []int64{30, 40, 50} {
		waitForTwitch(t, func() bool { state, _, _ := s.twitch.status(id); return state == "connected" })
	}
	previous := twitch.subscription(t, "101")
	healthy := twitch.subscription(t, "102")
	twitchTick(t, ticks)
	waitForTwitch(t, func() bool {
		twitch.mu.Lock()
		defer twitch.mu.Unlock()
		return twitch.subscriptions["101"].Transport.SessionID != previous.Transport.SessionID
	})
	twitch.publish(t, "103", "operator-rotation")
	persistedTwitch(t, f, "103", "operator-rotation")
	if other := twitch.subscription(t, "102"); other.Transport.SessionID != healthy.Transport.SessionID {
		t.Fatal("rotation restarted other grant")
	}
}
