package messages

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
)

func eventTestMessage(id, text string) map[string]any {
	return map[string]any{"id": id, "service": "Twitch", "channel": "101", "user": map[string]string{"service": "Twitch", "serviceId": "700", "name": "Mara"}, "time": "2026-11-19T00:00:00Z", "source": map[string]string{"kind": "twitch", "transport": "irc"}, "chunks": []map[string]any{{"type": 0, "data": text}}}
}
func eventTestPayload(id, text string) string {
	data, _ := json.Marshal(map[string]any{"schemaVersion": 1, "id": "epoch:" + id, "type": "message.created", "data": map[string]any{"resourceVersion": 3, "message": eventTestMessage(id, text)}})
	return "event: message.created\ndata: " + string(data) + "\n\n"
}

const eventReady = "event: stream.ready\ndata: {\"schemaVersion\":1,\"replay\":false,\"resyncRequired\":true,\"streamId\":\"0123456789abcdef0123456789abcdef\"}\n\n"

func parserFeed(t *testing.T, write func(http.ResponseWriter, *http.Request)) Feed {
	t.Helper()
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "types=message.created%2Cmessage.updated&service=Twitch" || r.Header.Get("Authorization") != "Bearer parser-only" {
			t.Error("stream credential/filter contract changed")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		write(w, r)
	}))
	t.Cleanup(host.Close)
	c := New(host.URL, "parser-only", "900", host.Client(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	feed, err := c.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(feed.Close)
	return feed
}
func TestMessageStreamFragmentedUTF8CRLFCommentsAndMultilineData(t *testing.T) {
	feed := parserFeed(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, eventReady)
		w.(http.Flusher).Flush()
		raw := strings.ReplaceAll(eventTestPayload("1", "Grüße 日本語 👋"), "\n", "\r\n")
		raw = strings.Replace(raw, `"data":{`, `"data":`+"\r\ndata: {", 1)
		for _, b := range []byte(": heartbeat\r\n\r\n" + raw) {
			w.Write([]byte{b})
			w.(http.Flusher).Flush()
		}
		<-r.Context().Done()
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	frame, err := feed.Next(ctx)
	if err != nil || frame.Line == nil || frame.Line.Text != "Grüße 日本語 👋" || frame.ID != "1" {
		t.Fatalf("fragmented event did not decode: %v", err)
	}
}
func TestMessageStreamCountAndByteOverflowCancelIndependently(t *testing.T) {
	for _, mode := range []string{"count", "bytes", "no-newline"} {
		t.Run(mode, func(t *testing.T) {
			release := make(chan struct{})
			feed := parserFeed(t, func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, eventReady)
				w.(http.Flusher).Flush()
				<-release
				switch mode {
				case "count":
					for i := 0; i < 65; i++ {
						io.WriteString(w, eventTestPayload("1", "bounded"))
					}
				case "bytes":
					for range 2 {
						io.WriteString(w, eventTestPayload("1", strings.Repeat("x", (frameLimit/2)+1024)))
					}
				case "no-newline":
					io.WriteString(w, strings.Repeat("x", frameLimit+2))
				}
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			})
			close(release)
			select {
			case <-feed.Context().Done():
			case <-time.After(3 * time.Second):
				t.Fatal("reader overflow did not invalidate generation")
			}
			if _, err := feed.Next(context.Background()); err == nil {
				t.Fatal("overflow exposed buffered data as synchronized")
			}
		})
	}
}
func TestMessageStreamMalformedSelectedFrameInvalidates(t *testing.T) {
	for _, payload := range []string{"event: stream.reset\ndata: {}\n\n", "event: message.created\ndata: {\"schemaVersion\":2}\n\n", "event: message.created\ndata: {\"schemaVersion\":1,\"id\":\"x\",\"type\":\"message.created\",\"data\":{\"resourceVersion\":4}}\n\n", "event: message.created\ndata: {\"schemaVersion\":1,\"id\":\"x\",\"type\":\"message.created\",\"data\":{\"resourceVersion\":3,\"message\":{}}}\n\n"} {
		t.Run(payload[:min(len(payload), 32)], func(t *testing.T) {
			release := make(chan struct{})
			feed := parserFeed(t, func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, eventReady)
				w.(http.Flusher).Flush()
				<-release
				io.WriteString(w, payload)
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			})
			close(release)
			select {
			case <-feed.Context().Done():
			case <-time.After(time.Second):
				t.Fatal("malformed selected frame ignored")
			}
		})
	}
}
func TestMessageSyncBoundsAndUnreadableObservations(t *testing.T) {
	for _, mode := range []string{"unreadable", "rows", "pages", "body", "partial"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				switch mode {
				case "unreadable":
					json.NewEncoder(w).Encode(map[string]any{"result": []any{eventTestMessage("1", "  ")}, "total": 1, "continue": nil})
				case "rows":
					io.WriteString(w, `{"result":[],"total":32001,"continue":null}`)
				case "body":
					io.WriteString(w, strings.Repeat("x", frameLimit+1))
				case "partial":
					io.WriteString(w, `{"result":[],"total":1,"continue":null}`)
				case "pages":
					json.NewEncoder(w).Encode(map[string]any{"result": []any{eventTestMessage("1", "readable")}, "total": 65, "continue": calls})
				}
			}))
			defer host.Close()
			c := New(host.URL, "sync-only", "900", host.Client(), slog.New(slog.NewTextHandler(io.Discard, nil)))
			observations := 0
			err := c.Sync(context.Background(), time.Now(), func(frame Frame) error {
				observations++
				if mode == "unreadable" && (frame.ID != "1" || frame.Line != nil) {
					t.Fatal("unreadable observation identity was lost")
				}
				return nil
			})
			if mode == "unreadable" {
				if err != nil || observations != 1 {
					t.Fatal("finite history omitted unreadable placeholder")
				}
			} else if err == nil {
				t.Fatal("overloaded/incomplete history declared synchronized")
			}
			if mode == "pages" && calls != 64 {
				t.Fatalf("history requests=%d want64", calls)
			}
		})
	}
}

func TestMessageStreamHandshakeRejectsMissingReplayAndOldReady(t *testing.T) {
	for _, body := range []string{`{"schemaVersion":1,"resyncRequired":true,"streamId":"0123456789abcdef0123456789abcdef"}`, `{"schemaVersion":1,"replay":null,"resyncRequired":true,"streamId":"0123456789abcdef0123456789abcdef"}`, `{"schemaVersion":1,"replay":false,"resyncRequired":true,"streamId":"old-process"}`, `{"schemaVersion":2,"replay":false,"resyncRequired":true,"streamId":"0123456789abcdef0123456789abcdef"}`} {
		t.Run(body, func(t *testing.T) {
			host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, "event: stream.ready\ndata: "+body+"\n\n")
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			}))
			defer host.Close()
			c := New(host.URL, "ready-only", "900", host.Client(), nil)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if feed, err := c.Open(ctx); err == nil {
				feed.Close()
				t.Fatal("invalid ready promoted synchronization")
			}
		})
	}
}
func TestMessageStreamCancellationDuringHandshakeStopsOwnedReader(t *testing.T) {
	entered := make(chan struct{})
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); <-r.Context().Done() }))
	defer host.Close()
	c := New(host.URL, "cancel-only", "900", host.Client(), nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := c.Open(ctx); done <- err }()
	<-entered
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled handshake was accepted")
		}
	case <-time.After(time.Second):
		t.Fatal("canceled reader remained active")
	}
}
