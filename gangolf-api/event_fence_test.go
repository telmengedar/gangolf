package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const fenceTestID = "0123456789abcdef0123456789abcdef"

func fenceRequest(t *testing.T, s *server, token, streamID, requestID string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"streamId": streamID, "requestId": requestID})
	return appRequest(t, s, "POST", "/api/v3/events/fence", token, string(body))
}
func TestEventFenceCannotOvertakeConfirmedProducer(t *testing.T) {
	_, s, p, event := inboundFixture(t)
	stream := openEventTestStream(t, eventTestHost(t, s), "key.secret", "types=message.created,message.updated&service=Twitch")
	defer s.eventHub().shutdown()
	entered, release := make(chan struct{}), make(chan struct{})
	var blocked atomic.Bool
	original := s.graph.http
	s.graph.http = &http.Client{Transport: videoTestTransport{base: original.Transport, after: func(r *http.Request) {
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/content") && blocked.CompareAndSwap(false, true) {
			close(entered)
			<-release
		}
	}}}
	done := make(chan error, 1)
	go func() { _, err := s.ingestTwitch(context.Background(), p, event); done <- err }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("producer did not reach committed publication gap")
	}
	fenced := make(chan *httptest.ResponseRecorder, 1)
	go func() { fenced <- fenceRequest(t, s, "key.secret", stream.id, fenceTestID) }()
	select {
	case <-fenced:
		t.Fatal("fence overtook producer holding commit lock")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	status(t, <-fenced, 200)
	frame := nextEventTestFrame(t, stream, time.Second)
	if !strings.HasPrefix(frame, "event: message.created\n") {
		t.Fatal("first frame was not producer event")
	}
	frame = nextEventTestFrame(t, stream, time.Second)
	if !strings.HasPrefix(frame, "event: stream.fence\n") || !strings.Contains(frame, fenceTestID) {
		t.Fatal("ordered fence frame missing")
	}
}
func TestEventFenceTargetsExactCredentialAndBoundedActiveSubscription(t *testing.T) {
	f, s := newResourceFixture(t)
	r := httptest.NewRequest("GET", "/api/v3/events", nil)
	r.Header.Set("Authorization", "Bearer key.secret")
	p, _, code, _ := s.eventAuthorization(context.Background(), r)
	if code != 0 {
		t.Fatal("positive authorization failed")
	}
	h := s.eventHub()
	sub, code := h.subscribe("", eventFilter{types: map[string]bool{"message.created": true, "message.updated": true}, service: "Twitch"}, nil)
	if code != 0 || !h.bind(sub, p.UserID) {
		t.Fatal("positive subscription failed")
	}
	defer h.shutdown()
	h.identify(sub, p, "key.secret")
	other := addTestKey(f, s, "other", "7", "admin")
	status(t, fenceRequest(t, s, other, sub.id, fenceTestID), 404)
	status(t, fenceRequest(t, s, "", sub.id, fenceTestID), 401)
	status(t, fenceRequest(t, s, "key.secret", strings.Repeat("0", 32), fenceTestID), 404)
	status(t, fenceRequest(t, s, "key.secret", sub.id, fenceTestID), 200)
	status(t, fenceRequest(t, s, "key.secret", sub.id, strings.Repeat("1", 32)), 409)
	queued := <-sub.queue
	if queued.fence != fenceTestID || !h.consume(sub, queued) {
		t.Fatal("fence not included in FIFO capacity")
	}
	status(t, fenceRequest(t, s, "key.secret", sub.id, strings.Repeat("1", 32)), 200)
	h.remove(sub)
	status(t, fenceRequest(t, s, "key.secret", sub.id, fenceTestID), 404)
	for _, body := range []string{`{}`, `{"streamId":"` + sub.id + `","requestId":"` + fenceTestID + `","extra":true}`, strings.Repeat("x", 1025)} {
		status(t, appRequest(t, s, "POST", "/api/v3/events/fence", "key.secret", body), 400)
	}
	status(t, appRequest(t, s, "GET", "/api/v3/events/fence", "key.secret", ""), 405)
}
func TestMessagePatchPublishesOnlyConfirmedFullUpdatedResource(t *testing.T) {
	f, s, p, event := inboundFixture(t)
	event.Message.Text = "  "
	id, err := s.ingestTwitch(context.Background(), p, event)
	if err != nil {
		t.Fatal(err)
	}
	stream := openEventTestStream(t, eventTestHost(t, s), "key.secret", "types=message.updated&service=Twitch")
	defer s.eventHub().shutdown()
	status(t, patchResource(t, s, strconv.FormatInt(id, 10), `[{"op":"replace","path":"/chunks","value":[{"type":0,"data":"first readable"}]}]`), 200)
	envelope, payload := eventTestEnvelope(t, nextEventTestFrame(t, stream, time.Second))
	if envelope.Type != "message.updated" || string(payload["resourceVersion"]) != "3" || !strings.Contains(string(payload["message"]), "first readable") {
		t.Fatal("confirmed full updated projection missing")
	}
	f.mu.Lock()
	f.fail = "content:message"
	f.mu.Unlock()
	status(t, patchResource(t, s, strconv.FormatInt(id, 10), `[{"op":"replace","path":"/tts","value":true}]`), 503)
	select {
	case _, ok := <-stream.frames:
		if ok {
			t.Fatal("uncertain patch emitted data")
		}
	case <-time.After(time.Second):
		t.Fatal("uncertain patch did not invalidate stream")
	}
}

func TestEventFenceRevocationStorageFailureAndOverflowFailClosed(t *testing.T) {
	for _, mode := range []string{"expiry", "revocation", "storage", "foreign filter", "overflow"} {
		t.Run(mode, func(t *testing.T) {
			f, s := newResourceFixture(t)
			r := httptest.NewRequest("GET", "/api/v3/events", nil)
			r.Header.Set("Authorization", "Bearer key.secret")
			p, _, code, _ := s.eventAuthorization(context.Background(), r)
			if code != 0 {
				t.Fatal("positive authorization failed")
			}
			h := s.eventHub()
			sub, code := h.subscribe("", eventFilter{types: map[string]bool{"message.created": true, "message.updated": true}, service: "Twitch"}, nil)
			if code != 0 || !h.bind(sub, p.UserID) {
				t.Fatal("positive subscription failed")
			}
			defer h.shutdown()
			h.identify(sub, p, "key.secret")
			want := 401
			switch mode {
			case "expiry":
				f.legacy.mu.Lock()
				key := f.legacy.cfg.APIKeys["key"]
				past := s.now().Add(-time.Second)
				key.ExpiresAt = &past
				f.legacy.cfg.APIKeys["key"] = key
				f.legacy.mu.Unlock()
			case "revocation":
				f.legacy.mu.Lock()
				key := f.legacy.cfg.APIKeys["key"]
				key.Enabled = false
				f.legacy.cfg.APIKeys["key"] = key
				f.legacy.mu.Unlock()
			case "storage":
				f.mu.Lock()
				f.fail = "list"
				f.mu.Unlock()
				want = 503
			case "foreign filter":
				sub.filter.channel = "other"
				want = 404
			case "overflow":
				for range eventQueue {
					s.publishEvent("message.created", "Twitch", "101", nil, map[string]string{"test": "queued"})
				}
				if len(sub.queue) != eventQueue {
					t.Fatal("positive queue saturation missing")
				}
				want = 409
			}
			status(t, fenceRequest(t, s, "key.secret", sub.id, fenceTestID), want)
			if mode == "overflow" && h.active(sub) {
				t.Fatal("overflow left a healthy fence target")
			}
			if mode != "overflow" && len(sub.queue) != 0 {
				t.Fatal("denied fence entered stream")
			}
		})
	}
}

type fenceBlockingWriter struct {
	header  http.Header
	entered chan struct{}
	release chan struct{}
}

func (w *fenceBlockingWriter) Header() http.Header { return w.header }
func (w *fenceBlockingWriter) WriteHeader(int)     {}
func (w *fenceBlockingWriter) Write(p []byte) (int, error) {
	close(w.entered)
	<-w.release
	return len(p), nil
}
func TestEventFenceHTTPResponseDoesNotHoldProducerLocks(t *testing.T) {
	_, s := newResourceFixture(t)
	r := httptest.NewRequest("GET", "/api/v3/events", nil)
	r.Header.Set("Authorization", "Bearer key.secret")
	p, _, _, _ := s.eventAuthorization(context.Background(), r)
	h := s.eventHub()
	sub, _ := h.subscribe("", eventFilter{types: map[string]bool{"message.created": true, "message.updated": true}, service: "Twitch"}, nil)
	h.bind(sub, p.UserID)
	h.identify(sub, p, "key.secret")
	defer h.shutdown()
	w := &fenceBlockingWriter{header: make(http.Header), entered: make(chan struct{}), release: make(chan struct{})}
	body, _ := json.Marshal(map[string]string{"streamId": sub.id, "requestId": fenceTestID})
	request := httptest.NewRequest("POST", "/api/v3/events/fence", strings.NewReader(string(body)))
	request.Header.Set("Authorization", "Bearer key.secret")
	done := make(chan struct{})
	go func() { s.ServeHTTP(w, request); close(done) }()
	select {
	case <-w.entered:
	case <-time.After(time.Second):
		t.Fatal("response write was not entered")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := lockResources(ctx)
	if err == nil {
		resourceLock.Unlock()
	}
	canPublish := make(chan struct{})
	go func() { s.publishEvent("message.created", "Twitch", "101", nil, "safe"); close(canPublish) }()
	published := false
	select {
	case <-canPublish:
		published = true
	case <-time.After(100 * time.Millisecond):
	}
	close(w.release)
	<-done
	if err != nil || !published {
		t.Fatal("held HTTP response retained producer/resource lock")
	}
}
