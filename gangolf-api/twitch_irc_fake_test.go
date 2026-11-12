package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type twitchIRCFake struct {
	listener  net.Listener
	mu        sync.Mutex
	sockets   []net.Conn
	frames    chan string
	connected chan net.Conn
}

const ircWelcome = ":tmi.twitch.tv 001 testbot :Welcome\r\n:tmi.twitch.tv CAP * ACK :twitch.tv/commands twitch.tv/tags twitch.tv/membership\r\n@user-id=900 :tmi.twitch.tv GLOBALUSERSTATE\r\n:testbot!testbot@testbot.tmi.twitch.tv JOIN #rugenforth\r\n@room-id=101 :tmi.twitch.tv ROOMSTATE #rugenforth\r\n"

func newIRCFake(t *testing.T, c *twitchClient, welcome string) *twitchIRCFake {
	t.Helper()
	cert := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &twitchIRCFake{listener: tls.NewListener(listener, cert.TLS.Clone()), frames: make(chan string, 100), connected: make(chan net.Conn, 10)}
	c.ircURL, c.ircTLS = "tls://"+listener.Addr().String(), cert.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	t.Cleanup(func() {
		f.listener.Close()
		f.mu.Lock()
		for _, socket := range f.sockets {
			socket.Close()
		}
		f.mu.Unlock()
		cert.Close()
	})
	go func() {
		for {
			socket, err := f.listener.Accept()
			if err != nil {
				return
			}
			f.mu.Lock()
			f.sockets = append(f.sockets, socket)
			f.mu.Unlock()
			go func() {
				defer socket.Close()
				reader := bufio.NewReader(socket)
				capabilities, password, authenticated, joined, globalEligible := false, false, false, false, false
				for range 4 {
					line, err := reader.ReadString('\n')
					if err != nil {
						return
					}
					switch line {
					case "CAP REQ :twitch.tv/commands twitch.tv/tags twitch.tv/membership\r\n":
						capabilities = true
					case "PASS oauth:local-access\r\n":
						password = true
					case "NICK testbot\r\n":
						authenticated, globalEligible = password, capabilities
					case "JOIN #rugenforth\r\n":
						joined = authenticated
					default:
						t.Error("incorrect IRC authentication command")
						return
					}
				}
				if !capabilities || !authenticated || !joined {
					t.Error("incomplete IRC authentication sequence")
					return
				}
				messages := strings.SplitAfter(welcome, "\r\n")
				if !globalEligible {
					kept := messages[:0]
					for _, line := range messages {
						frame, ok := parseIRCFrame(strings.TrimSuffix(line, "\r\n"))
						if !ok || frame.command != "GLOBALUSERSTATE" {
							kept = append(kept, line)
						}
					}
					messages = kept
				}
				if _, err := socket.Write([]byte(strings.Join(messages, ""))); err != nil {
					return
				}
				f.connected <- socket
				for {
					line, err := reader.ReadString('\n')
					if err != nil {
						return
					}
					f.frames <- line
				}
			}()
		}
	}()
	return f
}

func ircProfileFixture(t *testing.T) (*resourceFixture, *server, *twitchFake, twitchProfile) {
	t.Helper()
	f, s, tw := outboundFixture(t)
	changeProfile(f, 30, func(p *twitchProfile) { p.SchemaVersion, p.Transport, p.ChannelLogin = 2, "irc", "rugenforth" })
	if err := os.WriteFile(filepath.Join(tw.client.directory, "test-grant"), []byte("User=TestBot\r\nToken=oauth:local-access\r\n"), 0600); err != nil {
		t.Fatal(err)
	}
	tw.validate = func(r *http.Request) (int, any) {
		if r.Header.Get("Authorization") != "OAuth local-access" {
			t.Error("wrong IRC validation header")
		}
		return 200, map[string]any{"login": "testbot", "client_id": "testclient", "user_id": "900", "scopes": []string{"chat:read", "chat:edit"}, "expires_in": 3600}
	}
	cfg, err := s.graph.loadConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return f, s, tw, cfg.twitch[0]
}

func startIRCFixture(t *testing.T, s *server, p twitchProfile) (<-chan twitchEvent, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	queue, done := make(chan twitchEvent, 100), make(chan error, 1)
	go func() {
		defer close(done)
		done <- s.twitch.ircConnection(ctx, s.twitch.client, p, queue, func(p twitchProfile, state, code string, _ [32]byte) { s.twitch.set(p.id, state, code) })
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("IRC worker did not stop")
		}
	})
	return queue, done
}

func waitIRCReady(t *testing.T, s *server, p twitchProfile) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		credential, err := readProfileCredential(s.twitch.client.directory, p)
		if err == nil && s.twitch.ircReady(p, credential) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("IRC session did not become ready")
}

func takeIRCFrame(t *testing.T, f *twitchIRCFake) string {
	t.Helper()
	select {
	case line := <-f.frames:
		return line
	case <-time.After(2 * time.Second):
		t.Fatal("IRC frame missing")
		return ""
	}
}

func ircMessageLine(text string) string {
	return "@id=chat-irc-1;room-id=101;user-id=700;tmi-sent-ts=1794398400123;display-name=Speaker\\sName :speaker!speaker@speaker.tmi.twitch.tv PRIVMSG #rugenforth :" + text + "\r\n"
}

func assertNoIRCFrame(t *testing.T, f *twitchIRCFake) {
	t.Helper()
	select {
	case line := <-f.frames:
		t.Fatalf("unexpected frame command=%s", strings.SplitN(line, " ", 2)[0])
	default:
	}
}
