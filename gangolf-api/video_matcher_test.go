package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"
)

type videoTestTransport struct {
	base   http.RoundTripper
	before func(*http.Request) error
	after  func(*http.Request)
}

func (v videoTestTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if v.before != nil {
		if err := v.before(r); err != nil {
			return nil, err
		}
	}
	base := v.base
	if base == nil {
		base = http.DefaultTransport
	}
	response, err := base.RoundTrip(r)
	if v.after != nil {
		v.after(r)
	}
	return response, err
}

func automaticVideoFixture(t *testing.T) (*mediaFixture, *resourceFixture, *server, *eventSubscription, string) {
	t.Helper()
	media, f, s := managedMediaFixture(t)
	channel := createAppTestChannel(t, s, "key.secret", "owned")
	c := media.state(t)
	c.Triggers = []mediaTrigger{{ID: "1", Text: "hello", ClipID: new("1")}}
	media.set(c)
	sub, status := s.eventHub().subscribe("test", eventFilter{types: map[string]bool{"video.triggered": true}}, nil)
	if status != 0 {
		t.Fatal(status)
	}
	t.Cleanup(func() { s.eventHub().remove(sub) })
	return media, f, s, sub, channel
}

func createAutomaticMessage(t *testing.T, s *server, text string) messageResource {
	t.Helper()
	response := appRequest(t, s, "POST", "/api/v3/messages", "key.secret", eventLocalMessage("owned", text))
	status(t, response, 201)
	var m messageResource
	if json.Unmarshal(response.Body.Bytes(), &m) != nil {
		t.Fatal("invalid message response")
	}
	return m
}

func expectAutomaticVideo(t *testing.T, sub *eventSubscription, id string) {
	t.Helper()
	select {
	case frame := <-sub.queue:
		var payload struct {
			MessageID string       `json:"messageId"`
			Trigger   mediaTrigger `json:"trigger"`
			Clip      mediaClip    `json:"clip"`
		}
		body, _ := json.Marshal(frame.envelope.Data)
		if json.Unmarshal(body, &payload) != nil || frame.envelope.Type != "video.triggered" || payload.MessageID != id || payload.Trigger.ID != "1" || payload.Clip.ID != "1" {
			t.Fatal("wrong automatic video snapshot")
		}
	case <-time.After(time.Second):
		t.Fatal("automatic event missing")
	}
}

func noAutomaticVideo(t *testing.T, sub *eventSubscription) {
	t.Helper()
	select {
	case <-sub.queue:
		t.Fatal("unexpected automatic video")
	default:
	}
}

func TestAutomaticVideoProducerHooksAndReplay(t *testing.T) {
	_, f, s, sub, _ := automaticVideoFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.matcher = newVideoMatcher(ctx)
	local := createAutomaticMessage(t, s, "HELLO!")
	if len(s.matcher.queue) != 1 {
		t.Fatal("new local source not scheduled")
	}
	provisionTwitch(t, f, 30, "7", "101")
	changeProfile(f, 30, func(p *twitchProfile) { p.Enabled = new(true) })
	cfg, err := s.graph.loadConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	e := twitchEvent{BroadcasterUserID: "101", BroadcasterUserLogin: "room", ChatterUserID: "700", ChatterUserLogin: "speaker", ChatterUserName: "Speaker", MessageID: "match-inbound", publishedAt: s.now()}
	e.Message.Text = "hello"
	id, err := s.ingestTwitch(context.Background(), cfg.twitch[0], e)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.matcher.queue) != 2 {
		t.Fatal("inbound source not scheduled")
	}
	if _, err = s.ingestTwitch(context.Background(), cfg.twitch[0], e); err != nil {
		t.Fatal(err)
	}
	e.MessageID = "match-bot"
	e.ChatterUserID = "900"
	if _, err = s.ingestTwitch(context.Background(), cfg.twitch[0], e); err != nil {
		t.Fatal(err)
	}
	status(t, appRequest(t, s, "GET", "/api/v3/messages", "key.secret", ""), 200)
	status(t, appRequest(t, s, "POST", "/api/v2/messages", "key.secret", "["+legacyLocalMessage("owned", "hello")+"]"), 201)
	status(t, patchResource(t, s, local.ID, `[{"op":"replace","path":"/tts","value":true}]`), 200)
	if len(s.matcher.queue) != 2 {
		t.Fatal("bot/replay/history/v2/patch scheduled a new attempt")
	}
	s.matcher.choose = func(int) int { return 0 }
	go s.runVideoMatcher(s.matcher)
	expectAutomaticVideo(t, sub, local.ID)
	expectAutomaticVideo(t, sub, strconv.FormatInt(id, 10))
	cancel()
	select {
	case <-s.matcher.done:
	case <-time.After(time.Second):
		t.Fatal("worker did not stop")
	}
	noAutomaticVideo(t, sub)
	s.scheduleVideo(999)
	if len(s.matcher.queue) != 0 {
		t.Fatal("canceled worker accepted work")
	}
}

func TestMatchMessageVideo_PunctuatedCommittedMessage_PublishesEdgeTrigger(t *testing.T) {
	for _, text := range []string{"WAT?", "WaT? extra", "extra w.a.t?"} {
		t.Run(text, func(t *testing.T) {
			media, _, s, sub, _ := automaticVideoFixture(t)
			catalog := media.state(t)
			catalog.Triggers[0].Text = "wat"
			media.set(catalog)
			message := createAutomaticMessage(t, s, text)
			s.matchMessageVideo(context.Background(), mustResourceID(message.ID), func(int) int { return 0 })
			expectAutomaticVideo(t, sub, message.ID)
			noAutomaticVideo(t, sub)
		})
	}
}

func TestAutomaticVideoOutboundNeverScheduled(t *testing.T) {
	_, s, tw := outboundFixture(t)
	s.matcher = newVideoMatcher(context.Background())
	tw.send = func(w http.ResponseWriter, r *http.Request) {
		respond(w, 200, map[string]any{"data": []map[string]any{{"message_id": "sent", "is_sent": true}}})
	}
	w := outboundRequest(s, "key.secret", "video-outbound", outboundBody)
	status(t, w, 201)
	outboundResource(t, w, "sent")
	if sends(tw) != 1 || len(s.matcher.queue) != 0 {
		t.Fatal("outbound scheduled video")
	}
}

func TestAutomaticVideoUnavailableAndIneligibleSource(t *testing.T) {
	for _, scenario := range []string{"deleted", "disabled", "foreign", "shared", "media-unavailable", "no-media", "missing-source", "no-match"} {
		t.Run(scenario, func(t *testing.T) {
			media, f, s, sub, channel := automaticVideoFixture(t)
			m := createAutomaticMessage(t, s, "hello")
			id := mustResourceID(m.ID)
			switch scenario {
			case "deleted":
				status(t, appRequest(t, s, "DELETE", "/api/v3/messages/"+m.ID, "key.secret", ""), 204)
			case "disabled":
				status(t, appRequest(t, s, "PUT", "/api/v3/channels/"+channel, "key.secret", `{"service":"Local","channel":"owned","enabled":false}`), 200)
			case "foreign":
				s.media.tenant = "8"
				f.legacy.cfg.Users["8"] = grant{Enabled: true, Permissions: []string{"host"}}
				c := media.state(t)
				c.TenantUserID = "8"
				media.set(c)
			case "shared":
				n := f.nodes[mustResourceID(channel)]
				n.Root = 10
				n.Content = new(`{"schemaVersion":1,"service":"Local","channel":"owned"}`)
				f.nodes[n.ID] = n
			case "media-unavailable":
				media.fail = "all"
			case "no-media":
				s.media = nil
			case "missing-source":
				id = 999
			case "no-match":
				c := media.state(t)
				c.Triggers = nil
				media.set(c)
			}
			s.matchMessageVideo(context.Background(), id, func(int) int { return 0 })
			noAutomaticVideo(t, sub)
		})
	}
}

func TestAutomaticVideoQueueBoundAndContext(t *testing.T) {
	_, _, s, _, _ := automaticVideoFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	s.matcher = newVideoMatcher(ctx)
	for id := int64(1); id <= videoMatchQueue+2; id++ {
		s.scheduleVideo(id)
	}
	if len(s.matcher.queue) != videoMatchQueue || s.matcher.dropped.Load() != 2 {
		t.Fatal("queue not bounded")
	}
	cancel()
	go s.runVideoMatcher(s.matcher)
	select {
	case <-s.matcher.done:
	case <-time.After(time.Second):
		t.Fatal("canceled queue did not stop")
	}
	ctx, cancel = context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	s.media.mu.Lock()
	done := make(chan struct{})
	go func() { s.matchMessageVideo(ctx, 1, func(int) int { return 0 }); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		s.media.mu.Unlock()
		t.Fatal("media lock ignored context")
	}
	s.media.mu.Unlock()
}

func TestAutomaticSlowMediaDoesNotBlockDurableChat(t *testing.T) {
	_, _, s, sub, _ := automaticVideoFixture(t)
	first := createAutomaticMessage(t, s, "hello")
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	original := s.media.graph.http
	s.media.graph.http = &http.Client{Transport: videoTestTransport{base: original.Transport, before: func(r *http.Request) error {
		once.Do(func() { close(started) })
		select {
		case <-release:
			return nil
		case <-r.Context().Done():
			return r.Context().Err()
		}
	}}}
	done := make(chan struct{})
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), videoMatchTimeout)
		defer cancel()
		s.matchMessageVideo(ctx, mustResourceID(first.ID), func(int) int { return 0 })
		close(done)
	}()
	<-started
	response := make(chan int, 1)
	go func() {
		response <- appRequest(t, s, "POST", "/api/v3/messages", "key.secret", eventLocalMessage("owned", "later")).Code
	}()
	select {
	case code := <-response:
		if code != 201 {
			t.Fatalf("later chat=%d", code)
		}
	case <-time.After(time.Second):
		close(release)
		<-done
		t.Fatal("slow media blocked durable chat")
	}
	close(release)
	<-done
	expectAutomaticVideo(t, sub, first.ID)
}

func TestAutomaticSnapshotInvalidationAndBusyPublication(t *testing.T) {
	for _, scenario := range []string{"saved", "unknown-save", "early-save-error", "busy", "source-revoked"} {
		t.Run(scenario, func(t *testing.T) {
			media, f, s, sub, channel := automaticVideoFixture(t)
			source := createAutomaticMessage(t, s, "hello")
			snapshot := make(chan struct{})
			var once sync.Once
			original := s.media.graph.http
			s.media.graph.http = &http.Client{Transport: videoTestTransport{base: original.Transport, after: func(r *http.Request) {
				if r.URL.Path == "/api/nodes" && r.URL.Query().Get("id") == "901" {
					once.Do(func() { close(snapshot) })
				}
			}}}
			resourceLock.Lock()
			done := make(chan struct{})
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), videoMatchTimeout)
				defer cancel()
				s.matchMessageVideo(ctx, mustResourceID(source.ID), func(int) int { return 0 })
				close(done)
			}()
			<-snapshot
			s.media.mu.Lock()
			switch scenario {
			case "saved", "unknown-save", "early-save-error":
				c := media.state(t)
				c.Triggers[0].Text = "changed"
				if scenario == "unknown-save" {
					media.fail = "reject-write"
				}
				if scenario == "early-save-error" {
					c.SchemaVersion = 99
				}
				err := s.media.save(context.Background(), c)
				if scenario == "saved" && err != nil || scenario != "saved" && err == nil {
					s.media.mu.Unlock()
					resourceLock.Unlock()
					t.Fatal("save scenario not reached")
				}
				s.media.mu.Unlock()
			case "source-revoked":
				f.mu.Lock()
				n := f.nodes[mustResourceID(channel)]
				var v identityContent
				json.Unmarshal([]byte(*n.Content), &v)
				v.Enabled = new(false)
				raw, _ := json.Marshal(v)
				n.Content = new(string(raw))
				f.nodes[n.ID] = n
				f.mu.Unlock()
				s.media.mu.Unlock()
			}
			resourceLock.Unlock()
			select {
			case <-done:
			case <-time.After(time.Second):
				if scenario == "busy" {
					s.media.mu.Unlock()
				}
				t.Fatal("publication waited on media")
			}
			if scenario == "busy" {
				s.media.mu.Unlock()
			}
			noAutomaticVideo(t, sub)
		})
	}
}

func TestAutomaticWorkerAttemptDeadline(t *testing.T) {
	_, _, s, _, _ := automaticVideoFixture(t)
	source := createAutomaticMessage(t, s, "hello")
	reached := make(chan struct{})
	expired := make(chan time.Duration, 1)
	original := s.media.graph.http
	s.media.graph.http = &http.Client{Transport: videoTestTransport{base: original.Transport, before: func(r *http.Request) error {
		close(reached)
		start := time.Now()
		<-r.Context().Done()
		expired <- time.Since(start)
		return r.Context().Err()
	}}}
	ctx, cancel := context.WithCancel(context.Background())
	s.matcher = newVideoMatcher(ctx)
	s.scheduleVideo(mustResourceID(source.ID))
	go s.runVideoMatcher(s.matcher)
	<-reached
	select {
	case elapsed := <-expired:
		if elapsed < videoMatchTimeout-250*time.Millisecond || elapsed > videoMatchTimeout+time.Second {
			cancel()
			t.Fatalf("attempt deadline=%s", elapsed)
		}
	case <-time.After(videoMatchTimeout + 2*time.Second):
		cancel()
		t.Fatal("worker attempt unbounded")
	}
	cancel()
	select {
	case <-s.matcher.done:
	case <-time.After(time.Second):
		t.Fatal("worker cancellation failed")
	}
}
