package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type eventTestBody struct {
	io.ReadCloser
	cancel context.CancelFunc
	once   sync.Once
	err    error
}

func (b *eventTestBody) Close() error {
	b.once.Do(func() { b.cancel(); b.err = b.ReadCloser.Close() })
	return b.err
}

type eventTestStream struct {
	id     string
	frames chan string
	body   io.ReadCloser
}

func openEventTestStream(t *testing.T, host *httptest.Server, token, query string) *eventTestStream {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r, _ := http.NewRequestWithContext(ctx, "GET", host.URL+"/api/v3/events?"+query, nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := host.Client().Do(r)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if response.StatusCode != 200 || response.Header.Get("Content-Type") != "text/event-stream; charset=utf-8" || response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("X-Accel-Buffering") != "no" {
		cancel()
		response.Body.Close()
		t.Fatalf("handshake status=%d", response.StatusCode)
	}
	s := &eventTestStream{frames: make(chan string, 256), body: &eventTestBody{ReadCloser: response.Body, cancel: cancel}}
	t.Cleanup(func() { cancel(); s.body.Close() })
	go func() {
		defer close(s.frames)
		scanner := bufio.NewScanner(s.body)
		scanner.Buffer(make([]byte, 4096), eventBytes)
		var frame strings.Builder
		for scanner.Scan() {
			if scanner.Text() == "" {
				s.frames <- frame.String()
				frame.Reset()
			} else {
				frame.WriteString(scanner.Text() + "\n")
			}
		}
	}()
	ready := nextEventTestFrame(t, s, 3*time.Second)
	var value struct {
		StreamID       string `json:"streamId"`
		SchemaVersion  int
		Replay         bool
		ResyncRequired bool
	}
	if !strings.HasPrefix(ready, "event: stream.ready\ndata: ") || json.Unmarshal([]byte(strings.TrimPrefix(ready, "event: stream.ready\ndata: ")), &value) != nil || !eventCorrelation(value.StreamID) || value.SchemaVersion != 1 || value.Replay || !value.ResyncRequired {
		t.Fatal("invalid stream ready")
	}
	s.id = value.StreamID
	return s
}

func nextEventTestFrame(t *testing.T, s *eventTestStream, wait time.Duration) string {
	t.Helper()
	select {
	case frame, ok := <-s.frames:
		if !ok {
			t.Fatal("stream ended before expected frame")
		}
		return frame
	case <-time.After(wait):
		s.body.Close()
		t.Fatal("stream frame timeout")
	}
	return ""
}

func eventTestEnvelope(t *testing.T, frame string) (eventEnvelope, map[string]json.RawMessage) {
	t.Helper()
	lines := strings.Split(strings.TrimSuffix(frame, "\n"), "\n")
	var e eventEnvelope
	if len(lines) != 2 || !strings.HasPrefix(lines[1], "data: ") || json.Unmarshal([]byte(strings.TrimPrefix(lines[1], "data: ")), &e) != nil || lines[0] != "event: "+e.Type {
		t.Fatalf("invalid event frame=%q", frame)
	}
	body, _ := json.Marshal(e.Data)
	var data map[string]json.RawMessage
	json.Unmarshal(body, &data)
	return e, data
}

func noEventTestFrame(t *testing.T, stream *eventTestStream) {
	t.Helper()
	select {
	case frame := <-stream.frames:
		t.Fatalf("unexpected frame=%q", frame)
	case <-time.After(50 * time.Millisecond):
	}
}

func eventTestHost(t *testing.T, s *server) *httptest.Server {
	t.Helper()
	host := httptest.NewServer(s)
	t.Cleanup(host.Close)
	return host
}

func TestEventStreamFiltersAndFraming(t *testing.T) {
	_, f, s := newMediaFixture(t)
	user := f.legacy.cfg.Users["7"]
	user.Permissions = []string{"admin", "host"}
	f.legacy.cfg.Users["7"] = user
	key := addTestKey(f, s, "admin", "7", "admin")
	stream := openEventTestStream(t, eventTestHost(t, s), key, "types=message.created,video.triggered&service=Local&channel=room")
	s.publishEvent("message.created", "Local", "other", nil, "ignored")
	s.publishEvent("message.created", "Other", "room", nil, "ignored")
	text := "Grüße 👩🏽‍💻\n\nevent: injected\ndata: bad"
	wanted := s.publishEvent("message.created", "Local", "room", nil, text)
	e, _ := eventTestEnvelope(t, nextEventTestFrame(t, stream, time.Second))
	if e.ID != wanted.ID || e.Data != text || e.ChannelID != nil || e.Time.Location() != time.UTC {
		t.Fatalf("wrong envelope=%+v", e)
	}
	s.publishEvent("video.triggered", "Local", "room", nil, map[string]string{"clip": "test"})
	e, _ = eventTestEnvelope(t, nextEventTestFrame(t, stream, time.Second))
	if e.Type != "video.triggered" {
		t.Fatal(e.Type)
	}
	noEventTestFrame(t, stream)
}

func TestEventStreamValidation(t *testing.T) {
	_, s := newResourceFixture(t)
	for _, query := range []string{"", "types=", "types=unknown", "types=message.created,message.created", "types=message.created&types=video.triggered", "types=message.created&channel=x", "types=message.created&service=", "types=message.created&unknown=x", "types=message.created&service=%zz", "types=message.created&service=" + strings.Repeat("x", 257), "types=message.created&service=" + strings.Repeat("x", 4096)} {
		t.Run(query[:min(len(query), 50)], func(t *testing.T) {
			want := 400
			if strings.Contains(query, "%zz") {
				want = 401
			}
			status(t, request(t, s, "GET", "/api/v3/events?"+query, "key.secret", ""), want)
		})
	}
	for _, tc := range []struct {
		method, path, token, header, value, body string
		want                                     int
	}{
		{"POST", "/api/v3/events?types=message.created", "key.secret", "", "", "", 405},
		{"GET", "/api/v3/events?types=message.created", "", "", "", "", 401},
		{"GET", "/api/v3/events?types=message.created&apiKey=key.secret", "key.secret", "", "", "", 401},
		{"GET", "/api/v3/events?types=message.created", "key.secret", "Last-Event-ID", "old:1", "", 409},
		{"GET", "/api/v3/events?types=message.created", "key.secret", "Accept", "application/json", "", 406},
		{"GET", "/api/v3/events?types=message.created", "key.secret", "Accept", "text/event-stream;q=0", "", 406},
		{"GET", "/api/v3/events?types=message.created", "key.secret", "", "", "body", 400},
		{"GET", "/api/v3/events?types=message.created", "key.secret", "", "", "", 503},
		{"POST", "/api/v3/events", "key.secret", "", "", "{}", 405},
	} {
		r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		if tc.token != "" {
			r.Header.Set("Authorization", "Bearer "+tc.token)
		}
		if tc.header != "" {
			r.Header.Set(tc.header, tc.value)
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		status(t, w, tc.want)
	}
	if !acceptsEvents("application/json, text/event-stream;q=0.5") || !acceptsEvents("*/*") {
		t.Fatal("compatible accept rejected")
	}
	if acceptsEvents("text/event-stream;q=0, */*;q=1") {
		t.Fatal("explicit stream exclusion ignored")
	}
	if got := len(s.eventHub().subs); got != 0 {
		t.Fatalf("failed handshake leaked %d slots", got)
	}
}

type eventDeadlineWriter struct {
	net.Conn
}

func (w eventDeadlineWriter) Header() http.Header { return make(http.Header) }
func (w eventDeadlineWriter) WriteHeader(int)     {}
func (w eventDeadlineWriter) FlushError() error   { return nil }

func TestEventStreamWriteDeadline(t *testing.T) {
	writer, reader := net.Pipe()
	defer writer.Close()
	defer reader.Close()
	w := eventDeadlineWriter{writer}
	started := time.Now()
	if writeEventFrame(w, http.NewResponseController(w), []byte("stalled")) {
		t.Fatal("stalled writer unexpectedly succeeded")
	}
	if elapsed := time.Since(started); elapsed < eventWriteTimeout || elapsed > eventWriteTimeout+time.Second {
		t.Fatalf("stalled write duration=%s", elapsed)
	}
}

type eventReadyGate struct {
	http.ResponseWriter
	ready   chan struct{}
	release chan struct{}
	flushed bool
}

func (w *eventReadyGate) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *eventReadyGate) FlushError() error {
	if !w.flushed {
		w.flushed = true
		close(w.ready)
		<-w.release
	}
	return http.NewResponseController(w.ResponseWriter).Flush()
}

func TestEventStreamQueuedRevocation(t *testing.T) {
	f, s := newResourceFixture(t)
	ready, release := make(chan struct{}), make(chan struct{})
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.ServeHTTP(&eventReadyGate{ResponseWriter: w, ready: ready, release: release}, r)
	}))
	defer host.Close()
	response := make(chan *http.Response, 1)
	go func() {
		r, _ := http.NewRequest("GET", host.URL+"/api/v3/events?types=message.created", nil)
		r.Header.Set("Authorization", "Bearer key.secret")
		got, err := host.Client().Do(r)
		if err != nil {
			response <- nil
			return
		}
		response <- got
	}()
	select {
	case <-ready:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("ready never reached")
	}
	s.publishEvent("message.created", "Local", "room", nil, "queued-before-revocation")
	f.legacy.mu.Lock()
	k := f.legacy.cfg.APIKeys["key"]
	k.Enabled = false
	f.legacy.cfg.APIKeys["key"] = k
	f.legacy.mu.Unlock()
	close(release)
	select {
	case r := <-response:
		if r == nil {
			t.Fatal("request failed")
		}
		defer r.Body.Close()
		body, err := io.ReadAll(r.Body)
		if err != nil || !strings.Contains(string(body), "stream.ready") || strings.Contains(string(body), "queued-before-revocation") {
			t.Fatal("queued event leaked after revocation")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("queued revocation did not close")
	}
}

func TestEventStreamIdleRevocation(t *testing.T) {
	f, s := newResourceFixture(t)
	stream := openEventTestStream(t, eventTestHost(t, s), "key.secret", "types=message.created")
	f.legacy.mu.Lock()
	k := f.legacy.cfg.APIKeys["key"]
	k.Enabled = false
	f.legacy.cfg.APIKeys["key"] = k
	f.legacy.mu.Unlock()
	select {
	case frame, ok := <-stream.frames:
		if ok {
			t.Fatalf("idle revoked stream emitted %q", frame)
		}
	case <-time.After(eventHeartbeat + 2*time.Second):
		t.Fatal("idle revocation exceeded heartbeat budget")
	}
}

type eventByteReader struct{ io.Reader }

func (r eventByteReader) Read(p []byte) (int, error) { return r.Reader.Read(p[:min(len(p), 1)]) }

func TestEventStreamFragmentedUTF8(t *testing.T) {
	_, s := newResourceFixture(t)
	sub, _ := s.eventHub().subscribe("fixture", eventFilter{types: map[string]bool{"message.created": true}}, nil)
	s.publishEvent("message.created", "Local", "room", nil, "Grüße 👩🏽‍💻\n\ndata: malicious")
	frame := <-sub.queue
	scanner := bufio.NewScanner(eventByteReader{strings.NewReader(string(frame.bytes))})
	var lines []string
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if scanner.Err() != nil || len(lines) != 3 || lines[2] != "" {
		t.Fatal("fragmented stream framing corrupted")
	}
	e, _ := eventTestEnvelope(t, strings.Join(lines, "\n"))
	if e.Data != "Grüße 👩🏽‍💻\n\ndata: malicious" {
		t.Fatal("fragmented Unicode payload corrupted")
	}
}

func TestEventStreamAuthorization(t *testing.T) {
	f, s := newResourceFixture(t)
	provisionTwitch(t, f, 30, "7", "101")
	provisionTwitch(t, f, 40, "8", "202")
	hostKey := addTestKey(f, s, "host", "7", "host")
	stream := openEventTestStream(t, eventTestHost(t, s), hostKey, "types=message.created")
	s.publishEvent("message.created", "Twitch", "202", new("41"), "hidden")
	s.publishEvent("message.created", "Twitch", "101", new("999"), "wrongchannel")
	s.publishEvent("message.created", "Twitch", "101", new("31"), "visible-disabled-profile")
	e, _ := eventTestEnvelope(t, nextEventTestFrame(t, stream, time.Second))
	if e.Data != "visible-disabled-profile" {
		t.Fatal("tenant policy mismatch")
	}
	s.publishEvent("message.created", "Local", "shared", nil, "shared")
	e, _ = eventTestEnvelope(t, nextEventTestFrame(t, stream, time.Second))
	if e.Data != "shared" {
		t.Fatal("shared read policy mismatch")
	}
	noEventTestFrame(t, stream)
	admin := openEventTestStream(t, eventTestHost(t, s), "key.secret", "types=message.created")
	s.publishEvent("message.created", "Twitch", "202", new("41"), "admin")
	e, _ = eventTestEnvelope(t, nextEventTestFrame(t, admin, time.Second))
	if e.Data != "admin" {
		t.Fatal("admin lost access")
	}
	noEventTestFrame(t, stream)
}

func TestEventStreamRevocation(t *testing.T) {
	for _, mode := range []string{"key-disabled", "key-expired", "user-disabled", "scope-removed", "profile-removed", "storage"} {
		t.Run(mode, func(t *testing.T) {
			f, s := newResourceFixture(t)
			provisionTwitch(t, f, 30, "7", "101")
			key := addTestKey(f, s, "host", "7", "host")
			stream := openEventTestStream(t, eventTestHost(t, s), key, "types=message.created")
			f.legacy.mu.Lock()
			grant := f.legacy.cfg.APIKeys["host"]
			switch mode {
			case "key-disabled":
				grant.Enabled = false
			case "key-expired":
				expiry := s.now().Add(-time.Second)
				grant.ExpiresAt = &expiry
			case "user-disabled":
				u := f.legacy.cfg.Users["7"]
				u.Enabled = false
				f.legacy.cfg.Users["7"] = u
			case "scope-removed":
				grant.Permissions = nil
			}
			f.legacy.cfg.APIKeys["host"] = grant
			f.legacy.mu.Unlock()
			if mode == "profile-removed" {
				f.mu.Lock()
				changeProfile(f, 30, func(p *twitchProfile) { p.TenantUserID = "8" })
				f.mu.Unlock()
			}
			if mode == "storage" {
				f.mu.Lock()
				f.fail = "identity"
				f.mu.Unlock()
			}
			s.publishEvent("message.created", "Twitch", "101", new("31"), "must-not-leak")

			select {
			case frame, ok := <-stream.frames:
				if ok {
					t.Fatalf("revoked stream leaked %q", frame)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("revoked stream stayed open")
			}
		})
	}
}

func TestEventMessageProducers(t *testing.T) {
	t.Run("v2", func(t *testing.T) {
		_, s := newFixture(t)
		stream := openEventTestStream(t, eventTestHost(t, s), "key.secret", "types=message.created")
		w := request(t, s, "POST", "/api/v2/messages", "key.secret", `[{"service":"Local","channel":"one","user":{"service":"Local","serviceId":"u","name":"User"},"chunks":[{"type":0,"data":"first"}],"tts":false},{"service":"Local","channel":"two","user":{"service":"Local","serviceId":"u","name":"User"},"chunks":[{"type":0,"data":"second"}],"tts":false}]`)
		status(t, w, 201)
		for i := range 2 {
			e, data := eventTestEnvelope(t, nextEventTestFrame(t, stream, time.Second))
			var index, version int
			json.Unmarshal(data["index"], &index)
			json.Unmarshal(data["resourceVersion"], &version)
			if index != i || version != 2 || e.ChannelID != nil || len(data["batchId"]) == 0 {
				t.Fatal("legacy identity/order mismatch")
			}
			var msg message
			json.Unmarshal(data["message"], &msg)
			if !msg.Time.Equal(s.now()) || msg.PostTo != nil {
				t.Fatal("not committed v2 payload")
			}
		}
	})
	t.Run("v3", func(t *testing.T) {
		_, s := newResourceFixture(t)
		stream := openEventTestStream(t, eventTestHost(t, s), "key.secret", "types=message.created")
		w := request(t, s, "POST", "/api/v3/messages", "key.secret", eventLocalMessage("room", "hello"))
		status(t, w, 201)
		_, data := eventTestEnvelope(t, nextEventTestFrame(t, stream, time.Second))
		var actual, wanted messageResource
		json.Unmarshal(data["message"], &actual)
		json.Unmarshal(w.Body.Bytes(), &wanted)
		if !reflect.DeepEqual(actual, wanted) {
			t.Fatal("event differs from accepted resource")
		}
		status(t, patchResource(t, s, wanted.ID, `[{"op":"replace","path":"/tts","value":true}]`), 200)
		status(t, request(t, s, "DELETE", "/api/v3/messages/"+wanted.ID, "key.secret", ""), 204)
		noEventTestFrame(t, stream)
	})
	t.Run("inbound", func(t *testing.T) {
		_, s, p, event := inboundFixture(t)
		stream := openEventTestStream(t, eventTestHost(t, s), "key.secret", "types=message.created")
		id, err := s.ingestTwitch(context.Background(), p, event)
		if err != nil {
			t.Fatal(err)
		}
		_, data := eventTestEnvelope(t, nextEventTestFrame(t, stream, time.Second))
		var msg messageResource
		json.Unmarshal(data["message"], &msg)
		if msg.ID != strconv.FormatInt(id, 10) || msg.Chunks[0].Data != event.Message.Text {
			t.Fatal("inbound payload mismatch")
		}
		if _, err = s.ingestTwitch(context.Background(), p, event); err != nil {
			t.Fatal(err)
		}
		noEventTestFrame(t, stream)
	})
	t.Run("outbound", func(t *testing.T) {
		_, s, tw := outboundFixture(t)
		stream := openEventTestStream(t, eventTestHost(t, s), "key.secret", "types=message.created")
		tw.send = func(w http.ResponseWriter, r *http.Request) {
			_, data := eventTestEnvelope(t, nextEventTestFrame(t, stream, time.Second))
			var msg messageResource
			json.Unmarshal(data["message"], &msg)
			if msg.Delivery == nil || msg.Delivery.State != "unknown" {
				t.Error("event must precede send with unknown delivery")
			}
			respond(w, 200, map[string]any{"data": []any{map[string]any{"is_sent": true, "message_id": "sent"}}})
		}
		status(t, outboundRequest(s, "key.secret", "once", outboundBody), 201)
		status(t, outboundRequest(s, "key.secret", "once", outboundBody), 200)
		if sends(tw) != 1 {
			t.Fatal("replay sent again")
		}
		noEventTestFrame(t, stream)
	})
	t.Run("ambiguous", func(t *testing.T) {
		f, s := newResourceFixture(t)
		stream := openEventTestStream(t, eventTestHost(t, s), "key.secret", "types=message.created")
		f.mu.Lock()
		f.fail = "committed:message"
		f.mu.Unlock()
		w := request(t, s, "POST", "/api/v3/messages", "key.secret", eventLocalMessage("room", "uncertain"))
		status(t, w, 503)
		select {
		case frame, ok := <-stream.frames:
			if ok {
				t.Fatalf("unconfirmed frame %q", frame)
			}
		case <-time.After(time.Second):
			t.Fatal("ambiguous create retained stream")
		}
	})
}

func TestEventStreamBoundsAndCancellation(t *testing.T) {
	_, s := newResourceFixture(t)
	h := s.eventHub()
	filter := eventFilter{types: map[string]bool{"message.created": true}}
	slow, statusCode := h.subscribe("slow", filter, nil)
	if statusCode != 0 {
		t.Fatal(statusCode)
	}
	healthy, _ := h.subscribe("healthy", filter, nil)
	for i := 0; i <= eventQueue; i++ {
		s.publishEvent("message.created", "Local", "room", nil, i)
		frame := <-healthy.queue
		if !h.consume(healthy, frame) {
			t.Fatal("healthy stream invalid")
		}
	}
	if h.active(slow) || !h.active(healthy) || slow.bytes != 0 || len(slow.queue) != 0 {
		t.Fatal("event bound did not isolate slow consumer")
	}
	large, _ := h.subscribe("large", filter, nil)
	s.publishEvent("message.created", "Local", "room", nil, strings.Repeat("x", eventBytes/2))
	s.publishEvent("message.created", "Local", "room", nil, strings.Repeat("x", eventBytes/2))
	if h.active(large) {
		t.Fatal("byte bound not enforced")
	}
	oversize, _ := h.subscribe("oversize", filter, nil)
	s.publishEvent("message.created", "Local", "room", nil, strings.Repeat("x", eventBytes))
	if h.active(oversize) {
		t.Fatal("frame bound not enforced")
	}
	h.reset()
	for i := range eventStreams {
		if _, code := h.subscribe(fmt.Sprint(i), filter, nil); code != 0 {
			t.Fatal(code)
		}
	}
	if _, code := h.subscribe("extra", filter, nil); code != 429 {
		t.Fatal("global limit ignored")
	}
	h.reset()
	for range eventUserStreams {
		if _, code := h.subscribe("same", filter, nil); code != 0 {
			t.Fatal(code)
		}
	}
	if _, code := h.subscribe("same", filter, nil); code != 429 {
		t.Fatal("user limit ignored")
	}
	h.reset()
	host := eventTestHost(t, s)
	stream := openEventTestStream(t, host, "key.secret", "types=message.created")
	stream.body.Close()
	deadline := time.Now().Add(time.Second)
	for {
		h.mu.Lock()
		count := len(h.subs)
		h.mu.Unlock()
		if count == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("cancel retained slot")
		}
		time.Sleep(time.Millisecond)
	}
	stream = openEventTestStream(t, host, "key.secret", "types=message.created")
	h.shutdown()
	select {
	case _, ok := <-stream.frames:
		if ok {
			t.Fatal("shutdown frame")
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown retained handler")
	}
}

func TestEventStreamHTTPPerUserLimit(t *testing.T) {
	f, s := newResourceFixture(t)
	alternate := addTestKey(f, s, "alternate", "7", "host")
	f.legacy.cfg.Users["8"] = grant{Enabled: true, Permissions: []string{"host"}}
	other := addTestKey(f, s, "other", "8", "host")
	host := eventTestHost(t, s)
	var streams []*eventTestStream
	for i := range 4 {
		key := "key.secret"
		if i >= 2 {
			key = alternate
		}
		streams = append(streams, openEventTestStream(t, host, key, "types=message.created"))
	}
	openEventTestStream(t, host, other, "types=message.created")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	r, _ := http.NewRequestWithContext(ctx, "GET", host.URL+"/api/v3/events?types=message.created", nil)
	r.Header.Set("Authorization", "Bearer "+alternate)
	response, err := host.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 429 {
		t.Fatalf("fifth same-user HTTP stream status=%d want=429", response.StatusCode)
	}
	streams[0].body.Close()
	deadline := time.Now().Add(time.Second)
	for {
		h := s.eventHub()
		h.mu.Lock()
		count := 0
		for sub := range h.subs {
			if sub.user == "7" {
				count++
			}
		}
		h.mu.Unlock()
		if count == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("cancelled HTTP stream retained its user's slot")
		}
		time.Sleep(time.Millisecond)
	}
	openEventTestStream(t, host, alternate, "types=message.created")
}

func TestEventStreamLongLivedHTTP(t *testing.T) {
	_, s := newResourceFixture(t)
	host := httptest.NewUnstartedServer(s)
	host.Config.ReadTimeout = 35 * time.Second
	host.Config.WriteTimeout = 35 * time.Second
	host.Start()
	t.Cleanup(host.Close)
	stream := openEventTestStream(t, host, "key.secret", "types=message.created")
	start := time.Now()
	for range 2 {
		if frame := nextEventTestFrame(t, stream, 18*time.Second); frame != ": heartbeat\n" {
			t.Fatalf("heartbeat=%q", frame)
		}
	}
	time.Sleep(time.Until(start.Add(36 * time.Second)))
	s.publishEvent("message.created", "Local", "room", nil, "after-deadlines")
	e, _ := eventTestEnvelope(t, nextEventTestFrame(t, stream, time.Second))
	if e.Data != "after-deadlines" {
		t.Fatal("long stream payload mismatch")
	}
	s.eventHub().shutdown()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := host.Config.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestEventStreamReconnectContract(t *testing.T) {
	_, s := newResourceFixture(t)
	host := eventTestHost(t, s)
	first := openEventTestStream(t, host, "key.secret", "types=message.created")
	w := request(t, s, "POST", "/api/v3/messages", "key.secret", eventLocalMessage("room", "buffered"))
	status(t, w, 201)
	_, data := eventTestEnvelope(t, nextEventTestFrame(t, first, time.Second))
	var msg messageResource
	json.Unmarshal(data["message"], &msg)
	history := request(t, s, "GET", "/api/v3/messages", "key.secret", "")
	status(t, history, 200)
	var page struct {
		Result []messageResource `json:"result"`
	}
	json.Unmarshal(history.Body.Bytes(), &page)
	ids := map[string]bool{msg.ID: true}
	for _, m := range page.Result {
		ids[m.ID] = true
	}
	if len(ids) != 1 || len(page.Result) != 1 {
		t.Fatal("buffer/history ID merge omitted or duplicated creation")
	}
	first.body.Close()
	s.publishEvent("message.created", "Local", "room", nil, "while-disconnected")
	second := openEventTestStream(t, host, "key.secret", "types=message.created")
	noEventTestFrame(t, second)
	r := httptest.NewRequest("GET", "/api/v3/events?types=message.created", nil)
	r.Header.Set("Authorization", "Bearer key.secret")
	r.Header.Set("Last-Event-ID", "previous:1")
	out := httptest.NewRecorder()
	s.ServeHTTP(out, r)
	status(t, out, 409)
}

func eventLocalMessage(channel, text string) string {
	data, _ := json.Marshal(messageInput{Service: "Local", Channel: channel, User: author{Service: "Local", ServiceID: "u", Name: "User"}, Chunks: []chunk{{Type: 0, Data: text}}})
	return string(data)
}
