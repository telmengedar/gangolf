package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestIRCSessionReadinessAndReconnect(t *testing.T) {
	t.Run("readiness-ping-split-coalesced", func(t *testing.T) {
		_, s, tw, p := ircProfileFixture(t)
		fake := newIRCFake(t, tw.client, ircWelcome)
		queue, _ := startIRCFixture(t, s, p)
		waitIRCReady(t, s, p)
		socket := <-fake.connected
		parts := []string{"PING :tmi.twitch.tv\r\n" + ircMessageLine(" Grüße 日本語 👋 ")[:40], ircMessageLine(" Grüße 日本語 👋 ")[40:]}
		for _, part := range parts {
			if _, err := socket.Write([]byte(part)); err != nil {
				t.Fatal(err)
			}
		}
		if frame := takeIRCFrame(t, fake); frame != "PONG :tmi.twitch.tv\r\n" {
			t.Fatal("PING not answered")
		}
		select {
		case e := <-queue:
			if e.ChatterUserID != "700" || e.Message.Text != " Grüße 日本語 👋 " || e.ChatterUserName != "Speaker Name" || e.publishedAt.UnixMilli() != 1794398400123 {
				t.Fatalf("event=%+v", e)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("fragmented message lost")
		}
		if _, err := socket.Write([]byte("@user-id=900 :tmi.twitch.tv USERSTATE #rugenforth\r\n")); err != nil {
			t.Fatal(err)
		}
		assertNoIRCFrame(t, fake)
	})
	for _, mode := range []string{"room-mismatch", "bot-mismatch", "cap-nak", "auth-notice", "missing-join", "missing-cap"} {
		t.Run(mode, func(t *testing.T) {
			f, s, tw, p := ircProfileFixture(t)
			welcome := ircWelcome
			switch mode {
			case "room-mismatch":
				welcome = strings.Replace(welcome, "room-id=101", "room-id=102", 1)
			case "bot-mismatch":
				welcome = strings.Replace(welcome, "user-id=900", "user-id=901", 1)
			case "cap-nak":
				welcome = strings.Replace(welcome, " ACK ", " NAK ", 1)
			case "auth-notice":
				welcome = ":tmi.twitch.tv NOTICE * :Login authentication failed\r\n"
			case "missing-join":
				welcome = strings.Replace(welcome, ":testbot!testbot@testbot.tmi.twitch.tv JOIN #rugenforth\r\n", "", 1)
			case "missing-cap":
				welcome = strings.Replace(welcome, "twitch.tv/commands twitch.tv/tags twitch.tv/membership", "twitch.tv/commands twitch.tv/tags", 1)
			}
			fake := newIRCFake(t, tw.client, welcome)
			_, done := startIRCFixture(t, s, p)
			<-fake.connected
			if strings.HasPrefix(mode, "missing-") {
				status(t, outboundRequest(s, "key.secret", "unready", outboundBody), 503)
				if f.writes != 0 {
					t.Fatal("unready connection reserved intent")
				}
			} else {
				select {
				case err := <-done:
					if err == nil {
						t.Fatal("bad handshake accepted")
					}
				case <-time.After(2 * time.Second):
					t.Fatal("bad handshake not rejected")
				}
			}
			c, err := readProfileCredential(tw.client.directory, p)
			if err != nil || s.twitch.ircReady(p, c) {
				t.Fatal("bad session became ready")
			}
			assertNoIRCFrame(t, fake)
		})
	}
	t.Run("reconnect-never-resends", func(t *testing.T) {
		_, s, tw, p := ircProfileFixture(t)
		fake := newIRCFake(t, tw.client, ircWelcome)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		worker := &twitchWorker{profiles: []twitchProfile{p}}
		go func() { defer close(done); s.receiveTwitch(ctx, worker) }()
		defer func() {
			cancel()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Error("receiver did not stop")
			}
		}()
		waitIRCReady(t, s, p)
		first := <-fake.connected
		response := outboundRequest(s, "key.secret", "before-reconnect", outboundBody)
		status(t, response, 201)
		if !strings.HasPrefix(takeIRCFrame(t, fake), "PRIVMSG #rugenforth :") {
			t.Fatal("initial frame missing")
		}
		if _, err := first.Write([]byte(":tmi.twitch.tv RECONNECT\r\n")); err != nil {
			t.Fatal(err)
		}
		select {
		case <-fake.connected:
		case <-time.After(3 * time.Second):
			t.Fatal("receiver did not reconnect")
		}
		waitIRCReady(t, s, p)
		status(t, outboundRequest(s, "key.secret", "before-reconnect", outboundBody), 200)
		assertNoIRCFrame(t, fake)
	})
}

func TestIRCInboundGraphPublication(t *testing.T) {
	f, s, tw, p := ircProfileFixture(t)
	fake := newIRCFake(t, tw.client, ircWelcome)
	queue, _ := startIRCFixture(t, s, p)
	waitIRCReady(t, s, p)
	socket := <-fake.connected
	text := "  Grüße 👩🏽‍💻 日本語  "
	if _, err := socket.Write([]byte(ircMessageLine(text))); err != nil {
		t.Fatal(err)
	}
	var e twitchEvent
	select {
	case e = <-queue:
	case <-time.After(2 * time.Second):
		t.Fatal("message missing")
	}
	id, err := s.ingestTwitch(context.Background(), p, e)
	if err != nil {
		t.Fatal(err)
	}
	n := f.projection(f.nodes[id])
	m, err := decodeMessage(n)
	if err != nil || m.Source.Transport != "irc" || m.SchemaVersion != 2 || m.Chunks[0].Data != text || m.Channel != "101" || m.User.ServiceID != "700" || m.Source.PublishedAt.UnixMilli() != 1794398400123 || !n.privateAccess() {
		t.Fatalf("publication=%+v err=%v", m, err)
	}
	writes := f.writes
	if got, err := s.ingestTwitch(context.Background(), p, e); err != nil || got != id || f.writes != writes {
		t.Fatal("duplicate publication")
	}
	fresh := *s
	fresh.graph = &graph{base: s.graph.base, token: s.graph.token, root: 10, group: 11, config: 12, http: s.graph.http}
	if got, err := fresh.ingestTwitch(context.Background(), p, e); err != nil || got != id || f.writes != writes {
		t.Fatalf("restart dedupe failed: %v", err)
	}
	for _, bad := range []string{strings.Replace(ircMessageLine(text), "room-id=101", "room-id=102", 1), strings.Replace(ircMessageLine(text), "user-id=700", "user-id=spoof", 1), strings.Replace(ircMessageLine(text), "tmi-sent-ts=1794398400123", "tmi-sent-ts=no", 1), strings.Replace(ircMessageLine(text), "id=chat-irc-1;", "", 1), strings.Replace(ircMessageLine(text), "#rugenforth", "#other", 1)} {
		frame, ok := parseIRCFrame(strings.TrimSuffix(bad, "\r\n"))
		if !ok {
			t.Fatal("malformed-tag scenario not reached")
		}
		if _, ok := ircEvent(frame, p); ok {
			t.Fatal("bad identity accepted")
		}
	}
	f.legacy.cfg.Users["8"] = grant{Enabled: true, Permissions: []string{"host"}}
	other := addTestKey(f, s, "otherirc", "8", "host")
	w := request(t, s, "GET", "/api/v3/messages", other, "")
	status(t, w, 200)
	if strings.Contains(w.Body.String(), "chat-irc-1") {
		t.Fatal("cross-tenant input leak")
	}
	status(t, request(t, s, "GET", deliveryLocation(id), other, ""), 404)
}

func TestIRCOutboundOutcomeAndReplay(t *testing.T) {
	f, s, tw, p := ircProfileFixture(t)
	fake := newIRCFake(t, tw.client, ircWelcome)
	_, _ = startIRCFixture(t, s, p)
	waitIRCReady(t, s, p)
	response := outboundRequest(s, "key.secret", "irc-post", outboundBody)
	status(t, response, 201)
	m := outboundResource(t, response, "submitted")
	if frame := takeIRCFrame(t, fake); frame != "PRIVMSG #rugenforth :!Grüße 日本語 👋\r\n" {
		t.Fatal("incorrect frame")
	}
	if m.Delivery.Transport != "irc" || m.Delivery.MessageID != "" || m.Source != nil || storedOutbound(t, f, m.ID).SchemaVersion != 4 || sends(tw) != 0 {
		t.Fatal("submission incorrectly represented")
	}
	location := deliveryLocation(mustResourceID(m.ID))
	status(t, request(t, s, "GET", location, "key.secret", ""), 200)
	status(t, outboundRequest(s, "key.secret", "irc-post", outboundBody), 200)
	status(t, outboundRequest(s, "key.secret", "irc-post", strings.Replace(outboundBody, "Grüße", "Hello", 1)), 409)
	fresh := *s
	fresh.twitch = nil
	status(t, outboundRequest(&fresh, "key.secret", "irc-post", outboundBody), 200)
	status(t, patchResource(t, s, m.ID, `[{"op":"replace","path":"/chunks","value":[{"type":0,"data":"changed"}]}]`), 422)
	status(t, request(t, s, "DELETE", location, "key.secret", ""), 204)
	status(t, outboundRequest(s, "key.secret", "irc-post", outboundBody), 410)
	tomb := storedOutbound(t, f, m.ID)
	if !tomb.Deleted || tomb.Delivery.State != "submitted" || tomb.Delivery.Transport != "irc" || tomb.User != nil || tomb.Chunks != nil {
		t.Fatal("tombstone lost binding")
	}
	assertNoIRCFrame(t, fake)
}

func TestIRCMessageSchemaReaders(t *testing.T) {
	f, s, tw, p := ircProfileFixture(t)
	fake := newIRCFake(t, tw.client, ircWelcome)
	_, _ = startIRCFixture(t, s, p)
	waitIRCReady(t, s, p)
	response := outboundRequest(s, "key.secret", "irc-schema", outboundBody)
	status(t, response, 201)
	m := outboundResource(t, response, "submitted")
	takeIRCFrame(t, fake)
	n := f.projection(f.nodes[mustResourceID(m.ID)])
	original := *n.Content
	for _, mode := range []string{"sent", "message-id", "empty-message-id", "source", "null-source", "unknown-transport", "schema3", "unknown-field"} {
		var content map[string]any
		if err := json.Unmarshal([]byte(original), &content); err != nil {
			t.Fatal(err)
		}
		delivery := content["delivery"].(map[string]any)
		switch mode {
		case "sent":
			delivery["state"] = "sent"
		case "message-id":
			delivery["messageId"] = "fabricated"
		case "empty-message-id":
			delivery["messageId"] = ""
		case "null-source":
			content["source"] = nil
		case "source":
			content["source"] = map[string]any{"kind": "twitch"}
		case "unknown-transport":
			delivery["transport"] = "other"
		case "schema3":
			content["schemaVersion"] = 3
		case "unknown-field":
			delivery["acknowledged"] = true
		}
		data, _ := json.Marshal(content)
		text := string(data)
		n.Content = &text
		if _, err := decodeMessage(n); err == nil {
			t.Fatalf("invalid schema accepted: %s", mode)
		}
	}
	list := request(t, s, "GET", "/api/v3/messages", "key.secret", "")
	status(t, list, 200)
	if !strings.Contains(list.Body.String(), `"state":"submitted"`) {
		t.Fatal("list lost delivery")
	}
}

func TestParseIRCFrame_EscapesMalformedAndBoundedInput(t *testing.T) {
	f, ok := parseIRCFrame(`@a=hello\sworld\:\\end\r\n\z\ :speaker!host PRIVMSG #rugenforth :雪`)
	if !ok || f.tags["a"] != "hello world;\\end\r\nz" || f.params[1] != "雪" {
		t.Fatalf("tags=%q ok=%v", f.tags["a"], ok)
	}
	for _, line := range []string{"@broken", ":broken", "@a=1;a=2 CMD", "PING :bad\x00", strings.Repeat("x", 8193)} {
		if _, ok := parseIRCFrame(line); ok {
			t.Fatal("malformed frame accepted")
		}
	}
}

func TestSendIRC_StaleClosedSession_RecordsZeroByteFailure(t *testing.T) {
	_, s, tw, p := ircProfileFixture(t)
	a, b := net.Pipe()
	a.Close()
	b.Close()
	credential, err := readProfileCredential(tw.client.directory, p)
	if err != nil {
		t.Fatal(err)
	}
	s.twitch.irc = map[int64]*twitchIRCSession{p.id: {socket: a, profile: p, fingerprint: credential.fingerprint(), ready: true}}
	response := outboundRequest(s, "key.secret", "closed", outboundBody)
	status(t, response, 502)
	replay := outboundRequest(s, "key.secret", "closed", outboundBody)
	status(t, replay, 200)
	if outboundResource(t, replay, "failed").Delivery.Code != "irc_not_written" {
		t.Fatal("zero write classification missing")
	}
}

func TestIRCSession_ExplicitSendValidationFailure_StopsJoinedSession(t *testing.T) {
	_, s, tw, p := ircProfileFixture(t)
	fake := newIRCFake(t, tw.client, ircWelcome)
	_, _ = startIRCFixture(t, s, p)
	waitIRCReady(t, s, p)
	<-fake.connected
	tw.validate = func(*http.Request) (int, any) { return 401, map[string]any{} }
	response := outboundRequest(s, "key.secret", "revoked", outboundBody)
	status(t, response, 503)
	credential, err := readProfileCredential(tw.client.directory, p)
	if err != nil || s.twitch.ircReady(p, credential) {
		t.Fatal("revoked session remains eligible")
	}
	assertNoIRCFrame(t, fake)
}

func TestIRCSession_MissingRoomConfirmation_TimesOutWithoutSend(t *testing.T) {
	_, s, tw, p := ircProfileFixture(t)
	fake := newIRCFake(t, tw.client, strings.Replace(ircWelcome, "@room-id=101 :tmi.twitch.tv ROOMSTATE #rugenforth\r\n", "", 1))
	_, done := startIRCFixture(t, s, p)
	<-fake.connected
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("missing ROOMSTATE accepted")
		}
	case <-time.After(18 * time.Second):
		t.Fatal("readiness deadline not enforced")
	}
	c, err := readProfileCredential(tw.client.directory, p)
	if err != nil || s.twitch.ircReady(p, c) {
		t.Fatal("timed-out session remains ready")
	}
	assertNoIRCFrame(t, fake)
}

func TestIRCSession_OversizedRecord_ClosesBoundedReader(t *testing.T) {
	_, s, tw, p := ircProfileFixture(t)
	fake := newIRCFake(t, tw.client, ircWelcome)
	_, done := startIRCFixture(t, s, p)
	waitIRCReady(t, s, p)
	socket := <-fake.connected
	if _, err := socket.Write([]byte(strings.Repeat("x", 8193))); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("oversized record accepted")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reader not bounded")
	}
	assertNoIRCFrame(t, fake)
}
