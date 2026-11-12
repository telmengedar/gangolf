package core_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/telmengedar/gangolf/ai-core/internal/core"
	"github.com/telmengedar/gangolf/ai-core/internal/messages"
	"github.com/telmengedar/gangolf/ai-core/internal/processor"
	"github.com/telmengedar/gangolf/ai-core/internal/window"
)

func v3Row(id, author, speaker, text string, stamp time.Time) map[string]any {
	return map[string]any{"id": id, "service": "Twitch", "channel": "123", "user": map[string]string{"service": "Twitch", "serviceId": author, "name": speaker}, "time": stamp.Format(time.RFC3339Nano), "chunks": []map[string]any{{"type": 0, "data": text}}, "source": map[string]string{"kind": "twitch"}}
}
func TestV3SingleChannelCoreRoundTrip(t *testing.T)          { v3RoundTrip(t, false, false) }
func TestV3RestartDoesNotReplayHistory(t *testing.T)         { v3RoundTrip(t, true, false) }
func TestV3IRCSubmissionIsNotConfirmedDelivery(t *testing.T) { v3RoundTrip(t, true, true) }
func v3RoundTrip(t *testing.T, restart, irc bool) {
	t.Helper()
	clock := start
	rows := []map[string]any{v3Row("1", "11", "viewer", "earlier context", clock.Add(-time.Minute)), v3Row("2", "99", "RenamedBot", "earlier reply", clock.Add(-time.Minute+time.Second))}
	for i, state := range []string{"sent", "unknown", "failed"} {
		row := v3Row(fmt.Sprint(i+3), "99", "GangolfBot", "intent must be absent", clock)
		delete(row, "source")
		row["delivery"] = map[string]string{"state": state}
		if irc {
			row["delivery"] = map[string]string{"state": "submitted", "transport": "irc"}
		}
		rows = append(rows, row)
	}
	posts, runs := 0, 0
	var inputs []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/messages" || r.Header.Get("Authorization") != "Bearer sentinel-key" {
			t.Errorf("request=%s", r.URL)
		}
		if r.Method == "GET" {
			from, _ := time.Parse(time.RFC3339Nano, r.URL.Query().Get("from"))
			to, _ := time.Parse(time.RFC3339Nano, r.URL.Query().Get("to"))
			selected := []map[string]any{}
			for _, row := range rows {
				stamp, _ := time.Parse(time.RFC3339Nano, row["time"].(string))
				if !stamp.Before(from) && stamp.Before(to) {
					selected = append(selected, row)
				}
			}
			json.NewEncoder(w).Encode(map[string]any{"result": selected, "total": len(selected), "continue": nil})
			return
		}
		posts++
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["channel"] != "123" || body["service"] != "Twitch" || body["tts"] != false {
			t.Errorf("body=%v", body)
		}
		text := body["chunks"].([]any)[0].(map[string]any)["data"].(string)
		if text != fmt.Sprintf("fresh reply %d", posts) {
			t.Errorf("sanitized=%q", text)
		}
		body["id"] = fmt.Sprint(100 + posts)
		if irc {
			body["delivery"] = map[string]string{"state": "submitted", "transport": "irc", "requestKey": r.Header.Get("Idempotency-Key")}
		} else {
			body["delivery"] = map[string]string{"state": "sent", "messageId": fmt.Sprintf("platform-%d", posts), "requestKey": r.Header.Get("Idempotency-Key")}
			rows = append(rows, v3Row(fmt.Sprint(200+posts), "99", "ChangedDisplayName", text, clock))
		}
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(body)
	}))
	defer api.Close()
	proc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		runs++
		if r.Method != "POST" || r.URL.Path != "/runs" || r.URL.RawQuery != "" || r.Header.Get("Authorization") != "" {
			t.Errorf("processor request=%s", r.URL)
		}
		var body struct {
			Input           string
			Subject, Config int64
			History         []window.Exchange
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.Subject != 42 || body.Config != 77 {
			t.Errorf("processor settings=%+v", body)
		}
		if runs == 1 && (len(body.History) != 1 || body.History[0].Input != "viewer: earlier context" || body.History[0].Reply != "earlier reply") {
			t.Errorf("history=%+v", body.History)
		}
		inputs = append(inputs, body.Input)
		fmt.Fprintf(w, `{"reply":"Gangolf: fresh reply %d","produced":true}`, runs)
	}))
	defer proc.Close()
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	makeCore := func() *core.Core {
		return core.New(core.Config{Routes: selfRoutes(window.Key{Service: "Twitch", Channel: "123"}), BotName: "GangolfBot"}, messages.New(api.URL+"/api", "sentinel-key", "99", api.Client(), logger), processor.New(proc.URL, 42, 77, proc.Client()), logger, func() time.Time { return clock })
	}
	c := makeCore()
	if err := c.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(time.Second)
	rows = append(rows, v3Row("10", "11", "GangolfBot", "hello gangolf", clock))
	if irc {
		rows[len(rows)-1]["source"].(map[string]string)["transport"] = "irc"
	}
	c.Tick(context.Background())
	clock = clock.Add(core.Settle)
	c.Tick(context.Background())
	if runs != 1 || posts != 1 || len(inputs) != 1 || inputs[0] != "GangolfBot: hello gangolf" {
		t.Fatalf("runs=%d posts=%d inputs=%v", runs, posts, inputs)
	}
	for range 3 {
		clock = clock.Add(core.Settle)
		c.Tick(context.Background())
	}
	if irc && (!strings.Contains(logs.String(), "submitted; visibility unconfirmed") || !strings.Contains(logs.String(), "outcome=submitted") || strings.Contains(logs.String(), "outcome=replied") || strings.Contains(logs.String(), "delivery confirmed") || strings.Contains(logs.String(), "post failed")) {
		t.Fatalf("submission logs=%s", logs.String())
	}
	if runs != 1 || posts != 1 {
		t.Fatalf("echo caused runs=%d posts=%d", runs, posts)
	}
	if restart {
		c = makeCore()
		if err := c.Bootstrap(context.Background()); err != nil {
			t.Fatal(err)
		}
		clock = clock.Add(core.Settle)
		c.Tick(context.Background())
		if runs != 1 || posts != 1 {
			t.Fatalf("restart replayed runs=%d posts=%d", runs, posts)
		}
		clock = clock.Add(time.Second)
		rows = append(rows, v3Row("20", "12", "other", "new after restart", clock))
		c.Tick(context.Background())
		clock = clock.Add(core.Settle)
		c.Tick(context.Background())
		if runs != 2 || posts != 2 || inputs[1] != "other: new after restart" {
			t.Fatalf("fresh runs=%d posts=%d inputs=%v", runs, posts, inputs)
		}
	}
}
func TestV3FailedPageDoesNotAdvanceCoreCursor(t *testing.T) {
	calls := 0
	var froms []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		froms = append(froms, r.URL.Query().Get("from"))
		if r.URL.Query().Get("continue") == "1" {
			w.WriteHeader(503)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"result": []any{v3Row("1", "11", "viewer", "partial", start)}, "total": 2, "continue": 1})
	}))
	defer api.Close()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	runner := &fakeRunner{}
	c := core.New(core.Config{Routes: selfRoutes(channel)}, messages.New(api.URL, "key", "99", api.Client(), logger), runner, logger, func() time.Time { return start })
	if err := c.Bootstrap(context.Background()); err == nil {
		t.Fatal("partial bootstrap succeeded")
	}
	c.Tick(context.Background())
	if calls != 4 || froms[0] != froms[2] || len(runner.calls) != 0 {
		t.Fatalf("calls=%d froms=%v runs=%v", calls, froms, runner.calls)
	}
}
func TestV3FailedPostDoesNotLogDeliveredTurnOrOfferMemory(t *testing.T) {
	r := newRig(t)
	r.bootstrap(t)
	r.messages.postErr = fmt.Errorf("delivery unknown")
	r.advance(time.Second)
	r.messages.add(channel, "viewer", "hello gangolf", r.clock)
	r.turns()
	if len(r.runner.calls) != 1 || len(r.messages.posts) != 1 {
		t.Fatalf("runs=%d posts=%d", len(r.runner.calls), len(r.messages.posts))
	}
	if !strings.Contains(r.logs.String(), "outcome=post-failed") || strings.Contains(r.logs.String(), "outcome=replied") {
		t.Fatalf("logs=%s", r.logs.String())
	}
	r.advance(time.Minute)
	r.tick()
	if len(r.runner.calls) != 1 || len(r.messages.posts) != 1 {
		t.Fatal("failed post was retried")
	}
}
