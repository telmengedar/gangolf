package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type historyTestRead struct {
	entered  chan int
	release  chan struct{}
	attempts atomic.Int64
}

func blockHistoryReads(t *testing.T, s *server, blocked int) *historyTestRead {
	t.Helper()
	barrier := &historyTestRead{entered: make(chan int, 8), release: make(chan struct{}, 8)}
	var mu sync.Mutex
	seen := make(map[*graphReadMemo]bool)
	original := s.graph.http
	s.graph.http = &http.Client{Transport: videoTestTransport{base: original.Transport, before: func(r *http.Request) error {
		memo, _ := r.Context().Value(graphReadMemoKey).(*graphReadMemo)
		if memo == nil || r.URL.Query().Get("type") != "message" {
			return nil
		}
		mu.Lock()
		first := !seen[memo]
		seen[memo] = true
		mu.Unlock()
		if !first {
			return nil
		}
		attempt := int(barrier.attempts.Add(1))
		if attempt > blocked {
			return nil
		}
		barrier.entered <- attempt
		select {
		case <-barrier.release:
			return nil
		case <-r.Context().Done():
			return r.Context().Err()
		}
	}}}
	t.Cleanup(func() {
		for range blocked {
			barrier.release <- struct{}{}
		}
	})
	return barrier
}

func historyRequest(s *server, ctx context.Context, token, query string) <-chan *httptest.ResponseRecorder {
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		r := httptest.NewRequest("GET", "/api/v3/messages"+query, nil).WithContext(ctx)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		done <- w
	}()
	return done
}

func awaitHistoryBarrier(t *testing.T, barrier *historyTestRead) {
	t.Helper()
	select {
	case <-barrier.entered:
	case <-time.After(time.Second):
		t.Fatal("history scan did not enter blocked graph read")
	}
}

func awaitHistoryResponse(t *testing.T, done <-chan *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	t.Helper()
	select {
	case w := <-done:
		return w
	case <-time.After(3 * time.Second):
		t.Fatal("history request did not finish")
	}
	return nil
}

func TestV3HistoryPollingDoesNotBlockOverlayAndReceiver(t *testing.T) {
	_, f, s, _, channel := automaticVideoFixture(t)
	createAutomaticMessage(t, s, "initial")
	provisionTwitch(t, f, 30, "7", "101")
	changeProfile(f, 30, func(p *twitchProfile) { p.Enabled = new(true) })
	token := issueAppTestToken(t, s, "7", `{"scope":"channel-read","channelIds":["`+channel+`"]}`)
	s.player = &overlayPlayer{PlayerOrigin: "http://player.example", ParentOrigin: "http://parent.example"}
	barrier := blockHistoryReads(t, s, 1)
	done := historyRequest(s, context.Background(), "key.secret", "")
	awaitHistoryBarrier(t, barrier)
	twitch := newTwitchFake(t)
	s.twitch = &twitchRuntime{client: twitch.client}
	ctx, cancel := context.WithCancel(context.Background())
	ticks := make(chan time.Time)
	managerDone := make(chan struct{})
	go func() { defer close(managerDone); s.reconcileTwitch(ctx, ticks) }()
	defer func() { cancel(); <-managerDone }()
	host := eventTestHost(t, s)
	client := &http.Client{Timeout: time.Second}
	r, _ := http.NewRequest("GET", host.URL+"/overlays/video?apiKey="+url.QueryEscape(token), nil)
	response, err := client.Do(r)
	if err != nil {
		t.Fatal("video HTML did not finish while history scan was blocked")
	}
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("video HTML status=%d", response.StatusCode)
	}
	stream := openEventTestStream(t, host, token, "types=message.created")
	defer s.eventHub().shutdown()
	deadline := time.After(time.Second)
	for {
		state, _, _ := s.twitch.status(30)
		if state == "connected" {
			break
		}
		select {
		case <-deadline:
			t.Fatal("receiver reconcile blocked by history scan")
		case <-time.After(time.Millisecond):
		}
	}
	m := createAutomaticMessage(t, s, "live during history")
	envelope, _ := eventTestEnvelope(t, nextEventTestFrame(t, stream, time.Second))
	if envelope.Type != "message.created" {
		t.Fatal("new mutation did not publish live chat")
	}
	select {
	case <-done:
		t.Fatal("history barrier was not held during competing operations")
	default:
	}
	barrier.release <- struct{}{}
	w := awaitHistoryResponse(t, done)
	status(t, w, 200)
	if !strings.Contains(w.Body.String(), `"id":"`+m.ID+`"`) || barrier.attempts.Load() != 2 {
		t.Fatal("history did not restart to include concurrent live message")
	}
}

func TestV3HistoryContinuousPollingAndIngress(t *testing.T) {
	_, _, s, _, _ := automaticVideoFixture(t)
	for range 12 {
		createAutomaticMessage(t, s, "history")
	}
	original := s.graph.http
	s.graph.http = &http.Client{Transport: videoTestTransport{base: original.Transport, before: func(r *http.Request) error {
		if r.Context().Value(graphReadMemoKey) != nil {
			time.Sleep(time.Millisecond)
		}
		return nil
	}}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	writing := make(chan struct{})
	writerDone := make(chan struct{})
	var writes atomic.Int64
	go func() {
		defer close(writerDone)
		close(writing)
		for range 8 {
			r := httptest.NewRequest("POST", "/api/v3/messages", strings.NewReader(eventLocalMessage("owned", "concurrent"))).WithContext(ctx)
			r.Header.Set("Authorization", "Bearer key.secret")
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)
			if w.Code == 201 {
				writes.Add(1)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(40 * time.Millisecond):
			}
		}
	}()
	<-writing
	successes, conflicts := 0, 0
	for {
		select {
		case <-writerDone:
			goto finished
		default:
		}
		w := awaitHistoryResponse(t, historyRequest(s, ctx, "key.secret", "?count=1"))
		switch w.Code {
		case 200:
			successes++
		case 503:
			conflicts++
		default:
			t.Fatalf("unexpected concurrent history status=%d", w.Code)
		}
	}
finished:
	if writes.Load() != 8 || successes == 0 {
		t.Fatalf("writes=%d successful polls=%d conflict polls=%d", writes.Load(), successes, conflicts)
	}
	w := awaitHistoryResponse(t, historyRequest(s, ctx, "key.secret", ""))
	status(t, w, 200)
	if !strings.Contains(w.Body.String(), `"total":20`) {
		t.Fatal("concurrent history omitted confirmed messages")
	}
	t.Logf("confirmed_writes=%d successful_concurrent_polls=%d bounded_conflicts=%d", writes.Load(), successes, conflicts)
}

func TestV3HistoryReadRetriesConcurrentMutation(t *testing.T) {
	_, s := newResourceFixture(t)
	first := createResource(t, s, v3Message)
	barrier := blockHistoryReads(t, s, 1)
	done := historyRequest(s, context.Background(), "key.secret", "?count=1&descending=true")
	awaitHistoryBarrier(t, barrier)
	second := createResource(t, s, replyBody(first.ID))
	barrier.release <- struct{}{}
	w := awaitHistoryResponse(t, done)
	status(t, w, 200)
	var page struct {
		Result   []messageResource
		Total    int
		Continue *int
	}
	if json.Unmarshal(w.Body.Bytes(), &page) != nil || page.Total != 2 || len(page.Result) != 1 || page.Result[0].ID != second.ID || page.Continue == nil || *page.Continue != 1 || barrier.attempts.Load() != 2 {
		t.Fatal("concurrent mutation returned stale history or pagination")
	}
}

func TestV3HistoryReadRevocationFailsClosed(t *testing.T) {
	_, _, s, _, channel := automaticVideoFixture(t)
	createAutomaticMessage(t, s, "private")
	token := issueAppTestToken(t, s, "7", `{"scope":"channel-read","channelIds":["`+channel+`"]}`)
	barrier := blockHistoryReads(t, s, 1)
	done := historyRequest(s, context.Background(), token, "")
	awaitHistoryBarrier(t, barrier)
	id, _, _ := strings.Cut(token, ".")
	status(t, appRequest(t, s, "DELETE", "/api/v3/tokens/"+id, "key.secret", ""), 204)
	barrier.release <- struct{}{}
	w := awaitHistoryResponse(t, done)
	status(t, w, 401)
	if strings.Contains(w.Body.String(), "private") {
		t.Fatal("revoked token received scanned data")
	}
}

func TestV3HistoryExpiryWithoutMutationRequiresFinalAuthorization(t *testing.T) {
	f, s := newResourceFixture(t)
	createResource(t, s, v3Message)
	now := s.now()
	var advance atomic.Int64
	s.now = func() time.Time { return now.Add(time.Duration(advance.Load())) }
	f.legacy.mu.Lock()
	key := f.legacy.cfg.APIKeys["key"]
	expiry := now.Add(time.Minute)
	key.ExpiresAt = &expiry
	f.legacy.cfg.APIKeys["key"] = key
	f.legacy.mu.Unlock()
	status(t, request(t, s, "GET", "/api/v3/messages", "key.secret", ""), 200)
	barrier := blockHistoryReads(t, s, 1)
	done := historyRequest(s, context.Background(), "key.secret", "")
	awaitHistoryBarrier(t, barrier)
	epoch := s.graph.mutationEpoch.Load()
	advance.Store(int64(2 * time.Minute))
	barrier.release <- struct{}{}
	w := awaitHistoryResponse(t, done)
	if s.graph.mutationEpoch.Load() != epoch {
		t.Fatal("expiry changed mutation epoch")
	}
	status(t, w, 401)
	if strings.Contains(w.Body.String(), `"result"`) {
		t.Fatal("expired credential received scanned history")
	}
}

func TestV3HistoryConfigurationNarrowingWithoutMutationRequiresFreshSnapshot(t *testing.T) {
	f, s, _ := outboundFixture(t)
	status(t, outboundRequest(s, "key.secret", "config-only", outboundBody), 201)
	token := addTestKey(f, s, "scoped", "7", "host")
	f.legacy.mu.Lock()
	key := f.legacy.cfg.APIKeys["scoped"]
	key.Scope = "channel-read"
	key.ChannelIDs = []string{"31"}
	f.legacy.cfg.APIKeys["scoped"] = key
	f.legacy.mu.Unlock()
	before := request(t, s, "GET", "/api/v3/messages", token, "")
	status(t, before, 200)
	var initial struct{ Total int }
	if json.Unmarshal(before.Body.Bytes(), &initial) != nil || initial.Total != 1 {
		t.Fatal("enabled profile did not expose positive history row")
	}
	r := httptest.NewRequest("GET", "/api/v3/messages", nil)
	cfg, err := s.graph.loadConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	caller, code := s.authorize(context.Background(), r, cfg, token, false)
	if code != 0 {
		t.Fatal("positive principal authorization failed")
	}
	barrier := blockHistoryReads(t, s, 1)
	done := historyRequest(s, context.Background(), token, "")
	awaitHistoryBarrier(t, barrier)
	epoch := s.graph.mutationEpoch.Load()
	f.mu.Lock()
	changeProfile(f, 30, func(p *twitchProfile) { p.Enabled = new(false) })
	f.mu.Unlock()
	current, err := s.graph.loadConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	freshCaller, code := s.authorize(context.Background(), r, current, token, false)
	if code != 0 || !reflect.DeepEqual(caller, freshCaller) || reflect.DeepEqual(cfg, current) {
		t.Fatal("profile change did not preserve principal while narrowing configuration")
	}
	barrier.release <- struct{}{}
	w := awaitHistoryResponse(t, done)
	status(t, w, 200)
	if s.graph.mutationEpoch.Load() != epoch {
		t.Fatal("fixture profile narrowing changed mutation epoch")
	}
	var page struct {
		Result []messageResource
		Total  int
	}
	if json.Unmarshal(w.Body.Bytes(), &page) != nil || page.Total != 0 || len(page.Result) != 0 {
		t.Fatal("configuration narrowing returned stale history row")
	}
	if barrier.attempts.Load() != 2 {
		t.Fatalf("configuration narrowing attempts=%d want=2", barrier.attempts.Load())
	}
}

func TestV3HistoryReadUncertainWriteInvalidates(t *testing.T) {
	f, s := newResourceFixture(t)
	createResource(t, s, v3Message)
	barrier := blockHistoryReads(t, s, 1)
	done := historyRequest(s, context.Background(), "key.secret", "")
	awaitHistoryBarrier(t, barrier)
	f.mu.Lock()
	f.fail = "content:message"
	f.mu.Unlock()
	failed := request(t, s, "POST", "/api/v3/messages", "key.secret", v3Message)
	status(t, failed, 503)
	f.mu.Lock()
	injected := f.failed
	f.fail = ""
	f.mu.Unlock()
	if !injected {
		t.Fatal("uncertain write injection was not reached")
	}
	barrier.release <- struct{}{}
	status(t, awaitHistoryResponse(t, done), 200)
	if barrier.attempts.Load() != 2 {
		t.Fatal("uncertain write did not invalidate history attempt")
	}
}

func TestV3HistoryReadRetryLimitAndCancellation(t *testing.T) {
	t.Run("two mutations", func(t *testing.T) {
		_, s := newResourceFixture(t)
		m := createResource(t, s, v3Message)
		barrier := blockHistoryReads(t, s, 2)
		done := historyRequest(s, context.Background(), "key.secret", "")
		for range 2 {
			awaitHistoryBarrier(t, barrier)
			status(t, patchResource(t, s, m.ID, `[{"op":"replace","path":"/tts","value":true}]`), 200)
			barrier.release <- struct{}{}
		}
		status(t, awaitHistoryResponse(t, done), 503)
		if barrier.attempts.Load() != 2 {
			t.Fatal("history exceeded two attempts")
		}
	})
	t.Run("cancellation", func(t *testing.T) {
		_, s := newResourceFixture(t)
		createResource(t, s, v3Message)
		barrier := blockHistoryReads(t, s, 1)
		ctx, cancel := context.WithCancel(context.Background())
		done := historyRequest(s, ctx, "key.secret", "")
		awaitHistoryBarrier(t, barrier)
		cancel()
		status(t, awaitHistoryResponse(t, done), 503)
		check, finish := context.WithTimeout(context.Background(), time.Second)
		defer finish()
		if err := lockResources(check); err != nil {
			t.Fatal("canceled history retained resource lock")
		}
		resourceLock.Unlock()
	})
}

func TestV3HistoryPreservesFullValidationAndPagination(t *testing.T) {
	f, s, _ := outboundFixture(t)
	first := createResource(t, s, v3Message)
	second := createResource(t, s, replyBody(first.ID))
	cfg, err := s.graph.loadConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	incoming := twitchEvent{BroadcasterUserID: "101", BroadcasterUserLogin: "room", ChatterUserID: "700", ChatterUserLogin: "speaker", ChatterUserName: "Speaker", MessageID: "mixed-inbound", publishedAt: s.now()}
	incoming.Message.Text = "incoming"
	if _, err := s.ingestTwitch(context.Background(), cfg.twitch[0], incoming); err != nil {
		t.Fatal(err)
	}
	sent := outboundRequest(s, "key.secret", "mixed-sent", outboundBody)
	status(t, sent, 201)
	delivered := outboundResource(t, sent, "sent")
	stored := storedOutbound(t, f, delivered.ID)
	stored.SchemaVersion = 4
	stored.Delivery.Transport = "irc"
	stored.Delivery.State = "submitted"
	stored.Delivery.MessageID = ""
	stored.Delivery.RequestKey = "mixed-submitted"
	resourceLock.Lock()
	fourth, err := s.graph.reserveOutbound(context.Background(), outboundName("12", "7", "mixed-submitted"), stored, 20)
	resourceLock.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	tombstone := createResource(t, s, v3Message)
	status(t, request(t, s, "DELETE", "/api/v3/messages/"+tombstone.ID, "key.secret", ""), 204)
	for _, tc := range []struct {
		query string
		total int
		first string
		next  *int
	}{
		{"?count=1&descending=true", 5, strconv.FormatInt(fourth, 10), intPointer(1)},
		{"?replyTo=" + first.ID, 1, second.ID, nil},
		{"?service=twitch&channel=room-42&userId=" + first.UserID + "&channelId=" + first.ChannelID, 2, first.ID, nil},
		{"?count=1&continue=1", 5, second.ID, intPointer(2)},
		{"?from=" + url.QueryEscape(s.now().Format(time.RFC3339Nano)) + "&to=" + url.QueryEscape(s.now().Add(time.Second).Format(time.RFC3339Nano)), 5, first.ID, nil},
	} {
		w := request(t, s, "GET", "/api/v3/messages"+tc.query, "key.secret", "")
		status(t, w, 200)
		var page struct {
			Result   []messageResource
			Total    int
			Continue *int
		}
		if json.Unmarshal(w.Body.Bytes(), &page) != nil || page.Total != tc.total || len(page.Result) == 0 || page.Result[0].ID != tc.first {
			t.Fatalf("mixed schema pagination query=%s", tc.query)
		}
		if (page.Continue == nil) != (tc.next == nil) || page.Continue != nil && *page.Continue != *tc.next {
			t.Fatal("mixed schema continuation changed")
		}
	}
	f.mu.Lock()
	n := f.nodes[mustResourceID(first.UserID)]
	n.Content = new(`{"schemaVersion":1,"service":"twitch","serviceId":"wrong","name":"Speaker"}`)
	f.nodes[n.ID] = n
	f.mu.Unlock()
	status(t, request(t, s, "GET", "/api/v3/messages?count=1&from="+url.QueryEscape(s.now().Add(time.Hour).Format(time.RFC3339Nano)), "key.secret", ""), 500)
}

func TestV3HistoryMetadataMemoIsAttemptLocal(t *testing.T) {
	t.Run("reuse and fresh auth", func(t *testing.T) {
		_, s := newResourceFixture(t)
		for range 4 {
			createResource(t, s, v3Message)
		}
		var scanUsers, liveConfigs atomic.Int64
		original := s.graph.http
		s.graph.http = &http.Client{Transport: videoTestTransport{base: original.Transport, before: func(r *http.Request) error {
			memo, _ := r.Context().Value(graphReadMemoKey).(*graphReadMemo)
			if memo != nil && r.URL.Query().Get("type") == "user" {
				scanUsers.Add(1)
			}
			if memo == nil && r.URL.Query().Get("type") == authConfigType {
				liveConfigs.Add(1)
			}
			return nil
		}}}
		for range 2 {
			status(t, request(t, s, "GET", "/api/v3/messages", "key.secret", ""), 200)
		}
		if scanUsers.Load() != 2 || liveConfigs.Load() < 4 {
			t.Fatalf("scan user reads=%d live config reads=%d", scanUsers.Load(), liveConfigs.Load())
		}
	})
	t.Run("budgets and failed reads", func(t *testing.T) {
		_, s := newResourceFixture(t)
		memo := &graphReadMemo{graph: s.graph, responses: make(map[string][]byte)}
		for i := 0; i < graphReadMemoEntries+1; i++ {
			memo.retain(strconv.Itoa(i), []byte("ok"))
		}
		if len(memo.responses) != graphReadMemoEntries || memo.bytes > graphReadMemoBytes {
			t.Fatal("entry budget exceeded")
		}
		memo = &graphReadMemo{graph: s.graph, responses: make(map[string][]byte)}
		memo.retain("large", make([]byte, graphReadMemoBytes))
		if len(memo.responses) != 0 || memo.bytes != 0 {
			t.Fatal("oversized body retained")
		}
		memo.retain("fit", make([]byte, graphReadMemoBytes-3))
		memo.retain("overflow", []byte("x"))
		if len(memo.responses) != 1 || memo.bytes != graphReadMemoBytes {
			t.Fatal("byte budget exceeded")
		}
		var calls atomic.Int64
		original := s.graph.http
		s.graph.http = &http.Client{Transport: videoTestTransport{base: original.Transport, before: func(r *http.Request) error {
			if calls.Add(1) == 1 {
				return errors.New("injected transport failure")
			}
			return nil
		}}}
		ctx := context.WithValue(context.Background(), graphReadMemoKey, memo)
		for i := 0; i < 3; i++ {
			_, err := s.graph.request(ctx, "GET", "/users/me", nil, nil)
			if (i == 0) != (err != nil) {
				t.Fatal("failed GET cached or successful fallback failed")
			}
		}
		if calls.Load() != 3 || memo.bytes != graphReadMemoBytes {
			t.Fatal("full memo did not continue uncached reads")
		}
		memo = &graphReadMemo{graph: s.graph, responses: make(map[string][]byte)}
		ctx = context.WithValue(context.Background(), graphReadMemoKey, memo)
		calls.Store(0)
		for i := 0; i < 3; i++ {
			_, err := s.graph.request(ctx, "GET", "/users/me", nil, nil)
			if (i == 0) != (err != nil) {
				t.Fatal("failed GET was memoized")
			}
		}
		if calls.Load() != 2 || len(memo.responses) != 1 {
			t.Fatal("successful GET not reused after failed attempt")
		}

	})
}
