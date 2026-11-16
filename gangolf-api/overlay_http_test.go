package main

import (
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestOverlayHTMLRequiresScopedKeyAndFreshChannels(t *testing.T) {
	f, s := newResourceFixture(t)
	provisionTwitch(t, f, 30, "7", "101")
	changeProfile(f, 30, func(p *twitchProfile) { p.Enabled = new(true) })
	read := issueAppTestToken(t, s, "7", `{"scope":"channel-read","channelIds":["31"]}`)
	path := "/overlays/chat?apiKey=" + url.QueryEscape(read)
	w := appRequest(t, s, "GET", path, "", "")
	status(t, w, 200)
	if w.Header().Get("Content-Type") != "text/html; charset=utf-8" || w.Header().Get("Referrer-Policy") != "no-referrer" || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Content-Security-Policy") != overlayPolicy || !strings.Contains(w.Body.String(), "Gangolf chat overlay") || strings.Contains(w.Body.String(), read) {
		t.Fatal("unsafe overlay document")
	}
	status(t, appRequest(t, s, "GET", path+"&service=Twitch&channel=101", "", ""), 200)
	status(t, appRequest(t, s, "GET", path+"&service=Twitch&channel=102", "", ""), 403)
	for _, path := range []string{"/overlays/chat?apiKey=key.secret", "/overlays/chat?apiKey=invalid.secret", "/overlays/chat?apiKey=header.payload.signature", "/overlays/chat?apiKey=" + url.QueryEscape(read) + "&apiKey=" + url.QueryEscape(read)} {
		w := appRequest(t, s, "GET", path, "", "")
		if w.Code < 400 || strings.Contains(w.Body.String(), read) {
			t.Fatal("unsafe token acceptance/error")
		}
	}
	status(t, appRequest(t, s, "GET", path, "key.secret", ""), 401)
	changeProfile(f, 30, func(p *twitchProfile) { p.Enabled = new(false) })
	status(t, appRequest(t, s, "GET", path, "", ""), 403)
}

func TestOverlayFixedAssetsAndInvalidInput(t *testing.T) {
	_, s := newResourceFixture(t)
	for _, name := range []string{"chat.js", "shared.js", "overlay.css"} {
		w := appRequest(t, s, "GET", "/overlays/"+name, "", "")
		status(t, w, 200)
		if w.Body.Len() == 0 || w.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatal("missing real asset")
		}
		status(t, appRequest(t, s, "GET", "/overlays/"+name+"?apiKey=never-reflect-me", "", ""), 400)
	}
	for _, path := range []string{"/overlays/chat.html", "/overlays/test_browser.py", "/overlays/../server.go", "/overlays/", "/overlays/%2e%2e/server.go", "/overlays/unknown.js"} {
		status(t, appRequest(t, s, "GET", path, "", ""), 404)
	}
	for _, path := range []string{"/overlays/chat", "/overlays/chat?apiKey=", "/overlays/chat?apiKey=x&channel=room", "/overlays/chat?apiKey=x&service=", "/overlays/chat?apiKey=x&unknown=y", "/overlays/chat?apiKey=x&service=a&service=b", "/overlays/chat?apiKey=%ZZ", "/overlays/chat?apiKey=" + strings.Repeat("x", 8193)} {
		status(t, appRequest(t, s, "GET", path, "", ""), 400)
	}
	status(t, appRequest(t, s, "POST", "/overlays/chat?apiKey=x", "", ""), 405)
	status(t, appRequest(t, s, "HEAD", "/overlays/chat?apiKey=x", "", ""), 405)
	status(t, appRequest(t, s, "GET", "/overlays/chat?apiKey=x", "", "body"), 400)
	r := httptest.NewRequest("GET", "/overlays/shared.js?", nil)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	status(t, w, 400)
}
