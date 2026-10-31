package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func identityQueryFixture(t *testing.T, kind string) (*resourceFixture, *server, identityContent, int64) {
	t.Helper()
	f, s := newResourceFixture(t)
	if err := s.graph.privateDomainGroups(context.Background(), 20); err != nil {
		t.Fatal(err)
	}
	v := identityContent{SchemaVersion: 1, Service: "twitch", ServiceID: "speaker-7", Name: "Distinct Speaker"}
	group := int64(13)
	if kind == "channel" {
		v = identityContent{SchemaVersion: 1, Service: "twitch", Channel: "room-42"}
		group = 14
	}
	return f, s, v, group
}

func seedQueryIdentity(f *resourceFixture, kind string, v identityContent, linked, published bool) node {
	key := v.ServiceID
	if kind == "channel" {
		key = v.Channel
	}
	n := node{ID: 90, Name: identityName(kind, v.Service, key), Type: kind, Root: 10, Owner: 20, Access: json.RawMessage(`"None"`)}
	if published {
		raw, _ := json.Marshal(v)
		content := string(raw)
		n.Content = &content
		n.ContentType = "application/json"
	}
	f.nodes[n.ID] = n
	if linked {
		group := int64(13)
		if kind == "channel" {
			group = 14
		}
		f.edges = append(f.edges, relation{Source: n.ID, Target: group, Type: "None"})
	}
	return n
}

func observeIdentityWire(s *server, inspect func(*http.Request), edit func(*http.Request, *http.Response)) {
	next := s.graph.http.Transport
	if next == nil {
		next = http.DefaultTransport
	}
	s.graph.http.Transport = groupTypeTransport(func(r *http.Request) (*http.Response, error) {
		if inspect != nil {
			inspect(r)
		}
		response, err := next.RoundTrip(r)
		if err == nil && edit != nil {
			edit(r, response)
		}
		return response, err
	})
}

func rewriteIdentityPage(t *testing.T, response *http.Response, edit func(map[string]json.RawMessage)) {
	t.Helper()
	raw, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	var page map[string]json.RawMessage
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatal(err)
	}
	edit(page)
	raw, err = json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	response.Body = io.NopCloser(bytes.NewReader(raw))
	response.ContentLength = int64(len(raw))
}

func namedIdentityRequest(r *http.Request, kind string) bool {
	return r.Method == "GET" && r.URL.Path == "/api/nodes" && r.URL.Query().Get("type") == kind && r.URL.Query().Get("name") != ""
}

func TestIdentityQueries_CanonicalThenIntegrity(t *testing.T) {
	for _, kind := range []string{"user", "channel"} {
		for _, state := range []string{"create", "hit", "sterile", "linked-empty"} {
			t.Run(kind+"/"+state, func(t *testing.T) {
				f, s, v, group := identityQueryFixture(t, kind)
				if state != "create" {
					seedQueryIdentity(f, kind, v, state != "sterile", state == "hit")
				}
				key := v.ServiceID
				if kind == "channel" {
					key = v.Channel
				}
				name := identityName(kind, v.Service, key)
				var events []string
				observeIdentityWire(s, func(r *http.Request) {
					if namedIdentityRequest(r, kind) {
						q := r.URL.Query()
						tag := "C"
						expected := url.Values{"rootNodeId": {"10"}, "linkedto": {strconv.FormatInt(group, 10)}, "type": {kind}, "name": {name}, "fields": {"id,name,type,rootNodeId,ownerId,access,content,contentType,linkDetails"}, "sort": {"id"}, "descending": {"false"}, "count": {"500"}, "continue": {"0"}}
						if _, present := q["linkedto"]; !present {
							tag = "I"
							delete(expected, "linkedto")
						}
						if !reflect.DeepEqual(q, expected) {
							t.Errorf("%s request=%s want=%s", tag, q.Encode(), expected.Encode())
						}
						events = append(events, tag)
					} else if r.Method == "POST" {
						events = append(events, r.URL.Path)
					} else if r.URL.Query().Get("id") != "" {
						events = append(events, "item")
					}
				}, nil)
				id, err := s.graph.resolveIdentity(context.Background(), kind, v, 20)
				if err != nil {
					t.Fatal(err)
				}
				want := []string{"C", "I", "item"}
				if state == "create" {
					want = []string{"C", "I", "/api/nodes", fmt.Sprintf("/api/nodes/%d/links", id), "C", "item", fmt.Sprintf("/api/nodes/%d/content", id)}
				}
				if state == "sterile" {
					want = []string{"C", "I", fmt.Sprintf("/api/nodes/%d/links", id), "C", "item", fmt.Sprintf("/api/nodes/%d/content", id)}
				}
				if state == "linked-empty" {
					want = append(want, fmt.Sprintf("/api/nodes/%d/content", id))
				}
				if !reflect.DeepEqual(events, want) {
					t.Fatalf("events=%v want=%v", events, want)
				}
				if state != "create" && id != 90 {
					t.Fatalf("changed ID=%d", id)
				}
				before := f.writes
				again, err := s.graph.resolveIdentity(context.Background(), kind, v, 20)
				if err != nil || again != id || f.writes != before {
					t.Fatalf("reuse id=%d err=%v writes=%d", again, err, f.writes-before)
				}
			})
		}
	}
}

func TestIdentityQueries_ReadsCanonicalOnly(t *testing.T) {
	f, s := newResourceFixture(t)
	m := createResource(t, s, v3Message)
	before := f.writes
	seen := 0
	observeIdentityWire(s, func(r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/api/nodes" && r.URL.Query().Get("type") != "" {
			q := r.URL.Query()
			group := map[string]string{"user": "13", "channel": "14", "message": "11"}[q.Get("type")]
			if group != "" {
				seen++
				if len(q["linkedto"]) != 1 || q.Get("linkedto") != group || q.Get("rootNodeId") != "10" {
					t.Errorf("read escaped C: %s", q.Encode())
				}
			}
		}
	}, nil)
	status(t, request(t, s, "GET", "/api/v3/messages/"+m.ID, "key.secret", ""), 200)
	status(t, request(t, s, "GET", "/api/v3/messages", "key.secret", ""), 200)
	if seen == 0 || f.writes != before {
		t.Fatalf("seen=%d writes=%d", seen, f.writes-before)
	}
}

func TestIdentityQueries_MixedDuplicate(t *testing.T) {
	for _, kind := range []string{"user", "channel"} {
		for _, shape := range []string{"mixed-orphan", "mixed-wrong", "two-canonical", "two-hidden"} {
			t.Run(kind+"/"+shape, func(t *testing.T) {
				f, s, v, group := identityQueryFixture(t, kind)
				n := seedQueryIdentity(f, kind, v, shape != "two-hidden", true)
				n.ID = 91
				f.nodes[91] = n
				if shape == "mixed-wrong" {
					f.edges = append(f.edges, relation{Source: 91, Target: 31, Type: "None"})
				}
				if shape == "two-canonical" {
					f.edges = append(f.edges, relation{Source: 91, Target: group, Type: "None"})
				}
				f.pageSize = 1
				before := f.writes
				if _, err := s.graph.resolveIdentity(context.Background(), kind, v, 20); err == nil || f.writes != before || len(f.nodes) != 2 {
					t.Fatalf("duplicate accepted err=%v writes=%d nodes=%d", err, f.writes-before, len(f.nodes))
				}
				last := f.queries[len(f.queries)-1]
				if last.Get("continue") != "1" {
					t.Fatalf("did not consume later duplicate page: %s", last.Encode())
				}
			})
		}
	}
}

func TestIdentityQueries_CorruptionNoWrites(t *testing.T) {
	for _, kind := range []string{"user", "channel"} {
		for _, shape := range []string{"published-orphan", "published-wrong", "empty-wrong", "empty-foreign", "empty-semantic", "malformed-content"} {
			t.Run(kind+"/"+shape, func(t *testing.T) {
				f, s, v, group := identityQueryFixture(t, kind)
				n := seedQueryIdentity(f, kind, v, shape == "malformed-content", strings.HasPrefix(shape, "published"))
				switch shape {
				case "published-wrong", "empty-wrong":
					f.edges = append(f.edges, relation{Source: 90, Target: 11, Type: "None"})
				case "empty-foreign":
					f.edges = append(f.edges, relation{Source: 90, Target: 31, Type: "None"})
				case "empty-semantic":
					f.edges = append(f.edges, relation{Source: 90, Target: group, Type: "Unidirectional", Context: "authored-by"})
				case "malformed-content":
					bad := "{}"
					n.Content = &bad
					n.ContentType = "application/json"
					f.nodes[90] = n
				}
				before := f.writes
				if _, err := s.graph.resolveIdentity(context.Background(), kind, v, 20); err == nil || f.writes != before || len(f.nodes) != 1 {
					t.Fatalf("corruption accepted err=%v writes=%d", err, f.writes-before)
				}
			})
		}
	}
}

func TestIdentityQueries_PartialRetrySameID(t *testing.T) {
	for _, kind := range []string{"user", "channel"} {
		for _, point := range []string{"created:", "link:", "ignored:", "linked:", "content:"} {
			t.Run(kind+"/"+point, func(t *testing.T) {
				f, s, v, group := identityQueryFixture(t, kind)
				f.fail = point
				if point == "created:" || point == "content:" {
					f.fail += kind
				}
				if _, err := s.graph.resolveIdentity(context.Background(), kind, v, 20); err == nil || !f.failed || len(f.nodes) != 1 {
					t.Fatalf("failure not reached: err=%v failed=%v nodes=%d", err, f.failed, len(f.nodes))
				}
				original := f.nodes[100]
				if original.Content != nil {
					t.Fatal("published before confirmed link")
				}
				f.fail = ""
				f.queries = nil
				var events []string
				observeIdentityWire(s, func(r *http.Request) {
					if namedIdentityRequest(r, kind) {
						if r.URL.Query().Get("linkedto") == "" {
							events = append(events, "I")
						} else {
							events = append(events, "C")
						}
					}
					if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/content") {
						events = append(events, "publish")
					}
				}, nil)
				id, err := s.graph.resolveIdentity(context.Background(), kind, v, 20)
				if err != nil || id != 100 || len(f.nodes) != 1 || !exactMembership(f.projection(f.nodes[id]), group) {
					t.Fatalf("retry id=%d err=%v nodes=%d", id, err, len(f.nodes))
				}
				want := []string{"C", "I", "C", "publish"}
				if point == "linked:" || point == "content:" {
					want = []string{"C", "I", "publish"}
				}
				if !reflect.DeepEqual(events, want) {
					t.Fatalf("publication order=%v want=%v", events, want)
				}
				before := f.writes
				again, err := s.graph.resolveIdentity(context.Background(), kind, v, 20)
				if err != nil || again != id || f.writes != before {
					t.Fatalf("repeat retry=%d err=%v writes=%d", again, err, f.writes-before)
				}
			})
		}
	}
}

func TestIdentityQueries_ResponseMetadata(t *testing.T) {
	for _, kind := range []string{"user", "channel"} {
		for _, phase := range []string{"C", "I", "item"} {
			for _, field := range []string{"id", "root", "type", "name", "owner", "access", "edges", "content", "content-type"} {
				t.Run(kind+"/"+phase+"/"+field, func(t *testing.T) {
					f, s, v, _ := identityQueryFixture(t, kind)
					seedQueryIdentity(f, kind, v, true, true)
					exercised := false
					observeIdentityWire(s, nil, func(r *http.Request, response *http.Response) {
						q := r.URL.Query()
						match := namedIdentityRequest(r, kind) && (phase == "C" && q.Get("linkedto") != "" || phase == "I" && q.Get("linkedto") == "")
						if phase == "item" {
							match = q.Get("id") == "90"
						}
						if !match {
							return
						}
						exercised = true
						rewriteIdentityPage(t, response, func(page map[string]json.RawMessage) {
							var rows []map[string]json.RawMessage
							if err := json.Unmarshal(page["result"], &rows); err != nil || len(rows) != 1 {
								t.Fatalf("rows=%v err=%v", rows, err)
							}
							switch field {
							case "id":
								rows[0]["id"] = json.RawMessage("0")
							case "root":
								rows[0]["rootNodeId"] = json.RawMessage("30")
							case "type":
								rows[0]["type"] = json.RawMessage(`"message"`)
							case "name":
								rows[0]["name"] = json.RawMessage(`"different-key"`)
							case "owner":
								rows[0]["ownerId"] = json.RawMessage("21")
							case "access":
								rows[0]["access"] = json.RawMessage(`"Read"`)
							case "edges":
								delete(rows[0], "linkDetails")
							case "content":
								rows[0]["content"] = json.RawMessage(`"{}"`)
							case "content-type":
								rows[0]["contentType"] = json.RawMessage(`"text/plain"`)
							}
							page["result"], _ = json.Marshal(rows)
						})
					})
					before := f.writes
					if _, err := s.graph.resolveIdentity(context.Background(), kind, v, 20); err == nil || !exercised || f.writes != before {
						t.Fatalf("bad metadata accepted exercised=%v err=%v writes=%d", exercised, err, f.writes-before)
					}
				})
			}
		}
	}
}

func TestIdentityQueries_AllPages(t *testing.T) {
	for _, kind := range []string{"user", "channel"} {
		for _, phase := range []string{"C", "I"} {
			for _, fault := range []string{"missing-result", "null-result", "missing-total", "negative-total", "over-total", "empty-outstanding", "missing-continuation", "nonprogress", "wrong-next", "terminal-continuation", "http", "auth", "decode", "repeat-id", "descending-id", "changed-total", "later-metadata"} {
				t.Run(kind+"/"+phase+"/"+fault, func(t *testing.T) {
					f, s, v, group := identityQueryFixture(t, kind)
					first := seedQueryIdentity(f, kind, v, phase == "C", false)
					secondPage := fault == "repeat-id" || fault == "descending-id" || fault == "changed-total" || fault == "later-metadata"
					if secondPage {
						n := first
						n.ID = 91
						f.nodes[91] = n
						f.pageSize = 1
						if phase == "C" {
							f.edges = append(f.edges, relation{Source: 91, Target: group, Type: "None"})
						}
					}
					exercised := false
					observeIdentityWire(s, nil, func(r *http.Request, response *http.Response) {
						q := r.URL.Query()
						if !namedIdentityRequest(r, kind) || (q.Get("linkedto") == "") != (phase == "I") {
							return
						}
						if secondPage && q.Get("continue") != "1" {
							return
						}
						exercised = true
						switch fault {
						case "http":
							response.StatusCode = 503
							return
						case "auth":
							response.StatusCode = 401
							return
						case "decode":
							response.Body.Close()
							response.Body = io.NopCloser(strings.NewReader("{"))
							return
						}
						rewriteIdentityPage(t, response, func(page map[string]json.RawMessage) {
							switch fault {
							case "missing-result":
								delete(page, "result")
							case "null-result":
								page["result"] = json.RawMessage("null")
							case "missing-total":
								delete(page, "total")
							case "negative-total":
								page["total"] = json.RawMessage("-1")
							case "over-total":
								page["total"] = json.RawMessage("0")
							case "empty-outstanding":
								page["result"] = json.RawMessage("[]")
								page["total"] = json.RawMessage("1")
								page["continue"] = json.RawMessage("0")
							case "missing-continuation":
								page["total"] = json.RawMessage("2")
								delete(page, "continue")
							case "nonprogress":
								page["total"] = json.RawMessage("2")
								page["continue"] = json.RawMessage("0")
							case "wrong-next":
								page["total"] = json.RawMessage("3")
								page["continue"] = json.RawMessage("2")
							case "terminal-continuation":
								page["continue"] = json.RawMessage("1")
							case "changed-total":
								page["total"] = json.RawMessage("3")
								page["continue"] = json.RawMessage("2")
							case "repeat-id", "descending-id", "later-metadata":
								var rows []map[string]json.RawMessage
								json.Unmarshal(page["result"], &rows)
								if fault == "repeat-id" {
									rows[0]["id"] = json.RawMessage("90")
								}
								if fault == "descending-id" {
									rows[0]["id"] = json.RawMessage("89")
								}
								if fault == "later-metadata" {
									rows[0]["ownerId"] = json.RawMessage("99")
								}
								page["result"], _ = json.Marshal(rows)
							}
						})
					})
					before := f.writes
					if _, err := s.graph.resolveIdentity(context.Background(), kind, v, 20); err == nil || !exercised || f.writes != before {
						t.Fatalf("bad page accepted exercised=%v err=%v writes=%d", exercised, err, f.writes-before)
					}
				})
			}
		}
	}
}

func TestIdentityQueries_ValidMultipageAndPageSize(t *testing.T) {
	for _, kind := range []string{"user", "channel"} {
		for _, canonical := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/canonical-%v", kind, canonical), func(t *testing.T) {
				f, s, v, group := identityQueryFixture(t, kind)
				n := seedQueryIdentity(f, kind, v, canonical, false)
				n.ID = 91
				f.nodes[91] = n
				if canonical {
					f.edges = append(f.edges, relation{Source: 91, Target: group, Type: "None"})
				}
				f.pageSize = 1
				observeIdentityWire(s, func(r *http.Request) {
					if namedIdentityRequest(r, kind) {
						q := r.URL.Query()
						q.Set("count", "499")
						r.URL.RawQuery = q.Encode()
					}
				}, nil)
				var nodes []node
				var err error
				if canonical {
					nodes, err = s.graph.resourceNodes(context.Background(), kind, n.Name, 20)
				} else {
					nodes, err = s.graph.identityIntegrity(context.Background(), kind, n.Name, 20)
				}
				if err != nil || len(nodes) != 2 || nodes[0].ID != 90 || nodes[1].ID != 91 {
					t.Fatalf("valid pages nodes=%v err=%v", nodes, err)
				}
				if len(f.queries) != 2 || f.queries[1].Get("continue") != "1" || f.writes != 0 {
					t.Fatalf("pages=%v writes=%d", f.queries, f.writes)
				}
			})
		}
	}
}

func TestIdentityQueries_SingletonDisagreement(t *testing.T) {
	for _, kind := range []string{"user", "channel"} {
		for _, fault := range []string{"missing", "different-id", "different-state"} {
			t.Run(kind+"/"+fault, func(t *testing.T) {
				f, s, v, _ := identityQueryFixture(t, kind)
				seedQueryIdentity(f, kind, v, true, true)
				observeIdentityWire(s, nil, func(r *http.Request, response *http.Response) {
					if !namedIdentityRequest(r, kind) || r.URL.Query().Get("linkedto") != "" {
						return
					}
					rewriteIdentityPage(t, response, func(page map[string]json.RawMessage) {
						if fault == "missing" {
							page["result"] = json.RawMessage("[]")
							page["total"] = json.RawMessage("0")
							return
						}
						var rows []map[string]json.RawMessage
						json.Unmarshal(page["result"], &rows)
						if fault == "different-id" {
							rows[0]["id"] = json.RawMessage("91")
						} else {
							rows[0]["content"] = json.RawMessage("null")
						}
						page["result"], _ = json.Marshal(rows)
					})
				})
				if _, err := s.graph.resolveIdentity(context.Background(), kind, v, 20); err == nil || f.writes != 0 {
					t.Fatalf("C/I disagreement accepted err=%v writes=%d", err, f.writes)
				}
			})
		}
	}
}

func TestIdentityQueries_ProjectAndKeyIsolation(t *testing.T) {
	for _, kind := range []string{"user", "channel"} {
		t.Run(kind, func(t *testing.T) {
			f, s, v, group := identityQueryFixture(t, kind)
			foreign := seedQueryIdentity(f, kind, v, true, true)
			foreign.Root = 30
			f.nodes[90] = foreign
			other := foreign
			other.ID = 91
			other.Root = 10
			other.Type = "message"
			f.nodes[91] = other
			var ids []int64
			for _, key := range []string{"speaker-7", "Speaker-7", "a,b", "a", "b"} {
				if kind == "user" {
					v.ServiceID = key
				} else {
					v.Channel = key
				}
				id, err := s.graph.resolveIdentity(context.Background(), kind, v, 20)
				if err != nil || id == 90 || id == 91 {
					t.Fatalf("isolated key=%q id=%d err=%v", key, id, err)
				}
				for _, prior := range ids {
					if id == prior {
						t.Fatalf("key %q reused ID %d", key, id)
					}
				}
				ids = append(ids, id)
				if !exactMembership(f.projection(f.nodes[id]), group) {
					t.Fatal("missing membership")
				}
				again, err := s.graph.resolveIdentity(context.Background(), kind, v, 20)
				if err != nil || again != id {
					t.Fatalf("key retry id=%d want=%d err=%v", again, id, err)
				}
			}
			for _, q := range f.queries {
				if q.Get("rootNodeId") != "10" || q.Get("type") != kind || q.Get("id") == "" && len(q["name"]) != 1 {
					t.Fatalf("query escaped key scope: %s", q.Encode())
				}
			}
		})
	}
}

func TestIdentityQueries_ConfirmationBeforePublication(t *testing.T) {
	for _, kind := range []string{"user", "channel"} {
		for _, state := range []string{"create", "resume"} {
			for _, fault := range []string{"missing", "different-id", "different-name"} {
				t.Run(kind+"/"+state+"/"+fault, func(t *testing.T) {
					f, s, v, _ := identityQueryFixture(t, kind)
					if state == "resume" {
						seedQueryIdentity(f, kind, v, false, false)
					}
					canonical := 0
					exercised := false
					observeIdentityWire(s, nil, func(r *http.Request, response *http.Response) {
						if !namedIdentityRequest(r, kind) || r.URL.Query().Get("linkedto") == "" {
							return
						}
						canonical++
						if canonical != 2 {
							return
						}
						exercised = true
						rewriteIdentityPage(t, response, func(page map[string]json.RawMessage) {
							if fault == "missing" {
								page["result"] = json.RawMessage("[]")
								page["total"] = json.RawMessage("0")
								return
							}
							var rows []map[string]json.RawMessage
							json.Unmarshal(page["result"], &rows)
							if fault == "different-id" {
								rows[0]["id"] = json.RawMessage("999")
							} else {
								rows[0]["name"] = json.RawMessage(`"wrong"`)
							}
							page["result"], _ = json.Marshal(rows)
						})
					})
					if _, err := s.graph.resolveIdentity(context.Background(), kind, v, 20); err == nil || !exercised || len(f.nodes) != 1 {
						t.Fatalf("confirmation err=%v exercised=%v nodes=%d", err, exercised, len(f.nodes))
					}
					for _, n := range f.nodes {
						if n.Content != nil {
							t.Fatal("content published before canonical confirmation")
						}
					}
				})
			}
		}
	}
}

func TestIdentityQueries_HTTPMixedDuplicateNoWrites(t *testing.T) {
	for _, kind := range []string{"user", "channel"} {
		t.Run(kind, func(t *testing.T) {
			f, s := newResourceFixture(t)
			m := createResource(t, s, v3Message)
			original, _ := resourceID(m.UserID)
			if kind == "channel" {
				original, _ = resourceID(m.ChannelID)
			}
			n := f.nodes[original]
			n.ID = 200
			f.nodes[200] = n
			before := f.writes
			count := len(f.nodes)
			w := request(t, s, "POST", "/api/v3/messages", "key.secret", v3Message)
			if w.Code != 500 || f.writes != before || len(f.nodes) != count {
				t.Fatalf("mixed duplicate status=%d writes=%d nodes=%d", w.Code, f.writes-before, len(f.nodes))
			}
		})
	}
}
