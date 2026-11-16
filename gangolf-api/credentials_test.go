package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestCredentialHeaderAndQuery(t *testing.T) {
	f, s := newResourceFixture(t)
	id := createAppTestUser(t, s, "Alice")
	token := issueAppTestToken(t, s, id, `{}`)
	channel := createAppTestChannel(t, s, token, "one")
	read := issueAppTestToken(t, s, id, `{"scope":"channel-read","channelIds":["`+channel+`"]}`)
	for _, test := range []struct {
		query, header string
		duplicate     bool
		want          int
	}{
		{"apiKey=" + url.QueryEscape(read), "", false, 200},
		{"apiKey=" + url.QueryEscape(read), read, false, 401},
		{"apiKey=" + url.QueryEscape(read) + "&api%4Bey=" + url.QueryEscape(read), "", false, 401},
		{"apiKey=", "", false, 401},
		{"apiKey=%zz", "", false, 401},
		{"apiKey=" + url.QueryEscape(token), "", false, 401},
		{"apiKey=key.secret", "", false, 401},
		{"access_token=" + url.QueryEscape(read), read, false, 400},
		{"", read, true, 401},
		{"", read + ",other", false, 401},
		{"", "", false, 401},
	} {
		r := httptest.NewRequest("GET", "/api/v3/channels?"+test.query, nil)
		if test.header != "" {
			r.Header.Set("Authorization", "Bearer "+test.header)
		}
		if test.duplicate {
			r.Header.Add("Authorization", "Bearer "+read)
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		status(t, w, test.want)
		if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Referrer-Policy") != "no-referrer" {
			t.Fatal("credential response lacks privacy headers")
		}
		if strings.Contains(w.Body.String(), read) || strings.Contains(w.Header().Get("Location"), read) {
			t.Fatal("credential reflected")
		}
	}
	legacy := f.legacy.cfg.APIKeys["key"]
	legacy.Permissions = []string{"host"}
	f.legacy.cfg.APIKeys["key"] = legacy
	status(t, appRequest(t, s, "GET", "/api/v3/messages?apiKey=key.secret", "", ""), 200)
	status(t, appRequest(t, s, "GET", "/api/v2/messages?apiKey=key.secret", "", ""), 200)
	status(t, appRequest(t, s, "GET", "/api/v3/messages?apiKey=key.secret", "key.secret", ""), 401)
	status(t, appRequest(t, s, "GET", "/api/v3/messages?apiKey="+url.QueryEscape(token), "", ""), 401)
	status(t, appRequest(t, s, "GET", "/api/v3/channels?apiKey="+strings.Repeat("x", 4097), "", ""), 401)
}

func TestOwnedChannelPermissionReadsAreRequestLocalAndBounded(t *testing.T) {
	f, s := newResourceFixture(t)
	id := createAppTestUser(t, s, "Alice")
	token := issueAppTestToken(t, s, id, `{}`)
	channel := createAppTestChannel(t, s, token, "one")
	cfg, err := s.graph.loadConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	p := s.policy(principal{UserID: id, Permissions: []string{"host"}, ManagedOnly: true}, cfg)
	start := len(f.queries)
	for range 5 {
		allowed, err := p.allowed(context.Background(), "Local", "one", channel, false)
		if err != nil || !allowed {
			t.Fatal("owner lookup failed")
		}
	}
	nameAllowed, err := p.allowed(context.Background(), "Local", "one", "", false)
	if err != nil || !nameAllowed {
		t.Fatal("canonical name memo failed")
	}
	queries := f.queries[start:]
	if len(queries) != 2 {
		t.Fatalf("channel read count=%d want=2", len(queries))
	}
	for _, q := range queries {
		if q.Get("type") != "channel" || q.Get("id") == "" && q.Get("name") == "" {
			t.Fatal("ownership performed collection scan")
		}
	}
	n := f.nodes[mustResourceID(channel)]
	value, _ := channelIdentity(n)
	value.Enabled = new(false)
	raw, _ := json.Marshal(value)
	n.Content = new(string(raw))
	f.nodes[n.ID] = n
	fresh := s.policy(p.principal, cfg)
	allowed, err := fresh.allowed(context.Background(), "Local", "one", channel, true)
	if err != nil || allowed {
		t.Fatal("fresh request reused prior channel allow")
	}
}
