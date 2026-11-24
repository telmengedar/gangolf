package core

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/telmengedar/gangolf/ai-core/internal/processor"
	"github.com/telmengedar/gangolf/ai-core/internal/window"
)

type processSink struct {
	posts []string
	keys  []window.Key
}

func (s *processSink) Post(_ context.Context, key window.Key, text string) error {
	s.posts = append(s.posts, text)
	s.keys = append(s.keys, key)
	return nil
}

func TestTurn_ProcessOutcomes_PreservesDeliveryAndWithholding(t *testing.T) {
	cases := []struct {
		name, output  string
		status, calls int
		post          string
	}{
		{"reply", `{"reply":"Gangolf: supported correction","silent":false}`, 200, 1, "supported correction"},
		{"silence", `{"reply":"","silent":true}`, 200, 1, ""},
		{"malformed", `{"reply":"leaking draft","silent":true}`, 200, 1, ""},
		{"final reject", `{"reply":"leaking draft","silent":false}`, 422, 1, ""},
		{"sanitized empty", `{"reply":"Gangolf:","silent":false}`, 200, 1, ""},
		{"duplicate", `{"reply":"previous reply","silent":false}`, 200, 2, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var requests []struct {
				Arguments struct {
					Message string            `json:"message"`
					History []window.Exchange `json:"history"`
				} `json:"arguments"`
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/process-runs" {
					t.Error("legacy fallback requested")
				}
				var req struct {
					Arguments struct {
						Message string            `json:"message"`
						History []window.Exchange `json:"history"`
					} `json:"arguments"`
				}
				json.NewDecoder(r.Body).Decode(&req)
				requests = append(requests, req)
				w.WriteHeader(tc.status)
				if tc.status != 200 {
					io.WriteString(w, `{"status":"failed","error":{"code":"invalid_transition","message":"private checker feedback"},"output":`+tc.output+`}`)
					return
				}
				io.WriteString(w, `{"process":{"id":81,"name":"private","hash":"`+strings.Repeat("a", 64)+`"},"status":"completed","output":`+tc.output+`}`)
			}))
			defer server.Close()
			sink := &processSink{}
			var logs strings.Builder
			now := time.Now()
			key := window.Key{Service: "Twitch", Channel: "101"}
			c := New(Config{BotName: "GangolfBot"}, sink, processor.NewProcess(server.URL, 81, 92, server.Client()), slog.New(slog.NewTextHandler(&logs, nil)), func() time.Time { return now })
			c.windows.Ingest([]window.Line{{ID: "1", Key: key, Speaker: "Mara", Text: "Ada lives in Bonn", Time: now.Add(-time.Minute)}, {ID: "2", Key: key, Own: true, Speaker: "GangolfBot", Text: "previous reply", Time: now.Add(-50 * time.Second)}}, now)
			c.windows.MarkAllAnswered()
			c.windows.Ingest([]window.Line{{ID: "3", Key: key, Speaker: "Ada", Text: "correction: I live in Köln", Time: now.Add(-4 * time.Second)}, {ID: "4", Key: key, Speaker: "Mara", Text: "Gangolf, where does Ada live?", Time: now.Add(-3 * time.Second)}}, now)
			if !c.turn(context.Background(), key) {
				t.Fatal("turn did not execute")
			}
			if len(requests) != tc.calls || !strings.Contains(requests[0].Arguments.Message, "Ada: correction: I live in Köln") || !strings.Contains(requests[0].Arguments.Message, "Mara: Gangolf, where does Ada live?") || len(requests[0].Arguments.History) == 0 {
				t.Fatalf("actual workflow input/calls=%+v", requests)
			}
			if tc.calls == 2 && !strings.Contains(requests[1].Arguments.Message, "was not sent") {
				t.Fatal("repeat reask contract lost")
			}
			if tc.post != "" {
				if len(sink.posts) != 1 || sink.posts[0] != tc.post || sink.keys[0] != key {
					t.Fatalf("posts=%v keys=%v", sink.posts, sink.keys)
				}
			} else if len(sink.posts) != 0 {
				t.Fatalf("withheld output posted=%v", sink.posts)
			}
			if strings.Contains(logs.String(), "private checker feedback") || strings.Contains(logs.String(), "leaking draft") {
				t.Fatal("failure diagnostic leaked product text")
			}
			if tc.name == "silence" && !strings.Contains(logs.String(), "chose-silence") {
				t.Fatalf("chosen silence lost: %s", logs.String())
			}
			if tc.status != 200 && !strings.Contains(logs.String(), "invalid_transition") {
				t.Fatal("typed failure diagnostics lost")
			}
		})
	}
}

func TestTurn_CancelledProcess_ForbidsDelivery(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		cancel()
		io.WriteString(w, `{"process":{"id":81,"name":"private","hash":"`+strings.Repeat("a", 64)+`"},"status":"completed","output":{"reply":"late reply","silent":false}}`)
	}))
	defer server.Close()
	sink := &processSink{}
	now := time.Now()
	key := window.Key{Service: "Twitch", Channel: "101"}
	c := New(Config{BotName: "GangolfBot"}, sink, processor.NewProcess(server.URL, 81, 92, server.Client()), slog.New(slog.NewTextHandler(io.Discard, nil)), func() time.Time { return now })
	c.windows.Ingest([]window.Line{{ID: "1", Key: key, Speaker: "Mara", Text: "Gangolf, answer me", Time: now.Add(-3 * time.Second)}}, now)
	c.turn(ctx, key)
	if calls != 1 || ctx.Err() == nil {
		t.Fatal("process cancellation scenario did not occur")
	}
	if len(sink.posts) != 0 {
		t.Fatal("cancelled reply posted")
	}
}
