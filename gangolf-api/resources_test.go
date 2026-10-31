package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestV3_ServeHTTP_CRUDPreservesImmutableFieldsAndTombstones(t *testing.T) {
	f, s := newResourceFixture(t)
	m := createResource(t, s, v3Message)
	if m.ID != "102" || m.UserID != "100" || m.ChannelID != "101" || m.Service != "twitch" || m.Channel != "room-42" || m.User.Name != "Distinct Speaker" || m.TTS || m.ReplyTo != nil || !m.Time.Equal(s.now()) || m.Time.Location() != time.UTC || m.Chunks[0].Color != "blue" {
		t.Fatalf("resource=%+v", m)
	}
	path := "/api/v3/messages/" + m.ID
	status(t, request(t, s, "GET", path, "key.secret", ""), 200)
	w := patchResource(t, s, m.ID, `[{"op":"replace","path":"/chunks","value":[{"type":"Text","data":"updated"}]},{"op":"replace","path":"/tts","value":true}]`)
	status(t, w, 200)
	var updated messageResource
	if json.Unmarshal(w.Body.Bytes(), &updated) != nil || updated.ID != m.ID || updated.UserID != m.UserID || updated.ChannelID != m.ChannelID || !updated.Time.Equal(m.Time) || updated.User != m.User || !updated.TTS || updated.Chunks[0].Data != "updated" {
		t.Fatalf("patch=%s", w.Body.String())
	}
	child := createResource(t, s, replyBody(m.ID))
	edges := append([]relation{}, f.edges...)
	w = request(t, s, "DELETE", path, "key.secret", "")
	status(t, w, 204)
	if w.Body.Len() != 0 || !reflect.DeepEqual(f.edges, edges) || len(f.nodes) != 4 {
		t.Fatal("delete changed graph structure or returned a body")
	}
	id, _ := resourceID(m.ID)
	var tombstone map[string]any
	if json.Unmarshal([]byte(*f.nodes[id].Content), &tombstone) != nil || len(tombstone) != 6 || tombstone["deleted"] != true || tombstone["userId"] != m.UserID || tombstone["channelId"] != m.ChannelID || tombstone["replyTo"] != nil {
		t.Fatalf("tombstone=%v", tombstone)
	}
	status(t, request(t, s, "GET", path, "key.secret", ""), 404)
	status(t, patchResource(t, s, m.ID, `[{"op":"replace","path":"/tts","value":false}]`), 404)
	before := f.writes
	status(t, request(t, s, "DELETE", path, "key.secret", ""), 204)
	if f.writes != before {
		t.Fatal("repeated delete wrote")
	}
	w = request(t, s, "GET", "/api/v3/messages/"+child.ID, "key.secret", "")
	status(t, w, 200)
	if !strings.Contains(w.Body.String(), `"replyTo":"102"`) {
		t.Fatal(w.Body.String())
	}
	status(t, request(t, s, "POST", "/api/v3/messages", "key.secret", replyBody(m.ID)), 404)
	w = request(t, s, "GET", "/api/v3/messages", "key.secret", "")
	status(t, w, 200)
	if !strings.Contains(w.Body.String(), `"total":1`) {
		t.Fatal(w.Body.String())
	}
}

func TestV3_ServeHTTP_InvalidCreatesNeverWrite(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       int
	}{
		{"array", "[" + v3Message + "]", 400}, {"null", "null", 400}, {"empty", "{}", 400}, {"malformed", "{", 400},
		{"unknown", strings.TrimSuffix(v3Message, "}") + `,"unknown":1}`, 400},
		{"server-id", strings.TrimSuffix(v3Message, "}") + `,"id":"9"}`, 400},
		{"server-user", strings.TrimSuffix(v3Message, "}") + `,"userId":"9"}`, 400},
		{"server-channel", strings.TrimSuffix(v3Message, "}") + `,"channelId":"9"}`, 400},
		{"server-time", strings.TrimSuffix(v3Message, "}") + `,"time":"2026-10-30T01:00:00Z"}`, 400},
		{"delivery", strings.TrimSuffix(v3Message, "}") + `,"postTo":[]}`, 422},
		{"chunks", strings.Replace(v3Message, `"type":0`, `"type":2`, 1), 400},
		{"blank", strings.Replace(v3Message, `"speaker-7"`, `" "`, 1), 400},
		{"negative-size", strings.Replace(v3Message, `"color":"blue"`, `"width":-1`, 1), 400},
		{"invalid-reply", replyBody("01"), 400},
		{"large", strings.Repeat(" ", 1<<20+1), 413},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, s := newResourceFixture(t)
			status(t, request(t, s, "POST", "/api/v3/messages", "key.secret", tc.body), tc.want)
			if f.writes != 0 {
				t.Fatal("rejected create wrote")
			}
		})
	}
}

func TestV3_ServeHTTP_InvalidPatchesAreAtomic(t *testing.T) {
	f, s := newResourceFixture(t)
	m := createResource(t, s, v3Message)
	for _, body := range []string{
		`[]`, `null`, `{}`, `[null]`, `[{"op":"add","path":"/tts","value":true}]`,
		`[{"op":"replace","path":"/tts","value":null}]`, `[{"op":"replace","path":"/tts","value":"yes"}]`,
		`[{"op":"replace","path":"/chunks","value":[]}]`, `[{"op":"replace","path":"/chunks","value":[{"type":0,"data":" "}]}]`,
		`[{"op":"replace","path":"/tts","value":true},{"op":"replace","path":"/time","value":"bad"}]`,
		`[{"op":"replace","path":"/replyTo","value":"1"}]`, `[{"op":"replace","path":"/user","value":{}}]`,
		`[{"op":"replace","path":"/chunks/0","value":{}}]`, `[{"op":"replace","path":"/tts","value":true,"from":"/tts"}]`,
	} {
		t.Run(body, func(t *testing.T) {
			before := f.writes
			status(t, patchResource(t, s, m.ID, body), 400)
			if f.writes != before {
				t.Fatal("invalid patch wrote")
			}
			w := request(t, s, "GET", "/api/v3/messages/"+m.ID, "key.secret", "")
			status(t, w, 200)
			if !strings.Contains(w.Body.String(), `"tts":false`) || !strings.Contains(w.Body.String(), `"data":"hello"`) {
				t.Fatal(w.Body.String())
			}
		})
	}
	status(t, request(t, s, "PATCH", "/api/v3/messages/"+m.ID, "key.secret", `[{"op":"replace","path":"/tts","value":true}]`), 400)
	status(t, patchResource(t, s, m.ID, strings.Repeat(" ", 1<<20+1)), 413)
}

func TestV3_ServeHTTP_RoutesAndAuthorization(t *testing.T) {
	for _, tc := range []struct {
		method, path, token string
		want                int
		allow               string
	}{
		{"POST", "/api/v3/messages", "", 401, ""}, {"POST", "/api/v3/messages", "key.wrong", 401, ""},
		{"GET", "/api/v3/messages?apiKey=key.secret", "", 200, ""},
		{"GET", "/api/v3/messages?apiKey=key.secret", "bad", 401, ""},
		{"POST", "/api/v3/messages?apiKey=key.secret", "", 401, ""},
		{"PATCH", "/api/v3/messages/100?apiKey=key.secret", "", 401, ""},
		{"DELETE", "/api/v3/messages", "key.secret", 405, "GET, POST"},
		{"POST", "/api/v3/messages/100", "key.secret", 405, "GET, PATCH, DELETE"},
		{"PUT", "/api/v3/messages/100", "key.secret", 405, "GET, PATCH, DELETE"},
		{"GET", "/api/v3/messages/01", "key.secret", 400, ""}, {"GET", "/api/v3/messages/-1", "key.secret", 400, ""},
		{"GET", "/api/v3/messages/9223372036854775808", "key.secret", 400, ""},
		{"GET", "/api/v3/messages/100/foo", "key.secret", 404, ""},
		{"GET", "/api/v3/messages/", "key.secret", 404, ""},
		{"GET", "/api/v3/messages/999", "key.secret", 404, ""}, {"DELETE", "/api/v3/messages/999", "key.secret", 404, ""},
	} {
		t.Run(tc.method+tc.path+tc.token, func(t *testing.T) {
			f, s := newResourceFixture(t)
			w := request(t, s, tc.method, tc.path, tc.token, v3Message)
			status(t, w, tc.want)
			if w.Header().Get("Allow") != tc.allow || f.writes != 0 {
				t.Fatal("allow or authorization")
			}
		})
	}
	f, s := newResourceFixture(t)
	k := f.legacy.cfg.APIKeys["key"]
	k.Permissions = nil
	f.legacy.cfg.APIKeys["key"] = k
	status(t, request(t, s, "POST", "/api/v3/messages", "key.secret", v3Message), 403)
	if f.writes != 0 {
		t.Fatal("forbidden mutation")
	}
}

func TestV3_ResolveIdentity_ExactKeysReuseFirstNameAndRecoverEmptyContent(t *testing.T) {
	f, s := newResourceFixture(t)
	a := createResource(t, s, v3Message)
	b := createResource(t, s, strings.Replace(v3Message, "Distinct Speaker", "Second Name", 1))
	if a.ID == b.ID || a.UserID != b.UserID || a.ChannelID != b.ChannelID || b.User.Name != "Second Name" || len(f.nodes) != 4 {
		t.Fatal("identity reuse")
	}
	uid, _ := resourceID(a.UserID)
	if !strings.Contains(*f.nodes[uid].Content, `"name":"Distinct Speaker"`) {
		t.Fatal("identity renamed")
	}
	for _, value := range []string{"Speaker-7", " speaker-7", "speaker-7 ", `x\"y`, "speaker%_7"} {
		m := createResource(t, s, strings.Replace(v3Message, "speaker-7", value, 1))
		if m.UserID == a.UserID || m.ChannelID != a.ChannelID {
			t.Fatal("exact key distinction")
		}
	}
	n := f.nodes[uid]
	n.Content = nil
	f.nodes[uid] = n
	c := createResource(t, s, v3Message)
	if c.UserID != a.UserID || f.nodes[uid].Content == nil {
		t.Fatal("empty identity not recovered")
	}
	for _, n := range f.nodes {
		if n.Owner != 20 || !n.privateAccess() || n.Root != 10 {
			t.Fatal("private metadata")
		}
	}
}

func TestGraph_PrivateDomainGroups_TypeProjectionSemantics(t *testing.T) {
	for _, projection := range []string{"omitted", "null", "empty", "named"} {
		t.Run(projection, func(t *testing.T) {
			f, s := newResourceFixture(t)
			edited := 0
			observeIdentityWire(s, nil, func(r *http.Request, response *http.Response) {
				if r.URL.Path != "/api/nodes" || r.URL.Query().Get("name") == "" {
					return
				}
				rewriteIdentityPage(t, response, func(page map[string]json.RawMessage) {
					var rows []map[string]json.RawMessage
					if err := json.Unmarshal(page["result"], &rows); err != nil || len(rows) != 1 {
						t.Fatalf("group projection rows=%v error=%v", rows, err)
					}
					switch projection {
					case "omitted":
						delete(rows[0], "type")
					case "null":
						rows[0]["type"] = json.RawMessage("null")
					case "empty":
						rows[0]["type"] = json.RawMessage(`""`)
					case "named":
						rows[0]["type"] = json.RawMessage(`"user"`)
					}
					page["result"], _ = json.Marshal(rows)
					edited++
				})
			})
			err := s.graph.privateDomainGroups(context.Background(), 20)
			if edited == 0 || f.writes != 0 {
				t.Fatalf("projections=%d writes=%d", edited, f.writes)
			}
			if projection == "omitted" || projection == "null" {
				if err != nil || edited != 3 {
					t.Fatalf("valid groups error=%v projections=%d", err, edited)
				}
				for kind, want := range map[string]int64{"user": 13, "channel": 14, "message": 11} {
					if id, ok := s.graph.groupID(kind); !ok || id != want {
						t.Fatalf("group %s=%d resolved=%v want=%d", kind, id, ok, want)
					}
				}
			} else if err != errCorrupt {
				t.Fatalf("non-null type accepted: %v", err)
			}
		})
	}
}

func TestV3_ProjectGroupsRequireCanonicalMetadataAndAdjacency(t *testing.T) {
	for _, mode := range []string{"project-type", "project-name", "legacy-project-null", "legacy-messages-type", "legacy-messages-name", "missing-messages-project-link", "project-owner", "project-access", "missing-users", "duplicate-users", "users-type", "users-root", "users-owner", "users-access", "missing-project-link"} {
		t.Run(mode, func(t *testing.T) {
			f, s := newResourceFixture(t)
			switch mode {
			case "legacy-project-null":
				n := f.containers[10]
				n.Type = ""
				f.containers[10] = n
			case "legacy-messages-type":
				n := f.containers[11]
				n.Type = "gangolf-message-group"
				f.containers[11] = n
			case "legacy-messages-name":
				n := f.containers[11]
				n.Name = "gangolf-api-smoke-c691f065-messages"
				f.containers[11] = n
			case "missing-messages-project-link":
				f.containerEdges = f.containerEdges[1:]
			case "project-name":
				n := f.containers[10]
				n.Name = "gangolf-api-smoke-c691f065-root"
				f.containers[10] = n
			case "project-type":
				n := f.containers[10]
				n.Type = "group"
				f.containers[10] = n
			case "project-owner":
				n := f.containers[10]
				n.Owner = 99
				f.containers[10] = n
			case "project-access":
				n := f.containers[10]
				n.Access = json.RawMessage(`"Read"`)
				f.containers[10] = n
			case "missing-users":
				delete(f.containers, 13)
			case "duplicate-users":
				f.containers[15] = node{ID: 15, Name: "Users", Root: 10, Owner: 20, Access: json.RawMessage(`"None"`)}
				f.containerEdges = append(f.containerEdges, relation{Source: 10, Target: 15, Type: "None"})
			case "users-type":
				n := f.containers[13]
				n.Type = "user"
				f.containers[13] = n
			case "users-root":
				n := f.containers[13]
				n.Root = 99
				f.containers[13] = n
			case "users-owner":
				n := f.containers[13]
				n.Owner = 99
				f.containers[13] = n
			case "users-access":
				n := f.containers[13]
				n.Access = json.RawMessage(`"Read"`)
				f.containers[13] = n
			case "missing-project-link":
				f.containerEdges = f.containerEdges[1:]
			}
			w := request(t, s, "POST", "/api/v3/messages", "key.secret", v3Message)
			status(t, w, 500)
			if f.writes != 0 || len(f.nodes) != 0 {
				t.Fatalf("invalid topology reached writes: writes=%d nodes=%d", f.writes, len(f.nodes))
			}
		})
	}
}

func TestV3_ExactDomainMembershipScopesQueriesAndPreservesSemanticLinks(t *testing.T) {
	f, s := newResourceFixture(t)
	foreignGroup := node{ID: 31, Name: "Users", Root: 30, Owner: 20, Access: json.RawMessage(`"None"`)}
	foreignUser := node{ID: 32, Name: identityName("user", "twitch", "speaker-7"), Type: "user", Root: 30, Owner: 20, Access: json.RawMessage(`"None"`)}
	f.containers[30] = node{ID: 30, Type: "project", Owner: 20, Access: json.RawMessage(`"None"`)}
	f.containers[31] = foreignGroup
	f.containerEdges = append(f.containerEdges, relation{Source: 30, Target: 31, Type: "None"})
	f.nodes[32] = foreignUser
	f.edges = append(f.edges, relation{Source: 32, Target: 31, Type: "None"})
	m := createResource(t, s, v3Message)
	if m.UserID == "32" {
		t.Fatal("same identity key in another project was reused")
	}
	uid, _ := resourceID(m.UserID)
	cid, _ := resourceID(m.ChannelID)
	for _, edge := range []struct{ source, target int64 }{{uid, 13}, {cid, 14}} {
		if !plainLinkBetween(f.projection(f.nodes[edge.source]).Links, edge.source, edge.target) {
			t.Fatalf("missing plain membership edge: %+v", edge)
		}
	}
	for _, q := range f.queries {
		if q.Get("rootNodeId") != "10" || q.Get("linkedto") == "" && q.Get("name") == "" || q.Get("type") == "" {
			t.Fatalf("unscoped collection query: %v", q)
		}
	}
	var semantic []relation
	for _, e := range f.edges {
		if e.Source == int64(102) || e.Target == int64(102) {
			semantic = append(semantic, e)
		}
	}
	contexts := map[string]bool{}
	for _, e := range semantic {
		if e.Context != "" {
			if e.Type != "Unidirectional" {
				t.Fatalf("semantic link type changed: %+v", e)
			}
			contexts[e.Context] = true
		}
	}
	if !contexts["authored-by"] || !contexts["in-channel"] || contexts["member-of"] {
		t.Fatalf("message semantic links=%v", contexts)
	}
	if !plainLinkBetween(f.projection(f.nodes[102]).Links, 102, 11) {
		t.Fatal("message group membership is not plain")
	}
}

func TestV3_ItemReadRejectsMissingOrWrongDomainMembership(t *testing.T) {
	for _, wrongGroup := range []int64{0, 14} {
		t.Run(strconv.FormatInt(wrongGroup, 10), func(t *testing.T) {
			f, s := newResourceFixture(t)
			m := createResource(t, s, v3Message)
			uid, _ := resourceID(m.UserID)
			for i := 0; i < len(f.edges); i++ {
				if f.edges[i].Source == uid && (f.edges[i].Target == 13 || f.edges[i].Target == 14) {
					f.edges = append(f.edges[:i], f.edges[i+1:]...)
					i--
				}
			}
			if wrongGroup != 0 {
				f.edges = append(f.edges, relation{Source: uid, Target: wrongGroup, Type: "None"})
			}
			w := request(t, s, "GET", "/api/v3/messages/102", "key.secret", "")
			status(t, w, 500)
			if strings.Contains(w.Body.String(), "Distinct Speaker") || strings.Contains(w.Body.String(), "hello") {
				t.Fatal("invalid membership disclosed content")
			}
		})
	}
}

func TestV3_ResolveIdentity_CommaKeysReuseThroughNameArrayBinding(t *testing.T) {
	f, s := newResourceFixture(t)
	body := strings.Replace(v3Message, "twitch", "twitch,discord", 1)
	body = strings.Replace(body, "speaker-7", "speaker, seven", 1)
	body = strings.Replace(body, "room-42", "room,42", 1)
	a := createResource(t, s, body)
	b := createResource(t, s, body)
	if a.UserID != b.UserID || a.ChannelID != b.ChannelID || len(f.nodes) != 4 {
		t.Fatalf("identity tuples were allocated twice: a=%+v b=%+v nodes=%d", a, b, len(f.nodes))
	}
	for _, kindKey := range []struct{ kind, service, key string }{{"user", "twitch,discord", "speaker, seven"}, {"channel", "twitch,discord", "room,42"}} {
		name := identityName(kindKey.kind, kindKey.service, kindKey.key)
		if strings.Contains(name, ",") {
			t.Fatalf("identity name contains query-list separator: %q", name)
		}
		values, err := url.ParseQuery("name=" + url.QueryEscape(name))
		if err != nil || len(values["name"]) != 1 || strings.Contains(values.Get("name"), ",") {
			t.Fatalf("identity query is not one safe exact value: %v %v", values, err)
		}
	}
}

func TestV3_ResolveIdentity_ServiceAndTypeRootFiltersAreLoadBearing(t *testing.T) {
	f, s := newResourceFixture(t)
	userName := identityName("user", "twitch", "speaker-7")
	channelName := identityName("channel", "twitch", "room-42")
	f.nodes[13] = node{ID: 13, Name: userName, Type: "channel", Root: 10, Owner: 20, Access: json.RawMessage(`"None"`)}
	f.nodes[14] = node{ID: 14, Name: userName, Type: "user", Root: 99, Owner: 20, Access: json.RawMessage(`"None"`)}
	f.nodes[15] = node{ID: 15, Name: channelName, Type: "user", Root: 10, Owner: 20, Access: json.RawMessage(`"None"`)}
	f.nodes[16] = node{ID: 16, Name: channelName, Type: "channel", Root: 99, Owner: 20, Access: json.RawMessage(`"None"`)}
	m := createResource(t, s, v3Message)
	if m.UserID != "100" || m.ChannelID != "101" || len(f.nodes) != 7 {
		t.Fatalf("foreign root/type matched natural keys: %+v nodes=%d", m, len(f.nodes))
	}
}

func TestV3_ResolveIdentity_ServiceNamespacesRemainDistinct(t *testing.T) {
	f, s := newResourceFixture(t)
	first := createResource(t, s, v3Message)
	userService := strings.Replace(v3Message, `"user":{"service":"twitch"`, `"user":{"service":"discord"`, 1)
	if userService == v3Message {
		t.Fatal("user service namespace fixture did not change")
	}
	second := createResource(t, s, userService)
	if first.UserID == second.UserID || first.ChannelID != second.ChannelID || second.User.Service != "discord" {
		t.Fatalf("user namespace was ignored: first=%+v second=%+v", first, second)
	}
	channelService := strings.Replace(v3Message, `"service":"twitch","channel"`, `"service":"discord","channel"`, 1)
	if channelService == v3Message {
		t.Fatal("channel service namespace fixture did not change")
	}
	third := createResource(t, s, channelService)
	if first.UserID != third.UserID || first.ChannelID == third.ChannelID || third.Service != "discord" {
		t.Fatalf("channel namespace was ignored: first=%+v third=%+v", first, third)
	}
	userID, _ := resourceID(second.UserID)
	channelID, _ := resourceID(third.ChannelID)
	var user identityContent
	var channel identityContent
	if json.Unmarshal([]byte(*f.nodes[userID].Content), &user) != nil || user.Service != "discord" || user.ServiceID != "speaker-7" {
		t.Fatalf("wrong user tuple stored: %+v", user)
	}
	if json.Unmarshal([]byte(*f.nodes[channelID].Content), &channel) != nil || channel.Service != "discord" || channel.Channel != "room-42" {
		t.Fatalf("wrong channel tuple stored: %+v", channel)
	}
}

func TestV3_ResolveIdentity_DuplicateAndMismatchedKeysFailClosed(t *testing.T) {
	for _, mode := range []string{"duplicate", "mismatch", "malformed"} {
		t.Run(mode, func(t *testing.T) {
			f, s := newResourceFixture(t)
			m := createResource(t, s, v3Message)
			id, _ := resourceID(m.UserID)
			n := f.nodes[id]
			switch mode {
			case "duplicate":
				n.ID = f.next
				f.next++
				f.nodes[n.ID] = n
				f.edges = append(f.edges, relation{Source: n.ID, Target: 13, Type: "None"})
			case "mismatch":
				body := strings.Replace(*n.Content, "speaker-7", "other", 1)
				n.Content = &body
				f.nodes[id] = n
			case "malformed":
				body := "{"
				n.Content = &body
				f.nodes[id] = n
			}
			before := f.writes
			w := request(t, s, "POST", "/api/v3/messages", "key.secret", v3Message)
			status(t, w, 500)
			if f.writes != before || strings.Contains(w.Body.String(), "Distinct Speaker") {
				t.Fatal("corruption leaked or wrote")
			}
		})
	}
}

func TestV3_ServeHTTP_ReplyTargetsMustBePublishedAndSameChannel(t *testing.T) {
	f, s := newResourceFixture(t)
	m := createResource(t, s, v3Message)
	before := f.writes
	status(t, request(t, s, "POST", "/api/v3/messages", "key.secret", replyBody("999")), 404)
	status(t, request(t, s, "POST", "/api/v3/messages", "key.secret", strings.Replace(replyBody(m.ID), "room-42", "another", 1)), 422)
	if f.writes != before {
		t.Fatal("invalid reply wrote")
	}
	child := createResource(t, s, replyBody(m.ID))
	grandchild := createResource(t, s, replyBody(child.ID))
	if grandchild.ReplyTo == nil || *grandchild.ReplyTo != child.ID {
		t.Fatal("nested reply")
	}
	for _, e := range f.edges {
		if e.Context == "replies-to" && (e.Type != "Unidirectional" || e.Source <= e.Target) {
			t.Fatal("reply direction")
		}
	}
}

func TestV3_MessagesV3_MalformedRelationsAndIdentitiesFailClosed(t *testing.T) {
	for _, mode := range []string{"missing-author", "wrong-direction", "wrong-context", "wrong-target", "extra-author", "bad-identity", "wrong-parent-channel"} {
		t.Run(mode, func(t *testing.T) {
			f, s := newResourceFixture(t)
			m := createResource(t, s, v3Message)
			child := createResource(t, s, replyBody(m.ID))
			mid, _ := resourceID(m.ID)
			semantic := -1
			for i, e := range f.edges {
				if e.Source == mid && e.Context == "authored-by" {
					semantic = i
					break
				}
			}
			if semantic < 0 {
				t.Fatal("authored-by edge missing from fixture")
			}
			switch mode {
			case "missing-author":
				f.edges = append(f.edges[:semantic], f.edges[semantic+1:]...)
			case "wrong-direction":
				f.edges[semantic].Source, f.edges[semantic].Target = f.edges[semantic].Target, f.edges[semantic].Source
			case "wrong-context":
				f.edges[semantic].Context = "wrong"
			case "wrong-target":
				f.edges[semantic].Target = 999
			case "extra-author":
				f.edges = append(f.edges, f.edges[semantic])
			case "bad-identity":
				id, _ := resourceID(m.UserID)
				n := f.nodes[id]
				n.Owner = 99
				f.nodes[id] = n
			case "wrong-parent-channel":
				id, _ := resourceID(child.ID)
				n := f.nodes[id]
				data := strings.Replace(*n.Content, `"channelId":"101"`, `"channelId":"999"`, 1)
				n.Content = &data
				f.nodes[id] = n
			}
			w := request(t, s, "GET", "/api/v3/messages", "key.secret", "")
			status(t, w, 500)
			if strings.Contains(w.Body.String(), "hello") || strings.Contains(w.Body.String(), "Distinct Speaker") {
				t.Fatal("partial disclosure")
			}
		})
	}
}

func TestV3_ListV3_FiltersNumericOrderAndGraphPages(t *testing.T) {
	f, s := newResourceFixture(t)
	f.next = 97
	a := createResource(t, s, v3Message)
	b := createResource(t, s, replyBody(a.ID))
	c := createResource(t, s, strings.Replace(v3Message, "room-42", "different", 1))
	if a.ID != "99" || b.ID != "100" {
		t.Fatalf("IDs %s %s", a.ID, b.ID)
	}
	f.pageSize = 2
	for _, tc := range []struct {
		query string
		total int
		ids   []string
		next  *int
	}{
		{"?count=1", 3, []string{a.ID}, intPointer(1)},
		{"?count=1&continue=1", 3, []string{b.ID}, intPointer(2)},
		{"?descending=true&count=2", 3, []string{c.ID, b.ID}, intPointer(2)},
		{"?service=twitch&channel=room-42&userId=" + a.UserID + "&channelId=" + a.ChannelID, 2, []string{a.ID, b.ID}, nil},
		{"?replyTo=" + a.ID, 1, []string{b.ID}, nil},
		{"?service=TWITCH", 0, []string{}, nil},
		{"?continue=999", 3, []string{}, nil},
		{"?from=" + s.now().Format(time.RFC3339Nano) + "&to=" + s.now().Add(time.Second).Format(time.RFC3339Nano), 3, []string{a.ID, b.ID, c.ID}, nil},
		{"?to=" + s.now().Format(time.RFC3339Nano), 0, []string{}, nil},
	} {
		t.Run(tc.query, func(t *testing.T) {
			w := request(t, s, "GET", "/api/v3/messages"+tc.query, "key.secret", "")
			status(t, w, 200)
			var p struct {
				Result   []messageResource `json:"result"`
				Total    int               `json:"total"`
				Continue *int              `json:"continue"`
			}
			if json.Unmarshal(w.Body.Bytes(), &p) != nil || p.Total != tc.total || !reflect.DeepEqual(p.Continue, tc.next) {
				t.Fatal(w.Body.String())
			}
			ids := make([]string, 0)
			for _, m := range p.Result {
				ids = append(ids, m.ID)
			}
			if !reflect.DeepEqual(ids, tc.ids) {
				t.Fatalf("ids=%v want=%v", ids, tc.ids)
			}
			if len(ids) == 0 && !strings.Contains(w.Body.String(), `"result":[]`) {
				t.Fatal("null array")
			}
		})
	}
	if f.listedPages < 2 {
		t.Fatal("graph paging not exercised")
	}
	for _, q := range []string{"userId=01", "channelId=0", "replyTo=abc", "replyTo=1&replyTo=2", "count=0", "continue=-1", "sort=id", "descending=bad", "bogus=1", "service=a&service=b", "from=no", "to=no", "x=%XX", "count=1;continue=2"} {
		status(t, request(t, s, "GET", "/api/v3/messages?"+q, "key.secret", ""), 400)
	}
}

func TestV3_PublishMessage_PartialWritesStayInvisible(t *testing.T) {
	for _, point := range []string{"create:user", "created:user", "content:user", "committed:user", "create:channel", "created:channel", "content:channel", "committed:channel", "create:message", "created:message", "link:authored-by", "link:in-channel", "link:", "ignored:authored-by", "ignored:", "content:message"} {
		t.Run(point, func(t *testing.T) {
			f, s := newResourceFixture(t)
			f.fail = point
			w := request(t, s, "POST", "/api/v3/messages", "key.secret", v3Message)
			want := 503
			if strings.HasPrefix(point, "ignored:") {
				want = 500
			}
			status(t, w, want)
			if !f.failed {
				t.Fatal("failure point not reached")
			}
			if point == "content:message" && !strings.Contains(w.Body.String(), "write_outcome_unknown") {
				t.Fatal(w.Body.String())
			}
			f.fail = ""
			w = request(t, s, "GET", "/api/v3/messages", "key.secret", "")
			status(t, w, 200)
			if !strings.Contains(w.Body.String(), `"result":[]`) || !strings.Contains(w.Body.String(), `"total":0`) {
				t.Fatal(w.Body.String())
			}
			for id, n := range f.nodes {
				if n.Type == "message" {
					status(t, request(t, s, "GET", "/api/v3/messages/"+strconv.FormatInt(id, 10), "key.secret", ""), 404)
				}
			}
			createResource(t, s, v3Message)
			users := 0
			for _, n := range f.nodes {
				if n.Type == "user" {
					users++
				}
			}
			wantUsers := 1
			if users != wantUsers {
				t.Fatalf("unexpected user count after retry: got=%d want=%d", users, wantUsers)
			}
		})
	}
}

func TestV3_ReplaceContent_UncertainCommittedWritesAreNotReplayed(t *testing.T) {
	for _, method := range []string{"POST", "PATCH", "DELETE"} {
		t.Run(method, func(t *testing.T) {
			f, s := newResourceFixture(t)
			var m messageResource
			if method != "POST" {
				m = createResource(t, s, v3Message)
			}
			f.fail = "committed:message"
			var w *httptest.ResponseRecorder
			switch method {
			case "POST":
				w = request(t, s, method, "/api/v3/messages", "key.secret", v3Message)
			case "PATCH":
				w = patchResource(t, s, m.ID, `[{"op":"replace","path":"/tts","value":true}]`)
			case "DELETE":
				w = request(t, s, method, "/api/v3/messages/"+m.ID, "key.secret", "")
			}
			status(t, w, 503)
			if !f.failed || !strings.Contains(w.Body.String(), "write_outcome_unknown") {
				t.Fatal(w.Body.String())
			}
			f.fail = ""
			w = request(t, s, "GET", "/api/v3/messages", "key.secret", "")
			status(t, w, 200)
			want := `"total":1`
			if method == "DELETE" {
				want = `"total":0`
			}
			if !strings.Contains(w.Body.String(), want) {
				t.Fatal(w.Body.String())
			}
			messages := 0
			for _, n := range f.nodes {
				if n.Type == "message" {
					messages++
				}
			}
			if messages != 1 {
				t.Fatal("ambiguous write replayed")
			}
		})
	}
}

func TestV3_LoadConfig_RequiresServiceOwnedPrivateNodes(t *testing.T) {
	for _, target := range []string{"config", "root", "group", "user", "channel", "message"} {
		for _, bad := range []string{"owner", "access", "missing-access", "root"} {
			if target == "root" && bad == "root" {
				continue
			}
			t.Run(target+"/"+bad, func(t *testing.T) {
				f, s := newResourceFixture(t)
				createResource(t, s, v3Message)
				f.corrupt = func(n *node) {
					match := n.Type == target || target == "config" && n.ID == 12 || target == "root" && n.ID == 10 || target == "group" && n.ID == 11
					if !match {
						return
					}
					switch bad {
					case "owner":
						n.Owner = 99
					case "access":
						n.Access = json.RawMessage(`"Read"`)
					case "missing-access":
						n.Access = nil
					case "root":
						n.Root = 99
					}
				}
				w := request(t, s, "GET", "/api/v3/messages", "key.secret", "")
				status(t, w, 500)
				if strings.Contains(w.Body.String(), "Distinct Speaker") {
					t.Fatal("private data leaked")
				}
				if target == "config" || target == "root" || target == "group" {
					status(t, request(t, s, "GET", "/api/i1/readiness", "", ""), 503)
				}
			})
		}
	}
	for _, identity := range []map[string]any{{"id": 0, "enabled": true, "permissions": []string{"write"}}, {"id": 20, "enabled": false, "permissions": []string{"write"}}, {"id": 20, "enabled": true, "permissions": []string{"read"}}} {
		f, s := newResourceFixture(t)
		f.identity = identity
		status(t, request(t, s, "POST", "/api/v3/messages", "key.secret", v3Message), 500)
		if f.writes != 0 {
			t.Fatal("invalid identity wrote")
		}
	}
}

func TestV3_ServeHTTP_ConcurrentCreatesShareIdentitiesAndSerializeEdits(t *testing.T) {
	f, s := newResourceFixture(t)
	var wg sync.WaitGroup
	errors := make(chan string, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := request(t, s, "POST", "/api/v3/messages", "key.secret", v3Message)
			if w.Code != 201 {
				errors <- fmt.Sprint(w.Code, w.Body.String())
			}
		}()
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		t.Error(err)
	}
	users, channels, messages := 0, 0, 0
	for _, n := range f.nodes {
		switch n.Type {
		case "user":
			users++
		case "channel":
			channels++
		case "message":
			messages++
		}
	}
	if users != 1 || channels != 1 || messages != 20 {
		t.Fatalf("users=%d channels=%d messages=%d", users, channels, messages)
	}
	m := createResource(t, s, v3Message)
	wg.Add(2)
	var p, d *httptest.ResponseRecorder
	go func() {
		defer wg.Done()
		p = patchResource(t, s, m.ID, `[{"op":"replace","path":"/tts","value":true}]`)
	}()
	go func() { defer wg.Done(); d = request(t, s, "DELETE", "/api/v3/messages/"+m.ID, "key.secret", "") }()
	wg.Wait()
	if d.Code != 204 || p.Code != 200 && p.Code != 404 {
		t.Fatalf("delete=%d patch=%d", d.Code, p.Code)
	}
	status(t, request(t, s, "GET", "/api/v3/messages/"+m.ID, "key.secret", ""), 404)
}

func TestV3_ServeHTTP_RestartRetainsIdentitiesRepliesAndTombstones(t *testing.T) {
	f, s := newResourceFixture(t)
	a := createResource(t, s, v3Message)
	b := createResource(t, s, replyBody(a.ID))
	status(t, request(t, s, "DELETE", "/api/v3/messages/"+a.ID, "key.secret", ""), 204)
	restarted := &server{graph: s.graph, pepper: s.pepper, now: s.now}
	status(t, request(t, restarted, "GET", "/api/v3/messages/"+a.ID, "key.secret", ""), 404)
	status(t, request(t, restarted, "GET", "/api/v3/messages/"+b.ID, "key.secret", ""), 200)
	c := createResource(t, restarted, v3Message)
	if c.UserID != a.UserID || c.ChannelID != a.ChannelID || len(f.nodes) != 5 {
		t.Fatal("restart lost graph state")
	}
	w := request(t, restarted, "GET", "/api/v2/messages", "key.secret", "")
	status(t, w, 200)
	if !strings.Contains(w.Body.String(), `"total":0`) {
		t.Fatal("v3 mixed into v2")
	}
}
