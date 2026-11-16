package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"github.com/golang-jwt/jwt/v5"
	"math/big"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestTokenExplicitNullNever(t *testing.T) {
	for _, scope := range []string{"api", "channel-read"} {
		t.Run(scope, func(t *testing.T) {
			f, s := newResourceFixture(t)
			input := map[string]any{"scope": scope, "expiresAt": nil}
			if scope == "channel-read" {
				input["channelIds"] = []string{createAppTestChannel(t, s, "key.secret", "owned")}
			}
			raw, _ := json.Marshal(input)
			row := appRow(t, appRequest(t, s, "POST", "/api/v3/users/current/tokens", "key.secret", string(raw)), 201)
			expiry, present := row["expiresAt"]
			if !present || expiry != nil {
				t.Fatalf("explicit null became finite/absent: %v", expiry)
			}
			if f.legacy.cfg.APIKeys[row["id"].(string)].ExpiresAt != nil {
				t.Fatal("never expiry was not persisted nil")
			}
		})
	}
}

func TestTokenExpiryFiniteDefaultsAndCaps(t *testing.T) {
	for _, scope := range []string{"api", "channel-read"} {
		for _, mode := range []string{"default", "clamped", "finite", "offset", "past", "now", "zero", "scope-limit", "issuer-limit"} {
			t.Run(scope+"/"+mode, func(t *testing.T) {
				f, s := newResourceFixture(t)
				input := map[string]any{"scope": scope}
				if scope == "channel-read" {
					input["channelIds"] = []string{createAppTestChannel(t, s, "key.secret", "owned")}
				}
				now := s.now()
				maximum, def := 365*24*time.Hour, 30*24*time.Hour
				if scope == "channel-read" {
					maximum, def = 30*24*time.Hour, 24*time.Hour
				}
				expected := now.Add(def)
				if mode == "clamped" || mode == "issuer-limit" {
					expiry := now.Add(2 * time.Hour)
					key := f.legacy.cfg.APIKeys["key"]
					key.ExpiresAt = &expiry
					f.legacy.cfg.APIKeys["key"] = key
					expected = expiry
				}
				want := 201
				switch mode {
				case "finite":
					expected = now.Add(time.Hour)
					input["expiresAt"] = expected.Format(time.RFC3339Nano)
				case "offset":
					expected = now.Add(time.Hour)
					input["expiresAt"] = expected.In(time.FixedZone("offset", 2*3600)).Format(time.RFC3339Nano)
				case "past":
					input["expiresAt"] = now.Add(-time.Second).Format(time.RFC3339Nano)
					want = 400
				case "now":
					input["expiresAt"] = now.Format(time.RFC3339Nano)
					want = 400
				case "zero":
					input["expiresAt"] = "0001-01-01T00:00:00Z"
					want = 400
				case "scope-limit":
					input["expiresAt"] = now.Add(maximum + time.Second).Format(time.RFC3339Nano)
					want = 400
				case "issuer-limit":
					input["expiresAt"] = now.Add(3 * time.Hour).Format(time.RFC3339Nano)
					want = 400
				}
				before := f.writes
				raw, _ := json.Marshal(input)
				w := appRequest(t, s, "POST", "/api/v3/users/current/tokens", "key.secret", string(raw))
				row := appRow(t, w, want)
				if want == 400 {
					if row["code"] != "invalid_token" || f.writes != before {
						t.Fatal("finite policy failure changed semantics or wrote state")
					}
					return
				}
				expiry, err := time.Parse(time.RFC3339Nano, row["expiresAt"].(string))
				stored := f.legacy.cfg.APIKeys[row["id"].(string)].ExpiresAt
				if err != nil || !expiry.Equal(expected) || stored == nil || !stored.Equal(expected) || stored.Location() != time.UTC {
					t.Fatal("finite default/cap/UTC changed")
				}
			})
		}
	}
}

func TestTokenExpiryStrictInputAndAliasCompatibility(t *testing.T) {
	f, s := newResourceFixture(t)
	valid := s.now().Add(time.Hour).Format(time.RFC3339Nano)
	for _, body := range []string{
		`{"expiresAt":""}`, `{"expiresAt":" "}`, `{"expiresAt":"never"}`, `{"expiresAt":"2026-12-16"}`, `{"expiresAt":0}`, `{"expiresAt":true}`, `{"expiresAt":{}}`, `{"expiresAt":[]}`,
		`{"expiresAt":null,"expiresAt":null}`, `{"expiresAt":null,"expiresAt":"` + valid + `"}`, `{"expiresAt":null,"never":true}`, `{"expiresAt":null} {}`,
		`{"scope":"channel-read","channelIds":[],"expiresAt":false}`, `{"permissions":["unknown"],"expiresAt":"bad"}`,
	} {
		before := f.writes
		row := appRow(t, appRequest(t, s, "POST", "/api/v3/users/current/tokens", "key.secret", body), 400)
		if row["code"] != "invalid_body" || row["token"] != nil || f.writes != before {
			t.Fatalf("strict parse/order changed for %s", body)
		}
	}
	before := f.writes
	status(t, appRequest(t, s, "POST", "/api/v3/users/current/tokens", "key.secret", `{"name":"`+strings.Repeat("x", 1<<20)+`","expiresAt":null}`), 413)
	if f.writes != before {
		t.Fatal("oversized expiry request wrote")
	}
	for _, body := range []string{`{"ExpiresAt":null}`, `{"EXPIRESAT":null}`, `{"expiresAt":"` + valid + `","ExpiresAt":null}`, "{\"expiresAt\": \n null }"} {
		row := appRow(t, appRequest(t, s, "POST", "/api/v3/users/current/tokens", "key.secret", body), 201)
		if row["expiresAt"] != nil {
			t.Fatal("accepted expiry alias lost explicit null presence")
		}
	}
	row := appRow(t, appRequest(t, s, "POST", "/api/v3/users/current/tokens", "key.secret", `{"expiresAt":null,"ExpiresAt":"`+valid+`"}`), 201)
	if row["expiresAt"] != valid {
		t.Fatal("existing last case-alias value semantics changed")
	}
}

func TestTokenNeverPersistenceMetadataAndPreservation(t *testing.T) {
	f, s := newResourceFixture(t)
	expiry := s.now().Add(time.Hour)
	key := f.legacy.cfg.APIKeys["key"]
	key.ExpiresAt = &expiry
	f.legacy.cfg.APIKeys["key"] = key
	addTestKey(f, s, "old-never", "7", "host")
	original, _ := json.Marshal(f.legacy.cfg)
	row := appRow(t, appRequest(t, s, "POST", "/api/v3/users/current/tokens", "key.secret", `{"name":"Permanent","expiresAt":null}`), 201)
	id, token := row["id"].(string), row["token"].(string)
	afterRaw, _ := json.Marshal(f.legacy.cfg)
	var after authConfig
	json.Unmarshal(afterRaw, &after)
	if len(after.APIKeys) != 3 || after.APIKeys[id].ExpiresAt != nil {
		t.Fatal("new never key missing")
	}
	delete(after.APIKeys, id)
	var before authConfig
	json.Unmarshal(original, &before)
	if !sameJSON(before, after) {
		t.Fatal("existing key hashes/expiry/grants/JWT settings changed")
	}
	var stored map[string]any
	json.Unmarshal(afterRaw, &stored)
	newKey := stored["apiKeys"].(map[string]any)[id].(map[string]any)
	value, present := newKey["expiresAt"]
	if !present || value != nil {
		t.Fatal("stored never marker not explicit null")
	}
	fresh := *s
	fresh.graph = &graph{base: s.graph.base, http: s.graph.http, token: s.graph.token, root: s.graph.root, group: s.graph.group, config: s.graph.config}
	status(t, appRequest(t, &fresh, "GET", "/api/v3/users/current", token, ""), 200)
	for _, path := range []string{"/api/v3/tokens/" + id, "/api/v3/users/current/tokens"} {
		w := appRequest(t, &fresh, "GET", path, token, "")
		status(t, w, 200)
		if !strings.Contains(w.Body.String(), `"expiresAt":null`) || strings.Contains(w.Body.String(), token) || strings.Contains(w.Body.String(), `"hash"`) || strings.Contains(w.Body.String(), `"pepper"`) {
			t.Fatal("never metadata missing or secret disclosed")
		}
	}
}

func TestTokenNeverSurvivesIssuerExpiryAndDeletion(t *testing.T) {
	for _, mode := range []string{"expired", "deleted"} {
		t.Run(mode, func(t *testing.T) {
			_, f, s := managedMediaFixture(t)
			base := time.Date(2026, 12, 15, 12, 0, 0, 0, time.UTC)
			var clock atomic.Int64
			clock.Store(base.UnixNano())
			s.now = func() time.Time { return time.Unix(0, clock.Load()).UTC() }
			deadline := time.Date(2026, 12, 16, 0, 0, 0, 0, time.UTC)
			issuer := f.legacy.cfg.APIKeys["key"]
			issuer.ExpiresAt = &deadline
			f.legacy.cfg.APIKeys["key"] = issuer
			alice := createAppTestUser(t, s, "Alice")
			finite := issueAppTestToken(t, s, alice, `{}`)
			channel := createAppTestChannel(t, s, finite, "owned")
			admin := issueAppTestToken(t, s, "7", `{"permissions":["admin","host"],"expiresAt":null}`)
			read := issueAppTestToken(t, s, alice, `{"scope":"channel-read","channelIds":["`+channel+`"],"expiresAt":null}`)
			if mode == "expired" {
				clock.Store(deadline.Add(24 * time.Hour).UnixNano())
				status(t, appRequest(t, s, "GET", "/api/v3/users/current", finite, ""), 401)
			} else {
				status(t, appRequest(t, s, "DELETE", "/api/v3/tokens/key", admin, ""), 204)
			}
			status(t, appRequest(t, s, "GET", "/api/v3/users/current", "key.secret", ""), 401)
			status(t, appRequest(t, s, "POST", "/api/v3/users/current/tokens", "key.secret", `{"expiresAt":null}`), 401)
			status(t, appRequest(t, s, "GET", "/api/v3/users/current", admin, ""), 200)
			status(t, appRequest(t, s, "GET", "/api/v3/messages?apiKey="+url.QueryEscape(read), "", ""), 200)
			status(t, appRequest(t, s, "GET", "/overlays/chat?apiKey="+url.QueryEscape(read), "", ""), 200)
			stream := openEventTestStream(t, eventTestHost(t, s), read, "types=message.created,video.triggered")
			w := appRequest(t, s, "POST", "/api/v3/messages", admin, eventLocalMessage("owned", "permanent source"))
			status(t, w, 201)
			var message messageResource
			json.Unmarshal(w.Body.Bytes(), &message)
			e, _ := eventTestEnvelope(t, nextEventTestFrame(t, stream, time.Second))
			if e.Type != "message.created" {
				t.Fatal("never message event missing")
			}
			status(t, postVideoTest(s, admin, `{"messageId":"`+message.ID+`","triggerId":"1"}`), 200)
			e, _ = eventTestEnvelope(t, nextEventTestFrame(t, stream, time.Second))
			if e.Type != "video.triggered" {
				t.Fatal("never video event missing")
			}
			for _, path := range []string{"/api/v3/users/current", "/api/v3/clips", "/api/v2/messages"} {
				status(t, appRequest(t, s, "GET", path, read, ""), 403)
			}
			status(t, appRequest(t, s, "POST", "/api/v3/users/current/tokens", read, `{"expiresAt":null}`), 403)
			status(t, appRequest(t, s, "POST", "/api/v3/messages", read, eventLocalMessage("owned", "denied")), 403)
			status(t, appRequest(t, s, "GET", "/overlays/chat?apiKey="+url.QueryEscape(admin), "", ""), 401)
		})
	}
}

func TestTokenNeverFreshRevocation(t *testing.T) {
	for _, mode := range []string{"key", "user-disabled", "user-deleted", "grant", "channel", "legacy-profile"} {
		t.Run(mode, func(t *testing.T) {
			f, s := newResourceFixture(t)
			alice := createAppTestUser(t, s, "Alice")
			host := issueAppTestToken(t, s, alice, `{}`)
			channel := createAppTestChannel(t, s, host, "owned")
			service, name := "Local", "owned"
			if mode == "legacy-profile" {
				provisionTwitch(t, f, 30, alice, "101")
				changeProfile(f, 30, func(p *twitchProfile) { p.Enabled = new(true) })
				channel = "31"
				service, name = "Twitch", "101"
			}
			read := issueAppTestToken(t, s, alice, `{"scope":"channel-read","channelIds":["`+channel+`"],"expiresAt":null}`)
			readID, _, _ := strings.Cut(read, ".")
			stream := openEventTestStream(t, eventTestHost(t, s), read, "types=message.created")
			expected := 401
			switch mode {
			case "key":
				status(t, appRequest(t, s, "DELETE", "/api/v3/tokens/"+readID, "key.secret", ""), 204)
			case "user-disabled":
				status(t, appRequest(t, s, "PUT", "/api/v3/users/"+alice, "key.secret", `{"name":"Alice","enabled":false,"permissions":["host"]}`), 200)
			case "user-deleted":
				status(t, appRequest(t, s, "DELETE", "/api/v3/users/"+alice, "key.secret", ""), 204)
			case "grant":
				f.legacy.mu.Lock()
				user := f.legacy.cfg.Users[alice]
				user.Permissions = nil
				f.legacy.cfg.Users[alice] = user
				f.legacy.mu.Unlock()
				expected = 403
			case "channel":
				status(t, appRequest(t, s, "PUT", "/api/v3/channels/"+channel, "key.secret", `{"service":"Local","channel":"owned","enabled":false}`), 200)
				expected = 403
			case "legacy-profile":
				f.mu.Lock()
				changeProfile(f, 30, func(p *twitchProfile) { p.Enabled = new(false) })
				f.mu.Unlock()
				expected = 403
			}
			s.publishEvent("message.created", service, name, &channel, "must-not-leak")
			select {
			case frame, ok := <-stream.frames:
				if ok {
					t.Fatalf("never key bypassed fresh revocation: %q", frame)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("revoked never stream stayed open")
			}
			status(t, appRequest(t, s, "GET", "/api/v3/events?types=message.created", read, ""), expected)
			status(t, appRequest(t, s, "GET", "/overlays/chat?apiKey="+url.QueryEscape(read), "", ""), expected)
		})
	}
}

func TestTokenNeverJWTIssuerAndAuthority(t *testing.T) {
	f, s := newResourceFixture(t)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pub := key.PublicKey
	f.legacy.jwks, _ = json.Marshal(map[string]any{"keys": []map[string]any{{"kty": "RSA", "kid": "test", "n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes())}}})
	original := string(f.legacy.jwks)
	jwtExpiry := s.now().Add(time.Hour)
	claims := jwt.MapClaims{"iss": f.legacy.cfg.Issuer, "aud": f.legacy.cfg.Audience, "exp": jwtExpiry.Unix(), "userId": "7"}
	signed := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	signed.Header["kid"] = "test"
	encoded, err := signed.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	row := appRow(t, appRequest(t, s, "POST", "/api/v3/users/current/tokens", encoded, `{"expiresAt":null}`), 201)
	never := row["token"].(string)
	s.now = func() time.Time { return jwtExpiry.Add(time.Hour) }
	status(t, appRequest(t, s, "GET", "/api/v3/users/current", encoded, ""), 401)
	status(t, appRequest(t, s, "GET", "/api/v3/users/current", never, ""), 200)
	status(t, appRequest(t, s, "GET", "/api/v3/users", never, ""), 403)
	if row["expiresAt"] != nil || string(f.legacy.jwks) != original {
		t.Fatal("JWT policy/storage changed")
	}
	bob := createAppTestUser(t, s, "Bob")
	status(t, appRequest(t, s, "POST", "/api/v3/users/"+bob+"/tokens", never, `{"expiresAt":null}`), 403)
	status(t, appRequest(t, s, "POST", "/api/v3/users/"+bob+"/tokens", "key.secret", `{"permissions":["admin"],"expiresAt":null}`), 403)
	cfg, err := s.graph.loadConfig(context.Background())
	if err != nil || cfg.APIKeys[row["id"].(string)].ExpiresAt != nil {
		t.Fatal("never JWT-issued key failed readback")
	}
}

func TestTokenNeverScopeAndSelfAuthority(t *testing.T) {
	_, s := newResourceFixture(t)
	alice := createAppTestUser(t, s, "Alice")
	host := issueAppTestToken(t, s, alice, `{}`)
	own := createAppTestChannel(t, s, host, "owned")
	bob := createAppTestUser(t, s, "Bob")
	b := issueAppTestToken(t, s, bob, `{}`)
	other := createAppTestChannel(t, s, b, "other")
	row := appRow(t, appRequest(t, s, "POST", "/api/v3/users/current/tokens", host, `{"expiresAt":null}`), 201)
	if row["expiresAt"] != nil {
		t.Fatal("ordinary self never key was capped")
	}
	status(t, appRequest(t, s, "GET", "/api/v3/users/current", row["token"].(string), ""), 200)
	for _, input := range []struct {
		body string
		want int
	}{
		{`{"scope":"channel-read","expiresAt":null}`, 400},
		{`{"scope":"channel-read","channelIds":[],"expiresAt":null}`, 400},
		{`{"scope":"channel-read","channelIds":["` + own + `","` + own + `"],"expiresAt":null}`, 400},
		{`{"scope":"channel-read","channelIds":["` + other + `"],"expiresAt":null}`, 403},
		{`{"scope":"api","channelIds":["` + own + `"],"expiresAt":null}`, 400},
		{`{"scope":"invalid","expiresAt":null}`, 400},
		{`{"scope":"channel-read","channelIds":["` + own + `"],"permissions":["admin"],"expiresAt":null}`, 400},
		{`{"permissions":["admin"],"expiresAt":null}`, 403},
	} {
		status(t, appRequest(t, s, "POST", "/api/v3/users/current/tokens", host, input.body), input.want)
	}
	status(t, appRequest(t, s, "POST", "/api/v3/users/"+bob+"/tokens", host, `{"expiresAt":null}`), 403)
}

func TestTokenNeverIdleHeartbeatRevocation(t *testing.T) {
	_, s := newResourceFixture(t)
	alice := createAppTestUser(t, s, "Alice")
	host := issueAppTestToken(t, s, alice, `{}`)
	channel := createAppTestChannel(t, s, host, "owned")
	read := issueAppTestToken(t, s, alice, `{"scope":"channel-read","channelIds":["`+channel+`"],"expiresAt":null}`)
	stream := openEventTestStream(t, eventTestHost(t, s), read, "types=message.created")
	id, _, _ := strings.Cut(read, ".")
	status(t, appRequest(t, s, "DELETE", "/api/v3/tokens/"+id, "key.secret", ""), 204)
	select {
	case frame, ok := <-stream.frames:
		if ok {
			t.Fatalf("idle revoked never key emitted %q", frame)
		}
	case <-time.After(eventHeartbeat + 2*time.Second):
		t.Fatal("never key ignored heartbeat revocation")
	}
}
