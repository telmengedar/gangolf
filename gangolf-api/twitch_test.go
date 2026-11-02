package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func provisionTwitch(t *testing.T, f *resourceFixture, id int64, tenant, broadcaster string) twitchProfile {
	t.Helper()
	channelID := id + 1
	p := twitchProfile{twitchTuple: twitchTuple{AuthConfigID: "12", TenantUserID: tenant, ChannelID: strconv.FormatInt(channelID, 10), BroadcasterUserID: broadcaster}, SchemaVersion: 1, BotUserID: "900", ClientID: "testclient", CredentialRef: "test-grant", Enabled: new(false)}
	data, _ := json.Marshal(p)
	content := string(data)
	f.nodes[id] = node{ID: id, Name: "Twitch inbound — test-main — user-" + tenant + " — room-" + broadcaster, Type: twitchConfigType, Root: 10, Owner: 20, Access: json.RawMessage("0"), SkipEmbedding: new(true), Content: &content, ContentType: "application/json"}
	identity, _ := json.Marshal(identityContent{SchemaVersion: 1, Service: "Twitch", Channel: broadcaster})
	identityContent := string(identity)
	f.nodes[channelID] = node{ID: channelID, Name: identityName("channel", "Twitch", broadcaster), Type: "channel", Root: 10, Owner: 20, Access: json.RawMessage("0"), Content: &identityContent, ContentType: "application/json"}
	f.edges = append(f.edges, relation{Source: id, Target: 15, Type: "None"}, relation{Source: channelID, Target: 14, Type: "None"}, relation{Source: id, Target: channelID, Type: "Unidirectional", Context: "configured-for"}, relation{Source: id, Target: 12, Type: "Unidirectional", Context: "uses-auth-config"})
	return p
}

func changeProfile(f *resourceFixture, id int64, change func(*twitchProfile)) {
	n := f.nodes[id]
	var p twitchProfile
	json.Unmarshal([]byte(*n.Content), &p)
	change(&p)
	data, _ := json.Marshal(p)
	content := string(data)
	n.Content = &content
	f.nodes[id] = n
}

func addTestKey(f *resourceFixture, s *server, name, user string, permissions ...string) string {
	token := name + ".secret"
	h := hmac.New(sha256.New, []byte(s.pepper))
	h.Write([]byte(token))
	f.legacy.cfg.APIKeys[name] = keyGrant{UserID: user, Hash: h.Sum(nil), Enabled: true, Permissions: permissions}
	return token
}

func TestTwitchConfigProfilesAndSemanticEdges(t *testing.T) {
	for _, change := range []string{"valid", "wrong-root", "wrong-owner", "public", "wrong-type", "content-type", "schema", "missing-enabled", "tenant", "channel", "broadcaster", "bot", "client", "credential-path", "credential-url", "credential-secret", "name", "name-tenant", "embedding", "membership", "direction", "relation-type", "wrong-channel-target", "wrong-auth-target", "duplicate-relation", "extra-edge", "incoming-channel-forgery", "incoming-auth-forgery", "missing-configured-for"} {
		t.Run(change, func(t *testing.T) {
			f, s := newResourceFixture(t)
			provisionTwitch(t, f, 30, "7", "101")
			n := f.nodes[30]
			switch change {
			case "wrong-root":
				n.Root = 99
			case "wrong-owner":
				n.Owner = 99
			case "public":
				n.Access = json.RawMessage("1")
			case "wrong-type":
				n.Type = "user"
			case "content-type":
				n.ContentType = "application/json-invalid"
			case "schema":
				changeProfile(f, 30, func(p *twitchProfile) { p.SchemaVersion = 2 })
			case "missing-enabled":
				changeProfile(f, 30, func(p *twitchProfile) { p.Enabled = nil })
			case "tenant":
				changeProfile(f, 30, func(p *twitchProfile) { p.TenantUserID = "007" })
			case "channel":
				changeProfile(f, 30, func(p *twitchProfile) { p.ChannelID = "32" })
			case "broadcaster":
				changeProfile(f, 30, func(p *twitchProfile) { p.BroadcasterUserID = "102" })
			case "bot":
				changeProfile(f, 30, func(p *twitchProfile) { p.BotUserID = "bot" })
			case "client":
				changeProfile(f, 30, func(p *twitchProfile) { p.ClientID = "client secret" })
			case "credential-path":
				changeProfile(f, 30, func(p *twitchProfile) { p.CredentialRef = "../grant" })
			case "credential-url":
				changeProfile(f, 30, func(p *twitchProfile) { p.CredentialRef = "https://secret" })
			case "credential-secret":
				changeProfile(f, 30, func(p *twitchProfile) { p.CredentialRef = "oauth:secret" })
			case "name":
				n.Name = "Twitch inbound — test,* — user-7 — room"
			case "name-tenant":
				n.Name = "Twitch inbound — test-main — user-8 — room"
			case "embedding":
				n.SkipEmbedding = new(false)
			case "membership":
				f.edges[0].Context = "member-of"
			case "direction":
				f.edges[2].Source, f.edges[2].Target = f.edges[2].Target, f.edges[2].Source
			case "relation-type":
				f.edges[2].Type = "Bidirectional"
			case "wrong-channel-target":
				f.edges[2].Target = 14
			case "wrong-auth-target":
				f.edges[3].Target = 15
			case "duplicate-relation":
				f.edges = append(f.edges, f.edges[2])
			case "extra-edge":
				f.edges = append(f.edges, relation{Source: 30, Target: 10, Type: "None"})
			case "incoming-channel-forgery":
				f.edges = append(f.edges, relation{Source: 99, Target: 31, Type: "Unidirectional", Context: "configured-for"})
			case "incoming-auth-forgery":
				f.edges = append(f.edges, relation{Source: 99, Target: 12, Type: "Unidirectional", Context: "uses-auth-config"})
			case "missing-configured-for":
				f.edges = append(f.edges[:2], f.edges[3:]...)
			}
			switch change {
			case "schema", "missing-enabled", "tenant", "channel", "broadcaster", "bot", "client", "credential-path", "credential-url", "credential-secret":
			default:
				f.nodes[30] = n
			}
			cfg, err := s.graph.loadConfig(context.Background())
			if change == "valid" {
				if err != nil || len(cfg.twitch) != 1 || *cfg.twitch[0].Enabled {
					t.Fatalf("config=%+v err=%v", cfg, err)
				}
			} else if err != errCorrupt || cfg != nil {
				t.Fatalf("malformed %s accepted: %+v %v", change, cfg, err)
			}
			if f.writes != 0 || f.legacy.writes != 0 {
				t.Fatal("configuration load wrote graph")
			}
		})
	}
}

func TestTwitchProfileDuplicatesAndImmutableTuple(t *testing.T) {
	for _, change := range []string{"inactive-alternatives", "active-conflict", "tenant-conflict", "auth-conflict", "channel-conflict", "duplicate-name", "immutable-tenant", "immutable-channel", "immutable-broadcaster", "immutable-auth", "deleted", "credential-change"} {
		t.Run(change, func(t *testing.T) {
			f, s := newResourceFixture(t)
			provisionTwitch(t, f, 30, "7", "101")
			cfg, err := s.graph.loadConfig(context.Background())
			if err != nil || len(cfg.twitch) != 1 {
				t.Fatalf("initial config: %v", err)
			}
			switch change {
			case "immutable-tenant":
				changeProfile(f, 30, func(p *twitchProfile) { p.TenantUserID = "8" })
				n := f.nodes[30]
				n.Name = strings.Replace(n.Name, "user-7", "user-8", 1)
				f.nodes[30] = n
			case "immutable-channel":
				n := f.nodes[31]
				n.ID = 32
				f.nodes[32] = n
				f.edges = append(f.edges, relation{Source: 32, Target: 14, Type: "None"})
				f.edges[2].Target = 32
				changeProfile(f, 30, func(p *twitchProfile) { p.ChannelID = "32" })
			case "immutable-broadcaster":
				n := f.nodes[31]
				n.Name = identityName("channel", "Twitch", "102")
				content := strings.Replace(*n.Content, "101", "102", 1)
				n.Content = &content
				f.nodes[31] = n
				changeProfile(f, 30, func(p *twitchProfile) { p.BroadcasterUserID = "102" })
			case "immutable-auth":
				data, _ := json.Marshal(f.legacy.cfg)
				content := string(data)
				f.nodes[19] = node{ID: 19, Name: "Gangolf API auth — alternate", Type: authConfigType, Root: 10, Owner: 20, Access: json.RawMessage("0"), SkipEmbedding: new(true), Content: &content, ContentType: "application/json"}
				f.edges = append(f.edges, relation{Source: 19, Target: 15, Type: "None"})
				f.edges[3].Target = 19
				changeProfile(f, 30, func(p *twitchProfile) { p.AuthConfigID = "19" })
			case "deleted":
				delete(f.nodes, 30)
				f.edges = f.edges[1:2]
			case "credential-change":
				changeProfile(f, 30, func(p *twitchProfile) { p.CredentialRef = "rotated-grant"; p.BotUserID = "901" })
			default:
				n := f.nodes[30]
				n.ID = 40
				n.Name += "-alternate"
				f.nodes[40] = n
				f.edges = append(f.edges, relation{Source: 40, Target: 15, Type: "None"}, relation{Source: 40, Target: 31, Type: "Unidirectional", Context: "configured-for"}, relation{Source: 40, Target: 12, Type: "Unidirectional", Context: "uses-auth-config"})
				switch change {
				case "active-conflict":
					for _, id := range []int64{30, 40} {
						changeProfile(f, id, func(p *twitchProfile) { p.Enabled = new(true) })
					}
				case "tenant-conflict":
					changeProfile(f, 40, func(p *twitchProfile) { p.TenantUserID = "8" })
					n = f.nodes[40]
					n.Name = strings.Replace(n.Name, "user-7", "user-8", 1)
					f.nodes[40] = n
				case "auth-conflict":
					changeProfile(f, 40, func(p *twitchProfile) { p.AuthConfigID = "19" })
				case "channel-conflict":
					changeProfile(f, 40, func(p *twitchProfile) { p.ChannelID = "32" })
				case "duplicate-name":
					n = f.nodes[40]
					n.Name = f.nodes[30].Name
					f.nodes[40] = n
				}
			}
			cfg, err = s.graph.loadConfig(context.Background())
			if change == "inactive-alternatives" || change == "credential-change" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err != errCorrupt || cfg != nil {
				t.Fatalf("change %s accepted: %v", change, err)
			}
			if f.writes != 0 {
				t.Fatal("load wrote graph")
			}
		})
	}
}

func TestTwitchChannelsHeaderAuthRedactionAndEffectiveGrants(t *testing.T) {
	f, s := newResourceFixture(t)
	provisionTwitch(t, f, 30, "7", "101")
	provisionTwitch(t, f, 40, "8", "102")
	f.legacy.cfg.Users["8"] = grant{Enabled: true, Permissions: []string{"host"}}
	host := addTestKey(f, s, "host", "7", "host")
	for _, tc := range []struct {
		token, path string
		want, count int
	}{{host, "/api/i1/twitch/channels", 200, 1}, {"key.secret", "/api/i1/twitch/channels", 200, 2}, {"", "/api/i1/twitch/channels?apiKey=" + host, 401, 0}, {"wrong", "/api/i1/twitch/channels", 401, 0}, {"", "/api/i1/twitch/channels", 401, 0}} {
		w := request(t, s, "GET", tc.path, tc.token, "")
		status(t, w, tc.want)
		if tc.want != 200 {
			continue
		}
		var result struct{ Result []map[string]any }
		json.Unmarshal(w.Body.Bytes(), &result)
		if len(result.Result) != tc.count {
			t.Fatalf("profiles=%s", w.Body.String())
		}
		for _, profile := range result.Result {
			if profile["state"] != "disabled" || profile["enabled"] != false || len(profile) != 6 {
				t.Fatalf("profile=%v", profile)
			}
		}
		for _, secret := range []string{"credentialRef", "test-grant", "clientId", "botUserId", "hash", "secret", "token"} {
			if strings.Contains(w.Body.String(), secret) {
				t.Fatalf("leaked %s: %s", secret, w.Body.String())
			}
		}
	}
	status(t, request(t, s, "POST", "/api/i1/twitch/channels", host, ""), 405)
	f.legacy.cfg.Users["7"] = grant{Enabled: false, Permissions: []string{"admin"}}
	status(t, request(t, s, "GET", "/api/i1/twitch/channels", host, ""), 401)
}

func TestTwitchPrincipalJWTAndKeyGrants(t *testing.T) {
	f, s := newResourceFixture(t)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pub := key.PublicKey
	set := map[string]any{"keys": []map[string]any{{"kty": "RSA", "kid": "test", "n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes())}}}
	f.legacy.jwks, _ = json.Marshal(set)
	f.legacy.cfg.JWKSURL = f.legacy.host.URL + "/jwks"
	host := addTestKey(f, s, "host", "7", "host")
	for _, tc := range []struct {
		name        string
		claim       any
		permissions []string
		want        int
	}{{"string", "7", []string{"host"}, 0}, {"number", 7, []string{"admin"}, 0}, {"missing", 8, []string{"admin"}, 403}, {"no-grants", 7, nil, 403}} {
		t.Run(tc.name, func(t *testing.T) {
			f.legacy.cfg.Users["7"] = grant{Enabled: true, Permissions: tc.permissions}
			claims := jwt.MapClaims{"iss": f.legacy.cfg.Issuer, "aud": f.legacy.cfg.Audience, "exp": s.now().Add(time.Hour).Unix(), "userId": tc.claim}
			token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
			token.Header["kid"] = "test"
			encoded, _ := token.SignedString(key)
			p, status := s.authorize(context.Background(), nil, &f.legacy.cfg, encoded, false)
			if status != tc.want || status == 0 && (p.UserID != "7" || p.admin() != (tc.permissions[0] == "admin")) {
				t.Fatalf("principal=%+v status=%d", p, status)
			}
		})
	}
	provisionTwitch(t, f, 30, "7", "101")
	provisionTwitch(t, f, 40, "8", "102")
	for _, permissions := range [][]string{{"host"}, {"admin"}} {
		f.legacy.cfg.Users["7"] = grant{Enabled: true, Permissions: permissions}
		claims := jwt.MapClaims{"iss": f.legacy.cfg.Issuer, "aud": f.legacy.cfg.Audience, "exp": s.now().Add(time.Hour).Unix(), "userId": 7}
		token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
		token.Header["kid"] = "test"
		encoded, _ := token.SignedString(key)
		w := request(t, s, "GET", "/api/i1/twitch/channels", encoded, "")
		status(t, w, 200)
		var page struct{ Result []json.RawMessage }
		json.Unmarshal(w.Body.Bytes(), &page)
		want := 1
		if permissions[0] == "admin" {
			want = 2
		}
		if len(page.Result) != want {
			t.Fatal(w.Body.String())
		}
	}
	f.legacy.cfg.Users["7"] = grant{Enabled: true, Permissions: []string{"admin"}}
	p, status := s.authorize(context.Background(), nil, &f.legacy.cfg, host, false)
	if status != 0 || p.admin() || p.UserID != "7" {
		t.Fatalf("key inherited admin: %+v status=%d", p, status)
	}
}

func TestTwitchTenantIsolationV2AndV3BeforeCountAndPage(t *testing.T) {
	for _, version := range []string{"v2", "v3"} {
		t.Run(version, func(t *testing.T) {
			f, s := newResourceFixture(t)
			provisionTwitch(t, f, 30, "7", "101")
			provisionTwitch(t, f, 40, "8", "102")
			f.legacy.cfg.Users["8"] = grant{Enabled: true, Permissions: []string{"host"}}
			host := addTestKey(f, s, "host", "7", "host")
			other := addTestKey(f, s, "other", "8", "host")
			path := "/api/" + version + "/messages"
			for _, pair := range []struct{ service, channel string }{{"Twitch", "102"}, {"Twitch", "101"}, {"twitch", "unknown-login"}, {"TWITCH", "102"}, {"Twitch", "101"}, {"localhost", "host"}} {
				if version == "v2" {
					body := strings.Replace(testMessage, `"service":"localhost","channel":"host"`, `"service":"`+pair.service+`","channel":"`+pair.channel+`"`, 1)
					status(t, request(t, s, "POST", path, "key.secret", "["+body+"]"), 201)
				} else {
					body := strings.ReplaceAll(v3Message, "twitch", pair.service)
					body = strings.Replace(body, "room-42", pair.channel, 1)
					status(t, request(t, s, "POST", path, "key.secret", body), 201)
				}
			}
			for _, tc := range []struct {
				token string
				total int
			}{{host, 3}, {other, 3}, {"key.secret", 6}} {
				w := request(t, s, "GET", path+"?count=1&continue=1&descending=true", tc.token, "")
				status(t, w, 200)
				var page struct {
					Result   []json.RawMessage
					Total    int
					Continue *int
				}
				if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
					t.Fatal(err)
				}
				expected := tc.total
				if version == "v3" && tc.token == other {
					expected = 2
				}
				if page.Total != expected || len(page.Result) != 1 || (page.Continue != nil) != (expected > 2) || page.Continue != nil && *page.Continue != 2 {
					t.Fatalf("token=%s page=%s", tc.token, w.Body.String())
				}
				if tc.token == host && !strings.Contains(string(page.Result[0]), `"channel":"101"`) {
					t.Fatalf("wrong page member: %s", w.Body.String())
				}
			}
			w := request(t, s, "GET", path+"?channel=unknown-login", host, "")
			status(t, w, 200)
			if !strings.Contains(w.Body.String(), `"total":0`) {
				t.Fatal(w.Body.String())
			}
		})
	}
}

func TestTwitchMixedLegacyBatchFiltersMembersBeforeTotal(t *testing.T) {
	f, s := newResourceFixture(t)
	provisionTwitch(t, f, 30, "7", "101")
	provisionTwitch(t, f, 40, "8", "102")
	host := addTestKey(f, s, "host", "7", "host")
	a := strings.Replace(testMessage, `"service":"localhost","channel":"host"`, `"service":"twitch","channel":"101"`, 1)
	b := strings.Replace(a, `"channel":"101"`, `"channel":"102"`, 1)
	status(t, request(t, s, "POST", "/api/v2/messages", "key.secret", "["+b+","+a+","+b+"]"), 201)
	w := request(t, s, "GET", "/api/v2/messages?count=1", host, "")
	status(t, w, 200)
	if !strings.Contains(w.Body.String(), `"total":1`) || strings.Contains(w.Body.String(), `"channel":"102"`) || !strings.Contains(w.Body.String(), `"continue":null`) {
		t.Fatal(w.Body.String())
	}
}

func TestTwitchItemMutationReplyAndTombstoneIsolation(t *testing.T) {
	f, s := newResourceFixture(t)
	provisionTwitch(t, f, 30, "7", "101")
	provisionTwitch(t, f, 40, "8", "102")
	host := addTestKey(f, s, "host", "7", "host")
	body := strings.ReplaceAll(strings.Replace(v3Message, "room-42", "102", 1), "twitch", "Twitch")
	foreign := createResource(t, s, body)
	own := createResource(t, s, strings.Replace(body, "102", "101", 1))
	before := f.writes
	for _, method := range []string{"GET", "PATCH", "DELETE"} {
		payload := ""
		if method == "PATCH" {
			payload = `[{"op":"replace","path":"/tts","value":true}]`
		}
		status(t, twitchRequest(t, s, method, "/api/v3/messages/"+foreign.ID, host, payload), 404)
	}
	reply := strings.TrimSuffix(strings.ReplaceAll(v3Message, "twitch", "localhost"), "}") + `,"replyTo":"` + foreign.ID + `"}`
	status(t, request(t, s, "POST", "/api/v3/messages", host, reply), 404)
	if f.writes != before {
		t.Fatalf("cross-tenant operations wrote: %d -> %d", before, f.writes)
	}
	status(t, request(t, s, "GET", "/api/v3/messages/"+own.ID, host, ""), 200)
	status(t, twitchRequest(t, s, "PATCH", "/api/v3/messages/"+own.ID, host, `[{"op":"replace","path":"/tts","value":true}]`), 200)
	status(t, request(t, s, "DELETE", "/api/v3/messages/"+foreign.ID, "key.secret", ""), 204)
	before = f.writes
	status(t, request(t, s, "DELETE", "/api/v3/messages/"+foreign.ID, host, ""), 404)
	if f.writes != before {
		t.Fatal("foreign tombstone wrote")
	}
	status(t, request(t, s, "DELETE", "/api/v3/messages/"+own.ID, host, ""), 204)
	status(t, request(t, s, "DELETE", "/api/v3/messages/"+own.ID, host, ""), 204)
}

func TestTwitchManualAppendCannotBeSpoofedAndMixedV2IsAtomic(t *testing.T) {
	for _, service := range []string{"Twitch", "twitch", "TWITCH"} {
		for _, version := range []string{"v2", "v3"} {
			t.Run(version+service, func(t *testing.T) {
				f, s := newResourceFixture(t)
				host := addTestKey(f, s, "host", "7", "host")
				body := strings.Replace(testMessage, `"service":"localhost"`, `"service":"`+service+`"`, 1)
				if version == "v2" {
					body = "[" + testMessage + "," + body + "]"
				} else {
					body = strings.ReplaceAll(v3Message, "twitch", service)
				}
				status(t, request(t, s, "POST", "/api/"+version+"/messages", host, body), 403)
				if f.writes != 0 || f.legacy.writes != 0 || len(f.nodes) != 0 {
					t.Fatal("rejected append performed writes")
				}
			})
		}
	}
}

func twitchRequest(t *testing.T, s *server, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json-patch+json")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	return w
}

func TestTwitchUnconfiguredCaseVariantsAreAdminOnlyForItems(t *testing.T) {
	for _, service := range []string{"Twitch", "twitch", "TWITCH"} {
		t.Run(service, func(t *testing.T) {
			f, s := newResourceFixture(t)
			host := addTestKey(f, s, "host", "7", "host")
			body := strings.ReplaceAll(strings.Replace(v3Message, "room-42", "101", 1), "twitch", service)
			m := createResource(t, s, body)
			if service != "Twitch" {
				provisionTwitch(t, f, 30, "7", "101")
			}
			before := f.writes
			for _, method := range []string{"GET", "PATCH", "DELETE"} {
				status(t, twitchRequest(t, s, method, "/api/v3/messages/"+m.ID, host, `[{"op":"replace","path":"/tts","value":true}]`), 404)
			}
			if f.writes != before {
				t.Fatal("unknown channel mutated")
			}
			w := request(t, s, "GET", "/api/v3/messages?channel=101", host, "")
			status(t, w, 200)
			if !strings.Contains(w.Body.String(), `"total":0`) {
				t.Fatal(w.Body.String())
			}
			status(t, request(t, s, "GET", "/api/v3/messages/"+m.ID, "key.secret", ""), 200)
		})
	}
}

func TestTwitchInactiveAuthProfileDoesNotBlendTenantGrants(t *testing.T) {
	f, s := newResourceFixture(t)
	provisionTwitch(t, f, 30, "7", "101")
	provisionTwitch(t, f, 40, "7", "102")
	alternate := f.legacy.cfg
	alternate.Users = map[string]grant{"7": {Enabled: true, Permissions: []string{"admin"}}}
	data, _ := json.Marshal(alternate)
	content := string(data)
	f.nodes[19] = node{ID: 19, Name: "Gangolf API auth — alternate", Type: authConfigType, Root: 10, Owner: 20, Access: json.RawMessage("0"), SkipEmbedding: new(true), Content: &content, ContentType: "application/json"}
	f.edges = append(f.edges, relation{Source: 19, Target: 15, Type: "None"})
	f.edges[7].Target = 19
	changeProfile(f, 40, func(p *twitchProfile) { p.AuthConfigID = "19" })
	host := addTestKey(f, s, "host", "7", "host")
	w := request(t, s, "GET", "/api/i1/twitch/channels", host, "")
	status(t, w, 200)
	if strings.Contains(w.Body.String(), `"broadcasterUserId":"102"`) {
		t.Fatal(w.Body.String())
	}
	body := strings.Replace(testMessage, `"service":"localhost","channel":"host"`, `"service":"Twitch","channel":"102"`, 1)
	status(t, request(t, s, "POST", "/api/v2/messages", "key.secret", "["+body+"]"), 201)
	w = request(t, s, "GET", "/api/v2/messages", host, "")
	status(t, w, 200)
	if !strings.Contains(w.Body.String(), `"total":0`) {
		t.Fatal(w.Body.String())
	}
}

func TestTwitchProfileEndpointMetadataIsValidated(t *testing.T) {
	for _, target := range []int64{31, 12} {
		for _, change := range []string{"owner", "access", "root", "type", "group", "content", "identity", "missing-reciprocal-edge"} {
			t.Run(strconv.FormatInt(target, 10)+"/"+change, func(t *testing.T) {
				f, s := newResourceFixture(t)
				provisionTwitch(t, f, 30, "7", "101")
				changed := 0
				f.corrupt = func(n *node) {
					if n.ID != target {
						return
					}
					changed++
					switch change {
					case "owner":
						n.Owner = 99
					case "access":
						n.Access = json.RawMessage("1")
					case "root":
						n.Root = 99
					case "type":
						n.Type = "user"
					case "group":
						for i := range n.Links {
							if n.Links[i].Type == "None" {
								n.Links[i].Target = 11
							}
						}
					case "content":
						n.Content = nil
					case "identity":
						n.Name = "forged"
					case "missing-reciprocal-edge":
						for i, e := range n.Links {
							if e.Source == 30 {
								n.Links = append(n.Links[:i], n.Links[i+1:]...)
								break
							}
						}
					}
				}
				cfg, err := s.graph.loadConfig(context.Background())
				if changed == 0 || err != errCorrupt || cfg != nil || f.writes != 0 {
					t.Fatalf("changed=%d error=%v config=%v writes=%d", changed, err, cfg, f.writes)
				}
			})
		}
	}
}
