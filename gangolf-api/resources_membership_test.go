package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

func TestV3_RejectExtraDomainMembership(t *testing.T) {
	for _, kind := range []string{"user", "channel"} {
		for _, target := range []int64{11, 13, 14, 31, -1} {
			if kind == "user" && target == 13 || kind == "channel" && target == 14 {
				continue
			}
			t.Run(fmt.Sprintf("%s-%d", kind, target), func(t *testing.T) {
				f, s := newResourceFixture(t)
				m := createResource(t, s, v3Message)
				id, _ := resourceID(m.UserID)
				if kind == "channel" {
					id, _ = resourceID(m.ChannelID)
				}
				f.containers[31] = node{ID: 31, Name: "Users", Root: 30, Owner: 20, Access: json.RawMessage(`"None"`)}
				if target == -1 {
					target = 13
					if kind == "channel" {
						target = 14
					}
				}
				f.edges = append(f.edges, relation{Source: id, Target: target, Type: "None"})
				before := f.writes
				if !plainLinkBetween(f.projection(f.nodes[id]).Links, id, target) {
					t.Fatal("extra membership not installed")
				}
				w := request(t, s, "GET", "/api/v3/messages/"+m.ID, "key.secret", "")
				t.Logf("kind=%s record=%d extra-group=%d status=%d content-exposed=%v", kind, id, target, w.Code, w.Code == 200)
				if f.writes != before {
					t.Fatal("GET repaired membership")
				}
				if w.Code != 500 {
					t.Fatalf("expected corrupt storage rejection 500, got %d: %s", w.Code, w.Body.String())
				}
				post := request(t, s, "POST", "/api/v3/messages", "key.secret", v3Message)
				if post.Code != 500 || f.writes != before {
					t.Fatalf("POST accepted extra membership: status=%d writes=%d", post.Code, f.writes-before)
				}
			})
		}
	}
}

func TestV3_MissingIdentityMembershipMustNotDuplicateKey(t *testing.T) {
	for _, kind := range []string{"user", "channel"} {
		for _, wrong := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s-wrong-%v", kind, wrong), func(t *testing.T) {
				f, s := newResourceFixture(t)
				m := createResource(t, s, v3Message)
				id, _ := resourceID(m.UserID)
				group := int64(13)
				if kind == "channel" {
					id, _ = resourceID(m.ChannelID)
					group = 14
				}
				edges := f.edges[:0]
				for _, e := range f.edges {
					if e.Source == id && e.Target == group || e.Source == group && e.Target == id {
						continue
					}
					edges = append(edges, e)
				}
				f.edges = edges
				if wrong {
					f.edges = append(f.edges, relation{Source: id, Target: 11, Type: "None"})
				}
				before := f.writes
				w := request(t, s, "POST", "/api/v3/messages", "key.secret", v3Message)
				var next messageResource
				json.Unmarshal(w.Body.Bytes(), &next)
				count := 0
				for _, n := range f.nodes {
					if n.Type == kind && n.Name == f.nodes[id].Name {
						count++
					}
				}
				t.Logf("kind=%s original=%d nextUser=%s nextChannel=%s status=%d duplicate-key-count=%d added-writes=%d", kind, id, next.UserID, next.ChannelID, w.Code, count, f.writes-before)
				if w.Code != 500 || count != 1 || before != f.writes {
					t.Fatalf("corrupt existing identity must fail closed without duplicating its natural key: status=%d count=%d writes=%d", w.Code, count, f.writes-before)
				}
			})
		}
	}
}

func TestV3_PartialIdentityRetryPreservesKey(t *testing.T) {
	for _, point := range []string{"created:user", "created:channel", "link:", "linked:", "ignored:", "content:user", "content:channel"} {
		t.Run(point, func(t *testing.T) {
			f, s := newResourceFixture(t)
			f.fail = point
			w := request(t, s, "POST", "/api/v3/messages", "key.secret", v3Message)
			if w.Code < 500 || !f.failed {
				t.Fatalf("fixture did not fail: %d", w.Code)
			}
			kind := "user"
			if strings.HasSuffix(point, "channel") {
				kind = "channel"
			}
			original := int64(0)
			for id, n := range f.nodes {
				if n.Type == kind {
					original = id
					if n.Content != nil {
						t.Fatal("interrupted identity already published")
					}
				}
			}
			if original == 0 {
				t.Fatal("partial identity not created")
			}
			f.fail = ""
			retry := createResource(t, s, v3Message)
			retryID := retry.UserID
			if kind == "channel" {
				retryID = retry.ChannelID
			}
			if retryID != strconv.FormatInt(original, 10) {
				t.Fatalf("retry changed identity: original=%d retry=%s", original, retryID)
			}
			ids := []string{}
			for id, n := range f.nodes {
				if n.Type == kind {
					ids = append(ids, strconv.FormatInt(id, 10))
				}
			}
			t.Logf("point=%s retry-user=%s user-ids=%v", point, retry.UserID, ids)
			if len(ids) != 1 {
				t.Fatalf("retry duplicated user natural key: ids=%v", ids)
			}
		})
	}
}

type groupTypeTransport func(*http.Request) (*http.Response, error)

func (f groupTypeTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestV3_GroupTypeWireIntegration(t *testing.T) {
	for _, mode := range []string{"null", "empty", "missing"} {
		t.Run(mode, func(t *testing.T) {
			_, s := newResourceFixture(t)
			next := s.graph.http.Transport
			if next == nil {
				next = http.DefaultTransport
			}
			exercised := false
			s.graph.http.Transport = groupTypeTransport(func(r *http.Request) (*http.Response, error) {
				response, err := next.RoundTrip(r)
				if err != nil {
					return response, err
				}
				if r.Method == "GET" && r.URL.Path == "/api/nodes" && r.URL.Query().Get("name") == "Users" {
					raw, err := io.ReadAll(response.Body)
					response.Body.Close()
					if err != nil {
						return nil, err
					}
					var body map[string]json.RawMessage
					json.Unmarshal(raw, &body)
					var rows []map[string]json.RawMessage
					json.Unmarshal(body["result"], &rows)
					if len(rows) != 1 {
						t.Fatalf("unexpected Users rows: %s", raw)
					}
					exercised = true
					switch mode {
					case "null":
						rows[0]["type"] = json.RawMessage("null")
					case "empty":
						rows[0]["type"] = json.RawMessage(`""`)
					case "missing":
						delete(rows[0], "type")
					}
					body["result"], _ = json.Marshal(rows)
					raw, _ = json.Marshal(body)
					response.Body = io.NopCloser(bytes.NewReader(raw))
					response.ContentLength = int64(len(raw))
				}
				return response, nil
			})
			w := request(t, s, "POST", "/api/v3/messages", "key.secret", v3Message)
			want := 500
			if mode == "null" || mode == "missing" {
				want = 201
			}
			if !exercised || w.Code != want {
				t.Fatalf("wire type=%s exercised=%v status=%d want=%d", mode, exercised, w.Code, want)
			}
		})
	}
}

func TestV3_MembershipRelinkPreservesFirstPairMetadata(t *testing.T) {
	f, s := newResourceFixture(t)
	m := createResource(t, s, v3Message)
	id, _ := resourceID(m.UserID)
	original := relation{Source: 13, Target: id, Type: "Unidirectional", Context: "original"}
	for i, e := range f.edges {
		if e.Source == id && e.Target == 13 {
			f.edges[i] = original
		}
	}
	before := len(f.edges)
	if err := s.graph.addPlainLink(context.Background(), id, 13); err != nil {
		t.Fatal(err)
	}
	if len(f.edges) != before {
		t.Fatal("relink duplicated pair")
	}
	count := 0
	for _, e := range f.edges {
		if e.Source == id && e.Target == 13 || e.Source == 13 && e.Target == id {
			count++
			if e != original {
				t.Fatal("relink replaced metadata")
			}
		}
	}
	if count != 1 {
		t.Fatalf("pair count=%d", count)
	}
}
