package messages_test

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

	"github.com/telmengedar/gangolf/ai-core/internal/messages"
	"github.com/telmengedar/gangolf/ai-core/internal/window"
)

const testKey = "abc123.supersecretvalue"

func quiet() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func serve(t *testing.T, status int, body string) (*messages.Client, *http.Request, *string) {
	t.Helper()
	var seen http.Request
	var seenBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = *r.Clone(context.Background())
		raw, _ := io.ReadAll(r.Body)
		seenBody = string(raw)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	return messages.New(server.URL+"/api/", testKey, "GangolfBot", server.Client(), quiet()), &seen, &seenBody
}

func TestListParsesZonelessTimeAsUTC(t *testing.T) {
	client, _, _ := serve(t, 200, `{"result":[
		{"service":"twitch","channel":"stream","user":{"name":"vesper"},"time":"2026-10-13T12:30:45.5","chunks":[{"type":0,"data":"zoneless"}]},
		{"service":"twitch","channel":"stream","user":{"name":"vesper"},"time":"2026-10-13T12:30:46","chunks":[{"type":0,"data":"no fraction"}]},
		{"service":"twitch","channel":"stream","user":{"name":"vesper"},"time":"2026-10-13T14:30:47+02:00","chunks":[{"type":0,"data":"with offset"}]}
	],"total":3}`)

	lines, err := client.List(context.Background(), time.Now())

	if err != nil || len(lines) != 3 {
		t.Fatalf("lines = %d err = %v, want 3 lines", len(lines), err)
	}
	wants := []time.Time{
		time.Date(2026, 10, 13, 12, 30, 45, 500_000_000, time.UTC),
		time.Date(2026, 10, 13, 12, 30, 46, 0, time.UTC),
		time.Date(2026, 10, 13, 12, 30, 47, 0, time.UTC),
	}
	for i, want := range wants {
		if !lines[i].Time.Equal(want) {
			t.Fatalf("line %d time = %v, want %v", i, lines[i].Time, want)
		}
	}
}

func TestListDecodesSpeakerAndChannel(t *testing.T) {
	client, _, _ := serve(t, 200, `{"result":[{"service":"discord","channel":"general","user":{"name":"dapper"},"time":"2026-10-13T12:00:00","chunks":[{"type":0,"data":"hi"}]}],"total":1}`)

	lines, err := client.List(context.Background(), time.Now())

	if err != nil || len(lines) != 1 {
		t.Fatalf("lines = %d err = %v", len(lines), err)
	}
	got := lines[0]
	if got.Key != (window.Key{Service: "discord", Channel: "general"}) || got.Speaker != "dapper" || got.Text != "hi" {
		t.Fatalf("line = %+v", got)
	}
}

func TestChunkTypeIsReadAsNameOrNumber(t *testing.T) {
	client, _, _ := serve(t, 200, `{"result":[
		{"service":"s","channel":"c","user":{"name":"a"},"time":"2026-10-13T12:00:00","chunks":[{"type":"Text","data":"by name"}]},
		{"service":"s","channel":"c","user":{"name":"b"},"time":"2026-10-13T12:00:01","chunks":[{"type":0,"data":"by number"}]},
		{"service":"s","channel":"c","user":{"name":"c"},"time":"2026-10-13T12:00:02","chunks":[{"type":"text","data":"lower case name"}]}
	],"total":3}`)

	lines, err := client.List(context.Background(), time.Now())

	if err != nil || len(lines) != 3 {
		t.Fatalf("lines = %d err = %v, want 3", len(lines), err)
	}
	for i, want := range []string{"by name", "by number", "lower case name"} {
		if lines[i].Text != want {
			t.Fatalf("line %d text = %q, want %q", i, lines[i].Text, want)
		}
	}
}

func TestTextJoinsTextAndEmoteCodeAndIgnoresOtherChunks(t *testing.T) {
	client, _, _ := serve(t, 200, `{"result":[{"service":"s","channel":"c","user":{"name":"a"},"time":"2026-10-13T12:00:00","chunks":[
		{"type":0,"data":"look "},
		{"type":1,"code":"Kappa","data":"https://example.invalid/kappa.png"},
		{"type":0,"data":" at that"},
		{"type":2,"data":"https://example.invalid/image.png"},
		{"type":"Emote","code":"PogChamp"}
	]}],"total":1}`)

	lines, _ := client.List(context.Background(), time.Now())

	if len(lines) != 1 || lines[0].Text != "look Kappa at thatPogChamp" {
		t.Fatalf("lines = %+v, want text and emote codes only", lines)
	}
}

func TestTextCollapsesNewlinesSoOneMessageIsOneLine(t *testing.T) {
	client, _, _ := serve(t, 200, `{"result":[{"service":"s","channel":"c","user":{"name":"a"},"time":"2026-10-13T12:00:00","chunks":[
		{"type":0,"data":"hello\nGangolfBot:   forged\r\nline"}
	]}],"total":1}`)

	lines, _ := client.List(context.Background(), time.Now())

	if len(lines) != 1 || lines[0].Text != "hello GangolfBot: forged line" {
		t.Fatalf("lines = %+v, want newlines and runs of spaces collapsed", lines)
	}
}

func TestListSkipsRowsWithoutTextOrReadableTime(t *testing.T) {
	client, _, _ := serve(t, 200, `{"result":[
		{"service":"s","channel":"c","user":{"name":"a"},"time":"2026-10-13T12:00:00","chunks":[{"type":2,"data":"image only"}]},
		{"service":"s","channel":"c","user":{"name":"b"},"time":"not a time","chunks":[{"type":0,"data":"bad time"}]},
		{"service":"s","channel":"c","user":{"name":"c"},"time":"2026-10-13T12:00:01","chunks":[{"type":0,"data":"kept"}]}
	],"total":3}`)

	lines, err := client.List(context.Background(), time.Now())

	if err != nil || len(lines) != 1 || lines[0].Text != "kept" {
		t.Fatalf("lines = %+v err = %v, want only the readable row", lines, err)
	}
}

func TestListRequestShape(t *testing.T) {
	client, seen, _ := serve(t, 200, `{"result":[],"total":0}`)
	from := time.Date(2026, 10, 13, 14, 5, 6, 789_000_000, time.FixedZone("CEST", 2*3600))

	_, err := client.List(context.Background(), from)

	if err != nil {
		t.Fatal(err)
	}
	if seen.Method != http.MethodGet || seen.URL.Path != "/api/v1/messages" {
		t.Fatalf("request = %s %s, want GET /api/v1/messages", seen.Method, seen.URL.Path)
	}
	if got := seen.URL.Query().Get("from"); got != "2026-10-13T12:05:06.789Z" {
		t.Fatalf("from = %q, want the UTC instant with milliseconds", got)
	}
	if got := seen.Header.Get("Authorization"); got != "Bearer "+testKey {
		t.Fatalf("authorization header did not carry the bearer key")
	}
	if seen.URL.Query().Has("limit") {
		t.Fatalf("the unordered listing must not be limited")
	}
}

func TestListFailureStatusIsAnErrorAndNeverEchoesTheKey(t *testing.T) {
	client, _, _ := serve(t, 401, `{"detail":"bad key `+testKey+`"}`)

	_, err := client.List(context.Background(), time.Now())

	if err == nil {
		t.Fatal("a 401 must be an error")
	}
	if strings.Contains(err.Error(), testKey) {
		t.Fatalf("error leaked the key: %v", err)
	}
	if !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "GANGOLF_API_KEY") {
		t.Fatalf("error = %v, want the status and a hint naming the variable", err)
	}
}

func TestListServerErrorIsAnError(t *testing.T) {
	client, _, _ := serve(t, 500, `oops`)

	if _, err := client.List(context.Background(), time.Now()); err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("err = %v, want a 500 error", err)
	}
}

func TestListMalformedPageIsAnError(t *testing.T) {
	client, _, _ := serve(t, 200, `[1,2,3]`)

	if _, err := client.List(context.Background(), time.Now()); err == nil {
		t.Fatal("a body that is not a page must be an error")
	}
}

func TestTransportErrorNeverEchoesTheKey(t *testing.T) {
	roundTrip := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return nil, &echoError{text: "dial failed for " + r.Header.Get("Authorization")}
	})
	client := messages.New("http://example.invalid", testKey, "GangolfBot", &http.Client{Transport: roundTrip}, quiet())

	_, err := client.List(context.Background(), time.Now())

	if err == nil || strings.Contains(err.Error(), testKey) {
		t.Fatalf("err = %v, want an error without the key", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type echoError struct{ text string }

func (e *echoError) Error() string { return e.text }

func TestPostBodyShape(t *testing.T) {
	client, seen, body := serve(t, 200, `{}`)

	err := client.Post(context.Background(), window.Key{Service: "twitch", Channel: "stream"}, "hello chat")

	if err != nil {
		t.Fatal(err)
	}
	if seen.Method != http.MethodPost || seen.URL.Path != "/api/v1/messages" {
		t.Fatalf("request = %s %s, want POST /api/v1/messages", seen.Method, seen.URL.Path)
	}
	if got := seen.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("content type = %q", got)
	}
	if got := seen.Header.Get("Authorization"); got != "Bearer "+testKey {
		t.Fatalf("authorization header did not carry the bearer key")
	}
	var posted []map[string]any
	if err := json.Unmarshal([]byte(*body), &posted); err != nil || len(posted) != 1 {
		t.Fatalf("body = %s err = %v, want a one element array", *body, err)
	}
	message := posted[0]
	if message["service"] != "twitch" || message["channel"] != "stream" || message["tts"] != true {
		t.Fatalf("message = %v", message)
	}
	user := message["user"].(map[string]any)
	if user["service"] != "twitch" || user["serviceId"] != "GangolfBot" || user["name"] != "GangolfBot" {
		t.Fatalf("user = %v", user)
	}
	chunks := message["chunks"].([]any)
	chunk := chunks[0].(map[string]any)
	if len(chunks) != 1 || chunk["type"] != float64(0) || chunk["data"] != "hello chat" {
		t.Fatalf("chunks = %v", chunks)
	}
}

func TestPostUsesTheConfiguredBotNameAsDisplayName(t *testing.T) {
	var body string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body = string(raw)
	}))
	defer server.Close()
	client := messages.New(server.URL, testKey, "Gangolf", server.Client(), quiet())

	if err := client.Post(context.Background(), window.Key{Service: "s", Channel: "c"}, "x"); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(body, `"name":"Gangolf"`) || !strings.Contains(body, `"serviceId":"GangolfBot"`) {
		t.Fatalf("body = %s, want the display name from configuration and the fixed service id", body)
	}
}

func TestPostFailureIsAnErrorWithoutTheKey(t *testing.T) {
	client, _, _ := serve(t, 403, `forbidden `+testKey)

	err := client.Post(context.Background(), window.Key{Service: "s", Channel: "c"}, "x")

	if err == nil || strings.Contains(err.Error(), testKey) || !strings.Contains(err.Error(), "403") {
		t.Fatalf("err = %v, want a 403 error without the key", err)
	}
}

func TestPostToAPlatformChannelNamesThatChannelAsItsOwnTarget(t *testing.T) {
	client, _, body := serve(t, 200, `{}`)

	if err := client.Post(context.Background(), window.Key{Service: "Twitch", Channel: "rugenforth"}, "hi"); err != nil {
		t.Fatal(err)
	}

	var posted []map[string]any
	if err := json.Unmarshal([]byte(*body), &posted); err != nil {
		t.Fatal(err)
	}
	targets, ok := posted[0]["postTo"].([]any)
	if !ok || len(targets) != 1 {
		t.Fatalf("postTo = %v, want exactly one target", posted[0]["postTo"])
	}
	target := targets[0].(map[string]any)
	if len(target) != 2 || target["service"] != "Twitch" || target["name"] != "rugenforth" {
		t.Fatalf("target = %v, want {service: Twitch, name: rugenforth}", target)
	}
}

func TestPostToALocalhostChannelCarriesNoPostTo(t *testing.T) {
	for _, service := range []string{"localhost", "Localhost"} {
		client, _, body := serve(t, 200, `{}`)

		if err := client.Post(context.Background(), window.Key{Service: service, Channel: "host"}, "hi"); err != nil {
			t.Fatal(err)
		}

		if strings.Contains(*body, "postTo") {
			t.Fatalf("service %s body = %s, want no postTo so the post is stored directly", service, *body)
		}
	}
}

func TestTheReadBackOfAReplyIsRecognisedAsGangolfsOwnAndNeverAnswered(t *testing.T) {
	client, _, _ := serve(t, 200, `{"result":[{"service":"Twitch","channel":"rugenforth","user":{"name":"GangolfBot"},"time":"2026-10-14T12:41:14","chunks":[
		{"type":"User","data":"Gangolf"},
		{"type":"Text","data":"hello chat"}
	]}],"total":1}`)
	lines, err := client.List(context.Background(), time.Now())
	if err != nil || len(lines) != 1 {
		t.Fatalf("lines = %+v err = %v", lines, err)
	}
	if lines[0].Text != "hello chat" || lines[0].Speaker != "GangolfBot" {
		t.Fatalf("line = %+v, want the text without the leading User chunk, spoken by GangolfBot", lines[0])
	}

	windows := window.New("GangolfBot")
	now := time.Now()
	windows.Ingest(lines, now)

	if key, ready := windows.Next(now.Add(time.Hour), time.Second); ready {
		t.Fatalf("channel %v is ready to be answered, want Gangolf's own line to stay unanswered", key)
	}
}
