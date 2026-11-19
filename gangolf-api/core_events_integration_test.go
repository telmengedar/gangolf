package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestActualCoreConsumesProducerPushWithoutHealthyHistoryReads(t *testing.T) {
	_, s, _ := outboundFixture(t)
	s.now = time.Now
	var histories, posts, fences atomic.Int64
	synchronized := make(chan int64, 8)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v3/messages" && r.Method == "GET" {
			histories.Add(1)
		}
		if r.URL.Path == "/api/v3/messages" && r.Method == "POST" {
			posts.Add(1)
		}
		s.ServeHTTP(w, r)
		if r.URL.Path == "/api/v3/events/fence" {
			synchronized <- fences.Add(1)
		}
	}))
	defer api.Close()
	entered := make(chan string, 8)
	release := make(chan struct{})
	var runs atomic.Int64
	processor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("verbose") == "true" {
			respond(w, 200, map[string]any{"noteDecision": map[string]string{"state": "abstained"}, "modelCalls": 0})
			return
		}
		var input struct {
			Input   string
			History []struct{ Input string }
		}
		if json.NewDecoder(r.Body).Decode(&input) != nil {
			w.WriteHeader(400)
			return
		}
		entered <- input.Input
		n := runs.Add(1)
		if n == 1 {
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		respond(w, 200, map[string]any{"reply": "event answer " + string(rune('A'+n)), "produced": true})
	}))
	defer processor.Close()
	binary := filepath.Join(t.TempDir(), "ai-core")
	build := exec.Command("go", "build", "-o", binary, ".")
	build.Dir = "../ai-core"
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("core fixture build failed: %v %s", err, output)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	command := exec.CommandContext(ctx, binary)
	command.Env = []string{"GANGOLF_API_URL=" + api.URL + "/api", "GANGOLF_API_KEY=key.secret", "GANGOLF_TWITCH_BOT_USER_ID=900", "GANGOLF_CHANNELS=Twitch/101", "PROCESSOR_URL=" + processor.URL, "PROCESSOR_SUBJECT=1", "PROCESSOR_CONFIG=2", "PROCESSOR_MEMORY_CONFIG=88"}
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("core fixture did not stop")
		}
	}()
	waitFence := func(want int64) {
		t.Helper()
		for {
			select {
			case n := <-synchronized:
				if n >= want {
					return
				}
			case <-time.After(5 * time.Second):
				t.Fatal("real core did not reach ordered producer fence")
			}
		}
	}
	waitFence(1)
	time.Sleep(2200 * time.Millisecond)
	if histories.Load() != 1 || posts.Load() != 0 || runs.Load() != 0 {
		t.Fatal("idle actual core polled or produced work")
	}
	cfg, err := s.graph.loadConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	produce := func(id, text string) {
		t.Helper()
		event := twitchEvent{BroadcasterUserID: "101", BroadcasterUserLogin: "room", ChatterUserID: "700", ChatterUserLogin: "mara", ChatterUserName: "Mara", MessageID: id, publishedAt: time.Now().UTC()}
		event.Message.Text = text
		if _, err := s.ingestTwitch(context.Background(), cfg.twitch[0], event); err != nil {
			t.Fatal(err)
		}
	}
	produce("actual-one", "first real producer event")
	select {
	case text := <-entered:
		if !strings.Contains(text, "first real producer event") {
			t.Fatal("real producer payload lost")
		}
	case <-time.After(4 * time.Second):
		t.Fatal("real producer event did not wake model")
	}
	produce("actual-during", "during real blocked model")
	close(release)
	waitFence(2)
	if histories.Load() != 1 || posts.Load() != 1 {
		t.Fatal("healthy completed turn fetched history or repeated post")
	}
	produce("actual-after", "post fence reaction")
	select {
	case text := <-entered:
		if strings.Contains(text, "during real blocked model") || !strings.Contains(text, "post fence reaction") {
			t.Fatal("producer fence failed context-only turn boundary")
		}
	case <-time.After(4 * time.Second):
		t.Fatal("post-fence producer did not wake next turn")
	}
	waitFence(3)
	if histories.Load() != 1 || posts.Load() != 2 {
		t.Fatal("real event core retained hidden polling/closing history scan")
	}
}
