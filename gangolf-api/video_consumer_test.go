package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func blockAutomaticMedia(t *testing.T, s *server, id int64) (<-chan struct{}, func(), func()) {
	t.Helper()
	started, release := make(chan struct{}), make(chan struct{})
	var first, closed sync.Once
	var active atomic.Bool
	original := s.media.graph.http
	s.media.graph.http = &http.Client{Transport: videoTestTransport{base: original.Transport, before: func(r *http.Request) error {
		if !active.Load() {
			return nil
		}
		first.Do(func() { close(started) })
		select {
		case <-release:
			return nil
		case <-r.Context().Done():
			return r.Context().Err()
		}
	}}}
	ctx, cancel := context.WithCancel(context.Background())
	s.matcher = newVideoMatcher(ctx)
	go s.runVideoMatcher(s.matcher)
	activate := func() { active.Store(true); s.scheduleVideo(id) }
	unblock := func() { closed.Do(func() { close(release) }) }
	t.Cleanup(func() { unblock(); cancel(); <-s.matcher.done })
	return started, activate, unblock
}

func signalVideoPrimary(t *testing.T, s *server) (<-chan struct{}, func()) {
	t.Helper()
	reached := make(chan struct{})
	var once sync.Once
	var armed atomic.Bool
	original := s.graph.http
	s.graph.http = &http.Client{Transport: videoTestTransport{base: original.Transport, after: func(r *http.Request) {
		if armed.Load() && r.URL.Query().Get("type") == "gangolf-user" {
			once.Do(func() { close(reached) })
		}
	}}}
	return reached, func() { armed.Store(true) }
}

func TestVideoConsumerSlowWorkerDoesNotBlockChat(t *testing.T) {
	for _, scenario := range []string{"initial", "data", "heartbeat"} {
		t.Run(scenario, func(t *testing.T) {
			_, _, s, _, channel := automaticVideoFixture(t)
			m := createAutomaticMessage(t, s, "quiet")
			primary, arm := signalVideoPrimary(t, s)
			held, activate, release := blockAutomaticMedia(t, s, mustResourceID(m.ID))
			host := eventTestHost(t, s)
			var stream *eventTestStream
			if scenario != "initial" {
				stream = openEventTestStream(t, host, "key.secret", "types=video.triggered")
			}
			if scenario == "heartbeat" {
				time.Sleep(eventHeartbeat - 500*time.Millisecond)
			}
			arm()
			activate()
			<-held
			response := make(chan *http.Response, 1)
			failed := make(chan error, 1)
			if scenario == "initial" {
				go func() {
					r, _ := http.NewRequest("GET", host.URL+"/api/v3/events?types=video.triggered", nil)
					r.Header.Set("Authorization", "Bearer key.secret")
					res, err := host.Client().Do(r)
					if err != nil {
						failed <- err
					} else {
						response <- res
					}
				}()
			} else if scenario == "data" {
				s.publishEvent("video.triggered", "Local", "owned", &channel, map[string]string{"control": "fresh"})
			}
			select {
			case <-primary:
			case <-time.After(time.Second):
				release()
				t.Fatal("video subscriber did not reach primary gate")
			}
			chat := make(chan int, 1)
			go func() {
				chat <- appRequest(t, s, "POST", "/api/v3/messages", "key.secret", eventLocalMessage("owned", "later quiet")).Code
			}()
			select {
			case code := <-chat:
				if code != 201 {
					release()
					t.Fatalf("chat=%d", code)
				}
			case <-time.After(time.Second):
				release()
				t.Fatal("video media wait blocked chat")
			}
			release()
			if scenario == "initial" {
				select {
				case res := <-response:
					if res.StatusCode != 200 {
						res.Body.Close()
						t.Fatalf("video readiness=%d", res.StatusCode)
					}
					res.Body.Close()
				case err := <-failed:
					t.Fatal(err)
				case <-time.After(time.Second):
					t.Fatal("video handshake failed to resume")
				}
			} else {
				frame := nextEventTestFrame(t, stream, time.Second)
				if scenario == "heartbeat" && frame != ": heartbeat\n" {
					t.Fatalf("heartbeat=%q", frame)
				}
				if scenario == "data" && !strings.Contains(frame, "fresh") {
					t.Fatal("fresh data missing")
				}
			}
		})
	}
}

func TestVideoConsumerRechecksRevocationAfterMediaWait(t *testing.T) {
	for _, scenario := range []string{"key-initial", "key-data", "user-initial", "association-data"} {
		t.Run(scenario, func(t *testing.T) {
			_, f, s, _, _ := automaticVideoFixture(t)
			m := createAutomaticMessage(t, s, "quiet")
			token := "key.secret"
			if scenario == "association-data" {
				provisionTwitch(t, f, 30, "7", "101")
				changeProfile(f, 30, func(p *twitchProfile) { p.Enabled = new(true) })
				token = issueAppTestToken(t, s, "7", `{"scope":"channel-read","channelIds":["31"]}`)
			}
			primary, arm := signalVideoPrimary(t, s)
			held, activate, release := blockAutomaticMedia(t, s, mustResourceID(m.ID))
			host := eventTestHost(t, s)
			var stream *eventTestStream
			initial := strings.HasSuffix(scenario, "initial")
			if !initial {
				stream = openEventTestStream(t, host, token, "types=video.triggered")
			}
			arm()
			activate()
			<-held
			response := make(chan *http.Response, 1)
			failed := make(chan error, 1)
			if initial {
				go func() {
					r, _ := http.NewRequest("GET", host.URL+"/api/v3/events?types=video.triggered", nil)
					r.Header.Set("Authorization", "Bearer "+token)
					res, err := host.Client().Do(r)
					if err != nil {
						failed <- err
					} else {
						response <- res
					}
				}()
			} else {
				s.publishEvent("video.triggered", "Twitch", "101", new("31"), "must-not-leak")
			}
			select {
			case <-primary:
			case <-time.After(time.Second):
				release()
				t.Fatal("primary gate not reached")
			}
			f.mu.Lock()
			if strings.HasPrefix(scenario, "key") {
				f.legacy.mu.Lock()
				key := f.legacy.cfg.APIKeys["key"]
				key.Enabled = false
				f.legacy.cfg.APIKeys["key"] = key
				f.legacy.mu.Unlock()
			} else if strings.HasPrefix(scenario, "user") {
				f.legacy.mu.Lock()
				user := f.legacy.cfg.Users["7"]
				user.Enabled = false
				f.legacy.cfg.Users["7"] = user
				f.legacy.mu.Unlock()
			} else {
				changeProfile(f, 30, func(p *twitchProfile) { p.Enabled = new(false) })
			}
			f.mu.Unlock()
			release()
			if initial {
				select {
				case res := <-response:
					body, _ := io.ReadAll(res.Body)
					res.Body.Close()
					if res.StatusCode < 400 || strings.Contains(string(body), "stream.ready") {
						t.Fatal("revoked ready escaped")
					}
				case err := <-failed:
					t.Fatal(err)
				case <-time.After(time.Second):
					t.Fatal("revoked handshake did not finish")
				}
			} else {
				select {
				case frame, ok := <-stream.frames:
					if ok {
						t.Fatalf("revoked data escaped %q", frame)
					}
				case <-time.After(time.Second):
					t.Fatal("revoked data stream did not close")
				}
			}
		})
	}
}

func TestVideoConsumerCancellationMediaFailureAndMessageIndependence(t *testing.T) {
	media, _, s, _, _ := automaticVideoFixture(t)
	s.media.mu.Lock()
	bad := appRequest(t, s, "GET", "/api/v3/events?types=video.triggered", "invalid.secret", "")
	status(t, bad, 401)
	stream := openEventTestStream(t, eventTestHost(t, s), "key.secret", "types=message.created")
	createAutomaticMessage(t, s, "independent")
	nextEventTestFrame(t, stream, time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	r := httptest.NewRequest("GET", "/api/v3/events?types=video.triggered", nil).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer key.secret")
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { s.ServeHTTP(w, r); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		cancel()
		s.media.mu.Unlock()
		t.Fatal("canceled media waiter retained lock/slot")
	}
	cancel()
	s.media.mu.Unlock()
	status(t, w, 503)
	s.eventHub().mu.Lock()
	count := len(s.eventHub().subs)
	s.eventHub().mu.Unlock()
	if count != 2 {
		t.Fatalf("canceled subscriber slot retained count=%d", count)
	}
	media.mu.Lock()
	media.fail = "all"
	media.mu.Unlock()
	status(t, appRequest(t, s, "GET", "/api/v3/events?types=video.triggered", "key.secret", ""), 503)
}
