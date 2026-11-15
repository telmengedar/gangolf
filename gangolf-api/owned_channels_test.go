package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func managedMediaFixture(t *testing.T) (*mediaFixture, *resourceFixture, *server) {
	t.Helper()
	media, f, s := newMediaFixture(t)
	user := f.legacy.cfg.Users["7"]
	user.Permissions = []string{"admin", "host"}
	f.legacy.cfg.Users["7"] = user
	key := f.legacy.cfg.APIKeys["key"]
	key.Permissions = []string{"admin", "host"}
	f.legacy.cfg.APIKeys["key"] = key
	catalog := media.state(t)
	catalog.Clips = []mediaClip{{ID: "1", Provider: "youtube", RemoteVideoID: "abcdefghijk"}}
	catalog.Triggers = []mediaTrigger{{ID: "1", Text: "intent", ClipID: new("1")}}
	media.set(catalog)
	return media, f, s
}

func TestOwnedChannelMessageHistoryAndEvents(t *testing.T) {
	media, f, s := managedMediaFixture(t)
	alice := createAppTestUser(t, s, "Alice")
	bob := createAppTestUser(t, s, "Bob")
	a := issueAppTestToken(t, s, alice, `{}`)
	b := issueAppTestToken(t, s, bob, `{}`)
	channel := createAppTestChannel(t, s, a, "alice")
	read := issueAppTestToken(t, s, alice, `{"scope":"channel-read","channelIds":["`+channel+`"]}`)
	host := eventTestHost(t, s)
	stream := openEventTestStream(t, host, a, "types=message.created,video.triggered&service=Local&channel=alice")
	urlStream := openEventTestStream(t, host, "", "types=message.created,video.triggered&apiKey="+url.QueryEscape(read))
	foreign := openEventTestStream(t, host, b, "types=message.created,video.triggered")
	before := media.state(t)
	response := appRequest(t, s, "POST", "/api/v3/messages", a, eventLocalMessage("alice", "owned"))
	status(t, response, 201)
	var message messageResource
	json.Unmarshal(response.Body.Bytes(), &message)
	if message.ChannelID != channel {
		t.Fatal("message used alternate root identity")
	}
	for _, target := range []*eventTestStream{stream, urlStream} {
		envelope, _ := eventTestEnvelope(t, nextEventTestFrame(t, target, time.Second))
		if envelope.Type != "message.created" || envelope.ChannelID == nil || *envelope.ChannelID != channel {
			t.Fatal("wrong own message event")
		}
	}
	noEventTestFrame(t, foreign)
	status(t, postVideoTest(s, "key.secret", `{"messageId":"`+message.ID+`","triggerId":"1"}`), 200)
	for _, target := range []*eventTestStream{stream, urlStream} {
		envelope, data := eventTestEnvelope(t, nextEventTestFrame(t, target, time.Second))
		if envelope.Type != "video.triggered" || string(data["messageId"]) != `"`+message.ID+`"` {
			t.Fatal("wrong own video event")
		}
	}
	noEventTestFrame(t, foreign)
	if !sameJSON(before, media.state(t)) {
		t.Fatal("event changed catalog tenant or content")
	}
	history := appRequest(t, s, "GET", "/api/v3/messages?channelId="+channel+"&apiKey="+url.QueryEscape(read), "", "")
	status(t, history, 200)
	if !strings.Contains(history.Body.String(), "owned") {
		t.Fatal("owner history missing")
	}
	other := appRequest(t, s, "GET", "/api/v3/messages", b, "")
	status(t, other, 200)
	if strings.Contains(other.Body.String(), "owned") {
		t.Fatal("foreign history leaked")
	}
	status(t, appRequest(t, s, "GET", "/api/v3/messages/"+message.ID, b, ""), 404)
	status(t, appRequest(t, s, "POST", "/api/v3/messages", b, eventLocalMessage("alice", "foreign")), 403)
	status(t, appRequest(t, s, "GET", "/api/v3/clips", a, ""), 403)
	status(t, postVideoTest(s, a, `{"messageId":"`+message.ID+`","triggerId":"1"}`), 403)
	status(t, appRequest(t, s, "GET", "/api/v3/events?types=message.created&service=Local&channel=bob&apiKey="+url.QueryEscape(read), "", ""), 403)
	status(t, appRequest(t, s, "POST", "/api/v2/messages", a, "["+legacyLocalMessage("alice", "legacy owned")+"]"), 201)
	for _, target := range []*eventTestStream{stream, urlStream} {
		nextEventTestFrame(t, target, time.Second)
	}
	status(t, appRequest(t, s, "GET", "/api/v2/messages?apiKey="+url.QueryEscape(read), "", ""), 403)
	f.mu.Lock()
	nodes := 0
	for _, n := range f.nodes {
		if n.Type == "channel" && n.Name == identityName("channel", "Local", "alice") {
			nodes++
		}
	}
	f.mu.Unlock()
	if nodes != 1 {
		t.Fatal("producer created duplicate channel")
	}
}

func TestManagedChannelLifecycle(t *testing.T) {
	_, s := newResourceFixture(t)
	alice := createAppTestUser(t, s, "Alice")
	bob := createAppTestUser(t, s, "Bob")
	a := issueAppTestToken(t, s, alice, `{}`)
	b := issueAppTestToken(t, s, bob, `{}`)
	channel := createAppTestChannel(t, s, a, "one")
	status(t, appRequest(t, s, "GET", "/api/v3/channels/"+channel, b, ""), 404)
	status(t, appRequest(t, s, "PUT", "/api/v3/channels/"+channel, b, `{"service":"Local","channel":"one","enabled":false}`), 404)
	status(t, appRequest(t, s, "POST", "/api/v3/channels", b, `{"service":"Local","channel":"one"}`), 409)
	status(t, appRequest(t, s, "POST", "/api/v3/channels", b, `{"service":"Local","channel":"two","ownerUserId":"`+alice+`"}`), 403)
	status(t, appRequest(t, s, "POST", "/api/v3/messages", a, eventLocalMessage("one", "retained history")), 201)
	status(t, appRequest(t, s, "PUT", "/api/v3/channels/"+channel, a, `{"service":"Local","channel":"one","enabled":false}`), 200)
	status(t, appRequest(t, s, "POST", "/api/v3/messages", a, eventLocalMessage("one", "blocked")), 403)
	status(t, appRequest(t, s, "POST", "/api/v2/messages", a, "["+legacyLocalMessage("one", "blocked")+"]"), 403)
	history := appRequest(t, s, "GET", "/api/v3/messages", a, "")
	status(t, history, 200)
	if !strings.Contains(history.Body.String(), "retained history") {
		t.Fatal("disabled history lost")
	}
	status(t, appRequest(t, s, "PUT", "/api/v3/channels/"+channel, a, `{"service":"Local","channel":"other","enabled":true}`), 400)
	status(t, appRequest(t, s, "DELETE", "/api/v3/channels/"+channel, a, ""), 204)
	status(t, appRequest(t, s, "GET", "/api/v3/channels/"+channel, a, ""), 404)
	status(t, appRequest(t, s, "POST", "/api/v3/channels", a, `{"service":"Local","channel":"one"}`), 409)
	status(t, appRequest(t, s, "GET", "/api/v3/messages", a, ""), 200)
}

func TestOwnedChannelLegacyVisibilityBoundary(t *testing.T) {
	f, s := newResourceFixture(t)
	id := createAppTestUser(t, s, "Alice")
	token := issueAppTestToken(t, s, id, `{}`)
	createAppTestChannel(t, s, token, "owned")
	status(t, appRequest(t, s, "POST", "/api/v3/messages", "key.secret", eventLocalMessage("shared", "legacy content")), 201)
	status(t, appRequest(t, s, "POST", "/api/v3/messages", token, eventLocalMessage("owned", "private content")), 201)
	status(t, appRequest(t, s, "POST", "/api/v2/messages", "key.secret", "["+legacyLocalMessage("shared", "legacy batch")+"]"), 201)
	f.legacy.cfg.Users["8"] = grant{Enabled: true, Permissions: []string{"host"}}
	legacy := addTestKey(f, s, "legacy", "8", "host")
	for _, version := range []string{"2", "3"} {
		current := appRequest(t, s, "GET", "/api/v"+version+"/messages", token, "")
		status(t, current, 200)
		if strings.Contains(current.Body.String(), "legacy content") || strings.Contains(current.Body.String(), "legacy batch") {
			t.Fatal("new user inherited shared history")
		}
		old := appRequest(t, s, "GET", "/api/v"+version+"/messages", legacy, "")
		status(t, old, 200)
		if strings.Contains(old.Body.String(), "private content") {
			t.Fatal("legacy fallback exposed managed channel")
		}
		if !strings.Contains(old.Body.String(), "legacy") {
			t.Fatal("legacy shared behavior lost")
		}
	}
	status(t, appRequest(t, s, "POST", "/api/v3/messages", token, eventLocalMessage("unregistered", "blocked")), 403)
}

func TestManagedChannelTopologyAndUniqueness(t *testing.T) {
	for _, corruption := range []string{"root", "owner", "public", "binding", "embedding", "duplicate", "schema", "profile-content"} {
		t.Run(corruption, func(t *testing.T) {
			f, s := newResourceFixture(t)
			id := createAppTestUser(t, s, "Alice")
			token := issueAppTestToken(t, s, id, `{}`)
			channel := createAppTestChannel(t, s, token, "one")
			nodeID, _ := resourceID(channel)
			n := f.nodes[nodeID]
			if n.Root == 10 || n.Owner != 20 || !n.privateAccess() || !exactMembership(f.projection(n), 14) {
				t.Fatal("invalid fixture topology")
			}
			switch corruption {
			case "root":
				n.Root = 999
			case "owner":
				n.Owner = 99
			case "public":
				n.Access = json.RawMessage("2")
			case "binding":
				user := f.legacy.cfg.Users[id]
				user.ProfileNodeID = ""
				f.legacy.cfg.Users[id] = user
			case "embedding":
				n.SkipEmbedding = new(false)
			case "duplicate":
				copy := n
				copy.ID = 999
				copy.Root = 10
				copy.Content = new(`{"schemaVersion":1,"service":"Local","channel":"one"}`)
				f.nodes[999] = copy
				f.edges = append(f.edges, relation{Source: 999, Target: 14, Type: "None"})
			case "profile-content":
				profile := f.nodes[n.Root]
				var value appUser
				json.Unmarshal([]byte(*profile.Content), &value)
				value.UserID = "999"
				raw, _ := json.Marshal(value)
				profile.Content = new(string(raw))
				f.nodes[profile.ID] = profile
			case "schema":
				n.Content = new(`{"schemaVersion":1,"service":"Local","channel":"one"}`)
			}
			f.nodes[nodeID] = n
			w := appRequest(t, s, "GET", "/api/v3/channels/"+channel, token, "")
			if w.Code == 200 {
				t.Fatal("invalid owner topology accepted")
			}
			before := f.writes
			w = appRequest(t, s, "POST", "/api/v3/messages", token, eventLocalMessage("one", "must not write"))
			if w.Code < 400 || f.writes != before {
				t.Fatal("invalid topology permitted producer")
			}
		})
	}
}

func TestOwnedChannelFreshAuthorization(t *testing.T) {
	for _, revocation := range []string{"channel", "key", "user", "owner-binding"} {
		t.Run(revocation, func(t *testing.T) {
			_, f, s := managedMediaFixture(t)
			id := createAppTestUser(t, s, "Alice")
			token := issueAppTestToken(t, s, id, `{}`)
			channel := createAppTestChannel(t, s, token, "one")
			stream := openEventTestStream(t, eventTestHost(t, s), token, "types=message.created&service=Local&channel=one")
			f.mu.Lock()
			f.legacy.mu.Lock()
			switch revocation {
			case "channel":
				n := f.nodes[mustResourceID(channel)]
				v, _ := channelIdentity(n)
				v.Enabled = new(false)
				raw, _ := json.Marshal(v)
				n.Content = new(string(raw))
				f.nodes[n.ID] = n
			case "key":
				keyID, _, _ := strings.Cut(token, ".")
				delete(f.legacy.cfg.APIKeys, keyID)
			case "user":
				u := f.legacy.cfg.Users[id]
				u.Enabled = false
				f.legacy.cfg.Users[id] = u
			case "owner-binding":
				u := f.legacy.cfg.Users[id]
				u.ProfileNodeID = ""
				f.legacy.cfg.Users[id] = u
			}
			f.legacy.mu.Unlock()
			f.mu.Unlock()
			s.publishEvent("message.created", "Local", "one", &channel, "forbidden queued payload")
			select {
			case _, ok := <-stream.frames:
				if ok {
					t.Fatal("revoked event leaked")
				}
			case <-time.After(time.Second):
				t.Fatal("revoked stream stayed active")
			}
		})
	}
}

func TestManagedChannelProducerIntegration(t *testing.T) {
	f, s := newResourceFixture(t)
	userID := createAppTestUser(t, s, "Twitch owner")
	token := issueAppTestToken(t, s, userID, `{}`)
	row := appRow(t, appRequest(t, s, "POST", "/api/v3/channels", token, `{"service":"Twitch","channel":"101"}`), 201)
	channelID := row["id"].(string)
	profile := provisionTwitch(t, f, 60, userID, "101")
	oldID := mustResourceID(profile.ChannelID)
	delete(f.nodes, oldID)
	filtered := []relation{}
	for _, edge := range f.edges {
		if edge.Source == 60 && edge.Context == "configured-for" {
			edge.Target = mustResourceID(channelID)
		}
		if edge.Source == oldID || edge.Target == oldID {
			continue
		}
		filtered = append(filtered, edge)
	}
	f.edges = filtered
	changeProfile(f, 60, func(value *twitchProfile) { value.ChannelID = channelID; value.Enabled = new(true) })
	cfg, err := s.graph.loadConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	profile = cfg.twitch[0]
	event := twitchEvent{BroadcasterUserID: "101", BroadcasterUserLogin: "room", ChatterUserID: "700", ChatterUserLogin: "speaker", ChatterUserName: "Speaker", MessageID: "managed-inbound", publishedAt: time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)}
	event.Message.Text = "managed inbound"
	id, err := s.ingestTwitch(context.Background(), profile, event)
	if err != nil {
		t.Fatal(err)
	}
	before := f.writes
	again, err := s.ingestTwitch(context.Background(), profile, event)
	if err != nil || again != id || before != f.writes {
		t.Fatal("managed replay duplicated publication")
	}
	status(t, appRequest(t, s, "GET", "/api/v3/messages", token, ""), 200)
	tw := newTwitchFake(t)
	tw.validate = func(r *http.Request) (int, any) {
		return 200, map[string]any{"client_id": "testclient", "user_id": "900", "scopes": []string{"user:read:chat", "user:write:chat"}, "expires_in": 3600}
	}
	tw.send = func(w http.ResponseWriter, r *http.Request) {
		respond(w, 200, map[string]any{"data": []map[string]any{{"message_id": "sent", "is_sent": true}}})
	}
	s.twitch = &twitchRuntime{client: tw.client}
	response := outboundRequest(s, token, "managed-send", outboundBody)
	status(t, response, 201)
	var out messageResource
	json.Unmarshal(response.Body.Bytes(), &out)
	if out.ChannelID != channelID || sends(tw) != 1 {
		t.Fatal("outbound did not reuse managed channel")
	}
	status(t, outboundRequest(s, token, "managed-send", outboundBody), 200)
	if sends(tw) != 1 {
		t.Fatal("managed outbound replay sent twice")
	}
	n := f.nodes[mustResourceID(channelID)]
	v, _ := channelIdentity(n)
	v.Enabled = new(false)
	raw, _ := json.Marshal(v)
	n.Content = new(string(raw))
	f.nodes[n.ID] = n
	event.MessageID = "disabled-inbound"
	if _, err = s.ingestTwitch(context.Background(), profile, event); err == nil {
		t.Fatal("disabled managed ingest allowed")
	}
	status(t, outboundRequest(s, token, "disabled-send", outboundBody), 403)
	if sends(tw) != 1 {
		t.Fatal("disabled managed outbound sent")
	}
}

func legacyLocalMessage(channel, text string) string {
	var input messageInput
	json.Unmarshal([]byte(eventLocalMessage(channel, text)), &input)
	data, _ := json.Marshal(input.snapshot())
	return string(data)
}

func TestManagedChannelUnconfirmedRegistrationHasNoWriteGrant(t *testing.T) {
	f, s := newResourceFixture(t)
	userID := createAppTestUser(t, s, "Alice")
	token := issueAppTestToken(t, s, userID, `{}`)
	f.fail = "content:channel"
	status(t, appRequest(t, s, "POST", "/api/v3/channels", token, `{"service":"Local","channel":"staged"}`), 503)
	count := 0
	for _, n := range f.nodes {
		if n.Type == "channel" {
			count++
			if n.Content != nil || n.Root == 10 {
				t.Fatal("staging scenario absent")
			}
		}
	}
	if count != 1 {
		t.Fatal("staged channel not created exactly once")
	}
	before := f.writes
	response := appRequest(t, s, "POST", "/api/v3/messages", token, eventLocalMessage("staged", "must not publish"))
	if response.Code < 400 || f.writes != before {
		t.Fatal("unconfirmed channel granted producer or shadow fallback")
	}
}
