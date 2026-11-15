package main

import (
	"encoding/json"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func videoEventFixture(t *testing.T) (*mediaFixture, *resourceFixture, *server, messageResource, string) {
	t.Helper()
	media, f, s := newMediaFixture(t)
	provisionTwitch(t, f, 30, "7", "101")
	admin := addTestKey(f, s, "admin", "7", "admin")
	c := media.state(t)
	c.Clips = []mediaClip{{ID: "1", Provider: "youtube", RemoteVideoID: "abcdefghijk", Title: new("Test clip")}}
	c.Triggers = []mediaTrigger{{ID: "1", Text: "unmatched intent", ClipID: new("1")}, {ID: "2", Text: "unresolved", ClipID: nil, Legacy: &mediaLegacy{Snapshot: c.Origin.Snapshot, ArchiveNodeID: "200", LegacyID: "2", LegacyVideoID: "999", InitialResolution: "unresolved"}}}
	media.mu.Lock()
	media.set(c)
	media.mu.Unlock()
	body := `{"service":"Twitch","channel":"101","user":{"service":"Twitch","serviceId":"700","name":"User"},"chunks":[{"type":0,"data":"source without matching trigger"}],"tts":false}`
	w := request(t, s, "POST", "/api/v3/messages", admin, body)
	status(t, w, 201)
	var msg messageResource
	json.Unmarshal(w.Body.Bytes(), &msg)
	return media, f, s, msg, admin
}

func postVideoTest(s *server, token, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/api/v3/video-events", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func TestVideoEventProducer(t *testing.T) {
	media, f, s, msg, admin := videoEventFixture(t)
	stream := openEventTestStream(t, eventTestHost(t, s), "key.secret", "types=video.triggered")
	body := `{"messageId":"` + msg.ID + `","triggerId":"1"}`
	f.mu.Lock()
	before := f.writes
	f.mu.Unlock()
	media.mu.Lock()
	mediaBefore := media.writes
	media.mu.Unlock()
	var previous string
	for range 2 {
		w := postVideoTest(s, "key.secret", body)
		status(t, w, 200)
		if w.Header().Get("Location") != "" {
			t.Fatal("ephemeral intent has durable Location")
		}
		e, data := eventTestEnvelope(t, nextEventTestFrame(t, stream, time.Second))
		var clip mediaClip
		var trigger mediaTrigger
		var messageID string
		json.Unmarshal(data["clip"], &clip)
		json.Unmarshal(data["trigger"], &trigger)
		json.Unmarshal(data["messageId"], &messageID)
		if e.ID == previous || e.Type != "video.triggered" || e.ChannelID == nil || *e.ChannelID != msg.ChannelID || messageID != msg.ID || clip.RemoteVideoID != "abcdefghijk" || trigger.Text != "unmatched intent" {
			t.Fatal("video payload was not derived from source/catalog")
		}
		previous = e.ID
		var returned eventEnvelope
		json.Unmarshal(w.Body.Bytes(), &returned)
		if returned.ID != e.ID {
			t.Fatal("producer returned different intent")
		}
	}
	status(t, postVideoTest(s, admin, body), 200)
	nextEventTestFrame(t, stream, time.Second)
	f.mu.Lock()
	after := f.writes
	f.mu.Unlock()
	media.mu.Lock()
	mediaAfter := media.writes
	media.mu.Unlock()
	if after != before || mediaAfter != mediaBefore {
		t.Fatal("video intent persisted data")
	}
	for _, tc := range []struct {
		body string
		want int
	}{
		{`{"messageId":"` + msg.ID + `","triggerId":"2"}`, 409},
		{`{"messageId":"` + msg.ID + `","triggerId":"999"}`, 404},
		{`{"messageId":"999","triggerId":"1"}`, 404},
		{`{"messageId":"` + msg.ID + `","triggerId":"1","clip":{}}`, 400},
		{`{"messageId":"` + msg.ID + `","messageId":"` + msg.ID + `","triggerId":"1"}`, 400},
		{`{"messageId":"01","triggerId":"1"}`, 400},
		{`{"messageId":null,"triggerId":"1"}`, 400},
		{`{"MessageId":"` + msg.ID + `","triggerId":"1"}`, 400},
		{`{"messageId":"` + msg.ID + `"}`, 400},
		{strings.Repeat("x", (1<<20)+1), 413},
	} {
		status(t, postVideoTest(s, "key.secret", tc.body), tc.want)
	}
	status(t, postVideoTest(s, "", body), 401)
	local := request(t, s, "POST", "/api/v3/messages", "key.secret", eventLocalMessage("room", "shared"))
	status(t, local, 201)
	var shared messageResource
	json.Unmarshal(local.Body.Bytes(), &shared)
	status(t, postVideoTest(s, "key.secret", `{"messageId":"`+shared.ID+`","triggerId":"1"}`), 403)
	status(t, postVideoTest(s, admin, `{"messageId":"`+shared.ID+`","triggerId":"1"}`), 200)
	status(t, request(t, s, "DELETE", "/api/v3/messages/"+msg.ID, "key.secret", ""), 204)
	status(t, postVideoTest(s, "key.secret", body), 404)
}

func TestVideoEventSubscriptionIsolation(t *testing.T) {
	media, f, s, msg, admin := videoEventFixture(t)
	f.legacy.cfg.Users["8"] = grant{Enabled: true, Permissions: []string{"host"}}
	other := addTestKey(f, s, "other", "8", "host")
	for _, token := range []string{other} {
		status(t, request(t, s, "GET", "/api/v3/events?types=video.triggered", token, ""), 403)
		status(t, postVideoTest(s, token, `{"messageId":"`+msg.ID+`","triggerId":"1"}`), 403)
	}
	video := openEventTestStream(t, eventTestHost(t, s), "key.secret", "types=video.triggered")
	messages := openEventTestStream(t, eventTestHost(t, s), other, "types=message.created")
	adminVideo := openEventTestStream(t, eventTestHost(t, s), admin, "types=video.triggered")
	shared := createResource(t, s, eventLocalMessage("shared", "shared source"))
	status(t, postVideoTest(s, admin, `{"messageId":"`+shared.ID+`","triggerId":"1"}`), 200)
	e, _ := eventTestEnvelope(t, nextEventTestFrame(t, adminVideo, time.Second))
	if e.Type != "video.triggered" {
		t.Fatal("admin could not receive shared-source intent")
	}
	noEventTestFrame(t, video)
	nextEventTestFrame(t, messages, time.Second)
	media.mu.Lock()
	media.fail = "all"
	media.mu.Unlock()
	s.publishEvent("video.triggered", "Twitch", "101", new(msg.ChannelID), map[string]string{"clip": "must-not-leak"})
	select {
	case frame, ok := <-video.frames:
		if ok {
			t.Fatalf("unavailable catalog leaked %q", frame)
		}
	case <-time.After(time.Second):
		t.Fatal("video catalog outage kept stream open")
	}
	s.publishEvent("message.created", "Local", "shared", nil, "still-live")
	e, _ = eventTestEnvelope(t, nextEventTestFrame(t, messages, time.Second))
	if e.Data != "still-live" {
		t.Fatal("media outage stopped message-only stream")
	}
	status(t, postVideoTest(s, "key.secret", `{"messageId":"`+strconv.FormatInt(mustResourceID(msg.ID), 10)+`","triggerId":"1"}`), 503)
}

func TestEventStreamExistingRESTUnaffected(t *testing.T) {
	_, s := newResourceFixture(t)
	status(t, request(t, s, "POST", "/api/v3/events", "key.secret", `{}`), 405)
	status(t, request(t, s, "GET", "/api/v3/video-events", "key.secret", ""), 405)
	status(t, request(t, s, "GET", "/api/v3/messages", "key.secret", ""), 200)
	status(t, request(t, s, "GET", "/api/i1/health", "", ""), 200)
	status(t, request(t, s, "GET", "/api/v3/clips", "key.secret", ""), 503)
}
