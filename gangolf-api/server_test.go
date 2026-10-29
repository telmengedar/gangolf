package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

type graphFixture struct {
	mu              sync.Mutex
	cfg             authConfig
	nodes           []node
	writes, uploads int
	fail            string
	corruptConfig   bool
	badScope        bool
	authHeaders     []string
	jwks            []byte
	jwksAuth        string
	host            *httptest.Server
}

func newFixture(t *testing.T) (*graphFixture, *server) {
	t.Helper()
	f := &graphFixture{}
	f.mu.Lock()
	f.host = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.host.Close)
	h := hmac.New(sha256.New, []byte("test-pepper"))
	h.Write([]byte("key.secret"))
	f.cfg = authConfig{SchemaVersion: 1, Issuer: "test-issuer", Audience: "test-audience", UserIDClaim: "userId", JWKSURL: f.host.URL + "/jwks", Users: map[string]grant{"7": {Enabled: true, Permissions: []string{"admin"}}}, APIKeys: map[string]keyGrant{"key": {UserID: "7", Hash: h.Sum(nil), Enabled: true, Permissions: []string{"host"}}}}
	f.mu.Unlock()
	u, err := url.Parse(f.host.URL + "/api")
	if err != nil {
		t.Fatal(err)
	}
	s := &server{graph: &graph{base: u, token: "storage-only-test-token", root: 10, group: 11, config: 12, http: &http.Client{Timeout: time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, pepper: "test-pepper", now: func() time.Time { return time.Date(2026, 10, 30, 1, 2, 3, 456, time.UTC) }}
	return f, s
}

func (f *graphFixture) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path == "/jwks" {
		f.jwksAuth = r.Header.Get("Authorization")
		if f.fail == "jwks" {
			w.WriteHeader(503)
			return
		}
		w.Write(f.jwks)
		return
	}
	f.authHeaders = append(f.authHeaders, r.Header.Get("Authorization"))
	if r.Header.Get("Authorization") != "Bearer storage-only-test-token" {
		w.WriteHeader(401)
		return
	}
	if f.fail == "all" {
		w.WriteHeader(503)
		return
	}
	if r.URL.Path == "/api/nodes/12" {
		respond(w, 200, node{ID: 12, Root: 10, Access: intPointer(0)})
		return
	}
	if r.URL.Path == "/api/nodes/12/content" {
		if f.corruptConfig {
			w.Write([]byte(`{}`))
			return
		}
		respond(w, 200, f.cfg)
		return
	}
	if r.URL.Path == "/api/nodes" && r.Method == "POST" {
		f.writes++
		if f.fail == "create" {
			w.WriteHeader(503)
			return
		}
		var metadata struct {
			Type, Name    string
			RootNodeID    int64
			Access        *int
			SkipEmbedding bool
		}
		if json.NewDecoder(r.Body).Decode(&metadata) != nil || metadata.Type != batchType || metadata.Name != "message batch" || metadata.RootNodeID != 10 || metadata.Access == nil || *metadata.Access != 0 || !metadata.SkipEmbedding {
			w.WriteHeader(400)
			return
		}
		n := node{ID: int64(100 + len(f.nodes)), Type: batchType, Root: 10, Access: intPointer(0)}
		f.nodes = append(f.nodes, n)
		respond(w, 200, n)
		return
	}
	if r.URL.Path == "/api/nodes" && r.Method == "GET" {
		q := r.URL.Query()
		if q.Get("rootNodeId") != "10" || q.Get("linkedto") != "11" || q.Get("type") != batchType || q.Get("sort") != "id" || q.Get("descending") != "false" || !strings.Contains(q.Get("fields"), "content") {
			w.WriteHeader(400)
			return
		}
		if f.fail == "list" {
			w.WriteHeader(503)
			return
		}
		offset, _ := strconv.Atoi(q.Get("continue"))
		end := min(offset+500, len(f.nodes))
		if offset > len(f.nodes) {
			w.WriteHeader(400)
			return
		}
		rows := append([]node{}, f.nodes[offset:end]...)
		if f.badScope && len(rows) > 0 {
			rows[0].Root = 999
		}
		var next *int
		if end < len(f.nodes) {
			next = &end
		}
		respond(w, 200, graphPage{Result: rows, Total: len(f.nodes), Continue: next})
		return
	}
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) == 5 {
		id, _ := strconv.ParseInt(parts[3], 10, 64)
		index := int(id) - 100
		if index < 0 || index >= len(f.nodes) {
			w.WriteHeader(404)
			return
		}
		switch parts[4] {
		case "links":
			f.writes++
			if f.fail == "link" {
				w.WriteHeader(503)
				return
			}
			var target int64
			if json.NewDecoder(r.Body).Decode(&target) != nil || target != 11 {
				w.WriteHeader(400)
				return
			}
			w.WriteHeader(200)
			return
		case "content":
			f.writes++
			f.uploads++
			if f.fail == "upload" {
				w.WriteHeader(503)
				return
			}
			data, err := io.ReadAll(r.Body)
			if err != nil {
				w.WriteHeader(400)
				return
			}
			content := string(data)
			f.nodes[index].Content = &content
			f.nodes[index].ContentType = "application/json"
			if f.fail == "committed" {
				w.WriteHeader(503)
				return
			}
			w.WriteHeader(200)
			return
		}
	}
	w.WriteHeader(404)
}

const testMessage = `{"service":"localhost","channel":"host","user":{"service":"microphone","serviceId":"speaker-1","name":"Distinct Speaker"},"chunks":[{"type":0,"data":"!ordinary text","color":"blue","weight":"bold"},{"type":"Emote","code":":wave:","data":"image-url","width":24,"height":25}],"tts":true,"time":"2001-01-01T00:00:00Z"}`

func request(t *testing.T, s *server, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	return w
}

func status(t *testing.T, w *httptest.ResponseRecorder, want int) {
	t.Helper()
	if w.Code != want {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, want, w.Body.String())
	}
}

func TestMessagesRejectMissingAndInvalidCredentialsBeforeStorage(t *testing.T) {
	for _, token := range []string{"", "bad", "key.wrong", "unknown.secret"} {
		t.Run(token, func(t *testing.T) {
			f, s := newFixture(t)
			w := request(t, s, "POST", "/api/v2/messages", token, "["+testMessage+"]")
			status(t, w, 401)
			if f.writes != 0 {
				t.Fatalf("writes=%d", f.writes)
			}
			if strings.Contains(w.Body.String(), "secret") || strings.Contains(w.Body.String(), "storage-only-test-token") {
				t.Fatal("credential leaked")
			}
		})
	}
}

func TestAPIKeysEnforceHashExpiryEnabledAndScopedPermissions(t *testing.T) {
	tests := []struct {
		name   string
		change func(*graphFixture)
		want   int
	}{
		{"host", func(*graphFixture) {}, 201},
		{"admin", func(f *graphFixture) {
			k := f.cfg.APIKeys["key"]
			k.Permissions = []string{"admin"}
			f.cfg.APIKeys["key"] = k
		}, 201},
		{"scope-not-owner", func(f *graphFixture) { k := f.cfg.APIKeys["key"]; k.Permissions = nil; f.cfg.APIKeys["key"] = k }, 403},
		{"disabled-key", func(f *graphFixture) { k := f.cfg.APIKeys["key"]; k.Enabled = false; f.cfg.APIKeys["key"] = k }, 401},
		{"disabled-user", func(f *graphFixture) { f.cfg.Users["7"] = grant{Enabled: false, Permissions: []string{"admin"}} }, 401},
		{"unknown-user", func(f *graphFixture) { delete(f.cfg.Users, "7") }, 401},
		{"expired", func(f *graphFixture) {
			k := f.cfg.APIKeys["key"]
			v := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
			k.ExpiresAt = &v
			f.cfg.APIKeys["key"] = k
		}, 401},
		{"bad-hash", func(f *graphFixture) {
			k := f.cfg.APIKeys["key"]
			k.Hash = bytes.Repeat([]byte{1}, 32)
			f.cfg.APIKeys["key"] = k
		}, 401},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, s := newFixture(t)
			tt.change(f)
			status(t, request(t, s, "POST", "/api/v2/messages", "key.secret", "["+testMessage+"]"), tt.want)
			if tt.want != 201 && f.writes != 0 {
				t.Fatal("unauthorized mutation")
			}
		})
	}
}

func TestQueryKeysAreReadOnlyAndCannotRescueBadHeader(t *testing.T) {
	f, s := newFixture(t)
	status(t, request(t, s, "GET", "/api/v2/messages?apiKey=key.secret", "", ""), 200)
	status(t, request(t, s, "POST", "/api/v2/messages?apiKey=key.secret", "", "["+testMessage+"]"), 401)
	status(t, request(t, s, "GET", "/api/v2/messages?apiKey=key.secret", "wrong", ""), 401)
	if f.writes != 0 {
		t.Fatal("query key wrote messages")
	}
}

func TestAuthorizationOutageFailsClosed(t *testing.T) {
	f, s := newFixture(t)
	f.fail = "all"
	status(t, request(t, s, "POST", "/api/v2/messages", "key.secret", "["+testMessage+"]"), 503)
	if f.writes != 0 {
		t.Fatal("outage allowed mutation")
	}
}

func TestAppendRoundTripsAuthorChunksTTSAndUTCTimeAfterHandlerRecreation(t *testing.T) {
	f, s := newFixture(t)
	w := request(t, s, "POST", "/api/v2/messages", "key.secret", "["+testMessage+"]")
	status(t, w, 201)
	if f.uploads != 1 || len(f.nodes) != 1 || f.nodes[0].Content == nil {
		t.Fatalf("not published: uploads=%d nodes=%d", f.uploads, len(f.nodes))
	}
	restarted := &server{graph: s.graph, pepper: s.pepper, now: s.now}
	w = request(t, restarted, "GET", "/api/v2/messages", "key.secret", "")
	status(t, w, 200)
	var page struct {
		Result   []message
		Total    int
		Continue *int
	}
	if json.Unmarshal(w.Body.Bytes(), &page) != nil || len(page.Result) != 1 || page.Total != 1 || page.Continue != nil {
		t.Fatalf("bad page: %s", w.Body.String())
	}
	m := page.Result[0]
	if m.User != (author{Service: "microphone", ServiceID: "speaker-1", Name: "Distinct Speaker"}) || m.Service != "localhost" || m.Channel != "host" || !m.TTS || !m.Time.Equal(s.now()) || !strings.Contains(w.Body.String(), `"time":"2026-10-30T01:02:03.000000456Z"`) {
		t.Fatalf("round trip changed message: %+v", m)
	}
	if len(m.Chunks) != 2 || m.Chunks[0].Data != "!ordinary text" || m.Chunks[0].Color != "blue" || m.Chunks[0].Weight != "bold" || m.Chunks[1].Type != 1 || m.Chunks[1].Code != ":wave:" || *m.Chunks[1].Width != 24 || *m.Chunks[1].Height != 25 {
		t.Fatalf("chunks=%+v", m.Chunks)
	}
	for _, header := range f.authHeaders {
		if header != "Bearer storage-only-test-token" {
			t.Fatalf("caller credential forwarded: %q", header)
		}
	}
}

func TestInvalidBatchAndPostToNeverPublish(t *testing.T) {
	tests := []struct {
		body string
		want int
	}{
		{`[]`, 400}, {`null`, 400}, {`[null]`, 400}, {`[`, 400}, {`[` + testMessage + `,{}]`, 400},
		{`[` + strings.Replace(testMessage, `"tts":true`, `"extra":true`, 1) + `]`, 400},
		{`[` + strings.Replace(testMessage, `"type":0`, `"type":2`, 1) + `]`, 400},
		{`[` + strings.Replace(testMessage, `"tts":true`, `"postTo":[]`, 1) + `]`, 422},
		{`[` + strings.Replace(testMessage, `"tts":true`, `"postTo":[{"service":"twitch","name":"host"}]`, 1) + `]`, 422},
		{`[` + testMessage + `] {}`, 400},
		{`[` + strings.Replace(testMessage, `"name":"Distinct Speaker"`, `"name":" "`, 1) + `]`, 400},
		{`[` + strings.Replace(testMessage, `"width":24`, `"width":-1`, 1) + `]`, 400},
		{`[` + strings.Repeat(testMessage+",", 100) + testMessage + `]`, 400},
		{strings.Repeat(" ", 1<<20+1), 413},
	}
	for i, tt := range tests {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			f, s := newFixture(t)
			status(t, request(t, s, "POST", "/api/v2/messages", "key.secret", tt.body), tt.want)
			if f.writes != 0 {
				t.Fatalf("invalid batch made %d writes", f.writes)
			}
		})
	}
}

func TestBatchIsInvisibleUntilCompleteContentUpload(t *testing.T) {
	for _, step := range []string{"create", "link", "upload"} {
		t.Run(step, func(t *testing.T) {
			f, s := newFixture(t)
			f.fail = step
			status(t, request(t, s, "POST", "/api/v2/messages", "key.secret", "["+testMessage+"]"), 503)
			if step != "create" && len(f.nodes) != 1 {
				t.Fatal("staging node was not created")
			}
			f.fail = ""
			w := request(t, s, "GET", "/api/v2/messages", "key.secret", "")
			status(t, w, 200)
			if !strings.Contains(w.Body.String(), `"result":[]`) || !strings.Contains(w.Body.String(), `"total":0`) {
				t.Fatalf("staging visible: %s", w.Body.String())
			}
		})
	}
}

func TestAmbiguousUploadDoesNotRetryOrPromiseRollback(t *testing.T) {
	f, s := newFixture(t)
	f.fail = "committed"
	w := request(t, s, "POST", "/api/v2/messages", "key.secret", "["+testMessage+"]")
	status(t, w, 503)
	if !strings.Contains(w.Body.String(), `"code":"write_outcome_unknown"`) || f.uploads != 1 || f.nodes[0].Content == nil {
		t.Fatalf("ambiguous outcome not preserved: %s uploads=%d", w.Body.String(), f.uploads)
	}
	f.fail = ""
	w = request(t, s, "GET", "/api/v2/messages", "key.secret", "")
	status(t, w, 200)
	if !strings.Contains(w.Body.String(), `"total":1`) {
		t.Fatalf("committed message lost: %s", w.Body.String())
	}
}

func TestReadRevocationIsObservedOnNextRequest(t *testing.T) {
	f, s := newFixture(t)
	status(t, request(t, s, "GET", "/api/v2/messages", "key.secret", ""), 200)
	f.cfg.Users["7"] = grant{Enabled: false}
	status(t, request(t, s, "GET", "/api/v2/messages", "key.secret", ""), 401)
}

func TestCorruptAndUnavailableStorageAreNotEmptySuccess(t *testing.T) {
	for _, mode := range []string{"bad-content", "bad-config", "unavailable", "bad-scope"} {
		t.Run(mode, func(t *testing.T) {
			f, s := newFixture(t)
			status(t, request(t, s, "POST", "/api/v2/messages", "key.secret", "["+testMessage+"]"), 201)
			want := 500
			switch mode {
			case "bad-content":
				bad := `{"schemaVersion":1,"messages":[{}]}`
				f.nodes[0].Content = &bad
			case "bad-config":
				f.corruptConfig = true
			case "unavailable":
				f.fail = "list"
				want = 503
			case "bad-scope":
				f.badScope = true
			}
			w := request(t, s, "GET", "/api/v2/messages", "key.secret", "")
			status(t, w, want)
			if strings.Contains(w.Body.String(), "ordinary text") {
				t.Fatal("message leaked in error")
			}
		})
	}
}

func TestListFiltersBoundariesStableOrderAndEveryGraphPage(t *testing.T) {
	f, s := newFixture(t)
	for i := 0; i < 503; i++ {
		var m message
		if err := json.Unmarshal([]byte(testMessage), &m); err != nil {
			t.Fatal(err)
		}
		m.Time = s.now()
		m.User.Name = fmt.Sprintf("speaker-%03d", i)
		if i == 502 {
			m.Time = m.Time.Add(time.Second)
		}
		payload, _ := json.Marshal(batch{SchemaVersion: 1, Messages: []message{m}})
		body := string(payload)
		f.nodes = append(f.nodes, node{ID: int64(100 + i), Type: batchType, Root: 10, Access: intPointer(0), Content: &body, ContentType: "application/json"})
	}
	tests := []struct {
		query string
		total int
		names []string
		next  *int
	}{
		{"?count=2&continue=499", 503, []string{"speaker-499", "speaker-500"}, intPointer(501)},
		{"?count=2&descending=true", 503, []string{"speaker-502", "speaker-501"}, intPointer(2)},
		{"?from=" + url.QueryEscape(s.now().Format(time.RFC3339Nano)) + "&to=" + url.QueryEscape(s.now().Add(time.Second).Format(time.RFC3339Nano)) + "&continue=500", 502, []string{"speaker-500", "speaker-501"}, nil},
		{"?service=other", 0, []string{}, nil}, {"?channel=HOST", 0, []string{}, nil}, {"?continue=9999", 503, []string{}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			w := request(t, s, "GET", "/api/v2/messages"+tt.query, "key.secret", "")
			status(t, w, 200)
			var page struct {
				Result   []message
				Total    int
				Continue *int
			}
			if json.Unmarshal(w.Body.Bytes(), &page) != nil || page.Total != tt.total || len(page.Result) != len(tt.names) || (page.Continue == nil) != (tt.next == nil) {
				t.Fatalf("bad page: %s", w.Body.String())
			}
			if page.Continue != nil && *page.Continue != *tt.next {
				t.Fatalf("continue=%d", *page.Continue)
			}
			for i, name := range tt.names {
				if page.Result[i].User.Name != name {
					t.Fatalf("name=%s want=%s", page.Result[i].User.Name, name)
				}
			}
		})
	}
}

func intPointer(n int) *int { return &n }

func TestInvalidFiltersNeverReadMessages(t *testing.T) {
	for _, q := range []string{"count=0", "count=501", "continue=-1", "sort=id", "descending=maybe", "from=2026-10-30", "to=bad", "count=1&count=2", "rootNodeId=20", "service=x&service=y", "from=2026-10-30T01:00:00Z&to=2026-10-30T00:00:00Z"} {
		t.Run(q, func(t *testing.T) {
			_, s := newFixture(t)
			w := request(t, s, "GET", "/api/v2/messages?"+q, "key.secret", "")
			status(t, w, 400)
		})
	}
}

func TestLivenessAndReadinessHaveDistinctDependencyBehavior(t *testing.T) {
	f, s := newFixture(t)
	status(t, request(t, s, "GET", "/api/i1/readiness", "", ""), 200)
	f.fail = "all"
	w := request(t, s, "GET", "/api/i1/health", "", "")
	status(t, w, 200)
	w = request(t, s, "GET", "/api/i1/readiness", "", "")
	status(t, w, 503)
	if strings.Contains(w.Body.String(), "storage") || strings.Contains(w.Body.String(), "12") {
		t.Fatal("readiness exposed topology")
	}
}

func TestJWTRequiresSignatureIssuerAudienceLifetimeAndGraphGrant(t *testing.T) {
	private, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		change func(jwt.MapClaims, *graphFixture)
		key    *rsa.PrivateKey
		want   int
	}{
		{"valid-admin", nil, private, 200},
		{"wrong-issuer", func(c jwt.MapClaims, _ *graphFixture) { c["iss"] = "wrong" }, private, 401},
		{"wrong-audience", func(c jwt.MapClaims, _ *graphFixture) { c["aud"] = "wrong" }, private, 401},
		{"expired", func(c jwt.MapClaims, _ *graphFixture) { c["exp"] = 1 }, private, 401},
		{"missing-expiry", func(c jwt.MapClaims, _ *graphFixture) { delete(c, "exp") }, private, 401},
		{"future-not-before", func(c jwt.MapClaims, _ *graphFixture) { c["nbf"] = 4102444800 }, private, 401},
		{"wrong-key", nil, other, 401},
		{"unknown-user", func(c jwt.MapClaims, _ *graphFixture) { c["userId"] = "8" }, private, 403},
		{"disabled-user", func(_ jwt.MapClaims, f *graphFixture) {
			f.cfg.Users["7"] = grant{Enabled: false, Permissions: []string{"host"}}
		}, private, 403},
		{"claim-cannot-grant", func(c jwt.MapClaims, f *graphFixture) {
			c["permission"] = "admin"
			f.cfg.Users["7"] = grant{Enabled: true}
		}, private, 403},
		{"issuer-down", func(_ jwt.MapClaims, f *graphFixture) { f.fail = "jwks" }, private, 503},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, s := newFixture(t)
			f.jwks = []byte(fmt.Sprintf(`{"keys":[{"kty":"RSA","kid":"test-key","alg":"RS256","use":"sig","n":"%s","e":"%s"}]}`, base64.RawURLEncoding.EncodeToString(private.N.Bytes()), base64.RawURLEncoding.EncodeToString(big.NewInt(int64(private.E)).Bytes())))
			claims := jwt.MapClaims{"iss": "test-issuer", "aud": "test-audience", "exp": s.now().Add(time.Hour).Unix(), "userId": "7"}
			if tt.change != nil {
				tt.change(claims, f)
			}
			token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
			token.Header["kid"] = "test-key"
			encoded, err := token.SignedString(tt.key)
			if err != nil {
				t.Fatal(err)
			}
			status(t, request(t, s, "GET", "/api/v2/messages", encoded, ""), tt.want)
			if f.jwksAuth != "" {
				t.Fatal("storage credential sent to issuer")
			}
			if f.writes != 0 {
				t.Fatal("GET wrote graph")
			}
		})
	}
}

func TestRoutesAndHeaderFailuresDoNotWrite(t *testing.T) {
	tests := []struct {
		method, path, header string
		want                 int
	}{
		{"GET", "/api/v1/messages", "Bearer key.secret", 404},
		{"DELETE", "/api/v2/messages", "Bearer key.secret", 405},
		{"POST", "/api/v2/messages", "Basic key.secret", 401},
		{"POST", "/api/v2/messages", "Bearer", 401},
	}
	for _, tt := range tests {
		t.Run(tt.method+tt.path+tt.header, func(t *testing.T) {
			f, s := newFixture(t)
			req := httptest.NewRequest(tt.method, tt.path, strings.NewReader("["+testMessage+"]"))
			req.Header.Set("Authorization", tt.header)
			w := httptest.NewRecorder()
			s.ServeHTTP(w, req)
			status(t, w, tt.want)
			if f.writes != 0 {
				t.Fatal("rejected route wrote graph")
			}
		})
	}
}

func TestJWTDisallowedAlgorithmsAreRejected(t *testing.T) {
	for _, method := range []jwt.SigningMethod{jwt.SigningMethodHS256, jwt.SigningMethodNone} {
		t.Run(method.Alg(), func(t *testing.T) {
			f, s := newFixture(t)
			claims := jwt.MapClaims{"iss": "test-issuer", "aud": "test-audience", "exp": s.now().Add(time.Hour).Unix(), "userId": "7"}
			token := jwt.NewWithClaims(method, claims)
			token.Header["kid"] = "test-key"
			var key any = []byte("test-signing-key")
			if method == jwt.SigningMethodNone {
				key = jwt.UnsafeAllowNoneSignatureType
			}
			encoded, err := token.SignedString(key)
			if err != nil {
				t.Fatal(err)
			}
			status(t, request(t, s, "GET", "/api/v2/messages", encoded, ""), 401)
			if f.writes != 0 {
				t.Fatal("untrusted token caused mutation")
			}
		})
	}
}
