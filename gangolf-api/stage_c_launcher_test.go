package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

func loopbackTestURL(raw string) (*url.URL, bool) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, false
	}
	ip, err := netip.ParseAddr(u.Hostname())
	return u, err == nil && ip.IsLoopback() && (u.Scheme == "http" || u.Scheme == "https") && u.User == nil && u.RawQuery == "" && u.Fragment == "" && u.Opaque == ""
}

func stageCService() error {
	fake, ok := loopbackTestURL(os.Getenv("GANGOLF_TEST_TWITCH_URL"))
	if !ok || fake.Scheme != "http" || fake.Path != "" {
		return errUnavailable
	}
	if _, ok := loopbackTestURL(os.Getenv("GANGOLF_DIVOID_API_URL")); !ok {
		return errUnavailable
	}
	handler, addr, err := bootstrap()
	if err != nil {
		return err
	}
	host, _, err := net.SplitHostPort(addr)
	ip, ipErr := netip.ParseAddr(host)
	if err != nil || ipErr != nil || !ip.IsLoopback() {
		return errUnavailable
	}
	client := handler.twitch.client
	client.validateURL = fake.String() + "/validate"
	client.tokenURL = fake.String() + "/token"
	client.subscriptionURL = fake.String() + "/subscriptions"
	client.socketURL = "ws" + strings.TrimPrefix(fake.String(), "http") + "/ws"
	client.http.Transport = &http.Transport{Proxy: nil}
	return serve(handler, addr)
}

func TestStageCService_Subprocess_NormalStartup(t *testing.T) {
	if os.Getenv("GANGOLF_TEST_STAGE_C_CHILD") != "1" {
		t.Skip("test-only subprocess launcher")
	}
	if stageCService() != nil {
		t.Fatal("candidate startup or shutdown failed")
	}
}

func TestLoopbackTestURL_UnsafeTargets_Rejected(t *testing.T) {
	for _, raw := range []string{"https://example.test", "http://localhost", "http://127.0.0.1.example.test", "http://0.0.0.0", "http://192.168.1.1", "http://[::]", "http://127.0.0.1@evil.test", "http://user:secret@127.0.0.1", "http://127.0.0.1?target=public", "http://127.0.0.1#fragment", "ftp://127.0.0.1", "://broken"} {
		if _, ok := loopbackTestURL(raw); ok {
			t.Fatal("unsafe target accepted")
		}
	}
	for _, raw := range []string{"http://127.0.0.1:1234", "https://[::1]:1234/api"} {
		if _, ok := loopbackTestURL(raw); !ok {
			t.Fatal("loopback target rejected")
		}
	}
}

func TestStageCService_UnsafeBindings_RejectBeforeStartup(t *testing.T) {
	for _, settings := range []map[string]string{
		{"GANGOLF_TEST_TWITCH_URL": "https://example.test"},
		{"GANGOLF_TEST_TWITCH_URL": "http://127.0.0.1:1/path"},
		{"GANGOLF_TEST_TWITCH_URL": "http://127.0.0.1:1", "GANGOLF_DIVOID_API_URL": "https://example.test/api"},
		{"GANGOLF_TEST_TWITCH_URL": "http://127.0.0.1:1", "GANGOLF_DIVOID_API_URL": "http://127.0.0.1:1/api", "GANGOLF_DIVOID_TOKEN": "", "GANGOLF_API_KEY_PEPPER": ""},
		{"GANGOLF_TEST_TWITCH_URL": "http://127.0.0.1:1", "GANGOLF_DIVOID_API_URL": "http://127.0.0.1:1/api", "GANGOLF_DIVOID_TOKEN": "synthetic", "GANGOLF_API_KEY_PEPPER": "synthetic", "GANGOLF_DIVOID_ROOT_ID": "1", "GANGOLF_DIVOID_MESSAGES_ID": "2", "GANGOLF_DIVOID_CONFIG_ID": "3", "GANGOLF_LISTEN": "0.0.0.0:1234"},
	} {
		t.Run("binding", func(t *testing.T) {
			for key, value := range settings {
				t.Setenv(key, value)
			}
			if stageCService() == nil {
				t.Fatal("unsafe or incomplete binding accepted")
			}
		})
	}
}

func TestStageCService_PublicFakeOriginRejectedBeforeGraphIO(t *testing.T) {
	var graphRequests atomic.Int64
	graph := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		graphRequests.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer graph.Close()
	values := map[string]string{
		"GANGOLF_TEST_TWITCH_URL":       "http://192.0.2.1:1234",
		"GANGOLF_DIVOID_API_URL":        graph.URL + "/api",
		"GANGOLF_DIVOID_TOKEN":          "synthetic-storage-token",
		"GANGOLF_API_KEY_PEPPER":        "synthetic-key-pepper",
		"GANGOLF_DIVOID_ROOT_ID":        "1",
		"GANGOLF_DIVOID_MESSAGES_ID":    "2",
		"GANGOLF_DIVOID_CONFIG_ID":      "3",
		"GANGOLF_LISTEN":                "127.0.0.1:0",
		"GANGOLF_TWITCH_CREDENTIAL_DIR": t.TempDir(),
	}
	for key, value := range values {
		t.Setenv(key, value)
	}
	if err := stageCService(); err == nil {
		t.Fatal("public fake origin accepted")
	}
	if got := graphRequests.Load(); got != 0 {
		t.Fatalf("unsafe fake origin reached graph IO: calls=%d; want=0", got)
	}
}

func TestReconnectAddress_LocalTransport_CannotEscapeLoopback(t *testing.T) {
	client := &twitchClient{socketURL: "ws://127.0.0.1:1234/ws"}
	for _, target := range []string{"wss://eventsub.wss.twitch.tv/ws", "ws://example.test/ws", "ws://127.0.0.1:1235/ws", "ws://user:secret@127.0.0.1:1234/ws"} {
		if client.reconnectAddress(target) {
			t.Fatal("reconnect escaped local fake")
		}
	}
	if !client.reconnectAddress("ws://127.0.0.1:1234/transfer") {
		t.Fatal("local transfer rejected")
	}
	t.Setenv("GANGOLF_TEST_TWITCH_URL", "http://127.0.0.1:1234")
	production := productionTwitchClient("")
	if !production.reconnectAddress("wss://eventsub.wss.twitch.tv/ws?reconnect=opaque") || production.validateURL != "https://id.twitch.tv/oauth2/validate" || production.subscriptionURL != "https://api.twitch.tv/helix/eventsub/subscriptions" || production.tokenURL != "https://id.twitch.tv/oauth2/token" || production.socketURL != "wss://eventsub.wss.twitch.tv/ws" {
		t.Fatal("production destinations changed")
	}
}
