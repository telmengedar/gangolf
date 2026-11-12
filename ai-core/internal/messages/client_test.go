package messages_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/telmengedar/gangolf/ai-core/internal/messages"
	"github.com/telmengedar/gangolf/ai-core/internal/window"
)

const testKey = "abc123.supersecretvalue"

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
func inbound(id, author, name string) map[string]any {
	return map[string]any{"id": id, "channelId": "900", "userId": "800", "service": "Twitch", "channel": "123", "user": map[string]string{"service": "Twitch", "serviceId": author, "name": name}, "time": "2026-10-13T12:00:00Z", "chunks": []map[string]any{{"type": 0, "data": "hello chat"}}, "source": map[string]string{"kind": "twitch", "messageId": "source-id"}}
}
func clientFor(t *testing.T, handler http.HandlerFunc) *messages.Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return messages.New(server.URL+"/api/", testKey, "99", server.Client(), quiet())
}
func page(w http.ResponseWriter, rows []map[string]any, total int, next any) {
	json.NewEncoder(w).Encode(map[string]any{"result": rows, "total": total, "continue": next})
}
func TestV3MessageIdentityAndOwnEcho(t *testing.T) {
	rows := []map[string]any{inbound("1", "99", "ChangedName"), inbound("2", "11", "GangolfBot")}
	for _, state := range []string{"sent", "failed", "unknown"} {
		row := inbound("3", "99", "bot")
		row["delivery"] = map[string]string{"state": state}
		rows = append(rows, row)
	}
	synthetic := inbound("4", "11", "synthetic")
	delete(synthetic, "source")
	rows = append(rows, synthetic)
	client := clientFor(t, func(w http.ResponseWriter, r *http.Request) { page(w, rows, len(rows), nil) })
	lines, err := client.List(context.Background(), time.Now().Add(-time.Hour))
	if err != nil || len(lines) != 2 {
		t.Fatalf("lines=%+v err=%v", lines, err)
	}
	if lines[0].ID != "1" || lines[0].Channel != "123" || lines[0].Speaker != "ChangedName" || !lines[0].Own || lines[1].Own {
		t.Fatalf("identity=%+v", lines)
	}
	windows := window.New("GangolfBot")
	windows.Ingest(lines, time.Now())
	batch := windows.Take(lines[0].Key)
	if batch.Pending != 1 || batch.Input != "GangolfBot: hello chat" {
		t.Fatalf("batch=%+v", batch)
	}
}
func TestV3ListTextAndTimeDecoding(t *testing.T) {
	row := inbound("1", "11", "viewer")
	row["time"] = "2026-10-13T14:00:00+02:00"
	row["chunks"] = []map[string]any{{"type": "Text", "data": "look\n "}, {"type": 1, "code": "Kappa"}, {"type": 2, "data": "ignored"}, {"type": "text", "data": "  here"}}
	bad := inbound("2", "11", "viewer")
	bad["time"] = "unreadable"
	empty := inbound("3", "11", "viewer")
	empty["chunks"] = []map[string]any{{"type": 2, "data": "image"}}
	client := clientFor(t, func(w http.ResponseWriter, r *http.Request) { page(w, []map[string]any{row, bad, empty}, 3, nil) })
	lines, err := client.List(context.Background(), time.Now().Add(-time.Hour))
	if err != nil || len(lines) != 1 || lines[0].Text != "look Kappa here" || lines[0].Time.Format(time.RFC3339) != "2026-10-13T12:00:00Z" {
		t.Fatalf("lines=%+v err=%v", lines, err)
	}
}
func TestV3ReplyPostContract(t *testing.T) {
	var keys []string
	client := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/api/v3/messages" || r.URL.RawQuery != "" || r.Header.Get("Authorization") != "Bearer "+testKey || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("invalid request %s %s", r.Method, r.URL)
		}
		key := r.Header.Values("Idempotency-Key")
		if len(key) != 1 || !regexp.MustCompile(`^[a-f0-9]{32}$`).MatchString(key[0]) {
			t.Fatalf("keys=%v", key)
		}
		keys = append(keys, key[0])
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if len(body) != 5 || body["service"] != "Twitch" || body["channel"] != "123" || body["tts"] != false {
			t.Fatalf("body=%v", body)
		}
		user := body["user"].(map[string]any)
		if len(user) != 3 || user["service"] != "Twitch" || user["serviceId"] != "GangolfBot" || user["name"] != "GangolfBot" {
			t.Fatalf("user=%v", user)
		}
		body["id"] = "10"
		body["delivery"] = map[string]string{"state": "sent", "messageId": "twitch-send", "requestKey": key[0]}
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(body)
	})
	for range 2 {
		if err := client.Post(context.Background(), window.Key{Service: "Twitch", Channel: "123"}, "expected reply"); err != nil {
			t.Fatal(err)
		}
	}
	if len(keys) != 2 || keys[0] == keys[1] {
		t.Fatalf("keys=%v", keys)
	}
}
func TestV3PostRequiresConfirmedSent(t *testing.T) {
	for _, mode := range []string{"sent200", "sent201", "unknown", "failed", "missing", "wrong-key", "wrong-channel", "wrong-author", "wrong-text", "no-id", "no-message-id", "no-tts", "wrong-tts", "reply-to", "malformed", "401", "403", "422", "429", "502", "503", "redirect"} {
		t.Run(mode, func(t *testing.T) {
			attempts := 0
			client := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
				attempts++
				var body map[string]any
				json.NewDecoder(r.Body).Decode(&body)
				if len(mode) == 3 && mode[0] >= '4' && mode[0] <= '5' {
					status := 0
					for _, c := range mode {
						status = status*10 + int(c-'0')
					}
					w.WriteHeader(status)
					io.WriteString(w, testKey)
					return
				}
				if mode == "redirect" {
					w.Header().Set("Location", "/elsewhere")
					w.WriteHeader(307)
					return
				}
				if mode == "malformed" {
					io.WriteString(w, `broken `+testKey)
					return
				}
				body["id"] = "10"
				delivery := map[string]string{"state": "sent", "messageId": "twitch-id", "requestKey": r.Header.Get("Idempotency-Key")}
				body["delivery"] = delivery
				switch mode {
				case "unknown", "failed":
					delivery["state"] = mode
				case "missing":
					delete(body, "delivery")
				case "wrong-key":
					delivery["requestKey"] = "different"
				case "wrong-channel":
					body["channel"] = "456"
				case "wrong-author":
					body["user"] = map[string]string{"name": "other"}
				case "wrong-text":
					body["chunks"] = []map[string]any{{"type": 0, "data": "other"}}
				case "no-id":
					delete(body, "id")
				case "no-message-id":
					delivery["messageId"] = ""
				case "no-tts":
					delete(body, "tts")
				case "wrong-tts":
					body["tts"] = true
				case "reply-to":
					body["replyTo"] = "100"
				}
				if mode == "sent201" {
					w.WriteHeader(201)
				}
				json.NewEncoder(w).Encode(body)
			})
			err := client.Post(context.Background(), window.Key{Service: "Twitch", Channel: "123"}, "reply")
			success := mode == "sent200" || mode == "sent201"
			if attempts != 1 || (err == nil) != success {
				t.Fatalf("attempts=%d err=%v", attempts, err)
			}
			if err != nil && strings.Contains(err.Error(), testKey) {
				t.Fatal("credential leaked")
			}
		})
	}
}
func TestV3TransportAndAuthenticationErrorsNeverExposeSecrets(t *testing.T) {
	client := messages.New("http://example.invalid", testKey, "99", &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) { return nil, errors.New(testKey) })}, quiet())
	for _, err := range []error{func() error { _, e := client.List(context.Background(), time.Now()); return e }(), client.Post(context.Background(), window.Key{Service: "Twitch", Channel: "123"}, "reply")} {
		if err == nil || strings.Contains(err.Error(), testKey) {
			t.Fatalf("err=%v", err)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestV3PostDisablesTransportReplayAndReportsSafeFailureReference(t *testing.T) {
	attempts := 0
	client := messages.New("http://example.invalid", testKey, "99", &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		attempts++
		if r.GetBody != nil {
			t.Error("post body is replayable")
		}
		if r.Header.Get("Idempotency-Key") == "" {
			t.Error("missing request key")
		}
		return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader(`{"id":"1234","code":"delivery_outcome_unknown","text":"` + testKey + `"}`)), Header: make(http.Header)}, nil
	})}, quiet())
	err := client.Post(context.Background(), window.Key{Service: "Twitch", Channel: "123"}, "reply")
	if attempts != 1 || err == nil || !strings.Contains(err.Error(), "id=1234") || !strings.Contains(err.Error(), "code=delivery_outcome_unknown") || strings.Contains(err.Error(), testKey) {
		t.Fatalf("attempts=%d err=%v", attempts, err)
	}
}
func TestV3PostRejectsUnsupportedTargetsBeforeHTTP(t *testing.T) {
	attempts := 0
	client := messages.New("http://example.invalid", testKey, "99", &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		attempts++
		return nil, errors.New("unexpected request")
	})}, quiet())
	for _, key := range []window.Key{{Service: "twitch", Channel: "123"}, {Service: "Twitch", Channel: "login"}, {Service: "Twitch", Channel: "01"}} {
		if err := client.Post(context.Background(), key, "reply"); err == nil {
			t.Fatal("unsupported target accepted")
		}
	}
	if attempts != 0 {
		t.Fatalf("attempts=%d", attempts)
	}
}
