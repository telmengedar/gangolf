package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
)

type mediaFixture struct {
	mu      sync.Mutex
	catalog node
	root    node
	archive map[int64]node
	writes  int
	queries []url.Values
	fail    string
	owner   int64
}

func newMediaFixture(t *testing.T) (*mediaFixture, *resourceFixture, *server) {
	t.Helper()
	auth, s := newResourceFixture(t)
	auth.legacy.cfg.Users["7"] = grant{Enabled: true, Permissions: []string{"host"}}
	k := auth.legacy.cfg.APIKeys["key"]
	k.Permissions = []string{"host"}
	auth.legacy.cfg.APIKeys["key"] = k
	edge := []relation{{Source: 900, Target: 901, Type: "None"}}
	f := &mediaFixture{root: node{ID: 900, Name: "Gangolf Media", Type: "project", Owner: 4, Access: json.RawMessage("0"), Links: edge}, catalog: node{ID: 901, Name: "catalog", Type: "gangolf-media-catalog", Root: 900, Owner: 4, Access: json.RawMessage("0"), SkipEmbedding: new(true), Links: edge}, archive: map[int64]node{}, owner: 4}
	host := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(host.Close)
	base, _ := url.Parse(host.URL + "/api")
	s.media = &mediaStore{graph: &graph{base: base, http: host.Client(), token: "media-test-token", root: 900}, catalogID: 901, tenant: "7", authID: "12"}
	c := mediaCatalog{SchemaVersion: 1, AuthConfigID: "12", TenantUserID: "7", NextClipID: 102, NextTriggerID: 125, Clips: []mediaClip{}, Triggers: []mediaTrigger{}, Origin: mediaOrigin{ArchiveRootID: "145", Snapshot: strings.Repeat("a", 64), VideoCount: 90, TriggerCount: 122}}
	f.set(c)
	return f, auth, s
}
func (f *mediaFixture) set(c mediaCatalog) {
	data, _ := json.Marshal(c)
	body := string(data)
	f.catalog.Content = &body
	f.catalog.ContentType = "application/json"
}
func (f *mediaFixture) state(t *testing.T) mediaCatalog {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	var c mediaCatalog
	if json.Unmarshal([]byte(*f.catalog.Content), &c) != nil {
		t.Fatal("invalid fixture catalog")
	}
	return c
}
func (f *mediaFixture) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer media-test-token" {
		w.WriteHeader(401)
		return
	}
	if f.fail == "all" {
		w.WriteHeader(503)
		return
	}
	if r.URL.Path == "/api/users/me" {
		respond(w, 200, map[string]any{"id": f.owner, "enabled": true, "permissions": []string{"write"}})
		return
	}
	if r.URL.Path == "/api/nodes/901/content" && r.Method == "POST" {
		f.writes++
		body, _ := io.ReadAll(r.Body)
		if f.fail != "reject-write" {
			value := string(body)
			f.catalog.Content = &value
			f.catalog.ContentType = "application/json"
		}
		if f.fail == "reject-write" || f.fail == "committed-write" {
			w.WriteHeader(503)
			return
		}
		w.WriteHeader(204)
		return
	}
	if r.URL.Path == "/api/nodes/900" {
		respond(w, 200, f.root)
		return
	}
	if r.URL.Path == "/api/nodes/901" {
		respond(w, 200, f.catalog)
		return
	}
	if r.URL.Path == "/api/nodes" && r.URL.Query().Has("id") {
		id := mustInt(r.URL.Query().Get("id"))
		var n node
		found := true
		switch id {
		case 900:
			n = f.root
		case 901:
			n = f.catalog
		default:
			n, found = f.archive[id]
		}
		rows := []node{}
		if found {
			rows = append(rows, n)
		}
		respond(w, 200, graphPage{Result: rows, Total: len(rows)})
		return
	}
	if r.URL.Path == "/api/nodes" {
		q := r.URL.Query()
		f.queries = append(f.queries, q)
		var rows []node
		for _, n := range f.archive {
			if strconv.FormatInt(n.Root, 10) == q.Get("rootNodeId") && n.Type == q.Get("type") && plainLinkBetween(n.Links, n.ID, mustInt(q.Get("linkedto"))) {
				rows = append(rows, n)
			}
		}
		sortMediaNodes(rows)
		offset, _ := strconv.Atoi(q.Get("continue"))
		end := min(offset+37, len(rows))
		var next *int
		if end < len(rows) {
			next = &end
		}
		respond(w, 200, graphPage{Result: rows[offset:end], Total: len(rows), Continue: next})
		return
	}
	id, _ := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/api/nodes/"), 10, 64)
	if n, ok := f.archive[id]; ok {
		respond(w, 200, n)
		return
	}
	w.WriteHeader(404)
}
func mustInt(s string) int64 { n, _ := strconv.ParseInt(s, 10, 64); return n }
func sortMediaNodes(rows []node) {
	for i := range rows {
		for j := i + 1; j < len(rows); j++ {
			if rows[j].ID < rows[i].ID {
				rows[i], rows[j] = rows[j], rows[i]
			}
		}
	}
}
func mediaRequest(s *server, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer key.secret")
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}
func mediaWant(t *testing.T, s *server, method, path, body string, status int) *httptest.ResponseRecorder {
	t.Helper()
	w := mediaRequest(s, method, path, body)
	if w.Code != status {
		t.Fatalf("%s %s status=%d expected=%d", method, path, w.Code, status)
	}
	return w
}

const clipBody = `{"provider":"youtube","remoteVideoId":"abcdefghijk"}`

func TestMediaAuthorizationBinding(t *testing.T) {
	f, auth, s := newMediaFixture(t)
	mediaWant(t, s, "GET", "/api/v3/clips", "", 200)
	for _, mode := range []string{"foreign-host", "scoped-key", "admin", "wrong-auth", "wrong-tenant", "owner", "root", "public", "type", "disabled-media"} {
		t.Run(mode, func(t *testing.T) {
			f, a, s := newMediaFixture(t)
			status := 503
			switch mode {
			case "foreign-host":
				s.media.tenant = "8"
				status = 403
			case "scoped-key":
				key := a.legacy.cfg.APIKeys["key"]
				key.Permissions = []string{"read"}
				a.legacy.cfg.APIKeys["key"] = key
				status = 403
			case "admin":
				key := a.legacy.cfg.APIKeys["key"]
				key.Permissions = []string{"admin"}
				a.legacy.cfg.APIKeys["key"] = key
				status = 200
			case "wrong-auth":
				s.media.authID = "99"
			case "wrong-tenant":
				c := f.state(t)
				c.TenantUserID = "8"
				f.set(c)
			case "owner":
				f.owner = 3
			case "root":
				f.catalog.Root = 145
			case "public":
				f.catalog.Access = json.RawMessage("2")
			case "type":
				f.catalog.Type = "legacy-archive-video"
			case "disabled-media":
				s.media = nil
			}
			mediaWant(t, s, "GET", "/api/v3/clips", "", status)
			if f.writes != 0 || len(f.queries) != 0 {
				t.Fatal("authorization/binding touched archive")
			}
		})
	}
	for _, header := range []string{"", "Basic key.secret", "Bearer bad"} {
		r := httptest.NewRequest("GET", "/api/v3/clips?apiKey=key.secret", nil)
		if header != "" {
			r.Header.Set("Authorization", header)
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatal("query/header credential accepted")
		}
	}
	if f.writes != 0 || auth.writes != 0 {
		t.Fatal("read authorization wrote")
	}
}
func TestMediaClipCRUD(t *testing.T) {
	f, _, s := newMediaFixture(t)
	w := mediaWant(t, s, "POST", "/api/v3/clips", clipBody, 201)
	if w.Header().Get("Location") != "/api/v3/clips/102" {
		t.Fatal("location")
	}
	var c mediaClip
	json.Unmarshal(w.Body.Bytes(), &c)
	if c.ID != "102" || c.StartSeconds != nil || c.Title != nil {
		t.Fatal("created projection")
	}
	mediaWant(t, s, "GET", "/api/v3/clips/102", "", 200)
	updated := `{"provider":"youtube","remoteVideoId":"0123456789_","startSeconds":1,"endSeconds":2,"title":"new"}`
	mediaWant(t, s, "PUT", "/api/v3/clips/102", updated, 200)
	mediaWant(t, s, "PUT", "/api/v3/clips/102", clipBody, 200)
	s.media = &mediaStore{graph: s.media.graph, catalogID: 901, tenant: "7", authID: "12"}
	mediaWant(t, s, "GET", "/api/v3/clips/102", "", 200)
	mediaWant(t, s, "DELETE", "/api/v3/clips/102", "", 204)
	mediaWant(t, s, "DELETE", "/api/v3/clips/102", "", 404)
	mediaWant(t, s, "PUT", "/api/v3/clips/102", clipBody, 404)
	mediaWant(t, s, "POST", "/api/v3/clips", clipBody, 201)
	if f.state(t).Clips[0].ID != "103" {
		t.Fatal("ID reused")
	}
	for _, body := range []string{`{}`, `null`, `{"provider":null,"remoteVideoId":"abcdefghijk"}`, `{"provider":"Youtube","remoteVideoId":"abcdefghijk"}`, `{"provider":"youtube","remoteVideoId":"https://example.test"}`, `{"provider":"youtube","provider":"youtube","remoteVideoId":"abcdefghijk"}`, `{"Provider":"youtube","provider":"youtube","remoteVideoId":"abcdefghijk"}`, `{"provider":"youtube","remoteVideoId":"abcdefghijk","startSeconds":-1}`, `{"provider":"youtube","remoteVideoId":"abcdefghijk","endSeconds":0}`, `{"provider":"youtube","remoteVideoId":"abcdefghijk","endSeconds":1,"startSeconds":1}`, `{"provider":"youtube","remoteVideoId":"abcdefghijk","startSeconds":1e999}`, `{"provider":"youtube","remoteVideoId":"abcdefghijk","mode":"regex"}`, `{"provider":"youtube","remoteVideoId":"abcdefghijk","id":"1"}`, `{"provider":"youtube","remoteVideoId":"abcdefghijk","legacy":null}`} {
		before := f.writes
		mediaWant(t, s, "POST", "/api/v3/clips", body, 400)
		if f.writes != before {
			t.Fatal("invalid input wrote")
		}
	}
	for _, id := range []string{"0", "01", "-1", "x", "9223372036854775808"} {
		mediaWant(t, s, "GET", "/api/v3/clips/"+id, "", 400)
	}
	r := httptest.NewRequest("POST", "/api/v3/clips", strings.NewReader(clipBody))
	r.Header.Set("Authorization", "Bearer key.secret")
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 415 {
		t.Fatal("missing content type")
	}
	mediaWant(t, s, "POST", "/api/v3/clips", strings.Repeat("x", (1<<20)+1), 413)
	mediaWant(t, s, "PATCH", "/api/v3/clips/103", "[]", 405)
}
func TestMediaTriggerCRUDAndDanglingRepair(t *testing.T) {
	f, _, s := newMediaFixture(t)
	seedSyntheticArchive(t, f)
	f.catalog.Content = nil
	if _, err := s.seedMedia(context.Background(), 145, 151, 149, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	c := f.state(t)
	nulls := 0
	var dangling mediaTrigger
	for _, v := range c.Triggers {
		if v.ClipID == nil {
			nulls++
			dangling = v
		}
	}
	if len(c.Triggers) != 122 || nulls != 15 {
		t.Fatal("seed relation counts")
	}
	body := fmt.Sprintf(`{"text":" Literal * Text ","clipId":%q}`, c.Clips[0].ID)
	mediaWant(t, s, "PUT", "/api/v3/triggers/"+dangling.ID, body, 200)
	after := f.state(t)
	var repaired mediaTrigger
	for _, v := range after.Triggers {
		if v.ID == dangling.ID {
			repaired = v
		}
	}
	if repaired.ClipID == nil || repaired.Text != " Literal * Text " || !reflect.DeepEqual(repaired.Legacy, dangling.Legacy) {
		t.Fatal("repair changed evidence")
	}
	mediaWant(t, s, "POST", "/api/v3/triggers", body, 201)
	mediaWant(t, s, "POST", "/api/v3/triggers", body, 201)
	mediaWant(t, s, "POST", "/api/v3/triggers", `{"text":"x","clipId":"999999"}`, 404)
	for _, bad := range []string{`{"text":"x"}`, `{"text":"x","clipId":null}`, `{"text":" ","clipId":"1"}`, `{"text":"x","clipId":1}`, `{"text":"x","clipId":"01"}`, `{"text":"x","clipId":"1","options":{}}`} {
		mediaWant(t, s, "POST", "/api/v3/triggers", bad, 400)
	}
	mediaWant(t, s, "DELETE", "/api/v3/triggers/"+dangling.ID, "", 204)
	if len(f.state(t).Clips) != 90 {
		t.Fatal("trigger delete changed clips")
	}
}
func TestMediaClipDeleteRestrictsReferences(t *testing.T) {
	f, _, s := newMediaFixture(t)
	mediaWant(t, s, "POST", "/api/v3/clips", clipBody, 201)
	mediaWant(t, s, "POST", "/api/v3/triggers", `{"text":"x","clipId":"102"}`, 201)
	before := f.writes
	mediaWant(t, s, "DELETE", "/api/v3/clips/102", "", 409)
	if f.writes != before {
		t.Fatal("restricted delete wrote")
	}
	mediaWant(t, s, "DELETE", "/api/v3/triggers/125", "", 204)
	var wg sync.WaitGroup
	codes := make(chan int, 2)
	wg.Add(2)
	go func() { defer wg.Done(); codes <- mediaRequest(s, "DELETE", "/api/v3/clips/102", "").Code }()
	go func() {
		defer wg.Done()
		codes <- mediaRequest(s, "POST", "/api/v3/triggers", `{"text":"race","clipId":"102"}`).Code
	}()
	wg.Wait()
	close(codes)
	pair := map[int]bool{}
	for code := range codes {
		pair[code] = true
	}
	if !(pair[201] && pair[409] || pair[204] && pair[404]) {
		t.Fatal("nonserialized relationship result")
	}
	if !f.state(t).valid("12", "7") {
		t.Fatal("invalid relationship after race")
	}
}
func TestMediaPaginationAndFilters(t *testing.T) {
	_, _, s := newMediaFixture(t)
	for range 3 {
		mediaWant(t, s, "POST", "/api/v3/clips", clipBody, 201)
	}
	w := mediaWant(t, s, "GET", "/api/v3/clips?count=1&sort=id&descending=true&provider=youtube&remoteVideoId=abcdefghijk", "", 200)
	var page struct {
		Result   []mediaClip
		Total    int
		Continue *int
	}
	json.Unmarshal(w.Body.Bytes(), &page)
	if page.Total != 3 || len(page.Result) != 1 || page.Result[0].ID != "104" || page.Continue == nil || *page.Continue != 1 {
		t.Fatal("page sort/filter/count")
	}
	w = mediaWant(t, s, "GET", "/api/v3/clips?continue=1000", "", 200)
	json.Unmarshal(w.Body.Bytes(), &page)
	if len(page.Result) != 0 || page.Continue != nil {
		t.Fatal("empty page")
	}
	for _, q := range []string{"count=0", "count=501", "continue=-1", "count=1&count=2", "sort=title", "descending=1", "wat=1", "clipId=1", "unresolved=true", "apiKey=x", "count=%zz"} {
		mediaWant(t, s, "GET", "/api/v3/clips?"+q, "", 400)
	}
	mediaWant(t, s, "POST", "/api/v3/triggers", `{"text":"Case","clipId":"102"}`, 201)
	for _, q := range []string{"text=Case&clipId=102&unresolved=false", "text=case", "text=", "unresolved=true"} {
		w = mediaWant(t, s, "GET", "/api/v3/triggers?"+q, "", 200)
		var p struct{ Total int }
		json.Unmarshal(w.Body.Bytes(), &p)
		expected := 0
		if strings.HasPrefix(q, "text=Case") {
			expected = 1
		}
		if p.Total != expected {
			t.Fatal("literal/conjunctive filters")
		}
	}
}
func seedSyntheticArchive(t *testing.T, f *mediaFixture) {
	t.Helper()
	snapshot := strings.Repeat("a", 64)
	name := "legacy-archive:" + snapshot
	f.archive[145] = node{ID: 145, Name: name, Type: "project", Owner: 4, Access: json.RawMessage("0"), SkipEmbedding: new(true), Links: []relation{{Source: 145, Target: 151, Type: "None"}, {Source: 145, Target: 149, Type: "None"}}}
	for id, kind := range map[int64]string{151: "video", 149: "trigger"} {
		f.archive[id] = node{ID: id, Name: name + ":group:legacy-archive-" + kind, Type: "legacy-archive-group", Root: 145, Owner: 4, Access: json.RawMessage("0"), SkipEmbedding: new(true), Links: []relation{{Source: 145, Target: id, Type: "None"}}}
	}
	add := func(id int64, kind string, group int64, raw any, interpretation, resolution string) {
		data, _ := json.Marshal(raw)
		canonical, _ := archiveCanonical(data)
		sum := sha256.Sum256(canonical)
		digest := hex.EncodeToString(sum[:])
		key := kind + "s:" + digest + ":1"
		p := mediaArchiveEnvelope{SchemaVersion: 1, Snapshot: snapshot, Key: key, Raw: data, RecordSHA256: digest, ProviderInterpretation: interpretation, VideoResolution: resolution}
		body, _ := json.Marshal(p)
		text := string(body)
		f.archive[id] = node{ID: id, Name: name + ":" + key, Type: "legacy-archive-" + kind, Root: 145, Owner: 4, Access: json.RawMessage("0"), SkipEmbedding: new(true), Content: &text, ContentType: "application/json", Links: []relation{{Source: id, Target: group, Type: "None"}}}
	}
	for i := int64(1); i <= 90; i++ {
		add(1000+i, "video", 151, map[string]any{"id": i, "source": "Youtube", "videoId": "abcdefghijk"}, "Youtube", "")
	}
	for i := int64(1); i <= 122; i++ {
		target := (i-1)%90 + 1
		resolution := "resolved-numeric-FK"
		if i > 107 {
			target = 999
			resolution = "unresolved"
		}
		add(2000+i, "trigger", 149, map[string]any{"id": i, "text": "same", "videoId": target}, "", resolution)
		if i <= 107 {
			edge := relation{Source: 2000 + i, Target: 1000 + target, Type: "Unidirectional", Context: "references-video"}
			n := f.archive[edge.Source]
			n.Links = append(n.Links, edge)
			f.archive[n.ID] = n
			n = f.archive[edge.Target]
			n.Links = append(n.Links, edge)
			f.archive[n.ID] = n
		}
	}
}
func TestMediaSeedSnapshotAndIdempotence(t *testing.T) {
	f, _, s := newMediaFixture(t)
	seedSyntheticArchive(t, f)
	f.catalog.Content = nil
	f.fail = "committed-write"
	result, err := s.seedMedia(context.Background(), 145, 151, 149, strings.Repeat("a", 64))
	if err != nil || result != "initialized" {
		t.Fatal("seed unknown committed write not reconciled")
	}
	c := f.state(t)
	resolved := 0
	for _, v := range c.Triggers {
		if v.ClipID != nil {
			resolved++
		}
	}
	if len(c.Clips) != 90 || len(c.Triggers) != 122 || resolved != 107 || c.NextClipID != 91 || c.NextTriggerID != 123 {
		t.Fatal("seed projection/counters")
	}
	if c.Clips[0].StartSeconds != nil || c.Clips[0].Legacy == nil {
		t.Fatal("seed absent timing/provenance")
	}
	f.fail = ""
	mediaWant(t, s, "PUT", "/api/v3/clips/1", clipBody, 200)
	mediaWant(t, s, "DELETE", "/api/v3/triggers/122", "", 204)
	writes := f.writes
	result, err = s.seedMedia(context.Background(), 145, 151, 149, strings.Repeat("a", 64))
	if err != nil || result != "already_initialized" || f.writes != writes || len(f.state(t).Triggers) != 121 {
		t.Fatal("seed resurrected edited/deleted state")
	}
	if _, err = s.seedMedia(context.Background(), 145, 151, 149, strings.Repeat("b", 64)); err == nil || f.writes != writes {
		t.Fatal("different origin overwritten")
	}
	t.Run("corrupt-source", func(t *testing.T) {
		f, _, s := newMediaFixture(t)
		seedSyntheticArchive(t, f)
		f.catalog.Content = nil
		n := f.archive[1001]
		var p mediaArchiveEnvelope
		json.Unmarshal([]byte(*n.Content), &p)
		p.RecordSHA256 = strings.Repeat("0", 64)
		data, _ := json.Marshal(p)
		body := string(data)
		n.Content = &body
		f.archive[n.ID] = n
		if _, err := s.seedMedia(context.Background(), 145, 151, 149, strings.Repeat("a", 64)); err == nil || f.writes != 0 {
			t.Fatal("corrupt raw hash seeded")
		}
	})
	t.Run("uncommitted-write", func(t *testing.T) {
		f, _, s := newMediaFixture(t)
		seedSyntheticArchive(t, f)
		f.catalog.Content = nil
		f.fail = "reject-write"
		if _, err := s.seedMedia(context.Background(), 145, 151, 149, strings.Repeat("a", 64)); err != errOutcomeUnknown || f.writes != 1 || f.catalog.Content != nil {
			t.Fatal("seed ambiguous write retried")
		}
	})
}
func TestMediaArchiveIsolation(t *testing.T) {
	f, _, s := newMediaFixture(t)
	seedSyntheticArchive(t, f)
	before, _ := json.Marshal(f.archive)
	f.catalog.Content = nil
	if _, err := s.seedMedia(context.Background(), 145, 151, 149, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	if len(f.queries) < 2 {
		t.Fatal("seed pagination not exercised")
	}
	for _, q := range f.queries {
		if q.Get("rootNodeId") != "145" || !(q.Get("type") == "legacy-archive-video" && q.Get("linkedto") == "151" || q.Get("type") == "legacy-archive-trigger" && q.Get("linkedto") == "149") {
			t.Fatal("seed queried outside media groups")
		}
	}
	queries := len(f.queries)
	mediaWant(t, s, "GET", "/api/v3/clips", "", 200)
	mediaWant(t, s, "POST", "/api/v3/clips", clipBody, 201)
	mediaWant(t, s, "DELETE", "/api/v3/clips/91", "", 204)
	after, _ := json.Marshal(f.archive)
	if string(before) != string(after) || len(f.queries) != queries {
		t.Fatal("normal CRUD touched immutable archive")
	}
}
func TestMediaCatalogIntegrityAndUnknownWrites(t *testing.T) {
	for _, mode := range []string{"duplicate", "counter", "broken-ref", "null-ref", "wrong-origin", "uninitialized", "extra-edge", "duplicate-json"} {
		t.Run(mode, func(t *testing.T) {
			f, _, s := newMediaFixture(t)
			c := f.state(t)
			switch mode {
			case "duplicate":
				c.Clips = []mediaClip{{ID: "1", Provider: "youtube", RemoteVideoID: "abcdefghijk"}, {ID: "1", Provider: "youtube", RemoteVideoID: "abcdefghijk"}}
			case "counter":
				c.NextClipID = 0
			case "broken-ref":
				c.Triggers = []mediaTrigger{{ID: "1", Text: "x", ClipID: new("2")}}
			case "null-ref":
				c.Triggers = []mediaTrigger{{ID: "1", Text: "x"}}
			case "wrong-origin":
				c.Origin.VideoCount = 89
			case "extra-edge":
				f.catalog.Links = append(f.catalog.Links, relation{Source: 901, Target: 145, Type: "None"})
			}
			f.set(c)
			if mode == "uninitialized" {
				f.catalog.Content = nil
			}
			if mode == "duplicate-json" {
				data := strings.Replace(*f.catalog.Content, `"schemaVersion":1`, `"schemaVersion":1,"schemaVersion":1`, 1)
				f.catalog.Content = &data
			}
			mediaWant(t, s, "GET", "/api/v3/clips", "", 503)
			if f.writes != 0 {
				t.Fatal("invalid storage repaired implicitly")
			}
		})
	}
	for _, mode := range []string{"committed-write", "reject-write"} {
		t.Run(mode, func(t *testing.T) {
			f, _, s := newMediaFixture(t)
			f.fail = mode
			expected := 503
			if mode == "committed-write" {
				expected = 201
			}
			w := mediaWant(t, s, "POST", "/api/v3/clips", clipBody, expected)
			if f.writes != 1 {
				t.Fatal("write replayed")
			}
			if expected == 503 && !strings.Contains(w.Body.String(), "write_outcome_unknown") {
				t.Fatal("ambiguous status code")
			}
		})
	}
	f, _, s := newMediaFixture(t)
	c := f.state(t)
	for i := int64(1); i < 30000; i++ {
		c.Clips = append(c.Clips, mediaClip{ID: strconv.FormatInt(i, 10), Provider: "youtube", RemoteVideoID: "abcdefghijk", Title: new(strings.Repeat("x", 256))})
	}
	c.NextClipID = 30001
	if err := s.media.save(context.Background(), c); err != errMediaCapacity || f.writes != 0 {
		t.Fatal("catalog capacity wrote")
	}
}
func TestMediaRealArchiveProjection(t *testing.T) {
	path := os.Getenv("GANGOLF_MEDIA_TEST_ARCHIVE_FIXTURE")
	if path == "" {
		t.Skip("protected operator fixture not supplied")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("protected fixture unreadable")
	}
	var fixture struct {
		Nodes []node `json:"nodes"`
	}
	if json.Unmarshal(data, &fixture) != nil {
		t.Fatal("fixture shape")
	}
	videos, triggers := []node{}, []node{}
	snapshot := ""
	for _, n := range fixture.Nodes {
		var p mediaArchiveEnvelope
		if n.Content == nil || json.Unmarshal([]byte(*n.Content), &p) != nil {
			t.Fatal("fixture envelope")
		}
		snapshot = p.Snapshot
		if n.Type == "legacy-archive-video" {
			videos = append(videos, n)
		} else if n.Type == "legacy-archive-trigger" {
			triggers = append(triggers, n)
		} else {
			t.Fatal("fixture contains unrelated type")
		}
	}
	c, err := projectMediaArchive(videos, triggers, 145, 151, 149, snapshot, "44", "42")
	if err != nil {
		t.Fatal("real archive projector refused fixture")
	}
	resolved := 0
	for _, v := range c.Triggers {
		if v.ClipID != nil {
			resolved++
		}
	}
	if len(c.Clips) != 90 || len(c.Triggers) != 122 || resolved != 107 || c.NextClipID != 102 || c.NextTriggerID != 125 {
		t.Fatal("real archive projection aggregates")
	}
	t.Logf("verified archive projection: clips=%d triggers=%d resolved=%d unresolved=%d", len(c.Clips), len(c.Triggers), resolved, len(c.Triggers)-resolved)
}
func TestMediaConfiguration(t *testing.T) {
	_, _, s := newMediaFixture(t)
	names := []string{"GANGOLF_MEDIA_DIVOID_TOKEN_FILE", "GANGOLF_MEDIA_ROOT_ID", "GANGOLF_MEDIA_CATALOG_ID", "GANGOLF_MEDIA_TENANT_USER_ID"}
	for _, name := range names {
		t.Setenv(name, "")
	}
	if m, err := configureMedia(s.graph); err != nil || m != nil {
		t.Fatal("absent media changed bootstrap")
	}
	t.Setenv(names[1], "900")
	if _, err := configureMedia(s.graph); err == nil {
		t.Fatal("partial media configured")
	}
	path := filepath.Join(t.TempDir(), "media-key")
	if os.WriteFile(path, []byte("User=synthetic\nUserId=4\nKey=media-test-token\n"), 0600) != nil {
		t.Fatal("credential fixture")
	}
	values := []string{path, "900", "901", "7"}
	for i, name := range names {
		t.Setenv(name, values[i])
	}
	m, err := configureMedia(s.graph)
	if err != nil || m == nil || m.graph.token != "media-test-token" || m.graph.base != s.graph.base || m.graph.http != s.graph.http || s.graph.token != "storage-only-test-token" {
		t.Fatal("dedicated credential changed primary client")
	}
	for _, mode := range []string{"duplicate", "foreign-principal", "missing", "unsafe-mode", "symlink"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "credential")
			data := "User=test\nUserId=4\nKey=synthetic\n"
			switch mode {
			case "duplicate":
				data += "Key=second\n"
			case "foreign-principal":
				data = strings.Replace(data, "UserId=4", "UserId=3", 1)
			case "missing":
				data = "Key=synthetic\n"
			}
			if os.WriteFile(path, []byte(data), 0600) != nil {
				t.Fatal("credential fixture")
			}
			if mode == "unsafe-mode" {
				os.Chmod(path, 0644)
			}
			if mode == "symlink" {
				link := path + "-link"
				if os.Symlink(path, link) != nil {
					t.Fatal("symlink fixture")
				}
				path = link
			}
			if _, err := mediaCredential(path); err == nil {
				t.Fatal("unsafe credential accepted")
			}
		})
	}
}
func TestMediaSeedCLIHelper(t *testing.T) {
	if os.Getenv("GANGOLF_TEST_MEDIA_SEED_PROCESS") != "1" {
		return
	}
	os.Args = []string{"gangolf-api", "seed-media", "-archive-root", "145", "-videos-group", "151", "-triggers-group", "149", "-snapshot", strings.Repeat("a", 64)}
	if err := run(); err != nil {
		t.Fatal("seed process failed")
	}
}
func TestMediaSeedCLI(t *testing.T) {
	f, auth, s := newMediaFixture(t)
	seedSyntheticArchive(t, f)
	f.catalog.Content = nil
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer media-test-token" {
			f.serve(w, r)
		} else {
			auth.serve(w, r)
		}
	}))
	defer host.Close()
	path := filepath.Join(t.TempDir(), "media-key")
	if os.WriteFile(path, []byte("User=test\nUserId=4\nKey=media-test-token\n"), 0600) != nil {
		t.Fatal("credential fixture")
	}
	setBootstrap(t, auth.legacy)
	t.Setenv("GANGOLF_DIVOID_API_URL", host.URL+"/api")
	t.Setenv("GANGOLF_MEDIA_DIVOID_TOKEN_FILE", path)
	t.Setenv("GANGOLF_MEDIA_ROOT_ID", "900")
	t.Setenv("GANGOLF_MEDIA_CATALOG_ID", "901")
	t.Setenv("GANGOLF_MEDIA_TENANT_USER_ID", "7")
	binary, err := os.Executable()
	if err != nil {
		t.Fatal("test binary")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*1e9)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "-test.run=^TestMediaSeedCLIHelper$")
	cmd.Env = append(os.Environ(), "GANGOLF_TEST_MEDIA_SEED_PROCESS=1")
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "initialized") {
		t.Fatal("seed CLI did not exit with aggregate receipt")
	}
	if f.writes != 1 || len(f.state(t).Clips) != 90 {
		t.Fatal("CLI did not initialize catalog")
	}
	mediaWant(t, s, "GET", "/api/v3/clips", "", 200)
}
func TestMediaConfiguredReadiness(t *testing.T) {
	for _, mode := range []string{"healthy", "empty", "unavailable", "wrong-auth", "wrong-tenant", "disabled", "primary-failure"} {
		t.Run(mode, func(t *testing.T) {
			f, auth, s := newMediaFixture(t)
			expected := 503
			switch mode {
			case "healthy":
				expected = 200
			case "empty":
				f.catalog.Content = nil
			case "unavailable":
				f.fail = "all"
			case "wrong-auth":
				c := f.state(t)
				c.AuthConfigID = "99"
				f.set(c)
			case "wrong-tenant":
				c := f.state(t)
				c.TenantUserID = "99"
				f.set(c)
			case "disabled":
				s.media = nil
				f.fail = "all"
				expected = 200
			case "primary-failure":
				auth.fail = "identity"
			}
			w := mediaWant(t, s, "GET", "/api/i1/readiness", "", expected)
			if expected == 200 && !strings.Contains(w.Body.String(), `"ready"`) || expected == 503 && !strings.Contains(w.Body.String(), `"unready"`) {
				t.Fatal("readiness representation")
			}
			mediaWant(t, s, "GET", "/api/i1/health", "", 200)
			if f.writes != 0 || len(f.queries) != 0 {
				t.Fatal("readiness mutated or queried archive")
			}
			if mode != "primary-failure" {
				mediaWant(t, s, "GET", "/api/v3/messages", "", 200)
			}
		})
	}
}
