package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPrivateMemoryWiringUsesDistinctConfigAfterPublicPostAndNeverPublishesItsProse(t *testing.T) {
	var mu sync.Mutex
	var order []string
	var posts []string
	events := make(chan string, 16)
	var fences int
	var lists int
	private := make(chan []byte, 1)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/v3/events") {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "event: stream.ready\ndata: {\"schemaVersion\":1,\"replay\":false,\"resyncRequired\":true,\"streamId\":\"0123456789abcdef0123456789abcdef\"}\n\n")
			w.(http.Flusher).Flush()
			for {
				select {
				case frame := <-events:
					io.WriteString(w, frame)
					w.(http.Flusher).Flush()
				case <-r.Context().Done():
					return
				}
			}
		}
		if strings.HasSuffix(r.URL.Path, "/events/fence") {
			var fence map[string]string
			json.NewDecoder(r.Body).Decode(&fence)
			body, _ := json.Marshal(map[string]any{"schemaVersion": 1, "requestId": fence["requestId"]})
			events <- "event: stream.fence\ndata: " + string(body) + "\n\n"
			mu.Lock()
			fences++
			first := fences == 1
			mu.Unlock()
			json.NewEncoder(w).Encode(fence)
			if first {
				body, _ := json.Marshal(map[string]any{"schemaVersion": 1, "id": "epoch:1", "type": "message.created", "data": map[string]any{"resourceVersion": 3, "message": map[string]any{"id": "1", "source": map[string]string{"kind": "twitch"}, "service": "Twitch", "channel": "123", "user": map[string]string{"service": "Twitch", "serviceId": "11", "name": "Mara"}, "time": time.Now().UTC().Format(time.RFC3339Nano), "chunks": []map[string]any{{"type": 0, "data": "hello from the stream"}}}}})
				events <- "event: message.created\ndata: " + string(body) + "\n\n"
			}
			return
		}
		if r.Method == http.MethodPost {
			body, _ := io.ReadAll(r.Body)
			mu.Lock()
			order = append(order, "public")
			posts = append(posts, string(body))
			mu.Unlock()
			writeSent(w, r, body)
			return
		}
		mu.Lock()
		lists++
		mu.Unlock()
		io.WriteString(w, `{"result":[],"total":0,"continue":null}`)
	}))
	defer api.Close()
	processorServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct{ Config, Subject int64 }
		json.Unmarshal(body, &req)
		if req.Config == 88 {
			if req.Subject != 42 || r.URL.RawQuery != "verbose=true" {
				t.Errorf("private request subject=%d query=%q", req.Subject, r.URL.RawQuery)
			}
			mu.Lock()
			order = append(order, "private")
			mu.Unlock()
			io.WriteString(w, `{"reply":"NEVER PUBLIC PRIVATE PROSE","noteDecision":{"state":"abstained"},"modelCalls":1}`)
			private <- body
			return
		}
		if req.Config != 77 || r.URL.RawQuery != "" {
			t.Errorf("chat request config=%d query=%q", req.Config, r.URL.RawQuery)
		}
		io.WriteString(w, `{"reply":"Gangolf: accepted public reply","produced":true}`)
	}))
	defer processorServer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logs := &lockedBuilder{}
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, env(map[string]string{"GANGOLF_TWITCH_BOT_USER_ID": "99",
			"GANGOLF_API_KEY": "key.secret", "GANGOLF_CHANNELS": "Twitch/123", "GANGOLF_API_URL": api.URL, "PROCESSOR_URL": processorServer.URL, "PROCESSOR_CONFIG": "77", "PROCESSOR_SUBJECT": "42", "PROCESSOR_MEMORY_CONFIG": "88"}), slog.New(slog.NewTextHandler(logs, nil)))
	}()
	var raw []byte
	select {
	case raw = <-private:
	case <-time.After(12 * time.Second):
		cancel()
		t.Fatal("private review did not arrive")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown did not cancel memory client")
	}
	var review struct {
		Input      string
		History    []json.RawMessage
		NoteSource struct {
			Speaker string
			Text    string
		}
	}
	if err := json.Unmarshal(raw, &review); err != nil {
		t.Fatal(err)
	}
	if review.Input != review.NoteSource.Text || review.NoteSource.Speaker != "Conversation transcript" || review.History == nil || len(review.History) != 0 {
		t.Fatalf("review=%+v", review)
	}
	var transcript struct {
		History []json.RawMessage
		Latest  []struct {
			Speaker string
			Text    string
		}
	}
	if err := json.Unmarshal([]byte(review.Input), &transcript); err != nil {
		t.Fatal(err)
	}
	if len(transcript.History) != 0 || len(transcript.Latest) != 1 || transcript.Latest[0].Speaker != "Mara" || transcript.Latest[0].Text != "hello from the stream" {
		t.Fatalf("transcript=%+v", transcript)
	}
	mu.Lock()
	defer mu.Unlock()
	if lists != 1 || strings.Join(order, ",") != "public,private" || len(posts) != 1 || !strings.Contains(posts[0], "accepted public reply") || strings.Contains(posts[0], "NEVER PUBLIC") {
		t.Fatalf("order=%v posts=%v", order, posts)
	}
	if !strings.Contains(logs.String(), "memoryEnabled=true") || strings.Contains(logs.String(), "NEVER PUBLIC") || strings.Contains(logs.String(), "key.secret") {
		t.Fatalf("logs=%s", logs.String())
	}
}

func TestRunRejectsMissingMemoryPolicyBeforeStarting(t *testing.T) {
	for _, value := range []string{"", "   "} {
		logs := &lockedBuilder{}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		err := run(ctx, env(map[string]string{
			"GANGOLF_TWITCH_BOT_USER_ID": "99",
			"GANGOLF_API_KEY":            "key.secret",
			"GANGOLF_CHANNELS":           "twitch/stream",
			"GANGOLF_API_URL":            ":invalid-message-url",
			"PROCESSOR_URL":              ":invalid-processor-url",
			"PROCESSOR_MEMORY_CONFIG":    value,
		}), slog.New(slog.NewTextHandler(logs, nil)))
		cancel()
		if err == nil || err.Error() != "PROCESSOR_MEMORY_CONFIG is required" {
			t.Fatalf("err=%v", err)
		}
		if logs.String() != "" {
			t.Fatalf("started with missing memory policy: %s", logs.String())
		}
	}
}
