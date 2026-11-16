package main

import (
	"context"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestScopedLegacyTwitchReadsAndRevocation(t *testing.T) {
	_, f, s := managedMediaFixture(t)
	provisionTwitch(t, f, 30, "7", "101")
	changeProfile(f, 30, func(p *twitchProfile) { p.Enabled = new(true) })
	original := f.nodes[31]
	originalKey := f.legacy.cfg.APIKeys["key"]
	old := addTestKey(f, s, "legacy", "7", "host")
	local := createAppTestChannel(t, s, old, "owned")
	read := issueAppTestToken(t, s, "7", `{"scope":"channel-read","channelIds":["31","`+local+`"]}`)
	if !sameJSON(original, f.nodes[31]) || !sameJSON(originalKey, f.legacy.cfg.APIKeys["key"]) {
		t.Fatal("read-key issuance changed legacy channel or key")
	}
	row := appRow(t, appRequest(t, s, "GET", "/api/v3/channels/31", read, ""), 200)
	if row["ownerUserId"] != nil {
		t.Fatal("legacy channel was adopted")
	}
	status(t, appRequest(t, s, "GET", "/api/v3/channels", read, ""), 200)
	status(t, appRequest(t, s, "GET", "/api/v3/messages", read, ""), 200)
	for _, path := range []string{"/api/v2/messages", "/api/v3/clips", "/api/v3/users/current"} {
		status(t, appRequest(t, s, "GET", path, read, ""), 403)
	}
	status(t, appRequest(t, s, "DELETE", "/api/v3/channels/31", old, ""), 403)
	host := eventTestHost(t, s)
	stream := openEventTestStream(t, host, "", "types=message.created,video.triggered&apiKey="+url.QueryEscape(read))
	p := twitchProfile{}
	cfg, err := s.graph.loadConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	p = cfg.twitch[0]
	e := twitchEvent{BroadcasterUserID: "101", BroadcasterUserLogin: "room", ChatterUserID: "700", ChatterUserLogin: "speaker", ChatterUserName: "Speaker", MessageID: "scope-live", publishedAt: s.now()}
	e.Message.Text = "intent"
	id, err := s.ingestTwitch(context.Background(), p, e)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := eventTestEnvelope(t, nextEventTestFrame(t, stream, time.Second))
	if got.Type != "message.created" || got.ChannelID == nil || *got.ChannelID != "31" {
		t.Fatal("missing legacy event")
	}
	m, err := decodeMessage(f.projection(f.nodes[id]))
	if err != nil {
		t.Fatal(err)
	}
	status(t, postVideoTest(s, old, `{"messageId":"`+m.resource(id).ID+`","triggerId":"1"}`), 200)
	got, _ = eventTestEnvelope(t, nextEventTestFrame(t, stream, time.Second))
	if got.Type != "video.triggered" {
		t.Fatal("missing video snapshot")
	}
	status(t, appRequest(t, s, "GET", "/api/v3/messages/"+m.resource(id).ID, read, ""), 200)
	status(t, appRequest(t, s, "POST", "/api/v3/messages", old, eventLocalMessage("owned", "local")), 201)
	got, _ = eventTestEnvelope(t, nextEventTestFrame(t, stream, time.Second))
	if got.ChannelID == nil || *got.ChannelID != local {
		t.Fatal("merged channel missing")
	}
	f.mu.Lock()
	changeProfile(f, 30, func(p *twitchProfile) { p.Enabled = new(false) })
	f.mu.Unlock()
	status(t, appRequest(t, s, "GET", "/api/v3/channels/31", read, ""), 404)
	history := appRequest(t, s, "GET", "/api/v3/messages", read, "")
	status(t, history, 200)
	if strings.Contains(history.Body.String(), "scope-live") {
		t.Fatal("disabled legacy history leaked")
	}
	status(t, appRequest(t, s, "GET", "/api/v3/messages/"+m.resource(id).ID, read, ""), 404)
	status(t, appRequest(t, s, "GET", "/api/v3/messages/"+m.resource(id).ID, old, ""), 200)
	s.publishEvent("message.created", "Twitch", "101", new("31"), nil)
	select {
	case frame, ok := <-stream.frames:
		if ok {
			t.Fatalf("denied queued frame escaped: %q", frame)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("revoked stream did not close")
	}
	status(t, appRequest(t, s, "GET", "/api/v3/events?types=message.created&apiKey="+url.QueryEscape(read), "", ""), 403)
}

func TestScopedLegacyIssuanceRequiresTargetAssociation(t *testing.T) {
	for _, scenario := range []string{"valid", "disabled", "foreign", "wrong-auth", "wrong-channel", "wrong-broadcaster", "missing", "conflicting", "shared", "empty", "duplicate", "no-ids"} {
		t.Run(scenario, func(t *testing.T) {
			f, s := newResourceFixture(t)
			provisionTwitch(t, f, 30, "7", "101")
			changeProfile(f, 30, func(p *twitchProfile) { p.Enabled = new(true) })
			f.legacy.cfg.Users["8"] = grant{Enabled: true, Permissions: []string{"host"}}
			body := `{"scope":"channel-read","channelIds":["31"]}`
			target := "7"
			switch scenario {
			case "disabled":
				changeProfile(f, 30, func(p *twitchProfile) { p.Enabled = new(false) })
			case "foreign":
				target = "8"
			case "wrong-auth":
				changeProfile(f, 30, func(p *twitchProfile) { p.AuthConfigID = "99" })
			case "wrong-channel":
				changeProfile(f, 30, func(p *twitchProfile) { p.ChannelID = "99" })
			case "wrong-broadcaster":
				changeProfile(f, 30, func(p *twitchProfile) { p.BroadcasterUserID = "102" })
			case "missing":
				delete(f.nodes, 30)
				edges := []relation{}
				for _, edge := range f.edges {
					if edge.Source != 30 && edge.Target != 30 {
						edges = append(edges, edge)
					}
				}
				f.edges = edges
			case "conflicting":
				provisionTwitch(t, f, 40, "8", "101")
				changeProfile(f, 40, func(p *twitchProfile) { p.Enabled = new(true) })
			case "shared":
				status(t, appRequest(t, s, "POST", "/api/v3/messages", "key.secret", eventLocalMessage("shared", "test")), 201)
				for id, n := range f.nodes {
					if n.Type == "channel" && n.Name == identityName("channel", "Local", "shared") {
						body = `{"scope":"channel-read","channelIds":["` + strconv.FormatInt(id, 10) + `"]}`
					}
				}
			case "empty":
				body = `{"scope":"channel-read","channelIds":[]}`
			case "duplicate":
				body = `{"scope":"channel-read","channelIds":["31","31"]}`
			case "no-ids":
				body = `{"scope":"channel-read"}`
			}
			before := f.writes
			w := appRequest(t, s, "POST", "/api/v3/users/"+target+"/tokens", "key.secret", body)
			if scenario == "valid" {
				status(t, w, 201)
				return
			}
			if w.Code < 400 {
				t.Fatalf("untrusted scoped key issued: %s", scenario)
			}
			if f.writes != before {
				t.Fatal("rejected issue wrote storage")
			}
		})
	}
}

func TestScopedLegacyTupleChangeFailsClosed(t *testing.T) {
	f, s := newResourceFixture(t)
	provisionTwitch(t, f, 30, "7", "101")
	changeProfile(f, 30, func(p *twitchProfile) { p.Enabled = new(true) })
	read := issueAppTestToken(t, s, "7", `{"scope":"channel-read","channelIds":["31"]}`)
	status(t, appRequest(t, s, "GET", "/api/v3/channels/31", read, ""), 200)
	changeProfile(f, 30, func(p *twitchProfile) { p.TenantUserID = "8" })
	w := appRequest(t, s, "GET", "/api/v3/channels/31", read, "")
	if w.Code < 400 {
		t.Fatal("changed tuple retained scoped access")
	}
}

func TestScopedLegacyIdleHeartbeatRevocation(t *testing.T) {
	f, s := newResourceFixture(t)
	provisionTwitch(t, f, 30, "7", "101")
	changeProfile(f, 30, func(p *twitchProfile) { p.Enabled = new(true) })
	read := issueAppTestToken(t, s, "7", `{"scope":"channel-read","channelIds":["31"]}`)
	stream := openEventTestStream(t, eventTestHost(t, s), "", "types=message.created&apiKey="+url.QueryEscape(read))
	f.mu.Lock()
	changeProfile(f, 30, func(p *twitchProfile) { p.Enabled = new(false) })
	f.mu.Unlock()
	select {
	case frame, ok := <-stream.frames:
		if ok {
			t.Fatalf("idle revoked stream emitted frame %q", frame)
		}
	case <-time.After(eventHeartbeat + 3*time.Second):
		t.Fatal("idle revocation missed heartbeat")
	}
}
