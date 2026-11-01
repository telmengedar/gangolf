package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestLoadConfig_ConfigurationTopologyFailsClosedWithoutWrites(t *testing.T) {
	for _, change := range []string{"missing", "duplicate", "root", "owner", "access", "type", "project-edge", "group-edge", "edge-kind", "edge-context", "group-id", "embedding", "missing-embedding"} {
		t.Run(change, func(t *testing.T) {
			f, s := newResourceFixture(t)
			n := f.containers[15]
			switch change {
			case "missing":
				delete(f.containers, 15)
			case "duplicate":
				other := n
				other.ID = 16
				f.containers[16] = other
				f.pageSize = 1
			case "root":
				n.Root = 15
			case "owner":
				n.Owner++
			case "access":
				n.Access = json.RawMessage(`"Read"`)
			case "type":
				n.Type = "channel"
			case "project-edge", "group-edge", "edge-kind", "edge-context":
				f.corrupt = func(n *node) {
					if change == "project-edge" && n.ID == 10 || change != "project-edge" && n.ID == 15 {
						for i := range n.Links {
							if n.Links[i].Source == 10 && n.Links[i].Target == 15 {
								n.Links = append([]relation{}, n.Links...)
								switch change {
								case "edge-kind":
									n.Links[i].Type = "Unidirectional"
								case "edge-context":
									n.Links[i].Context = "membership"
								default:
									n.Links = append(n.Links[:i], n.Links[i+1:]...)
								}
								return
							}
						}
					}
				}
			case "embedding":
				n.SkipEmbedding = new(false)
			case "missing-embedding":
				n.SkipEmbedding = nil
			case "group-id":
				f.corrupt = func(n *node) {
					if n.Name == "Configuration" {
						n.ID = 10
					}
				}
			}
			if change != "missing" {
				f.containers[15] = n
			}
			cfg, err := s.graph.loadConfig(context.Background())
			if f.listedPages == 0 || err != errCorrupt || cfg != nil || f.writes != 0 {
				t.Fatalf("pages=%d error=%v loaded=%v writes=%d", f.listedPages, err, cfg != nil, f.writes)
			}
		})
	}
}

func TestLoadConfig_SelectedProfileRejectsMalformedMetadataEdgesAndContent(t *testing.T) {
	for _, change := range []string{"id", "root", "type", "owner", "access", "missing-access", "missing-links", "wrong-group", "extra-membership", "duplicate-membership", "edge-kind", "edge-context", "semantic-edge", "empty-name", "generic-name", "unsafe-name", "blank-profile", "wildcard", "leading-hyphen", "trailing-hyphen", "long-profile", "missing-content", "content-type", "content-type-prefix", "invalid-content", "unknown-field", "schema", "unsafe-jwks", "short-verifier", "embedding", "missing-embedding"} {
		t.Run(change, func(t *testing.T) {
			f, s := newResourceFixture(t)
			changed := 0
			f.corrupt = func(n *node) {
				if n.ID != 12 {
					return
				}
				changed++
				switch change {
				case "embedding":
					n.SkipEmbedding = new(false)
				case "missing-embedding":
					n.SkipEmbedding = nil
				case "id":
					n.ID++
				case "root":
					n.Root++
				case "type":
					n.Type = "config"
				case "owner":
					n.Owner++
				case "access":
					n.Access = json.RawMessage(`"Write"`)
				case "missing-access":
					n.Access = nil
				case "missing-links":
					n.Links = nil
				case "wrong-group":
					n.Links[0].Target = 11
				case "extra-membership":
					n.Links = append(n.Links, relation{Source: 12, Target: 11, Type: "None"})
				case "duplicate-membership":
					n.Links = append(n.Links, n.Links[0])
				case "edge-kind":
					n.Links[0].Type = "Bidirectional"
				case "edge-context":
					n.Links[0].Context = "membership"
				case "semantic-edge":
					n.Links = append(n.Links, relation{Source: 99, Target: 12, Type: "Unidirectional", Context: "uses-auth-config"})
				case "empty-name":
					n.Name = ""
				case "generic-name":
					n.Name = "config"
				case "unsafe-name":
					n.Name = "Gangolf API auth — main,other"
				case "blank-profile":
					n.Name = "Gangolf API auth — "
				case "wildcard":
					n.Name = "Gangolf API auth — *"
				case "leading-hyphen":
					n.Name = "Gangolf API auth — -main"
				case "trailing-hyphen":
					n.Name = "Gangolf API auth — main-"
				case "long-profile":
					n.Name = "Gangolf API auth — " + strings.Repeat("a", 129)
				case "missing-content":
					n.Content = nil
				case "content-type":
					n.ContentType = "text/plain"
				case "content-type-prefix":
					n.ContentType = "application/json-invalid"
				default:
					content := *n.Content
					switch change {
					case "invalid-content":
						content = `{`
					case "unknown-field":
						content = strings.TrimSuffix(content, "}") + `,"unexpected":true}`
					case "schema":
						content = strings.Replace(content, `"schemaVersion":1`, `"schemaVersion":2`, 1)
					case "unsafe-jwks":
						var cfg authConfig
						json.Unmarshal([]byte(content), &cfg)
						cfg.JWKSURL = "http://remote.example.test/jwks"
						data, _ := json.Marshal(cfg)
						content = string(data)
					case "short-verifier":
						var cfg authConfig
						json.Unmarshal([]byte(content), &cfg)
						cfg.APIKeys["invalid"] = keyGrant{UserID: "7", Hash: []byte{1}}
						data, _ := json.Marshal(cfg)
						content = string(data)
					}
					n.Content = &content
				}
			}
			cfg, err := s.graph.loadConfig(context.Background())
			if changed == 0 || err != errCorrupt || cfg != nil || f.writes != 0 {
				t.Fatalf("changed=%d error=%v loaded=%v writes=%d", changed, err, cfg != nil, f.writes)
			}
		})
	}
}

func TestLoadConfig_SelectedIDAndExactNameQueriesUseCanonicalScope(t *testing.T) {
	f, s := newResourceFixture(t)
	queries := 0
	observeIdentityWire(s, func(r *http.Request) {
		q := r.URL.Query()
		if q.Get("type") != "gangolf-auth-config" {
			return
		}
		queries++
		if q.Get("rootNodeId") != "10" || q.Get("count") != "500" || q.Get("continue") != "0" {
			t.Errorf("incorrect auth scope: %v", q)
		}
		switch queries {
		case 1:
			if q.Get("id") != "12" || q.Get("linkedto") != "15" || q.Get("name") != "" {
				t.Errorf("incorrect selected item query: %v", q)
			}
		case 2:
			if q.Get("id") != "" || q.Get("linkedto") != "15" || q.Get("name") != "Gangolf API auth — test-main" {
				t.Errorf("incorrect canonical name query: %v", q)
			}
		case 3:
			if q.Get("id") != "" || q.Get("linkedto") != "" || q.Get("name") != "Gangolf API auth — test-main" {
				t.Errorf("incorrect bounded integrity query: %v", q)
			}
		}
		for _, field := range []string{"id", "name", "type", "rootNodeId", "ownerId", "access", "content", "contentType", "linkDetails"} {
			if !strings.Contains(q.Get("fields"), field) {
				t.Errorf("missing field %s", field)
			}
		}
	}, nil)
	cfg, err := s.graph.loadConfig(context.Background())
	if queries != 3 || err != nil || cfg == nil || cfg.Issuer != "test-issuer" || cfg.serviceOwner != 20 || f.writes != 0 {
		t.Fatalf("queries=%d error=%v loaded=%v writes=%d", queries, err, cfg != nil, f.writes)
	}
}

func TestLoadConfig_MultipleProfilesSelectOnlyExplicitIDWithoutGrantBlending(t *testing.T) {
	f, s := newResourceFixture(t)
	private, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f.legacy.jwks, _ = json.Marshal(map[string]any{"keys": []map[string]string{{"kty": "RSA", "kid": "test", "n": base64.RawURLEncoding.EncodeToString(private.N.Bytes()), "e": "AQAB"}}})
	other := f.legacy.cfg
	other.Issuer = "other-issuer"
	other.Audience = "other-audience"
	other.Users = map[string]grant{"8": {Enabled: true, Permissions: []string{"host"}}}
	other.APIKeys = map[string]keyGrant{}
	payload, _ := json.Marshal(other)
	content := string(payload)
	f.nodes[16] = node{ID: 16, Name: "Gangolf API auth — other-main", Type: "gangolf-auth-config", Root: 10, Owner: 20, Access: json.RawMessage("0"), SkipEmbedding: new(true), Content: &content, ContentType: "application/json; charset=utf-8"}
	f.edges = append(f.edges, relation{Source: 15, Target: 16, Type: "None"})
	for _, selected := range []int64{12, 16, 12} {
		s.graph.config = selected
		cfg, err := s.graph.loadConfig(context.Background())
		if err != nil || cfg == nil {
			t.Fatalf("selected %d: %v", selected, err)
		}
		wantIssuer := "test-issuer"
		wantKeyStatus := 200
		if selected == 16 {
			wantIssuer = "other-issuer"
			wantKeyStatus = 401
		}
		if cfg.Issuer != wantIssuer || len(cfg.Users) != 1 || selected == 16 && cfg.Users["7"].Enabled || selected == 12 && cfg.Users["8"].Enabled {
			t.Fatal("selected profile blended grants")
		}
		status(t, request(t, s, "GET", "/api/v2/messages", "key.secret", ""), wantKeyStatus)
		status(t, request(t, s, "GET", "/api/v2/messages?apiKey=key.secret", "", ""), wantKeyStatus)
		for _, issuer := range []string{"test-issuer", "other-issuer"} {
			for _, user := range []string{"7", "8"} {
				claims := jwt.MapClaims{"iss": issuer, "aud": cfg.Audience, "userId": user, "exp": s.now().Add(time.Hour).Unix()}
				token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
				token.Header["kid"] = "test"
				encoded, err := token.SignedString(private)
				if err != nil {
					t.Fatal(err)
				}
				want := 200
				if issuer != wantIssuer {
					want = 401
				} else if !cfg.Users[user].Enabled {
					want = 403
				}
				status(t, request(t, s, "GET", "/api/v2/messages", encoded, ""), want)
			}
		}
	}
	s.graph.config = 99
	cfg, err := s.graph.loadConfig(context.Background())
	if err != errCorrupt || cfg != nil || f.writes != 0 {
		t.Fatalf("missing selected profile fell back: error=%v loaded=%v writes=%d", err, cfg != nil, f.writes)
	}
}

func TestLoadConfig_DuplicateProfileNameOnLaterPageIsRejected(t *testing.T) {
	f, s := newResourceFixture(t)
	f.pageSize = 1
	f.nodes[16] = node{ID: 16, Name: "Gangolf API auth — test-main", Type: "gangolf-auth-config", Root: 10, Owner: 20, Access: json.RawMessage("0"), SkipEmbedding: new(true)}
	f.edges = append(f.edges, relation{Source: 16, Target: 17, Type: "None"})
	secondPage := 0
	observeIdentityWire(s, func(r *http.Request) {
		q := r.URL.Query()
		if q.Get("type") == "gangolf-auth-config" && q.Get("name") != "" && q.Get("continue") == "1" && len(q["linkedto"]) == 0 {
			secondPage++
		}
	}, nil)
	cfg, err := s.graph.loadConfig(context.Background())
	if secondPage != 1 || err != errCorrupt || cfg != nil || f.writes != 0 {
		t.Fatalf("second pages=%d error=%v loaded=%v writes=%d", secondPage, err, cfg != nil, f.writes)
	}
}

func TestLoadConfig_AuthPaginationRejectsInvalidEnvelopes(t *testing.T) {
	for _, phase := range []string{"selected", "named", "integrity", "group"} {
		for _, change := range []string{"missing-total", "negative-total", "empty-result", "missing-result", "too-many", "missing-cursor", "stalled-cursor", "unexpected-cursor", "changed-total", "duplicate-id"} {
			t.Run(phase+"/"+change, func(t *testing.T) {
				f, s := newResourceFixture(t)
				changed := 0
				observeIdentityWire(s, nil, func(r *http.Request, response *http.Response) {
					q := r.URL.Query()
					if phase == "group" {
						if q.Get("name") != "Configuration" {
							return
						}
					} else if q.Get("type") != "gangolf-auth-config" || (q.Get("id") != "") != (phase == "selected") || (len(q["linkedto"]) == 0) != (phase == "integrity") {
						return
					}
					changed++
					rewriteIdentityPage(t, response, func(page map[string]json.RawMessage) {
						switch change {
						case "missing-total":
							delete(page, "total")
						case "negative-total":
							page["total"] = json.RawMessage("-1")
						case "empty-result":
							page["result"] = json.RawMessage("[]")
						case "missing-result":
							delete(page, "result")
						case "too-many":
							page["total"] = json.RawMessage("0")
						case "missing-cursor":
							page["total"] = json.RawMessage("2")
						case "stalled-cursor":
							page["total"] = json.RawMessage("2")
							page["continue"] = json.RawMessage("0")
						case "unexpected-cursor":
							page["continue"] = json.RawMessage("1")
						case "changed-total", "duplicate-id":
							page["total"] = json.RawMessage("2")
							page["continue"] = json.RawMessage("1")
							if changed == 2 {
								if change == "changed-total" {
									page["total"] = json.RawMessage("3")
								} else {
									payload, _ := json.Marshal(f.legacy.cfg)
									content := string(payload)
									page["result"], _ = json.Marshal([]node{{ID: 12, Name: "Gangolf API auth — test-main", Type: "gangolf-auth-config", Root: 10, Owner: 20, Access: json.RawMessage("0"), SkipEmbedding: new(true), Content: &content, ContentType: "application/json", Links: []relation{{Source: 12, Target: 15, Type: "None"}}}})
									if phase == "group" {
										page["result"], _ = json.Marshal([]node{f.projection(f.containers[15])})
									}
								}
							}
						}
					})
				})
				cfg, err := s.graph.loadConfig(context.Background())
				if changed == 0 || err != errCorrupt || cfg != nil || f.writes != 0 {
					t.Fatalf("changed=%d error=%v loaded=%v writes=%d", changed, err, cfg != nil, f.writes)
				}
			})
		}
	}
}

func TestConfigurationFailureMakesReadinessAndBothMessageAPIsFailClosed(t *testing.T) {
	f, s := newResourceFixture(t)
	delete(f.containers, 15)
	for _, tc := range []struct {
		path string
		code int
	}{{"/api/i1/readiness", 503}, {"/api/v2/messages", 500}, {"/api/v3/messages", 500}} {
		status(t, request(t, s, "GET", tc.path, "key.secret", ""), tc.code)
	}
	if f.listedPages == 0 || f.writes != 0 {
		t.Fatalf("pages=%d writes=%d", f.listedPages, f.writes)
	}
}

func TestLoadConfig_ConflictingSelectedAndNamedReadbacksAreRejected(t *testing.T) {
	f, s := newResourceFixture(t)
	changed := 0
	observeIdentityWire(s, nil, func(r *http.Request, response *http.Response) {
		if r.URL.Query().Get("type") != "gangolf-auth-config" || r.URL.Query().Get("name") == "" {
			return
		}
		rewriteIdentityPage(t, response, func(page map[string]json.RawMessage) {
			var rows []node
			if err := json.Unmarshal(page["result"], &rows); err != nil || len(rows) != 1 || rows[0].Content == nil {
				t.Fatal("missing selected profile readback")
			}
			content := strings.Replace(*rows[0].Content, "test-issuer", "other-issuer", 1)
			rows[0].Content = &content
			page["result"], _ = json.Marshal(rows)
			changed++
		})
	})
	cfg, err := s.graph.loadConfig(context.Background())
	if changed != 1 || err != errCorrupt || cfg != nil || f.writes != 0 {
		t.Fatalf("changed=%d error=%v loaded=%v writes=%d", changed, err, cfg != nil, f.writes)
	}
}

func TestLoadConfig_SelectedProfileCannotAliasDomainGroup(t *testing.T) {
	for _, id := range []int64{11, 13, 14, 15} {
		f, s := newResourceFixture(t)
		s.graph.config = id
		cfg, err := s.graph.loadConfig(context.Background())
		if f.listedPages != 5 || err != errCorrupt || cfg != nil || f.writes != 0 {
			t.Fatalf("selected=%d pages=%d error=%v loaded=%v writes=%d", id, f.listedPages, err, cfg != nil, f.writes)
		}
	}
}
