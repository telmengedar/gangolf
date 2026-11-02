package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

func inboundFixture(t *testing.T) (*resourceFixture, *server, twitchProfile, twitchEvent) {
	t.Helper()
	f, s := newResourceFixture(t)
	provisionTwitch(t, f, 30, "7", "101")
	changeProfile(f, 30, func(p *twitchProfile) { p.Enabled = new(true) })
	cfg, err := s.graph.loadConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	event := twitchEvent{BroadcasterUserID: "101", BroadcasterUserLogin: "room", ChatterUserID: "700", ChatterUserLogin: "speaker", ChatterUserName: "Speaker", MessageID: "chat-1", publishedAt: time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)}
	event.Message.Text = "  Grüße 👩🏽‍💻 日本語\n!hello  "
	return f, s, cfg.twitch[0], event
}

func TestTwitchInboundUnicodeIdentityTopologyAndSource(t *testing.T) {
	f, s, p, event := inboundFixture(t)
	id, err := s.ingestTwitch(context.Background(), p, event)
	if err != nil {
		t.Fatal(err)
	}
	n := f.projection(f.nodes[id])
	m, err := decodeMessage(n)
	if err != nil || m.SchemaVersion != 2 || m.Source == nil || m.Source.PublishedAt == nil || !m.Source.PublishedAt.Equal(event.publishedAt) || m.Chunks[0].Data != event.Message.Text || m.Time.IsZero() || n.Name != twitchMessageName("101", "chat-1") || n.Root != 10 || n.Owner != 20 || !n.privateAccess() {
		t.Fatalf("invalid publication: %+v %v", m, err)
	}
	if n.Name != "gangolf-twitch-message:WyIxMDEiLCJjaGF0LTEiXQ" {
		t.Fatal(n.Name)
	}
	if err := s.graph.validateRelations(context.Background(), n, m, 20); err != nil {
		t.Fatal(err)
	}
	if len(n.Links) != 3 || m.ChannelID != "31" {
		t.Fatalf("links=%v channel=%s", n.Links, m.ChannelID)
	}
	before := f.writes
	event.ChatterUserName = "New name"
	event.ChatterUserLogin = "renamed"
	event.MessageID = "chat-2"
	if _, err := s.ingestTwitch(context.Background(), p, event); err != nil {
		t.Fatal(err)
	}
	users := 0
	for _, node := range f.nodes {
		if node.Type == "user" {
			users++
		}
	}
	if users != 1 || f.writes <= before {
		t.Fatalf("users=%d writes=%d", users, f.writes)
	}
	w := request(t, s, "GET", "/api/v3/messages", "key.secret", "")
	status(t, w, 200)
	if !strings.Contains(w.Body.String(), `"source":{"kind":"twitch"`) {
		t.Fatal(w.Body.String())
	}
}

func TestTwitchInboundRedeliveryPreservesPatchAndTombstone(t *testing.T) {
	f, s, p, event := inboundFixture(t)
	id, err := s.ingestTwitch(context.Background(), p, event)
	if err != nil {
		t.Fatal(err)
	}
	status(t, patchResource(t, s, strconv.FormatInt(id, 10), `[{"op":"replace","path":"/chunks","value":[{"type":0,"data":"redacted"}]}]`), 200)
	before := f.writes
	if got, err := s.ingestTwitch(context.Background(), p, event); err != nil || got != id || f.writes != before {
		t.Fatalf("duplicate: %d %v writes=%d", got, err, f.writes-before)
	}
	status(t, request(t, s, "DELETE", "/api/v3/messages/"+strconv.FormatInt(id, 10), "key.secret", ""), 204)
	m, err := decodeMessage(f.projection(f.nodes[id]))
	if err != nil || !m.Deleted || m.Source == nil || m.Source.MessageID != "chat-1" || m.Source.PublishedAt != nil || m.Chunks != nil {
		t.Fatalf("tombstone=%+v err=%v", m, err)
	}
	before = f.writes
	fresh := *s
	fresh.graph = &graph{base: s.graph.base, token: s.graph.token, root: 10, group: 11, config: 12, http: s.graph.http}
	if got, err := fresh.ingestTwitch(context.Background(), p, event); err != nil || got != id || f.writes != before {
		t.Fatalf("deleted duplicate: %d %v", got, err)
	}
}

func TestTwitchInboundProcessHelper(t *testing.T) {
	if os.Getenv("GANGOLF_TEST_INGEST") != "1" {
		return
	}
	s, _, err := bootstrap()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := s.graph.loadConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	e := twitchEvent{BroadcasterUserID: "101", BroadcasterUserLogin: "room", ChatterUserID: "700", ChatterUserLogin: "speaker", ChatterUserName: "Speaker", MessageID: "restart-message", publishedAt: time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)}
	e.Message.Text = "restart 🐾"
	if _, err := s.ingestTwitch(context.Background(), cfg.twitch[0], e); err != nil {
		t.Fatal(err)
	}
}

func TestTwitchInboundDedupeAcrossRealProcessRestart(t *testing.T) {
	f, s, _, _ := inboundFixture(t)
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "GANGOLF_TEST_INGEST=1", "GANGOLF_DIVOID_API_URL="+s.graph.base.String(), "GANGOLF_DIVOID_TOKEN=storage-only-test-token", "GANGOLF_API_KEY_PEPPER=test-pepper", "GANGOLF_DIVOID_ROOT_ID=10", "GANGOLF_DIVOID_MESSAGES_ID=11", "GANGOLF_DIVOID_CONFIG_ID=12")
	run := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, binary, "-test.run=^TestTwitchInboundProcessHelper$")
		command.Env = env
		if data, err := command.CombinedOutput(); err != nil {
			t.Fatalf("process: %v %s", err, data)
		}
	}
	run()
	f.mu.Lock()
	before := f.writes
	f.mu.Unlock()
	run()
	f.mu.Lock()
	messages := 0
	var id int64
	for _, n := range f.nodes {
		if n.Name == twitchMessageName("101", "restart-message") {
			messages++
			id = n.ID
		}
	}
	after := f.writes
	f.mu.Unlock()
	if messages != 1 || after != before {
		t.Fatalf("messages=%d extra writes=%d", messages, after-before)
	}
	status(t, request(t, s, "DELETE", "/api/v3/messages/"+strconv.FormatInt(id, 10), "key.secret", ""), 204)
	f.mu.Lock()
	before = f.writes
	f.mu.Unlock()
	run()
	f.mu.Lock()
	after = f.writes
	f.mu.Unlock()
	if after != before {
		t.Fatal("process restart resurrected tombstone")
	}

}

func TestTwitchInboundReplyPresentMissingAndOtherRecipient(t *testing.T) {
	for _, kind := range []string{"present", "missing", "other-recipient", "deleted", "later-parent"} {
		t.Run(kind, func(t *testing.T) {
			f, s, p, event := inboundFixture(t)
			parentID := int64(0)
			if kind != "missing" {
				var err error
				parentID, err = s.ingestTwitch(context.Background(), p, event)
				if err != nil {
					t.Fatal(err)
				}
				if kind == "deleted" {
					status(t, request(t, s, "DELETE", "/api/v3/messages/"+strconv.FormatInt(parentID, 10), "key.secret", ""), 204)
				}
				if kind == "other-recipient" {
					n := f.nodes[parentID]
					n.Name = twitchMessageName("102", "chat-1")
					f.nodes[parentID] = n
				}
			}
			event.MessageID = "reply"
			event.Reply = &struct {
				ParentMessageID string `json:"parent_message_id"`
			}{ParentMessageID: "chat-1"}
			if kind == "later-parent" {
				f.nodes[99] = node{ID: 99, Name: twitchMessageName("101", "reply"), Type: "message", Root: 10, Owner: 20, Access: json.RawMessage("0")}
			}
			id, err := s.ingestTwitch(context.Background(), p, event)
			if err != nil {
				t.Fatal(err)
			}
			m, err := decodeMessage(f.projection(f.nodes[id]))
			if err != nil || m.Source.ParentMessageID != "chat-1" {
				t.Fatal(err)
			}
			if (m.ReplyTo != nil) != (kind == "present") {
				t.Fatalf("reply=%v kind=%s parent=%d", m.ReplyTo, kind, parentID)
			}
		})
	}
}

func TestTwitchInboundPublicationPartialFailureRecovery(t *testing.T) {
	for _, point := range []string{"create:message", "created:message", "link:", "linked:", "link:authored-by", "linked:authored-by", "link:in-channel", "linked:in-channel", "content:message", "committed:message"} {
		t.Run(point, func(t *testing.T) {
			f, s, p, event := inboundFixture(t)
			f.fail = point
			_, firstErr := s.ingestTwitch(context.Background(), p, event)
			if !f.failed {
				t.Fatal("failure point not reached")
			}
			messages := 0
			for _, n := range f.nodes {
				if n.Type == "message" {
					messages++
					if n.Content != nil && *n.Content != "" && len(f.projection(n).Links) != 3 {
						t.Fatal("early publication")
					}
				}
			}
			if messages > 1 {
				t.Fatal("duplicate staging")
			}
			f.fail = ""
			id, err := s.ingestTwitch(context.Background(), p, event)
			if err != nil {
				t.Fatalf("recovery: %v first=%v", err, firstErr)
			}
			n := f.projection(f.nodes[id])
			m, err := decodeMessage(n)
			if err != nil || len(n.Links) != 3 {
				t.Fatalf("projection: %v %v", n, err)
			}
			if err := s.graph.validateRelations(context.Background(), n, m, 20); err != nil {
				t.Fatal(err)
			}
			count := 0
			for _, n := range f.nodes {
				if n.Name == twitchMessageName("101", "chat-1") {
					count++
				}
			}
			if count != 1 {
				t.Fatalf("count=%d", count)
			}
		})
	}
}

func TestTwitchInboundRejectsCorruptKeysAndSpoofedSourceWithoutWrites(t *testing.T) {
	for _, kind := range []string{"duplicate", "public", "owner", "foreign-edge", "wrong-author", "nonempty", "missing-profile", "source-spoof", "schema-spoof", "patch-source", "disabled", "two-enabled"} {
		t.Run(kind, func(t *testing.T) {
			f, s, p, event := inboundFixture(t)
			if kind == "source-spoof" || kind == "schema-spoof" {
				field := `"source":{"kind":"twitch"}`
				if kind == "schema-spoof" {
					field = `"schemaVersion":2`
				}
				before := f.writes
				status(t, request(t, s, "POST", "/api/v3/messages", "key.secret", strings.TrimSuffix(v3Message, "}")+","+field+"}"), 400)
				if f.writes != before {
					t.Fatal("spoof wrote")
				}
				return
			}
			id, err := s.ingestTwitch(context.Background(), p, event)
			if err != nil {
				t.Fatal(err)
			}
			n := f.nodes[id]
			switch kind {
			case "duplicate":
				copy := n
				copy.ID = 900
				f.nodes[900] = copy
			case "public":
				n.Access = json.RawMessage("1")
				f.nodes[id] = n
			case "owner":
				n.Owner = 77
				f.nodes[id] = n
			case "foreign-edge":
				f.edges = append(f.edges, relation{Source: id, Target: 999, Type: "None"})
			case "wrong-author":
				event.ChatterUserID = "701"
			case "nonempty":
				body := "{}"
				n.Content = &body
				f.nodes[id] = n
			case "missing-profile":
				delete(f.nodes, 30)
			case "disabled":
				changeProfile(f, 30, func(p *twitchProfile) { p.Enabled = new(false) })
			case "two-enabled":
				provisionTwitch(t, f, 40, "7", "102")
				changeProfile(f, 40, func(p *twitchProfile) { p.Enabled = new(true) })
			case "patch-source":
				status(t, patchResource(t, s, strconv.FormatInt(id, 10), `[{"op":"replace","path":"/source","value":{}}]`), 400)
				return
			}
			before := f.writes
			if _, err := s.ingestTwitch(context.Background(), p, event); err == nil {
				t.Fatal("corruption accepted")
			}
			if f.writes != before {
				t.Fatal("corruption repaired")
			}
		})
	}
}

func TestTwitchInboundSharedChatUsesRecipientAndRetainsProvenance(t *testing.T) {
	f, s, p, event := inboundFixture(t)
	event.SourceBroadcasterUserID = "999"
	event.SourceMessageID = "shared-1"
	id, err := s.ingestTwitch(context.Background(), p, event)
	if err != nil {
		t.Fatal(err)
	}
	m, err := decodeMessage(f.projection(f.nodes[id]))
	if err != nil || m.Channel != "101" || m.Source.SourceBroadcasterUserID != "999" || m.Source.SourceMessageID != "shared-1" {
		t.Fatalf("message=%+v err=%v", m, err)
	}
}

func TestTwitchInboundWhitespaceOnlyTextIsPreserved(t *testing.T) {
	f, s, p, event := inboundFixture(t)
	event.Message.Text = " \t\n  "
	id, err := s.ingestTwitch(context.Background(), p, event)
	if err != nil {
		t.Fatal(err)
	}
	m, err := decodeMessage(f.projection(f.nodes[id]))
	if err != nil || m.Chunks[0].Data != event.Message.Text {
		t.Fatal("text changed")
	}
}

func TestTwitchInboundAlternateProfileRedeliveryRetainsOriginalSource(t *testing.T) {
	f, s, p, event := inboundFixture(t)
	id, err := s.ingestTwitch(context.Background(), p, event)
	if err != nil {
		t.Fatal(err)
	}
	changeProfile(f, 30, func(p *twitchProfile) { p.Enabled = new(false) })
	alternate := p
	alternate.id = 40
	data, _ := json.Marshal(alternate)
	body := string(data)
	f.nodes[40] = node{ID: 40, Name: "Twitch inbound — alternate — user-7 — room-101", Type: twitchConfigType, Root: 10, Owner: 20, Access: json.RawMessage("0"), SkipEmbedding: new(true), Content: &body, ContentType: "application/json"}
	f.edges = append(f.edges, relation{Source: 40, Target: 15, Type: "None"}, relation{Source: 40, Target: 31, Type: "Unidirectional", Context: "configured-for"}, relation{Source: 40, Target: 12, Type: "Unidirectional", Context: "uses-auth-config"})
	cfg, err := s.graph.loadConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	active, ok := eligibleTwitch(cfg, s.graph.config)
	if !ok || active.id != 40 {
		t.Fatal("alternate did not activate")
	}
	before := f.writes
	duplicate, err := s.ingestTwitch(context.Background(), active, event)
	if err != nil || duplicate != id || f.writes != before {
		t.Fatalf("duplicate=%d err=%v", duplicate, err)
	}
	m, err := decodeMessage(f.projection(f.nodes[id]))
	if err != nil || m.Source.ConfigID != "30" {
		t.Fatal("source changed")
	}
}

func TestTwitchInboundRejectsInvalidStagingBeforeIdentityWrites(t *testing.T) {
	for _, kind := range []string{"foreign-link", "wrong-channel", "wrong-author", "duplicate-membership", "wrong-direction", "nonempty", "hidden-published"} {
		t.Run(kind, func(t *testing.T) {
			f, s, p, event := inboundFixture(t)
			n := node{ID: 99, Name: twitchMessageName("101", "chat-1"), Type: "message", Root: 10, Owner: 20, Access: json.RawMessage("0")}
			switch kind {
			case "foreign-link":
				f.edges = append(f.edges, relation{Source: 99, Target: 900, Type: "None"})
			case "wrong-channel":
				f.edges = append(f.edges, relation{Source: 99, Target: 900, Type: "Unidirectional", Context: "in-channel"})
			case "wrong-author":
				f.edges = append(f.edges, relation{Source: 99, Target: 900, Type: "Unidirectional", Context: "authored-by"})
			case "duplicate-membership":
				f.edges = append(f.edges, relation{Source: 99, Target: 11, Type: "None"}, relation{Source: 99, Target: 11, Type: "None"})
			case "wrong-direction":
				f.edges = append(f.edges, relation{Source: 31, Target: 99, Type: "Unidirectional", Context: "in-channel"})
			case "nonempty", "hidden-published":
				body := "{}"
				n.Content = &body
				n.ContentType = "application/json"
			}
			f.nodes[99] = n
			before := f.writes
			if _, err := s.ingestTwitch(context.Background(), p, event); err == nil {
				t.Fatal("invalid staging accepted")
			}
			if f.writes != before {
				t.Fatal("invalid staging caused identity side effects")
			}
		})
	}
}

func TestTwitchInboundIdentityPartialFailuresReuseOriginalIdentity(t *testing.T) {
	for _, point := range []string{"create:user", "created:user", "link:", "linked:", "content:user", "committed:user"} {
		t.Run(point, func(t *testing.T) {
			f, s, p, event := inboundFixture(t)
			f.fail = point
			s.ingestTwitch(context.Background(), p, event)
			if !f.failed {
				t.Fatal("failure not reached")
			}
			f.fail = ""
			if _, err := s.ingestTwitch(context.Background(), p, event); err != nil {
				t.Fatal(err)
			}
			users := 0
			for _, n := range f.nodes {
				if n.Type == "user" {
					users++
				}
			}
			if users != 1 {
				t.Fatalf("users=%d", users)
			}
		})
	}
}

func TestTwitchInboundAndLegacySchemasRemainSeparateAndPrivate(t *testing.T) {
	f, s, p, event := inboundFixture(t)
	legacy := createResource(t, s, v3Message)
	id, err := s.ingestTwitch(context.Background(), p, event)
	if err != nil {
		t.Fatal(err)
	}
	status(t, request(t, s, "POST", "/api/v2/messages", "key.secret", "["+v3Message+"]"), 201)
	status(t, request(t, s, "GET", "/api/v3/messages/"+legacy.ID, "key.secret", ""), 200)
	status(t, request(t, s, "GET", "/api/v3/messages/"+strconv.FormatInt(id, 10), "key.secret", ""), 200)
	status(t, request(t, s, "GET", "/api/v2/messages", "key.secret", ""), 200)
	for _, n := range f.nodes {
		if !n.privateAccess() || n.Root != 10 || n.Owner != 20 {
			t.Fatal("nonprivate graph write")
		}
	}
	for _, q := range f.queries {
		if q.Get("rootNodeId") != "10" {
			t.Fatal("unscoped query")
		}
		if q.Get("linkedto") == "" && q.Get("name") == "" {
			t.Fatal("unbounded integrity query")
		}
	}
	status(t, request(t, s, "DELETE", "/api/v3/messages/"+legacy.ID, "key.secret", ""), 204)
	n := f.nodes[mustResourceID(legacy.ID)]
	tombstone, err := decodeMessage(f.projection(n))
	if err != nil || tombstone.SchemaVersion != 1 || tombstone.Source != nil || !tombstone.Deleted {
		t.Fatal("schema1 tombstone changed")
	}
}

func TestTwitchInboundSharedSourceAcrossRecipientsRemainsTenantIsolated(t *testing.T) {
	f, s, p, event := inboundFixture(t)
	event.SourceBroadcasterUserID = "999"
	event.SourceMessageID = "shared-source"
	first, err := s.ingestTwitch(context.Background(), p, event)
	if err != nil {
		t.Fatal(err)
	}
	changeProfile(f, 30, func(p *twitchProfile) { p.Enabled = new(false) })
	provisionTwitch(t, f, 40, "8", "102")
	changeProfile(f, 40, func(p *twitchProfile) { p.Enabled = new(true) })
	f.legacy.cfg.Users["8"] = grant{Enabled: true, Permissions: []string{"host"}}
	cfg, err := s.graph.loadConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	active, ok := eligibleTwitch(cfg, s.graph.config)
	if !ok || active.id != 40 {
		t.Fatal("second recipient not eligible")
	}
	event.BroadcasterUserID = "102"
	second, err := s.ingestTwitch(context.Background(), active, event)
	if err != nil || second == first {
		t.Fatalf("second=%d first=%d err=%v", second, first, err)
	}
	host := addTestKey(f, s, "tenant-a", "7", "host")
	w := request(t, s, "GET", "/api/v3/messages", host, "")
	status(t, w, 200)
	var result struct {
		Result []messageResource
		Total  int
	}
	if json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Total != 1 || len(result.Result) != 1 || result.Result[0].ID != strconv.FormatInt(first, 10) {
		t.Fatal(w.Body.String())
	}
	status(t, request(t, s, "GET", "/api/v3/messages/"+strconv.FormatInt(second, 10), host, ""), 404)
	status(t, request(t, s, "DELETE", "/api/v3/messages/"+strconv.FormatInt(second, 10), host, ""), 404)
	status(t, request(t, s, "GET", "/api/v3/messages/"+strconv.FormatInt(first, 10), host, ""), 200)
}

func TestTwitchInboundStagingAndSourceBindingAreStrict(t *testing.T) {
	for _, kind := range []string{"legacy-name", "arbitrary-name", "schema-one", "source-message", "source-kind", "source-config", "source-published-at", "source-channel", "reply-provenance", "staging-not-listed"} {
		t.Run(kind, func(t *testing.T) {
			f, s, p, event := inboundFixture(t)
			id, err := s.ingestTwitch(context.Background(), p, event)
			if err != nil {
				t.Fatal(err)
			}
			n := f.nodes[id]
			var m messageContent
			if json.Unmarshal([]byte(*n.Content), &m) != nil {
				t.Fatal("missing publication")
			}
			switch kind {
			case "legacy-name":
				n.Name = "message"
			case "arbitrary-name":
				n.Name = "arbitrary"
			case "schema-one":
				m.SchemaVersion = 1
			case "source-message":
				m.Source.MessageID = "other"
			case "source-kind":
				m.Source.Kind = "other"
			case "source-config":
				m.Source.ConfigID = "900"
			case "source-published-at":
				m.Source.PublishedAt = nil
			case "source-channel":
				m.Channel = "102"
			case "reply-provenance":
				parent := strconv.FormatInt(id, 10)
				m.ReplyTo = &parent
			case "staging-not-listed":
				n.Content = nil
			}
			if kind != "staging-not-listed" {
				data, _ := json.Marshal(m)
				body := string(data)
				n.Content = &body
			}
			f.nodes[id] = n
			before := f.writes
			w := request(t, s, "GET", "/api/v3/messages", "key.secret", "")
			if kind == "staging-not-listed" {
				status(t, w, 200)
				if !strings.Contains(w.Body.String(), `"total":0`) {
					t.Fatal(w.Body.String())
				}
			} else {
				status(t, w, 500)
			}
			if f.writes != before {
				t.Fatal("read repaired corrupted source")
			}
		})
	}
}
