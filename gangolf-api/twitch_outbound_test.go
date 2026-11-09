package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const outboundBody = `{"service":"Twitch","channel":"101","user":{"service":"Twitch","serviceId":"GangolfBot","name":"GangolfBot"},"chunks":[{"type":"Text","data":"!Grüße "},{"type":0,"data":"日本語 👋"}],"tts":false}`

func outboundFixture(t *testing.T) (*resourceFixture, *server, *twitchFake) {
	t.Helper()
	f, s := newResourceFixture(t)
	provisionTwitch(t, f, 30, "7", "101")
	changeProfile(f, 30, func(p *twitchProfile) { p.Enabled = new(true) })
	tw := newTwitchFake(t)
	tw.validate = func(r *http.Request) (int, any) {
		if r.Header.Get("Authorization") != "OAuth local-access" && r.Header.Get("Authorization") != "OAuth rotated-access" {
			t.Errorf("unexpected validation authorization")
		}
		return 200, map[string]any{"client_id": "testclient", "user_id": "900", "scopes": []string{"user:read:chat", "user:write:chat"}, "expires_in": 3600}
	}
	s.twitch = &twitchRuntime{client: tw.client}
	return f, s, tw
}

func outboundRequest(s *server, token, key, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/api/v3/messages", strings.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func outboundResource(t *testing.T, w *httptest.ResponseRecorder, state string) messageResource {
	t.Helper()
	var m messageResource
	if json.Unmarshal(w.Body.Bytes(), &m) != nil || m.ID == "" || m.Delivery == nil || m.Delivery.State != state || w.Header().Get("Location") != deliveryLocation(mustResourceID(m.ID)) {
		t.Fatalf("unexpected delivery resource: status=%d body=%s", w.Code, w.Body.String())
	}
	return m
}

func sends(tw *twitchFake) int {
	tw.mu.Lock()
	defer tw.mu.Unlock()
	return tw.sendCalls
}

func storedOutbound(t *testing.T, f *resourceFixture, id string) messageContent {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	n, exists := f.nodes[mustResourceID(id)]
	if !exists || n.Content == nil {
		t.Fatal("missing graph content")
	}
	var m messageContent
	if json.Unmarshal([]byte(*n.Content), &m) != nil {
		t.Fatal("invalid graph content")
	}
	return m
}

func TestCreateV3_ExactMarker_PublishesAndSendsConfiguredBot(t *testing.T) {
	f, s, tw := outboundFixture(t)
	host := addTestKey(f, s, "host", "7", "host")
	var payload map[string]string
	tw.send = func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.RawQuery != "" || r.Header.Get("Authorization") != "Bearer local-access" || r.Header.Get("Client-Id") != "testclient" || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Idempotency-Key") != "" {
			t.Error("incorrect Helix request headers")
		}
		if json.NewDecoder(r.Body).Decode(&payload) != nil {
			t.Error("invalid send JSON")
		}
		f.mu.Lock()
		intents := 0
		for _, n := range f.nodes {
			if validOutboundName(n.Name) && n.Content != nil {
				var m messageContent
				json.Unmarshal([]byte(*n.Content), &m)
				if m.SchemaVersion == 3 && m.Delivery != nil && m.Delivery.State == "unknown" && n.SkipEmbedding != nil && *n.SkipEmbedding && n.Root == 10 && n.Owner == 20 && n.privateAccess() && len(f.projection(n).Links) == 3 {
					intents++
				}
			}
		}
		f.mu.Unlock()
		if intents != 1 {
			t.Errorf("confirmed graph intents before send=%d", intents)
		}
		respond(w, 200, map[string]any{"data": []any{map[string]any{"is_sent": true, "message_id": "sent-1"}}})
	}
	w := outboundRequest(s, host, "request_1", outboundBody)
	status(t, w, 201)
	m := outboundResource(t, w, "sent")
	if !reflect.DeepEqual(payload, map[string]string{"broadcaster_id": "101", "sender_id": "900", "message": "!Grüße 日本語 👋"}) || sends(tw) != 1 || m.Source != nil || m.Delivery.MessageID != "sent-1" || m.Delivery.AuthUserID != "7" || m.Delivery.ConfigID != "30" {
		t.Fatalf("incorrect send payload/result: %v %+v", payload, m)
	}
	stored := storedOutbound(t, f, m.ID)
	if stored.SchemaVersion != 3 || stored.ChannelID != "31" || !reflect.DeepEqual(stored.Delivery, m.Delivery) {
		t.Fatal("graph did not retain delivery")
	}
	status(t, request(t, s, "GET", deliveryLocation(mustResourceID(m.ID)), host, ""), 200)
	list := request(t, s, "GET", "/api/v3/messages", host, "")
	status(t, list, 200)
	if !strings.Contains(list.Body.String(), `"state":"sent"`) {
		t.Fatal(list.Body.String())
	}
}

func TestCreateV3_SendShape_RejectsBeforeNetwork(t *testing.T) {
	cases := []struct {
		name, body, key string
		status          int
		code            string
	}{
		{"missing-key", outboundBody, "", 400, "invalid_idempotency_key"},
		{"bad-key", outboundBody, "bad key", 400, "invalid_idempotency_key"},
		{"long-key", outboundBody, strings.Repeat("a", 129), 400, "invalid_idempotency_key"},
		{"tts", strings.Replace(outboundBody, `"tts":false`, `"tts":true`, 1), "key", 422, "delivery_not_supported"},
		{"reply", strings.TrimSuffix(outboundBody, "}") + `,"replyTo":"100"}`, "key", 422, "delivery_not_supported"},
		{"post", strings.TrimSuffix(outboundBody, "}") + `,"postTo":[]}`, "key", 422, "delivery_not_supported"},
		{"emote", strings.Replace(outboundBody, `"type":"Text"`, `"type":"Emote"`, 1), "key", 422, "delivery_not_supported"},
		{"login", strings.Replace(outboundBody, `"channel":"101"`, `"channel":"login"`, 1), "key", 422, "delivery_not_supported"},
		{"newline", strings.Replace(outboundBody, "!Grüße ", `hello\n`, 1), "key", 422, "delivery_not_supported"},
		{"nul", strings.Replace(outboundBody, "!Grüße ", `hello\u0000`, 1), "key", 422, "delivery_not_supported"},
		{"unicode-limit", strings.Replace(outboundBody, "!Grüße ", strings.Repeat("界", 501), 1), "key", 422, "message_too_large"},
		{"empty-chunks", `{"service":"Twitch","channel":"101","user":{"service":"Twitch","serviceId":"GangolfBot","name":"GangolfBot"},"chunks":[],"tts":false}`, "key", 422, "delivery_not_supported"},
		{"invalid-utf8", strings.Replace(outboundBody, "!Grüße ", string([]byte{0xff}), 1), "key", 422, "delivery_not_supported"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, s, tw := outboundFixture(t)
			w := outboundRequest(s, "key.secret", c.key, c.body)
			status(t, w, c.status)
			if !strings.Contains(w.Body.String(), `"code":"`+c.code+`"`) || sends(tw) != 0 || tw.validationCalls != 0 || f.writes != 0 {
				t.Fatalf("unexpected rejection: %s sends=%d validations=%d writes=%d", w.Body.String(), sends(tw), tw.validationCalls, f.writes)
			}
		})
	}
}

func TestCreateV3_OrdinaryStorageAndDuplicateHeader_PreservesBoundary(t *testing.T) {
	for _, variation := range []string{"ordinary", "wrong-marker", "v2", "null-post", "duplicate-key", "unauthenticated"} {
		t.Run(variation, func(t *testing.T) {
			_, s, tw := outboundFixture(t)
			var w *httptest.ResponseRecorder
			switch variation {
			case "ordinary":
				w = request(t, s, "POST", "/api/v3/messages", "key.secret", v3Message)
			case "wrong-marker":
				w = outboundRequest(s, "key.secret", "", strings.Replace(outboundBody, `"name":"GangolfBot"`, `"name":"Other"`, 1))
			case "v2":
				w = request(t, s, "POST", "/api/v2/messages", "key.secret", "["+outboundBody+"]")
			case "null-post":
				w = outboundRequest(s, "key.secret", "key", strings.TrimSuffix(outboundBody, "}")+`,"postTo":null}`)
			case "unauthenticated":
				w = outboundRequest(s, "", "key", outboundBody)
			case "duplicate-key":
				r := httptest.NewRequest("POST", "/api/v3/messages", strings.NewReader(outboundBody))
				r.Header.Set("Authorization", "Bearer key.secret")
				r.Header["Idempotency-Key"] = []string{"key", "key"}
				w = httptest.NewRecorder()
				s.ServeHTTP(w, r)
			}
			want := 201
			if variation == "duplicate-key" {
				want = 400
			}
			if variation == "unauthenticated" {
				want = 401
			}
			status(t, w, want)
			count := 0
			if variation == "null-post" {
				count = 1
			}
			if sends(tw) != count {
				t.Fatalf("sends=%d", sends(tw))
			}
		})
	}
}

func TestCreateV3_TenantAndProfileAuthorization_DeniesUnboundTargets(t *testing.T) {
	for _, variation := range []string{"host-own", "host-foreign", "admin-foreign", "disabled-profile", "disabled-tenant", "nonhost-tenant", "unconfigured", "wrong-auth", "key-no-host", "corrupt-config"} {
		t.Run(variation, func(t *testing.T) {
			f, s, tw := outboundFixture(t)
			token := addTestKey(f, s, "host", "7", "host")
			want := 403
			if variation == "host-own" {
				want = 201
			}
			switch variation {
			case "host-foreign", "admin-foreign":
				f.legacy.cfg.Users["8"] = grant{Enabled: true, Permissions: []string{"host"}}
				provisionTwitch(t, f, 40, "8", "102")
				changeProfile(f, 40, func(p *twitchProfile) { p.Enabled = new(true) })
				if variation == "admin-foreign" {
					token = "key.secret"
					want = 201
				}
			case "disabled-profile":
				changeProfile(f, 30, func(p *twitchProfile) { p.Enabled = new(false) })
			case "disabled-tenant":
				f.legacy.cfg.Users["7"] = grant{Enabled: false, Permissions: []string{"host"}}
				token = "key.secret"
				want = 401
			case "nonhost-tenant":
				f.legacy.cfg.Users["7"] = grant{Enabled: true, Permissions: []string{"read"}}
				token = "key.secret"
			case "wrong-auth":
				changeProfile(f, 30, func(p *twitchProfile) { p.AuthConfigID = "999" })
				want = 500
			case "key-no-host":
				token = addTestKey(f, s, "limited", "7", "read")
			case "corrupt-config":
				f.legacy.corruptConfig = true
				want = 500
			}
			body := outboundBody
			if variation == "host-foreign" || variation == "admin-foreign" || variation == "unconfigured" {
				body = strings.Replace(body, `"channel":"101"`, `"channel":"102"`, 1)
			}
			w := outboundRequest(s, token, "key", body)
			status(t, w, want)
			expected := 0
			if want == 201 {
				expected = 1
			}
			if sends(tw) != expected {
				t.Fatalf("sends=%d", sends(tw))
			}
			if want != 201 && tw.validationCalls != 0 {
				t.Fatal("unauthorized request reached credentials")
			}
		})
	}
}

func TestCreateV3_ConcurrentAndRestartReplay_UsesOneAttempt(t *testing.T) {
	_, s, tw := outboundFixture(t)
	entered, release := make(chan struct{}, 1), make(chan struct{})
	var unblock sync.Once
	defer unblock.Do(func() { close(release) })
	tw.send = func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		respond(w, 200, map[string]any{"data": []any{map[string]any{"is_sent": true, "message_id": "sent-concurrent"}}})
	}
	first := make(chan *httptest.ResponseRecorder, 1)
	go func() { first <- outboundRequest(s, "key.secret", "same-key", outboundBody) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("send did not enter fake")
	}
	repeated := make(chan *httptest.ResponseRecorder, 1)
	go func() { repeated <- outboundRequest(s, "key.secret", "same-key", outboundBody) }()
	var pending messageResource
	select {
	case w := <-repeated:
		status(t, w, 200)
		pending = outboundResource(t, w, "unknown")
	case <-time.After(time.Second):
		t.Fatal("replay blocked on in-flight send")
	}
	if sends(tw) != 1 {
		t.Fatal("concurrent replay sent again")
	}
	unblock.Do(func() { close(release) })
	w := <-first
	status(t, w, 201)
	sent := outboundResource(t, w, "sent")
	if pending.ID != sent.ID {
		t.Fatal("key produced different nodes")
	}
	conflict := outboundRequest(s, "key.secret", "same-key", strings.Replace(outboundBody, "!Grüße ", "changed ", 1))
	status(t, conflict, 409)
	restarted := restartOutbound(s)
	w = outboundRequest(restarted, "key.secret", "same-key", outboundBody)
	status(t, w, 200)
	replay := outboundResource(t, w, "sent")
	if replay.ID != sent.ID || sends(tw) != 1 {
		t.Fatal("restart replay made another attempt")
	}
}

func restartOutbound(s *server) *server {
	return &server{graph: &graph{base: s.graph.base, token: s.graph.token, root: s.graph.root, group: s.graph.group, config: s.graph.config, http: s.graph.http}, twitch: s.twitch, pepper: s.pepper, now: s.now}
}

func TestCreateV3_IdempotencyScopeAndCorruption_SeparatesCallersAndRejectsDamagedKeys(t *testing.T) {
	for _, variation := range []string{"other-caller", "duplicate", "missing-membership", "wrong-state", "missing-content", "disabled-historical"} {
		t.Run(variation, func(t *testing.T) {
			f, s, tw := outboundFixture(t)
			w := outboundRequest(s, "key.secret", "same-key", outboundBody)
			status(t, w, 201)
			m := outboundResource(t, w, "sent")
			want := 500
			token := "key.secret"
			f.mu.Lock()
			n := f.nodes[mustResourceID(m.ID)]
			switch variation {
			case "other-caller":
				f.legacy.cfg.Users["8"] = grant{Enabled: true, Permissions: []string{"admin"}}
				token = addTestKey(f, s, "other", "8", "admin")
				want = 201
			case "duplicate":
				copy := n
				copy.ID = f.next
				f.next++
				f.nodes[copy.ID] = copy
				for _, e := range append([]relation(nil), f.edges...) {
					if e.Source == n.ID {
						e.Source = copy.ID
						f.edges = append(f.edges, e)
					}
				}
			case "missing-membership":
				edges := f.edges[:0]
				for _, e := range f.edges {
					if !(e.Source == n.ID && e.Target == 11) {
						edges = append(edges, e)
					}
				}
				f.edges = edges
			case "wrong-state":
				content := strings.Replace(*n.Content, `"state":"sent"`, `"state":"invalid"`, 1)
				n.Content = &content
				f.nodes[n.ID] = n
			case "missing-content":
				n.Content = nil
				f.nodes[n.ID] = n
			case "disabled-historical":
				changeProfile(f, 30, func(p *twitchProfile) { p.Enabled = new(false) })
				want = 200
			}
			f.mu.Unlock()
			w = outboundRequest(s, token, "same-key", outboundBody)
			status(t, w, want)
			count := 1
			if variation == "other-caller" {
				count = 2
				other := outboundResource(t, w, "sent")
				if other.ID == m.ID || other.Delivery.AuthUserID != "8" {
					t.Fatal("caller scope collapsed")
				}
			}
			if sends(tw) != count {
				t.Fatalf("attempts=%d", sends(tw))
			}
		})
	}
}

func TestV3_Schema3PatchDeleteAndInboundEcho_NeverSendsAgain(t *testing.T) {
	f, s, tw := outboundFixture(t)
	w := outboundRequest(s, "key.secret", "lifecycle", outboundBody)
	status(t, w, 201)
	m := outboundResource(t, w, "sent")
	patch := patchResource(t, s, m.ID, `[{"op":"replace","path":"/chunks","value":[{"type":0,"data":"changed"}]}]`)
	status(t, patch, 422)
	if !strings.Contains(patch.Body.String(), "immutable_delivery_message") {
		t.Fatal(patch.Body.String())
	}
	cfg, err := s.graph.loadConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	event := twitchEvent{BroadcasterUserID: "101", BroadcasterUserLogin: "room", ChatterUserID: "900", ChatterUserLogin: "bot", ChatterUserName: "Bot", MessageID: "sent-1", publishedAt: s.now()}
	event.Message.Text = "!Grüße 日本語 👋"
	inbound, err := s.ingestTwitch(context.Background(), cfg.twitch[0], event)
	if err != nil {
		t.Fatal(err)
	}
	observation := storedOutbound(t, f, strconv.FormatInt(inbound, 10))
	if observation.SchemaVersion != 2 || observation.Source == nil || observation.Delivery != nil || observation.User.ServiceID != "900" || inbound == mustResourceID(m.ID) {
		t.Fatal("echo provenance changed")
	}
	if _, err := s.ingestTwitch(context.Background(), cfg.twitch[0], event); err != nil {
		t.Fatal(err)
	}
	status(t, request(t, s, "DELETE", deliveryLocation(mustResourceID(m.ID)), "key.secret", ""), 204)
	tombstone := storedOutbound(t, f, m.ID)
	if !tombstone.Deleted || tombstone.User != nil || tombstone.Chunks != nil || !reflect.DeepEqual(tombstone.Delivery, m.Delivery) || tombstone.UserID != m.UserID || tombstone.ChannelID != m.ChannelID {
		t.Fatal("tombstone lost delivery bindings or retained text")
	}
	status(t, request(t, s, "GET", deliveryLocation(mustResourceID(m.ID)), "key.secret", ""), 404)
	list := request(t, s, "GET", "/api/v3/messages", "key.secret", "")
	status(t, list, 200)
	if strings.Contains(list.Body.String(), `"delivery"`) || !strings.Contains(list.Body.String(), `"source"`) {
		t.Fatal("deleted intent visible or inbound missing")
	}
	for _, body := range []string{outboundBody, strings.Replace(outboundBody, "!Grüße ", "changed ", 1)} {
		status(t, outboundRequest(restartOutbound(s), "key.secret", "lifecycle", body), 410)
	}
	if sends(tw) != 1 {
		t.Fatal("read/mutation/echo caused send")
	}
}

func TestCreateV3_SendOutcomes_PersistsExplicitStateWithoutRetry(t *testing.T) {
	cases := []struct {
		name              string
		httpStatus        int
		body, state, code string
		want              int
	}{
		{"refused", 200, `{"data":[{"is_sent":false,"message_id":"","drop_reason":{"message":"private upstream detail"}}]}`, "failed", "twitch_rejected", 502},
		{"bad-request", 400, `private upstream detail`, "failed", "twitch_bad_request", 502},
		{"unauthorized", 401, `private upstream detail`, "failed", "twitch_unauthorized", 502},
		{"forbidden", 403, `private upstream detail`, "failed", "twitch_forbidden", 502},
		{"unprocessable", 422, `private upstream detail`, "failed", "twitch_unprocessable", 502},
		{"rate-limit", 429, `private upstream detail`, "failed", "twitch_rate_limited", 429},
		{"server-error", 503, `private upstream detail`, "unknown", "", 503},
		{"malformed", 200, `broken`, "unknown", "", 503},
		{"missing-sent", 200, `{"data":[{"message_id":"sent-1"}]}`, "unknown", "", 503},
		{"invalid-id", 200, `{"data":[{"is_sent":true,"message_id":"bad/id"}]}`, "unknown", "", 503},
		{"empty-data", 200, `{"data":[]}`, "unknown", "", 503},
		{"multiple-data", 200, `{"data":[{"is_sent":true,"message_id":"one"},{"is_sent":true,"message_id":"two"}]}`, "unknown", "", 503},
		{"oversized", 200, strings.Repeat("x", (1<<20)+1), "unknown", "", 503},
		{"redirect", 307, ``, "unknown", "", 503},
		{"disconnect", 0, ``, "unknown", "", 503},
		{"timeout", 0, ``, "unknown", "", 503},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, s, tw := outboundFixture(t)
			if c.name == "timeout" {
				tw.client.http.Timeout = 50 * time.Millisecond
			}
			tw.send = func(w http.ResponseWriter, r *http.Request) {
				if c.name == "disconnect" {
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					conn.Close()
					return
				}
				if c.name == "timeout" {
					io.Copy(io.Discard, r.Body)
					<-r.Context().Done()
					return
				}
				w.Header().Set("Retry-After", "999999")
				if c.name == "redirect" {
					w.Header().Set("Location", "https://example.invalid/never-contact")
				}
				w.WriteHeader(c.httpStatus)
				io.WriteString(w, c.body)
			}
			w := outboundRequest(s, "key.secret", "outcome", outboundBody)
			status(t, w, c.want)
			var body map[string]string
			if json.Unmarshal(w.Body.Bytes(), &body) != nil || body["id"] == "" {
				t.Fatal(w.Body.String())
			}
			stored := storedOutbound(t, f, body["id"])
			if stored.Delivery == nil || stored.Delivery.State != c.state || stored.Delivery.Code != c.code || stored.Delivery.MessageID != "" || strings.Contains(w.Body.String(), "private upstream detail") {
				t.Fatalf("incorrect persisted failure: %+v %s", stored.Delivery, w.Body.String())
			}
			if c.name == "rate-limit" && w.Header().Get("Retry-After") != "300" {
				t.Fatal("unbounded retry-after")
			}
			replay := outboundRequest(restartOutbound(s), "key.secret", "outcome", outboundBody)
			status(t, replay, 200)
			outboundResource(t, replay, c.state)
			if sends(tw) != 1 {
				t.Fatalf("retried ambiguous/refused send: %d", sends(tw))
			}
		})
	}
}

func TestCreateV3_TerminalWriteFaults_ReplaysActualStoredState(t *testing.T) {
	for _, mode := range []string{"no-commit", "lost-ack", "readback-fail"} {
		t.Run(mode, func(t *testing.T) {
			f, s, tw := outboundFixture(t)
			var triggered, readFailure atomic.Bool
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if readFailure.Load() && r.Method == "GET" {
					w.WriteHeader(503)
					return
				}
				if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/content") {
					body, _ := io.ReadAll(r.Body)
					r.Body = io.NopCloser(strings.NewReader(string(body)))
					var m messageContent
					json.Unmarshal(body, &m)
					if m.Delivery != nil && m.Delivery.State == "sent" && triggered.CompareAndSwap(false, true) {
						if mode == "no-commit" {
							w.WriteHeader(503)
							return
						}
						recorder := httptest.NewRecorder()
						f.serve(recorder, r)
						if recorder.Code != 200 {
							t.Errorf("terminal fixture update=%d", recorder.Code)
						}
						if mode == "lost-ack" {
							w.WriteHeader(503)
							return
						}
						readFailure.Store(true)
						w.WriteHeader(recorder.Code)
						w.Write(recorder.Body.Bytes())
						return
					}
				}
				f.serve(w, r)
			}))
			defer backend.Close()
			s.graph.base, _ = url.Parse(backend.URL + "/api")
			w := outboundRequest(s, "key.secret", "terminal-fault", outboundBody)
			status(t, w, 503)
			if !triggered.Load() {
				t.Fatal("terminal fault did not execute")
			}
			var failure map[string]string
			json.Unmarshal(w.Body.Bytes(), &failure)
			stored := storedOutbound(t, f, failure["id"])
			state := "sent"
			if mode == "no-commit" {
				state = "unknown"
			}
			if stored.Delivery.State != state {
				t.Fatalf("durable state=%s", stored.Delivery.State)
			}
			readFailure.Store(false)
			replay := outboundRequest(restartOutbound(s), "key.secret", "terminal-fault", outboundBody)
			status(t, replay, 200)
			outboundResource(t, replay, state)
			if sends(tw) != 1 {
				t.Fatal("terminal write uncertainty resent")
			}
		})
	}
}

func TestCreateV3_ReservationWriteFault_NeverAttemptsSend(t *testing.T) {
	for _, point := range []string{"content:message", "committed:message", "created:message", "link:authored-by"} {
		t.Run(point, func(t *testing.T) {
			f, s, tw := outboundFixture(t)
			f.fail = point
			w := outboundRequest(s, "key.secret", "reservation-fault", outboundBody)
			status(t, w, 503)
			if !f.failed || sends(tw) != 0 {
				t.Fatal("reservation fault not reached or send occurred")
			}
			f.fail = ""
			replay := outboundRequest(restartOutbound(s), "key.secret", "reservation-fault", outboundBody)
			if point == "committed:message" {
				status(t, replay, 200)
				outboundResource(t, replay, "unknown")
			} else {
				status(t, replay, 500)
			}
			if sends(tw) != 0 {
				t.Fatal("uncertain reservation resent")
			}
		})
	}
}

func TestCreateV3_ConfigChangesDuringCredentialPreflight_ReauthorizesBeforeReservation(t *testing.T) {
	for _, mode := range []string{"disable-profile", "revoke-key", "change-bot", "corrupt-config"} {
		t.Run(mode, func(t *testing.T) {
			f, s, tw := outboundFixture(t)
			var changed atomic.Bool
			tw.validate = func(*http.Request) (int, any) {
				if changed.CompareAndSwap(false, true) {
					f.mu.Lock()
					switch mode {
					case "disable-profile":
						changeProfile(f, 30, func(p *twitchProfile) { p.Enabled = new(false) })
					case "change-bot":
						changeProfile(f, 30, func(p *twitchProfile) { p.BotUserID = "901" })
					case "revoke-key":
						f.legacy.mu.Lock()
						key := f.legacy.cfg.APIKeys["key"]
						key.Enabled = false
						f.legacy.cfg.APIKeys["key"] = key
						f.legacy.mu.Unlock()
					case "corrupt-config":
						f.legacy.mu.Lock()
						f.legacy.corruptConfig = true
						f.legacy.mu.Unlock()
					}
					f.mu.Unlock()
				}
				return 200, map[string]any{"client_id": "testclient", "user_id": "900", "scopes": []string{"user:read:chat", "user:write:chat"}, "expires_in": 3600}
			}
			w := outboundRequest(s, "key.secret", "config-race", outboundBody)
			want := 403
			if mode == "revoke-key" {
				want = 401
			}
			if mode == "corrupt-config" {
				want = 503
			}
			status(t, w, want)
			if !changed.Load() || sends(tw) != 0 || f.writes != 0 {
				t.Fatal("changed authorization was not checked before reservation")
			}
		})
	}
}

func TestCreateV3_DeleteDuringSend_PreservesTombstoneOnTerminalWrite(t *testing.T) {
	f, s, tw := outboundFixture(t)
	entered, release := make(chan struct{}, 1), make(chan struct{})
	var unblock sync.Once
	defer unblock.Do(func() { close(release) })
	tw.send = func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		respond(w, 200, map[string]any{"data": []any{map[string]any{"is_sent": true, "message_id": "deleted-inflight"}}})
	}
	result := make(chan *httptest.ResponseRecorder, 1)
	go func() { result <- outboundRequest(s, "key.secret", "delete-inflight", outboundBody) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("send not reached")
	}
	pending := outboundRequest(s, "key.secret", "delete-inflight", outboundBody)
	status(t, pending, 200)
	m := outboundResource(t, pending, "unknown")
	status(t, request(t, s, "DELETE", deliveryLocation(mustResourceID(m.ID)), "key.secret", ""), 204)
	unblock.Do(func() { close(release) })
	status(t, <-result, 201)
	stored := storedOutbound(t, f, m.ID)
	if !stored.Deleted || stored.Chunks != nil || stored.User != nil || stored.Delivery.State != "sent" || stored.Delivery.MessageID != "deleted-inflight" {
		t.Fatal("terminal write undid deletion")
	}
	status(t, outboundRequest(restartOutbound(s), "key.secret", "delete-inflight", outboundBody), 410)
	if sends(tw) != 1 {
		t.Fatal("delete caused another send")
	}
}

func TestCreateV3_UnicodeLimitAndWhitespaceChunks_PreservesConcatenation(t *testing.T) {
	for _, body := range []string{
		`{"service":"Twitch","channel":"101","user":{"service":"Twitch","serviceId":"GangolfBot","name":"GangolfBot"},"chunks":[{"type":0,"data":"` + strings.Repeat("界", 500) + `"}],"tts":false}`,
		`{"service":"Twitch","channel":"101","user":{"service":"Twitch","serviceId":"GangolfBot","name":"GangolfBot"},"chunks":[{"type":0,"data":" "},{"type":0,"data":"hello"},{"type":0,"data":""}],"tts":false}`,
	} {
		t.Run(strconv.Itoa(len(body)), func(t *testing.T) {
			_, s, tw := outboundFixture(t)
			w := outboundRequest(s, "key.secret", "text-boundary", body)
			status(t, w, 201)
			outboundResource(t, w, "sent")
			if sends(tw) != 1 {
				t.Fatal("valid text did not send")
			}
		})
	}
}
