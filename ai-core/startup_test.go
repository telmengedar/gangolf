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

func TestSetAndDefaultedVariablesAreNamedInOrder(t *testing.T) {
	cfg, err := loadConfig(env(map[string]string{
		"GANGOLF_API_KEY":   "k.s",
		"GANGOLF_CHANNELS":  "twitch/stream",
		"PROCESSOR_SUBJECT": "42",
		"PROCESSOR_CONFIG":  "7",
	}))

	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(cfg.set, ",") != "GANGOLF_API_KEY,GANGOLF_CHANNELS,PROCESSOR_SUBJECT,PROCESSOR_CONFIG" {
		t.Fatalf("set = %v", cfg.set)
	}
	if strings.Join(cfg.defaulted, ",") != "GANGOLF_API_URL,GANGOLF_BOT_NAME,PROCESSOR_URL" {
		t.Fatalf("defaulted = %v", cfg.defaulted)
	}
}

func TestStartupLogNamesVariablesAndNeverPrintsValues(t *testing.T) {
	cfg, err := loadConfig(env(map[string]string{
		"GANGOLF_API_URL":   "https://user:hunter2@example.invalid/api",
		"GANGOLF_API_KEY":   "keyid.topsecretvalue",
		"GANGOLF_CHANNELS":  "twitch/privatechannel",
		"GANGOLF_BOT_NAME":  "Distinctivebot",
		"PROCESSOR_URL":     "http://internal-host.invalid:8080",
		"PROCESSOR_SUBJECT": "424242",
		"PROCESSOR_CONFIG":  "737373",
	}))
	if err != nil {
		t.Fatal(err)
	}
	var logs strings.Builder

	logStartup(slog.New(slog.NewTextHandler(&logs, nil)), cfg)

	out := logs.String()
	for _, value := range []string{"hunter2", "topsecretvalue", "privatechannel", "Distinctivebot", "internal-host", "424242", "737373", "example.invalid"} {
		if strings.Contains(out, value) {
			t.Fatalf("startup log printed a configured value %q: %s", value, out)
		}
	}
	for _, name := range []string{"GANGOLF_API_KEY", "GANGOLF_API_URL", "PROCESSOR_SUBJECT", "PROCESSOR_CONFIG"} {
		if !strings.Contains(out, name) {
			t.Fatalf("startup log = %s, want the variable name %s", out, name)
		}
	}
	if !strings.Contains(out, "channels=1") {
		t.Fatalf("startup log = %s, want the channel count", out)
	}
}

type recordedRequest struct {
	method string
	path   string
	auth   string
	body   []byte
}

type recorder struct {
	mu       sync.Mutex
	requests []recordedRequest
}

func (r *recorder) record(req *http.Request) recordedRequest {
	body, _ := io.ReadAll(req.Body)
	entry := recordedRequest{method: req.Method, path: req.URL.Path, auth: req.Header.Get("Authorization"), body: body}
	r.mu.Lock()
	r.requests = append(r.requests, entry)
	r.mu.Unlock()
	return entry
}

func (r *recorder) all() []recordedRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recordedRequest(nil), r.requests...)
}

func TestEveryConfiguredValueReachesTheWire(t *testing.T) {
	const (
		apiSegment = "/wiretap-api"
		apiKey     = "wirekey.distinctive-secret"
		botName    = "Wiretap"
	)
	var (
		api       recorder
		runs      recorder
		lists     int
		stamp     string
		posted    = make(chan struct{}, 1)
		listMutex sync.Mutex
	)
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entry := api.record(r)
		if entry.method == http.MethodPost {
			select {
			case posted <- struct{}{}:
			default:
			}
			return
		}
		listMutex.Lock()
		lists++
		first := lists == 1
		if stamp == "" && !first {
			stamp = time.Now().UTC().Format(time.RFC3339Nano)
		}
		at := stamp
		listMutex.Unlock()
		if first {
			io.WriteString(w, `{"result":[],"total":0}`)
			return
		}
		io.WriteString(w, `{"result":[{"service":"wireservice","channel":"wirechannel","user":{"name":"viewer"},"time":"`+at+`","chunks":[{"type":0,"data":"anyone there"}]}],"total":1}`)
	}))
	defer apiServer.Close()
	processorServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		runs.record(r)
		io.WriteString(w, `{"reply":"`+botName+`: hello","produced":true}`)
	}))
	defer processorServer.Close()
	values := map[string]string{
		"GANGOLF_API_URL":   apiServer.URL + apiSegment,
		"GANGOLF_API_KEY":   apiKey,
		"GANGOLF_BOT_NAME":  botName,
		"GANGOLF_CHANNELS":  "wireservice/wirechannel",
		"PROCESSOR_URL":     processorServer.URL,
		"PROCESSOR_SUBJECT": "42",
		"PROCESSOR_CONFIG":  "7",
	}
	logs := &lockedBuilder{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		finished <- run(ctx, env(values), slog.New(slog.NewTextHandler(logs, nil)))
	}()

	select {
	case <-posted:
	case <-time.After(20 * time.Second):
		cancel()
		t.Fatalf("no reply was posted within the deadline\nprocessor requests: %d\nmessage requests: %+v\nlogs: %s", len(runs.all()), api.all(), logs.String())
	}
	cancel()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatalf("run returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return after the context ended")
	}

	runRequests := runs.all()
	if len(runRequests) == 0 {
		t.Fatal("the processor at PROCESSOR_URL received no request")
	}
	for _, request := range runRequests {
		if request.method != http.MethodPost || request.path != "/runs" {
			t.Fatalf("processor request = %s %s, want POST /runs", request.method, request.path)
		}
		var body struct {
			Subject int64 `json:"subject"`
			Config  int64 `json:"config"`
		}
		if err := json.Unmarshal(request.body, &body); err != nil {
			t.Fatal(err)
		}
		if body.Subject != 42 {
			t.Fatalf("run subject = %d, want 42", body.Subject)
		}
		if body.Config != 7 {
			t.Fatalf("run config = %d, want 7", body.Config)
		}
	}
	var posts []recordedRequest
	for _, request := range api.all() {
		if !strings.HasPrefix(request.path, apiSegment+"/v1/messages") {
			t.Fatalf("message request path = %s, want prefix %s/v1/messages", request.path, apiSegment)
		}
		if request.auth != "Bearer "+apiKey {
			t.Fatalf("message request Authorization = %q, want the configured key", request.auth)
		}
		if request.method == http.MethodPost {
			posts = append(posts, request)
		}
	}
	if len(posts) == 0 {
		t.Fatal("no reply reached the message API")
	}
	var sent []struct {
		Service string `json:"service"`
		Channel string `json:"channel"`
		User    struct {
			Name string `json:"name"`
		} `json:"user"`
		Chunks []struct {
			Data string `json:"data"`
		} `json:"chunks"`
	}
	if err := json.Unmarshal(posts[0].body, &sent); err != nil || len(sent) != 1 || len(sent[0].Chunks) != 1 {
		t.Fatalf("posted body = %s (%v)", posts[0].body, err)
	}
	if sent[0].User.Name != botName {
		t.Fatalf("posted user name = %q, want %q", sent[0].User.Name, botName)
	}
	if sent[0].Chunks[0].Data != "hello" {
		t.Fatalf("posted text = %q, want %q: the configured bot name is not stripped as a label", sent[0].Chunks[0].Data, "hello")
	}
	if sent[0].Service != "wireservice" || sent[0].Channel != "wirechannel" {
		t.Fatalf("posted to %s/%s, want wireservice/wirechannel", sent[0].Service, sent[0].Channel)
	}
}

func TestRunReturnsAConfigurationErrorWithoutStartingAnything(t *testing.T) {
	logs := &lockedBuilder{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		finished <- run(ctx, env(map[string]string{"GANGOLF_CHANNELS": "twitch/stream"}), slog.New(slog.NewTextHandler(logs, nil)))
	}()

	select {
	case err := <-finished:
		if err == nil || !strings.Contains(err.Error(), "GANGOLF_API_KEY") {
			t.Fatalf("run returned %v, want the missing key error", err)
		}
	case <-time.After(3 * time.Second):
		cancel()
		<-finished
		t.Fatalf("run kept running after an invalid configuration\nlogs: %s", logs.String())
	}
	if logs.String() != "" {
		t.Fatalf("run logged %q before the configuration was valid", logs.String())
	}
}

type lockedBuilder struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuilder) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuilder) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}
