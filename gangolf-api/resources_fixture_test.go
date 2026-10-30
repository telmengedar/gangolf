package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

type resourceFixture struct {
	mu          sync.Mutex
	legacy      *graphFixture
	nodes       map[int64]node
	edges       []relation
	next        int64
	writes      int
	fail        string
	failed      bool
	corrupt     func(*node)
	identity    map[string]any
	pageSize    int
	listedPages int
}

func newResourceFixture(t *testing.T) (*resourceFixture, *server) {
	t.Helper()
	legacy, s := newFixture(t)
	f := &resourceFixture{legacy: legacy, nodes: make(map[int64]node), next: 100, identity: map[string]any{"id": 20, "enabled": true, "permissions": []string{"write"}}, pageSize: 500}
	host := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(host.Close)
	s.graph.base, _ = url.Parse(host.URL + "/api")
	return f, s
}

func (f *resourceFixture) inject(w http.ResponseWriter, point string) bool {
	if f.fail != point {
		return false
	}
	f.failed = true
	w.WriteHeader(503)
	return true
}

func (f *resourceFixture) projection(n node) node {
	n.Links = make([]relation, 0)
	for _, e := range f.edges {
		if e.Source == n.ID || e.Target == n.ID {
			n.Links = append(n.Links, e)
		}
	}
	if f.corrupt != nil {
		f.corrupt(&n)
	}
	return n
}

func (f *resourceFixture) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer storage-only-test-token" {
		w.WriteHeader(401)
		return
	}
	if r.URL.Path == "/api/users/me" {
		if !f.inject(w, "identity") {
			respond(w, 200, f.identity)
		}
		return
	}
	if r.Method == "GET" && (r.URL.Path == "/api/nodes/10" || r.URL.Path == "/api/nodes/11" || r.URL.Path == "/api/nodes/12") {
		id, _ := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/api/nodes/"), 10, 64)
		respond(w, 200, f.projection(node{ID: id, Root: 10, Owner: 20, Access: json.RawMessage(`"None"`)}))
		return
	}
	if r.URL.Path == "/api/nodes" && r.Method == "POST" {
		var n struct {
			Name          string `json:"name"`
			Type          string `json:"type"`
			Root          int64  `json:"rootNodeId"`
			Access        *int   `json:"access"`
			SkipEmbedding bool   `json:"skipEmbedding"`
		}
		if json.NewDecoder(r.Body).Decode(&n) != nil || n.Root != 10 || n.Access == nil || *n.Access != 0 || !n.SkipEmbedding {
			w.WriteHeader(400)
			return
		}
		if n.Type == batchType {
			body, _ := json.Marshal(n)
			r.Body = io.NopCloser(strings.NewReader(string(body)))
			f.legacy.serve(w, r.Clone(r.Context()))
			return
		}
		f.writes++
		if f.inject(w, "create:"+n.Type) {
			return
		}
		id := f.next
		f.next++
		created := node{ID: id, Name: n.Name, Type: n.Type, Root: n.Root, Owner: 20, Access: json.RawMessage(`"None"`)}
		f.nodes[id] = created
		if f.inject(w, "created:"+n.Type) {
			return
		}
		respond(w, 200, f.projection(created))
		return
	}
	if r.URL.Path == "/api/nodes" && r.Method == "GET" && r.URL.Query().Get("type") != batchType {
		if f.inject(w, "list") {
			return
		}
		q := r.URL.Query()
		if !strings.Contains(q.Get("fields"), "ownerId") || !strings.Contains(q.Get("fields"), "linkDetails") {
			w.WriteHeader(400)
			return
		}
		root, _ := strconv.ParseInt(q.Get("rootNodeId"), 10, 64)
		rows := make([]node, 0)
		nameValues := q["name"]
		if len(nameValues) == 1 {
			nameValues = strings.Split(nameValues[0], ",")
		}
		for _, n := range f.nodes {
			nameMatches := len(nameValues) == 0
			for _, name := range nameValues {
				if n.Name == name {
					nameMatches = true
					break
				}
			}
			if q.Get("rootNodeId") != "" && n.Root != root || q.Get("type") != "" && n.Type != q.Get("type") || q.Get("id") != "" && strconv.FormatInt(n.ID, 10) != q.Get("id") || !nameMatches {
				continue
			}
			if q.Get("linkedto") != "" {
				group, _ := strconv.ParseInt(q.Get("linkedto"), 10, 64)
				linked := false
				for _, e := range f.edges {
					if e.Source == n.ID && e.Target == group || e.Target == n.ID && e.Source == group {
						linked = true
					}
				}
				if !linked {
					continue
				}
			}
			rows = append(rows, f.projection(n))
		}
		sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
		offset, _ := strconv.Atoi(q.Get("continue"))
		count, _ := strconv.Atoi(q.Get("count"))
		count = min(count, f.pageSize)
		offset = min(offset, len(rows))
		end := offset + min(count, len(rows)-offset)
		var next *int
		if end < len(rows) {
			next = &end
		}
		f.listedPages++
		respond(w, 200, graphPage{Result: rows[offset:end], Total: len(rows), Continue: next})
		return
	}
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) == 5 && r.Method == "POST" {
		id, _ := strconv.ParseInt(parts[3], 10, 64)
		n, ok := f.nodes[id]
		if ok {
			f.writes++
			switch parts[4] {
			case "links":
				context := r.URL.Query().Get("context")
				if f.inject(w, "link:"+context) {
					return
				}
				var target int64
				if json.NewDecoder(r.Body).Decode(&target) != nil {
					w.WriteHeader(400)
					return
				}
				if f.fail != "ignored:"+context {
					f.edges = append(f.edges, relation{Source: id, Target: target, Type: r.URL.Query().Get("linkType"), Context: context})
				} else {
					f.failed = true
				}
				w.WriteHeader(200)
				return
			case "content":
				if f.inject(w, "content:"+n.Type) {
					return
				}
				body, _ := io.ReadAll(r.Body)
				content := string(body)
				n.Content = &content
				n.ContentType = "application/json"
				f.nodes[id] = n
				if f.inject(w, "committed:"+n.Type) {
					return
				}
				w.WriteHeader(200)
				return
			}
		}
	}
	f.legacy.serve(w, r)
}

const v3Message = `{"service":"twitch","channel":"room-42","user":{"service":"twitch","serviceId":"speaker-7","name":"Distinct Speaker"},"chunks":[{"type":0,"data":"hello","color":"blue"}],"tts":false}`

func createResource(t *testing.T, s *server, body string) messageResource {
	t.Helper()
	w := request(t, s, "POST", "/api/v3/messages", "key.secret", body)
	status(t, w, 201)
	var m messageResource
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil || m.ID == "" || w.Header().Get("Location") != "/api/v3/messages/"+m.ID {
		t.Fatalf("bad resource: %s", w.Body.String())
	}
	return m
}

func patchResource(t *testing.T, s *server, id, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("PATCH", "/api/v3/messages/"+id, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer key.secret")
	r.Header.Set("Content-Type", "application/json-patch+json")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func replyBody(parent string) string {
	return strings.TrimSuffix(v3Message, "}") + `,"replyTo":"` + parent + `"}`
}
