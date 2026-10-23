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

func TestMemoryJobFreezesTrustedUtterancesAndHistoryBeforeDispatch(t *testing.T) {
	requests := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		requests <- body
		io.WriteString(w, `{"noteDecision":{"state":"abstained"},"modelCalls":1}`)
	}))
	defer server.Close()
	c := New(Config{MemoryReviewer: processor.New(server.URL, 1090, 4242, server.Client())}, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Now)
	c.memoryJobs = make(chan memoryJob, 1)
	batch := window.Batch{
		Input:   "Sora: I keep locally adapted seeds.",
		Lines:   []window.Utterance{{Speaker: "Sora", Text: "I keep locally adapted seeds."}},
		History: []window.Exchange{{Input: "Mara: earlier source", Reply: "excluded reply"}},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.offerMemory(ctx, window.Key{Service: "twitch", Channel: "stream"}, batch, "delivered reply")
	if len(c.memoryJobs) != 1 {
		t.Fatal("review was not enqueued")
	}
	batch.Lines[0].Speaker = "changed speaker after offer"
	batch.Lines[0].Text = "changed text after offer"
	batch.History[0].Input = "changed history after offer"
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.reviewMemory(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("memory worker did not stop")
		}
	})
	var raw []byte
	select {
	case raw = <-requests:
	case <-time.After(time.Second):
		t.Fatal("queued review did not reach Processor")
	}
	var request struct {
		Input      string
		NoteSource struct{ Speaker, Text string }
	}
	if err := json.Unmarshal(raw, &request); err != nil {
		t.Fatal(err)
	}
	_, data, ok := strings.Cut(request.Input, "===== COMPLETED INTERACTION DATA =====\n")
	var modelSource struct{ Speaker, Text string }
	if !ok || json.Unmarshal([]byte(data), &modelSource) != nil {
		t.Fatalf("model source missing: %s", request.Input)
	}
	if modelSource != request.NoteSource || modelSource.Speaker != "Conversation transcript" {
		t.Fatalf("source mismatch: model=%+v carrier=%+v", modelSource, request.NoteSource)
	}
	want := `{"history":[{"input":"Mara: earlier source"}],"latest":[{"speaker":"Sora","text":"I keep locally adapted seeds."}]}`
	if modelSource.Text != want {
		t.Fatalf("source changed after offer: got %s want %s", modelSource.Text, want)
	}
}
