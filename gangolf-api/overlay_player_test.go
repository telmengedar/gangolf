package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestOverlayOriginCanonicalization(t *testing.T) {
	for _, input := range []struct{ raw, want string }{
		{"HTTP://EXAMPLE.COM:80/", "http://example.com"},
		{"https://Example.COM:443", "https://example.com"},
		{"http://127.0.0.1:08080", "http://127.0.0.1:8080"},
		{"http://[0:0:0:0:0:0:0:1]:80/", "http://[::1]"},
		{"https://example.com:8443/", "https://example.com:8443"},
	} {
		got, ok := canonicalOverlayOrigin(input.raw)
		if !ok || got != input.want {
			t.Fatalf("origin %q=%q,%t", input.raw, got, ok)
		}
	}
	for _, raw := range []string{"", "null", "data:text/html,test", "https://user:secret@example.com", "https://example.com/path", "https://example.com?", "https://example.com#", "https://*.example.com", "https://example.com;bad", "http://127.1", "http://2130706433", "http://0177.0.0.1", "http://example.com:65536", "https://example.com\n", "https://example.com\\evil", "http://[::1%25lo]", "//example.com", "https://example.com/%2f", "https://example.com:bad"} {
		if got, ok := canonicalOverlayOrigin(raw); ok {
			t.Fatalf("invalid origin accepted %q as %q", raw, got)
		}
	}
}

func TestOverlayPlayerConfiguration(t *testing.T) {
	names := []string{"GANGOLF_OVERLAY_PLAYER_LISTEN_ADDR", "GANGOLF_OVERLAY_PLAYER_ORIGIN", "GANGOLF_OVERLAY_PARENT_ORIGIN"}
	for _, name := range names {
		t.Setenv(name, "")
	}
	if p, err := configureOverlayPlayer("127.0.0.1:8080"); err != nil || p != nil {
		t.Fatal("disabled configuration rejected")
	}
	values := []string{"127.0.0.1:8081", "https://PLAYER.EXAMPLE:443/", "https://parent.example"}
	for mask := 1; mask < 8; mask++ {
		for i, name := range names {
			value := ""
			if mask&(1<<i) != 0 {
				value = values[i]
			}
			t.Setenv(name, value)
		}
		p, err := configureOverlayPlayer("127.0.0.1:8080")
		if mask == 7 {
			if err != nil || p.PlayerOrigin != "https://player.example" {
				t.Fatal("complete origin configuration rejected")
			}
		} else if err == nil {
			t.Fatal("partial configuration accepted")
		}
	}
	t.Setenv(names[1], "https://parent.example:443/")
	if _, err := configureOverlayPlayer("127.0.0.1:8080"); err == nil {
		t.Fatal("default-port origin alias accepted")
	}
	t.Setenv(names[1], values[1])
	for _, address := range []string{"127.0.0.1:8080", "invalid", "127.0.0.1:bad", "127.0.0.1:70000"} {
		t.Setenv(names[0], address)
		if _, err := configureOverlayPlayer("127.0.0.1:8080"); err == nil {
			t.Fatal("invalid/conflicting bind accepted")
		}
	}
}

func TestCleanPlayerHandlerIsKeyFree(t *testing.T) {
	_, s := newResourceFixture(t)
	p := &overlayPlayer{PlayerOrigin: "https://player.example", ParentOrigin: "https://parent.example"}
	for _, path := range []string{"/api/i1/health", "/api/v3/events", "/overlays/chat", "/overlays/video", "/overlays/shared.js", "/overlays/test_browser.py", "/overlays/../server.go"} {
		r := httptest.NewRequest("GET", path, nil)
		w := httptest.NewRecorder()
		p.ServeHTTP(w, r)
		status(t, w, 404)
	}
	for _, suffix := range []string{"?key=secret", "?apiKey=secret", "?"} {
		r := httptest.NewRequest("GET", "/overlays/overlay.css"+suffix, nil)
		w := httptest.NewRecorder()
		p.ServeHTTP(w, r)
		status(t, w, 400)
		if strings.Contains(w.Body.String(), "secret") {
			t.Fatal("clean listener reflected credential")
		}
	}
	for _, method := range []string{"POST", "HEAD", "PUT"} {
		r := httptest.NewRequest(method, "/overlays/overlay.css", nil)
		w := httptest.NewRecorder()
		p.ServeHTTP(w, r)
		status(t, w, 405)
	}
	r := httptest.NewRequest("GET", "/overlays/overlay.css", nil)
	r.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)
	status(t, w, 400)
	r = httptest.NewRequest("GET", "/overlays/overlay.css", strings.NewReader("body"))
	w = httptest.NewRecorder()
	p.ServeHTTP(w, r)
	status(t, w, 400)
	r = httptest.NewRequest("GET", "/overlays/overlay.css", nil)
	w = httptest.NewRecorder()
	p.ServeHTTP(w, r)
	status(t, w, 200)
	if w.Header().Get("Referrer-Policy") != "strict-origin" || !strings.Contains(w.Header().Get("Content-Security-Policy"), "frame-ancestors https://parent.example") || w.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("unsafe clean player headers")
	}
	for _, path := range []string{"/overlays/player.html", "/overlays/player.js"} {
		status(t, appRequest(t, s, "GET", path, "", ""), 404)
	}
}

func TestOverlayListenerPairStopsTogether(t *testing.T) {
	_, s := newResourceFixture(t)
	main, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	player, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		main.Close()
		t.Fatal(err)
	}
	s.player = &overlayPlayer{ListenAddr: player.Addr().String(), PlayerOrigin: "http://" + player.Addr().String(), ParentOrigin: "http://" + main.Addr().String()}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serveOverlayListeners(ctx, s, main, player) }()
	client := &http.Client{Timeout: time.Second}
	for _, entry := range []struct {
		url    string
		status int
	}{{s.player.ParentOrigin + "/api/i1/health", 200}, {s.player.PlayerOrigin + "/overlays/overlay.css", 200}, {s.player.PlayerOrigin + "/api/i1/health", 404}} {
		response, err := client.Get(entry.url)
		if err != nil {
			cancel()
			<-done
			t.Fatal(err)
		}
		io.Copy(io.Discard, response.Body)
		response.Body.Close()
		if response.StatusCode != entry.status {
			cancel()
			<-done
			t.Fatalf("listener status=%d", response.StatusCode)
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("listener shutdown blocked")
	}
	for _, address := range []string{main.Addr().String(), player.Addr().String()} {
		conn, err := net.DialTimeout("tcp", address, 100*time.Millisecond)
		if err == nil {
			conn.Close()
			t.Fatal("listener survived lifecycle shutdown")
		}
	}
}

func TestOverlayPlayerBindFailureStopsStartup(t *testing.T) {
	_, s := newResourceFixture(t)
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	s.player = &overlayPlayer{ListenAddr: held.Addr().String(), PlayerOrigin: "https://player.example", ParentOrigin: "https://parent.example"}
	if err := serve(s, "127.0.0.1:0"); err != errUnavailable {
		t.Fatal("occupied clean listener did not fail startup safely")
	}
	if s.matcher != nil {
		t.Fatal("failed listener startup launched worker")
	}
}
