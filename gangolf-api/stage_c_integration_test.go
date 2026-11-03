package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func stageCGet(t *testing.T, client *http.Client, base, path, token string, query url.Values, value any) int {
	t.Helper()
	target, err := url.Parse(base)
	if err != nil {
		t.Fatal("invalid probe base")
	}
	target.Path = strings.TrimRight(target.Path, "/") + path
	target.RawQuery = query.Encode()
	req, err := http.NewRequest("GET", target.String(), nil)
	if err != nil {
		t.Fatal("invalid probe request")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := client.Do(req)
	if err != nil {
		return 0
	}
	defer res.Body.Close()
	if res.StatusCode == 200 && value != nil {
		if json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(value) != nil {
			t.Fatal("invalid probe JSON")
		}
	} else {
		io.Copy(io.Discard, io.LimitReader(res.Body, 4<<20))
	}
	return res.StatusCode
}

func stageCNodes(t *testing.T, client *http.Client, base, token, root, kind, name string, group int64) []node {
	t.Helper()
	query := url.Values{"rootNodeId": {root}, "type": {kind}, "name": {name}, "fields": {"id,name,type,rootNodeId,ownerId,access,content,contentType,linkDetails"}, "count": {"500"}}
	if group > 0 {
		query.Set("linkedto", strconv.FormatInt(group, 10))
	}
	var page struct {
		Result   []node `json:"result"`
		Total    *int   `json:"total"`
		Continue *int   `json:"continue"`
	}
	if stageCGet(t, client, base, "/nodes", token, query, &page) != 200 || page.Total == nil || page.Result == nil || *page.Total != len(page.Result) || page.Continue != nil {
		t.Fatal("independent graph query failed or exceeded bounded fixture size")
	}
	for _, n := range page.Result {
		if n.Name != name || n.Type != kind {
			t.Fatal("independent exact-key query returned an unexpected node")
		}
	}
	for i := range page.Result {
		sort.Slice(page.Result[i].Links, func(a, b int) bool {
			x, _ := json.Marshal(page.Result[i].Links[a])
			y, _ := json.Marshal(page.Result[i].Links[b])
			return string(x) < string(y)
		})
	}
	return page.Result
}

func startStageCProcess(t *testing.T, fakeURL, directory string) (string, func()) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal("candidate port allocation failed")
	}
	addr := listener.Addr().String()
	listener.Close()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal("candidate executable unavailable")
	}
	cmd := exec.Command(binary, "-test.run=^TestStageCService_Subprocess_NormalStartup$", "-test.timeout=0")
	cmd.Env = append(os.Environ(), "GANGOLF_TEST_STAGE_C_CHILD=1", "GANGOLF_LISTEN="+addr, "GANGOLF_TEST_TWITCH_URL="+fakeURL, "GANGOLF_TWITCH_CREDENTIAL_DIR="+directory)
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if cmd.Start() != nil {
		t.Fatal("candidate process start failed")
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cmd.Process.Signal(syscall.SIGTERM)
			select {
			case err := <-done:
				if err != nil {
					t.Error("candidate exited unsuccessfully")
				}
			case <-time.After(15 * time.Second):
				cmd.Process.Kill()
				<-done
				t.Error("candidate required forced cleanup")
			}
			released, err := net.Listen("tcp", addr)
			if err != nil {
				t.Error("candidate listener was not released")
			} else {
				released.Close()
			}
		})
	}
	t.Cleanup(stop)
	return "http://" + addr, stop
}

func TestServe_RealIsolatedDiVoid_AuthenticatedInboundSurvivesRestart(t *testing.T) {
	if os.Getenv("GANGOLF_STAGE_C_REAL_DIVOID") != "1" {
		t.Skip("requires Operations-owned disposable real DiVoid bindings and cleanup")
	}
	root := os.Getenv("GANGOLF_DIVOID_ROOT_ID")
	runID := os.Getenv("GANGOLF_STAGE_C_RUN_ID")
	apiToken := os.Getenv("GANGOLF_STAGE_C_API_TOKEN")
	if root == "" || os.Getenv("GANGOLF_STAGE_C_OWNED_ROOT_ID") != root || !safeMessageID(runID) || len(runID) > 128 || apiToken == "" {
		t.Fatal("explicit owned fixture root, fresh run ID and API caller authentication required")
	}
	graphBase := os.Getenv("GANGOLF_DIVOID_API_URL")
	if _, ok := loopbackTestURL(graphBase); !ok {
		t.Fatal("real isolated DiVoid must use a literal loopback endpoint")
	}
	s, _, err := bootstrap()
	if err != nil {
		t.Fatal("isolated candidate bindings invalid")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cfg, err := s.graph.loadConfig(ctx)
	if err != nil {
		t.Fatal("isolated fixture configuration invalid or unavailable")
	}
	var profiles []twitchProfile
	for _, p := range cfg.twitch {
		if _, ok := eligibleTwitch(cfg, s.graph.config, p.id); ok {
			profiles = append(profiles, p)
		}
	}
	if len(profiles) != 1 {
		t.Fatal("fixture requires exactly one eligible Twitch configuration")
	}
	p := profiles[0]
	if p.BroadcasterUserID != "101" || p.BotUserID != "900" || p.ClientID != "testclient" || p.CredentialRef != "test-grant" {
		t.Fatal("fixture must use deterministic fake identities and test-grant reference")
	}
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	graphToken := os.Getenv("GANGOLF_DIVOID_TOKEN")
	event := twitchEvent{BroadcasterUserID: "101", BroadcasterUserLogin: "stagec-room", ChatterUserID: strconv.FormatInt(time.Now().UnixNano(), 10), ChatterUserLogin: "stagec-speaker", ChatterUserName: "Stage C Speaker", MessageID: "stage-c-" + runID}
	event.Message.Text = "  Grüße 👩🏽‍💻 日本語\n" + runID + "  "
	name := twitchMessageName(event.BroadcasterUserID, event.MessageID)
	for _, key := range []string{name, twitchMessageName(event.BroadcasterUserID, event.MessageID+"-barrier-before-restart"), twitchMessageName(event.BroadcasterUserID, event.MessageID+"-barrier-after-restart")} {
		if len(stageCNodes(t, client, graphBase, graphToken, root, "message", key, 0)) != 0 {
			t.Fatal("run message already exists; use a fresh disposable run")
		}
	}
	if len(stageCNodes(t, client, graphBase, graphToken, root, "user", identityName("user", "Twitch", event.ChatterUserID), 0)) != 0 {
		t.Fatal("run author already exists; use a fresh disposable run")
	}
	t.Logf("fixture root=%s config=%d event=%s; Operations owns disposal of this dedicated graph", root, p.id, event.MessageID)
	t.Cleanup(func() {
		t.Logf("graph fixture root=%s retained for Operations-owned fixture/identity revocation and removal", root)
	})
	fake := newTwitchFake(t)
	fake.validate = func(r *http.Request) (int, any) {
		if r.Header.Get("Authorization") != "OAuth local-access" {
			return 401, nil
		}
		return 200, map[string]any{"client_id": p.ClientID, "user_id": p.BotUserID, "scopes": []string{"user:read:chat"}, "expires_in": 3600}
	}
	fake.subscribe = func(r *http.Request, sub twitchSubscription) (int, any) {
		if r.Header.Get("Authorization") != "Bearer local-access" || sub.Condition.BroadcasterUserID != p.BroadcasterUserID || sub.Condition.UserID != p.BotUserID {
			return 403, nil
		}
		return 202, map[string]any{"data": []twitchSubscription{fakeSubscription(sub.Transport.SessionID)}}
	}
	deliver := make(chan struct {
		event twitchEvent
		done  chan struct{}
	})
	fake.socket = func(socket *websocket.Conn, session, path string) {
		select {
		case subscribed := <-fake.subscribed:
			if subscribed != session {
				return
			}
		case <-time.After(10 * time.Second):
			return
		}
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			var frame twitchEnvelope
			select {
			case delivery := <-deliver:
				frame = fakeNotification(session, delivery.event)
				if socket.WriteJSON(frame) != nil {
					return
				}
				close(delivery.done)
				continue
			case <-ticker.C:
				frame = fakeEnvelope("session_keepalive")
			}
			if socket.WriteJSON(frame) != nil {
				return
			}
		}
	}
	send := func(value twitchEvent) {
		t.Helper()
		ack := make(chan struct{})
		select {
		case deliver <- struct {
			event twitchEvent
			done  chan struct{}
		}{value, ack}:
		case <-time.After(10 * time.Second):
			t.Fatal("fake did not observe a validated subscription")
		}
		select {
		case <-ack:
		case <-time.After(5 * time.Second):
			t.Fatal("fake delivery failed")
		}
	}
	ready := func(base string) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			if stageCGet(t, client, base, "/api/i1/readiness", "", nil, nil) == 200 {
				if stageCGet(t, client, base, "/api/v3/messages", "", nil, nil) != 401 || stageCGet(t, client, base, "/api/i1/twitch/channels", apiToken, nil, nil) != 200 {
					t.Fatal("candidate authentication boundary failed")
				}
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatal("candidate readiness failed")
	}
	base, stop := startStageCProcess(t, fake.host.URL, fake.client.directory)
	ready(base)
	awaitPublication := func(key string) node {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			rows := stageCNodes(t, client, graphBase, graphToken, root, "message", key, 0)
			if len(rows) == 1 && rows[0].Content != nil && *rows[0].Content != "" {
				return rows[0]
			}
			if len(rows) > 1 {
				t.Fatal("duplicate canonical key")
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatal("receiver did not publish to real graph")
		return node{}
	}
	send(event)
	stored := awaitPublication(name)
	barriers := map[int64]bool{}
	var originalAuthors, originalChannels []node
	verify := func(base string) {
		t.Helper()
		rows := stageCNodes(t, client, graphBase, graphToken, root, "message", name, s.graph.group)
		integrity := stageCNodes(t, client, graphBase, graphToken, root, "message", name, 0)
		if len(rows) != 1 || len(integrity) != 1 || !reflect.DeepEqual(rows[0], stored) || !reflect.DeepEqual(integrity[0], stored) {
			t.Fatal("durable message projection changed or duplicated")
		}
		n := rows[0]
		var m messageContent
		if n.Root != s.graph.root || n.Owner != cfg.serviceOwner || !n.privateAccess() || n.Content == nil || json.Unmarshal([]byte(*n.Content), &m) != nil || m.SchemaVersion != 2 || m.Source == nil || m.Source.Kind != "twitch" || m.Source.ConfigID != strconv.FormatInt(p.id, 10) || m.Source.MessageID != event.MessageID || m.Source.PublishedAt == nil || m.Source.PublishedAt.Format(time.RFC3339) != "2026-11-02T12:00:00Z" || m.Source.BroadcasterLogin != event.BroadcasterUserLogin || m.Source.ChatterLogin != event.ChatterUserLogin || m.ChannelID != p.ChannelID || len(m.Chunks) != 1 || m.Chunks[0].Type != 0 || m.Chunks[0].Data != event.Message.Text || m.User == nil || m.User.Service != "Twitch" || m.User.ServiceID != event.ChatterUserID || m.User.Name != event.ChatterUserName || m.ReplyTo != nil || m.Time.IsZero() {
			t.Fatal("independent schema/source/identity/text readback failed")
		}
		authors := stageCNodes(t, client, graphBase, graphToken, root, "user", identityName("user", "Twitch", event.ChatterUserID), 0)
		channels := stageCNodes(t, client, graphBase, graphToken, root, "channel", identityName("channel", "Twitch", event.BroadcasterUserID), 0)
		for _, identity := range [][]node{authors, channels} {
			if len(identity) != 1 || identity[0].Root != s.graph.root || identity[0].Owner != cfg.serviceOwner || !identity[0].privateAccess() || identity[0].Content == nil {
				t.Fatal("independent identity readback failed")
			}
		}
		for _, pair := range []struct {
			nodes   []node
			context string
		}{{authors, "authored-by"}, {channels, "in-channel"}} {
			links := make([]relation, 0, len(pair.nodes[0].Links))
			seen := map[int64]bool{}
			for _, edge := range pair.nodes[0].Links {
				if barriers[edge.Source] && edge.Target == pair.nodes[0].ID && edge.Type == "Unidirectional" && edge.Context == pair.context && !seen[edge.Source] {
					seen[edge.Source] = true
					continue
				}
				links = append(links, edge)
			}
			if len(seen) != len(barriers) {
				t.Fatal("barrier publication identity links missing")
			}
			pair.nodes[0].Links = links
		}
		if originalAuthors == nil {
			originalAuthors, originalChannels = authors, channels
		} else if !reflect.DeepEqual(originalAuthors, authors) || !reflect.DeepEqual(originalChannels, channels) {
			t.Fatal("redelivery changed identity nodes or links")
		}
		var authorValue, channelValue identityContent
		if json.Unmarshal([]byte(*authors[0].Content), &authorValue) != nil || json.Unmarshal([]byte(*channels[0].Content), &channelValue) != nil || authorValue.SchemaVersion != 1 || channelValue.SchemaVersion != 1 || authorValue.Service != "Twitch" || authorValue.ServiceID != event.ChatterUserID || channelValue.Service != "Twitch" || channelValue.Channel != event.BroadcasterUserID || m.UserID != strconv.FormatInt(authors[0].ID, 10) || m.ChannelID != strconv.FormatInt(channels[0].ID, 10) {
			t.Fatal("identity content or reference mismatch")
		}
		if len(n.Links) != 3 {
			t.Fatal("unexpected message links")
		}
		membership, authored, channel := 0, 0, 0
		for _, e := range n.Links {
			if e.Type == "None" && e.Context == "" && (e.Source == n.ID && e.Target == s.graph.group || e.Target == n.ID && e.Source == s.graph.group) {
				membership++
			}
			if e.Source == n.ID && e.Target == authors[0].ID && e.Type == "Unidirectional" && e.Context == "authored-by" {
				authored++
			}
			if e.Source == n.ID && e.Target == channels[0].ID && e.Type == "Unidirectional" && e.Context == "in-channel" {
				channel++
			}
		}
		if membership != 1 || authored != 1 || channel != 1 {
			t.Fatal("independent link direction/endpoints mismatch")
		}
		if base == "" {
			return
		}
		var resource messageResource
		if stageCGet(t, client, base, "/api/v3/messages/"+strconv.FormatInt(n.ID, 10), apiToken, nil, &resource) != 200 || resource.ID != strconv.FormatInt(n.ID, 10) || len(resource.Chunks) != 1 || resource.Chunks[0].Data != event.Message.Text || !reflect.DeepEqual(resource.Source, m.Source) {
			t.Fatal("authenticated candidate API readback failed")
		}
	}
	redeliver := func(phase string) {
		t.Helper()
		send(event)
		barrier := event
		barrier.MessageID += "-barrier-" + phase
		send(barrier)
		published := awaitPublication(twitchMessageName(barrier.BroadcasterUserID, barrier.MessageID))
		barriers[published.ID] = true
	}
	verify(base)
	redeliver("before-restart")
	stop()
	verify("")
	base, stop = startStageCProcess(t, fake.host.URL, fake.client.directory)
	ready(base)
	verify(base)
	redeliver("after-restart")
	stop()
	verify("")
	t.Logf("real graph/API publication and restart dedupe observed: root=%s message=%d", root, stored.ID)
}
