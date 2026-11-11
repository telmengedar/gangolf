package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type twitchIRCWriteFault struct {
	net.Conn
	beforeWrite func()
	limit       int
}

func (c *twitchIRCWriteFault) Write(data []byte) (int, error) {
	c.beforeWrite()
	if c.limit >= 0 {
		n, err := c.Conn.Write(data[:c.limit])
		if err != nil {
			return n, err
		}
		return n, io.ErrUnexpectedEOF
	}
	return c.Conn.Write(data)
}

func TestIRCOutboundReservationBeforeWriteAndPartialOutcome(t *testing.T) {
	for _, limit := range []int{-1, 4} {
		t.Run(map[int]string{-1: "full", 4: "partial"}[limit], func(t *testing.T) {
			f, s, tw, p := ircProfileFixture(t)
			a, b := net.Pipe()
			defer a.Close()
			defer b.Close()
			var calls atomic.Int64
			connection := &twitchIRCWriteFault{Conn: a, limit: limit, beforeWrite: func() {
				calls.Add(1)
				f.mu.Lock()
				defer f.mu.Unlock()
				count := 0
				for _, n := range f.nodes {
					if !validOutboundName(n.Name) || n.Content == nil {
						continue
					}
					var m messageContent
					if err := json.Unmarshal([]byte(*n.Content), &m); err != nil {
						t.Error(err)
					}
					if m.SchemaVersion == 4 && m.Delivery != nil && m.Delivery.State == "unknown" {
						count++
					}
				}
				if count != 1 {
					t.Error("durable reservation not present before frame write")
				}
			}}
			credential, err := readProfileCredential(tw.client.directory, p)
			if err != nil {
				t.Fatal(err)
			}
			s.twitch.irc = map[int64]*twitchIRCSession{p.id: {socket: connection, profile: p, fingerprint: credential.fingerprint(), ready: true}}
			received := make(chan []byte, 1)
			go func() { data := make([]byte, 4096); n, _ := b.Read(data); received <- data[:n] }()
			response := outboundRequest(s, "key.secret", "write-outcome", outboundBody)
			expected, state := 201, "submitted"
			if limit >= 0 {
				expected, state = 503, "unknown"
			}
			status(t, response, expected)
			select {
			case data := <-received:
				if len(data) == 0 || limit >= 0 && len(data) != limit {
					t.Fatal("write scenario not reached")
				}
			case <-time.After(time.Second):
				t.Fatal("no socket bytes observed")
			}
			if calls.Load() != 1 {
				t.Fatal("frame written more than once")
			}
			replay := outboundRequest(s, "key.secret", "write-outcome", outboundBody)
			status(t, replay, 200)
			m := outboundResource(t, replay, state)
			if m.Delivery.Transport != "irc" || m.Delivery.MessageID != "" || calls.Load() != 1 {
				t.Fatal("replay changed outcome or resent")
			}
		})
	}
}

func TestIRCTerminalPersistenceAmbiguity(t *testing.T) {
	for _, mode := range []string{"no-commit", "lost-ack", "readback-fail"} {
		t.Run(mode, func(t *testing.T) {
			f, s, tw, p := ircProfileFixture(t)
			fake := newIRCFake(t, tw.client, ircWelcome)
			_, _ = startIRCFixture(t, s, p)
			waitIRCReady(t, s, p)
			var triggered, readFailure atomic.Bool
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if readFailure.Load() && r.Method == "GET" {
					w.WriteHeader(503)
					return
				}
				if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/content") {
					body, _ := io.ReadAll(r.Body)
					r.Body = io.NopCloser(strings.NewReader(string(body)))
					var m messageContent
					json.Unmarshal(body, &m)
					if m.Delivery != nil && m.Delivery.State == "submitted" && triggered.CompareAndSwap(false, true) {
						if mode == "no-commit" {
							w.WriteHeader(503)
							return
						}
						recorder := httptest.NewRecorder()
						f.serve(recorder, r)
						if recorder.Code != 200 {
							t.Errorf("terminal update status=%d", recorder.Code)
						}
						if mode == "lost-ack" {
							w.WriteHeader(503)
							return
						}
						readFailure.Store(true)
						w.WriteHeader(recorder.Code)
						w.Write(recorder.Body.Bytes())
						return
					}
				}
				f.serve(w, r)
			}))
			defer backend.Close()
			s.graph.base, _ = url.Parse(backend.URL + "/api")
			response := outboundRequest(s, "key.secret", "terminal-irc", outboundBody)
			status(t, response, 503)
			if !triggered.Load() || !strings.HasPrefix(takeIRCFrame(t, fake), "PRIVMSG ") {
				t.Fatal("terminal uncertainty not reached")
			}
			var failure map[string]string
			json.Unmarshal(response.Body.Bytes(), &failure)
			stored := storedOutbound(t, f, failure["id"])
			expected := "submitted"
			if mode == "no-commit" {
				expected = "unknown"
			}
			if stored.Delivery.State != expected {
				t.Fatalf("durable state=%s", stored.Delivery.State)
			}
			readFailure.Store(false)
			replay := outboundRequest(restartOutbound(s), "key.secret", "terminal-irc", outboundBody)
			status(t, replay, 200)
			outboundResource(t, replay, expected)
			status(t, request(t, s, "GET", deliveryLocation(mustResourceID(failure["id"])), "key.secret", ""), 200)
			assertNoIRCFrame(t, fake)
		})
	}
}

func TestIRCValidationMetadata_UnknownExpiryAuthenticatesAndPositiveExpiryRechecks(t *testing.T) {
	for _, mode := range []string{"unknown", "short-expiry"} {
		t.Run(mode, func(t *testing.T) {
			_, s, tw, p := ircProfileFixture(t)
			var validations atomic.Int64
			tw.validate = func(*http.Request) (int, any) {
				n := validations.Add(1)
				if mode == "short-expiry" && n > 1 {
					return 401, map[string]any{}
				}
				expires := 0
				if mode == "short-expiry" {
					expires = 1
				}
				return 200, map[string]any{"login": "testbot", "user_id": "900", "client_id": "testclient", "expires_in": expires}
			}
			fake := newIRCFake(t, tw.client, ircWelcome)
			_, done := startIRCFixture(t, s, p)
			waitIRCReady(t, s, p)
			<-fake.connected
			state, code, _ := s.twitch.status(p.id)
			if state != "connected" || code != "irc_metadata_unconfirmed" {
				t.Fatalf("state=%s code=%s", state, code)
			}
			if mode == "short-expiry" {
				select {
				case err := <-done:
					if err != errTwitchAuth {
						t.Fatalf("recheck=%v", err)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("positive expiry did not revalidate")
				}
				if validations.Load() < 2 {
					t.Fatal("expiry validation not reached")
				}
				credential, err := readProfileCredential(tw.client.directory, p)
				if err != nil || s.twitch.ircReady(p, credential) {
					t.Fatal("invalid session remains eligible")
				}
			} else {
				credential, err := tw.client.authorize(context.Background(), p)
				if err != nil || time.Until(credential.revalidateAt) < 59*time.Minute || time.Until(credential.revalidateAt) > time.Hour {
					t.Fatal("unknown expiry lost hourly schedule")
				}
			}
			if tw.refreshCalls != 0 {
				t.Fatal("IRC refreshed OAuth")
			}
			assertNoIRCFrame(t, fake)
		})
	}
}
