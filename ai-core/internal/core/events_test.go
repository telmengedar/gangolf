package core_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/telmengedar/gangolf/ai-core/internal/core"
	"github.com/telmengedar/gangolf/ai-core/internal/messages"
	"github.com/telmengedar/gangolf/ai-core/internal/processor"
	"github.com/telmengedar/gangolf/ai-core/internal/window"
)

type eventRunner func(context.Context, string, []window.Exchange) (processor.Result, error)

func (f eventRunner) Run(ctx context.Context, input string, history []window.Exchange) (processor.Result, error) {
	return f(ctx, input, history)
}

type liveRig struct {
	mu         sync.Mutex
	streams    map[string]chan string
	current    string
	rows       []map[string]any
	history    atomic.Int64
	posts      atomic.Int64
	fences     atomic.Int64
	opens      atomic.Int64
	ready      chan int64
	posted     chan struct{}
	holdPost   atomic.Bool
	failPost   atomic.Bool
	holdFence  atomic.Bool
	submission atomic.Bool
	clock      atomic.Int64
	host       *httptest.Server
	cancel     context.CancelFunc
	done       chan struct{}
}

func newLiveRig(t *testing.T, runner core.Runner, reviewer core.MemoryReviewer) *liveRig {
	t.Helper()
	f := &liveRig{streams: make(map[string]chan string), ready: make(chan int64, 64), posted: make(chan struct{}, 8), done: make(chan struct{})}
	f.host = httptest.NewServer(http.HandlerFunc(f.serve))
	client := messages.New(f.host.URL, "private.test-key", "900", f.host.Client(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	f.cancel = cancel
	c := core.New(core.Config{Routes: selfRoutes(window.Key{Service: "Twitch", Channel: "101"}), BotName: "GangolfBot", MemoryReviewer: reviewer}, client, runner, slog.New(slog.NewTextHandler(io.Discard, nil)), func() time.Time { return time.Now().Add(time.Duration(f.clock.Load())) })
	go func() { defer close(f.done); c.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-f.done:
		case <-time.After(3 * time.Second):
			t.Error("event core shutdown deadline")
		}
		f.host.Close()
	})
	f.waitFence(t, 1)
	return f
}
func (f *liveRig) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer private.test-key" {
		w.WriteHeader(401)
		return
	}
	switch r.URL.Path {
	case "/v3/events":
		id := fmt.Sprintf("%032x", f.opens.Add(1))
		frames := make(chan string, 256)
		f.mu.Lock()
		f.streams[id] = frames
		f.current = id
		f.mu.Unlock()
		defer func() { f.mu.Lock(); delete(f.streams, id); f.mu.Unlock() }()
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "event: stream.ready\ndata: {\"schemaVersion\":1,\"replay\":false,\"resyncRequired\":true,\"streamId\":%q}\n\n", id)
		w.(http.Flusher).Flush()
		for {
			select {
			case frame := <-frames:
				if frame == "disconnect" {
					return
				}
				io.WriteString(w, frame)
				w.(http.Flusher).Flush()
			case <-r.Context().Done():
				return
			}
		}
	case "/v3/messages":
		if r.Method == "GET" {
			f.history.Add(1)
			f.mu.Lock()
			rows := append([]map[string]any{}, f.rows...)
			f.mu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{"result": rows, "total": len(rows), "continue": nil})
			return
		}
		f.posts.Add(1)
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if f.holdPost.Load() {
			select {
			case f.posted <- struct{}{}:
			default:
			}
			<-r.Context().Done()
			return
		}
		if f.failPost.Load() {
			w.WriteHeader(503)
			return
		}
		state := "sent"
		delivery := map[string]any{"state": state, "messageId": "provider-id", "requestKey": r.Header.Get("Idempotency-Key")}
		if f.submission.Load() {
			delivery = map[string]any{"state": "submitted", "transport": "irc", "requestKey": r.Header.Get("Idempotency-Key")}
		}
		body["id"] = "999"
		body["delivery"] = delivery
		body["source"] = nil
		body["replyTo"] = nil
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(body)
		select {
		case f.posted <- struct{}{}:
		default:
		}
	case "/v3/events/fence":
		var body map[string]string
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			w.WriteHeader(400)
			return
		}
		f.mu.Lock()
		stream := f.streams[body["streamId"]]
		f.mu.Unlock()
		if stream == nil {
			w.WriteHeader(404)
			return
		}
		stream <- "event: stream.fence\ndata: {\"schemaVersion\":1,\"requestId\":\"" + body["requestId"] + "\"}\n\n"
		n := f.fences.Add(1)
		f.ready <- n
		if f.holdFence.Load() {
			<-r.Context().Done()
			return
		}
		json.NewEncoder(w).Encode(body)
	default:
		w.WriteHeader(404)
	}
}
func liveRow(id, text string) map[string]any {
	return map[string]any{"id": id, "service": "Twitch", "channel": "101", "time": time.Now().UTC().Format(time.RFC3339Nano), "source": map[string]string{"kind": "twitch", "transport": "irc"}, "user": map[string]string{"service": "Twitch", "serviceId": "700", "name": "Mara"}, "chunks": []map[string]any{{"type": 0, "data": text}}}
}
func (f *liveRig) push(t *testing.T, kind string, row map[string]any) {
	t.Helper()
	data, _ := json.Marshal(map[string]any{"schemaVersion": 1, "id": "epoch:" + row["id"].(string), "type": kind, "data": map[string]any{"resourceVersion": 3, "message": row}})
	f.mu.Lock()
	stream := f.streams[f.current]
	f.mu.Unlock()
	select {
	case stream <- "event: " + kind + "\ndata: " + string(data) + "\n\n":
	case <-time.After(time.Second):
		t.Fatal("live fixture has no subscribed stream")
	}
}
func (f *liveRig) waitFence(t *testing.T, n int64) {
	t.Helper()
	for {
		select {
		case got := <-f.ready:
			if got >= n {
				return
			}
		case <-time.After(4 * time.Second):
			t.Fatal("ordered fence did not arrive")
		}
	}
}
func awaitLiveRun(t *testing.T, calls <-chan call) call {
	t.Helper()
	select {
	case got := <-calls:
		return got
	case <-time.After(4 * time.Second):
		t.Fatal("producer event did not start settled turn")
	}
	return call{}
}
func TestEventRunIdleAndHealthyTurnsNeverDiscoverThroughHistory(t *testing.T) {
	calls := make(chan call, 4)
	f := newLiveRig(t, eventRunner(func(_ context.Context, input string, history []window.Exchange) (processor.Result, error) {
		calls <- call{input, history}
		return processor.Result{Reply: "fresh reply"}, nil
	}), nil)
	f.clock.Store(int64(time.Hour))
	time.Sleep(30 * time.Millisecond)
	if f.history.Load() != 1 || f.posts.Load() != 0 {
		t.Fatal("idle healthy stream performed discovery or posted")
	}
	f.push(t, "message.created", liveRow("1", "hello event"))
	if got := awaitLiveRun(t, calls); got.input != "Mara: hello event" {
		t.Fatal("event payload not used by turn")
	}
	f.waitFence(t, 2)
	if f.history.Load() != 1 || f.posts.Load() != 1 {
		t.Fatalf("history=%d posts=%d", f.history.Load(), f.posts.Load())
	}
	f.push(t, "message.updated", liveRow("1", "old edit must not retrigger"))
	select {
	case <-calls:
		t.Fatal("readable resource edit triggered another run")
	case <-time.After(30 * time.Millisecond):
	}
}
func TestEventRunClosingFenceConsumesInRunLinesAndHeldHTTPResponse(t *testing.T) {
	calls := make(chan call, 4)
	release := make(chan struct{})
	var runs atomic.Int64
	f := newLiveRig(t, eventRunner(func(_ context.Context, input string, history []window.Exchange) (processor.Result, error) {
		calls <- call{input, history}
		if runs.Add(1) == 1 {
			<-release
		}
		return processor.Result{Reply: fmt.Sprintf("reply %d", runs.Load())}, nil
	}), nil)
	f.push(t, "message.created", liveRow("1", "first trigger"))
	awaitLiveRun(t, calls)
	f.push(t, "message.created", liveRow("2", "during blocked model"))
	f.holdFence.Store(true)
	close(release)
	f.waitFence(t, 2)
	f.push(t, "message.created", liveRow("3", "after producer fence"))
	got := awaitLiveRun(t, calls)
	if got.input != "Mara: after producer fence" {
		t.Fatal("in-run queued line became follow-up input")
	}
	found := false
	for _, exchange := range got.history {
		found = found || strings.Contains(exchange.Input, "during blocked model")
	}
	if !found || f.history.Load() != 1 {
		t.Fatal("closing fence lost context or fetched history")
	}
}
func TestEventRunOverflowCancelsLateModelWithoutPostOrPrivateReview(t *testing.T) {
	entered, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	reviewer := &fakeReviewer{calls: make(chan reviewCall, 4)}
	f := newLiveRig(t, eventRunner(func(ctx context.Context, _ string, _ []window.Exchange) (processor.Result, error) {
		close(entered)
		<-ctx.Done()
		close(canceled)
		<-release
		return processor.Result{Reply: "late unsafe reply"}, nil
	}), reviewer)
	f.push(t, "message.created", liveRow("1", "begin"))
	select {
	case <-entered:
	case <-time.After(4 * time.Second):
		t.Fatal("model was not entered")
	}
	for i := 2; i < 80; i++ {
		f.push(t, "message.created", liveRow(fmt.Sprint(i), "queued while blocked"))
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("reader overflow did not cancel active generation")
	}
	close(release)
	f.waitFence(t, 2)
	if f.posts.Load() != 0 {
		t.Fatal("late canceled model invoked public post")
	}
	select {
	case <-reviewer.calls:
		t.Fatal("canceled model offered private review")
	default:
	}
	if f.history.Load() != 2 {
		t.Fatal("gap did not establish exactly one finite reconnect episode")
	}
}
func TestEventRunAcceptedIRCSubmissionOffersReviewExactlyOnce(t *testing.T) {
	calls := make(chan call, 2)
	reviewer := &fakeReviewer{calls: make(chan reviewCall, 4)}
	f := newLiveRig(t, eventRunner(func(_ context.Context, input string, history []window.Exchange) (processor.Result, error) {
		calls <- call{input, history}
		return processor.Result{Reply: "accepted submission"}, nil
	}), reviewer)
	f.submission.Store(true)
	f.push(t, "message.created", liveRow("1", "speaker evidence"))
	awaitLiveRun(t, calls)
	review := receiveReview(t, reviewer.calls)
	if review.input != "Mara: speaker evidence" {
		t.Fatal("event review lost original speaker input")
	}
	f.waitFence(t, 2)
	f.push(t, "message.updated", liveRow("1", "edited speaker evidence"))
	select {
	case <-reviewer.calls:
		t.Fatal("recovery/edit replayed private review")
	case <-time.After(30 * time.Millisecond):
	}
	if f.posts.Load() != 1 || f.history.Load() != 1 {
		t.Fatal("accepted submission replayed post or polled")
	}
}

func TestEventRunPostGapDoesNotReplayUnknownPublicAttempt(t *testing.T) {
	reviewer := &fakeReviewer{calls: make(chan reviewCall, 4)}
	f := newLiveRig(t, eventRunner(func(context.Context, string, []window.Exchange) (processor.Result, error) {
		return processor.Result{Reply: "single attempt"}, nil
	}), reviewer)
	f.holdPost.Store(true)
	f.push(t, "message.created", liveRow("1", "start public attempt"))
	select {
	case <-f.posted:
	case <-time.After(4 * time.Second):
		t.Fatal("public attempt was not dispatched")
	}
	f.mu.Lock()
	stream := f.streams[f.current]
	f.mu.Unlock()
	stream <- "disconnect"
	f.waitFence(t, 2)
	if f.posts.Load() != 1 {
		t.Fatal("unknown public outcome was replayed after reconnect")
	}
	select {
	case <-reviewer.calls:
		t.Fatal("unknown public outcome offered private review")
	default:
	}
}
func TestEventRunUnreadableUpdatesAndRecoveryHistoryAreContextSafe(t *testing.T) {
	calls := make(chan call, 4)
	f := newLiveRig(t, eventRunner(func(_ context.Context, input string, history []window.Exchange) (processor.Result, error) {
		calls <- call{input, history}
		return processor.Result{Silent: true}, nil
	}), nil)
	blank := liveRow("1", "  ")
	readable := liveRow("1", "first readable observation")
	readable["time"] = blank["time"]
	f.push(t, "message.created", blank)
	f.push(t, "message.updated", readable)
	if got := awaitLiveRun(t, calls); got.input != "Mara: first readable observation" {
		t.Fatal("first readable transition lost")
	}
	f.waitFence(t, 2)
	f.mu.Lock()
	f.rows = []map[string]any{readable, liveRow("2", "outage message")}
	stream := f.streams[f.current]
	f.mu.Unlock()
	stream <- "disconnect"
	f.waitFence(t, 3)
	f.push(t, "message.updated", liveRow("999", "unknown old edit"))
	f.push(t, "message.created", liveRow("3", "post recovery reaction"))
	got := awaitLiveRun(t, calls)
	if got.input != "Mara: post recovery reaction" {
		t.Fatal("outage history or unknown update became new pending work")
	}
	found := false
	for _, entry := range got.history {
		found = found || strings.Contains(entry.Input, "outage message")
	}
	if !found {
		t.Fatal("finite reconnect history was not retained as context")
	}
	if f.history.Load() != 2 || f.posts.Load() != 0 {
		t.Fatal("recovery added discovery polling or silent public post")
	}
}
func TestEventRunSilentAndFailedOutcomesDoNotOfferPrivateReview(t *testing.T) {
	for _, mode := range []string{"silent", "run failed", "post failed"} {
		t.Run(mode, func(t *testing.T) {
			reviewer := &fakeReviewer{calls: make(chan reviewCall, 4)}
			f := newLiveRig(t, eventRunner(func(context.Context, string, []window.Exchange) (processor.Result, error) {
				if mode == "run failed" {
					return processor.Result{}, errors.New("synthetic failure")
				}
				return processor.Result{Reply: "outcome reply", Silent: mode == "silent"}, nil
			}), reviewer)
			f.failPost.Store(mode == "post failed")
			f.push(t, "message.created", liveRow("1", "outcome trigger"))
			f.waitFence(t, 2)
			select {
			case <-reviewer.calls:
				t.Fatal("unsuccessful turn offered review")
			default:
			}
			want := int64(0)
			if mode == "post failed" {
				want = 1
			}
			if f.posts.Load() != want || f.history.Load() != 1 {
				t.Fatal("failed/silent event turn posted or polled unexpectedly")
			}
		})
	}
}
