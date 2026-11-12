package messages_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/telmengedar/gangolf/ai-core/internal/messages"
	"github.com/telmengedar/gangolf/ai-core/internal/window"
)

func TestV3IRCSubmissionIsNotConfirmedDelivery(t *testing.T) {
	for _, mode := range []string{"submitted201", "submitted200", "wrong-key", "wrong-target", "wrong-text", "wrong-author", "wrong-transport", "no-transport", "sent", "unknown", "failed", "message-id", "empty-message-id", "null-message-id", "no-id", "source"} {
		t.Run(mode, func(t *testing.T) {
			attempts := 0
			var requestKey string
			var logs strings.Builder
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempts++
				requestKey = r.Header.Get("Idempotency-Key")
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				body["id"] = "77"
				delivery := map[string]any{"transport": "irc", "state": "submitted", "requestKey": requestKey}
				body["delivery"] = delivery
				switch mode {
				case "wrong-key":
					delivery["requestKey"] = "other"
				case "wrong-target":
					body["channel"] = "456"
				case "wrong-text":
					body["chunks"] = []map[string]any{{"type": 0, "data": "other"}}
				case "wrong-author":
					body["user"] = map[string]string{"service": "Twitch", "serviceId": "11", "name": "viewer"}
				case "wrong-transport":
					delivery["transport"] = "other"
				case "no-transport":
					delete(delivery, "transport")
				case "sent", "unknown", "failed":
					delivery["state"] = mode
				case "message-id":
					delivery["messageId"] = "made-up"
				case "empty-message-id":
					delivery["messageId"] = ""
				case "null-message-id":
					delivery["messageId"] = nil
				case "no-id":
					delete(body, "id")
				case "source":
					body["source"] = map[string]string{"kind": "twitch"}
				}
				if mode != "submitted200" {
					w.WriteHeader(201)
				}
				json.NewEncoder(w).Encode(body)
			}))
			defer server.Close()
			client := messages.New(server.URL, testKey, "99", server.Client(), slog.New(slog.NewTextHandler(&logs, nil)))
			err := client.Post(context.Background(), window.Key{Service: "Twitch", Channel: "123"}, "reply")
			var submission *messages.UnconfirmedSubmission
			accepted := errors.As(err, &submission)
			if err == nil || attempts != 1 || accepted != (mode == "submitted201" || mode == "submitted200") {
				t.Fatalf("attempts=%d accepted=%v err=%v", attempts, accepted, err)
			}
			if accepted && (submission.ID != "77" || submission.RequestKey != requestKey || !strings.Contains(err.Error(), "visibility unconfirmed")) {
				t.Fatalf("submission=%+v", submission)
			}
			if strings.Contains(logs.String(), "delivery confirmed") || strings.Contains(logs.String(), testKey) || strings.Contains(err.Error(), testKey) {
				t.Fatalf("unsafe outcome logs=%s err=%v", logs.String(), err)
			}
		})
	}
}
func TestV3PaginationIdentityAndOwn(t *testing.T) {
	bot := inbound("1", "99", "ChangedName")
	bot["source"].(map[string]string)["transport"] = "irc"
	human := inbound("2", "11", "GangolfBot")
	human["source"].(map[string]string)["transport"] = "irc"
	intent := inbound("3", "99", "GangolfBot")
	intent["delivery"] = map[string]string{"transport": "irc", "state": "submitted"}
	unknown := inbound("4", "11", "other")
	unknown["source"].(map[string]string)["transport"] = "unsupported"
	calls := 0
	client := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Query().Get("continue") == "" {
			page(w, []map[string]any{bot, human}, 5, 2)
			return
		}
		page(w, []map[string]any{human, intent, unknown}, 5, nil)
	})
	lines, err := client.List(context.Background(), time.Now().Add(-time.Hour))
	if err != nil || calls != 2 || len(lines) != 3 {
		t.Fatalf("calls=%d lines=%+v err=%v", calls, lines, err)
	}
	windows := window.New("GangolfBot")
	if added := windows.Ingest(lines, time.Now()); added != 2 {
		t.Fatalf("added=%d", added)
	}
	batch := windows.Take(window.Key{Service: "Twitch", Channel: "123"})
	if batch.Pending != 1 || batch.Input != "GangolfBot: hello chat" {
		t.Fatalf("batch=%+v", batch)
	}
	if _, ready := windows.Next(time.Now().Add(time.Hour), time.Second); ready {
		t.Fatal("own or intent triggered another turn")
	}
}
func TestV3IRCSourceDoesNotAcceptUserstateAsChat(t *testing.T) {
	row := inbound("1", "99", "bot")
	row["source"] = map[string]string{"kind": "userstate", "transport": "irc"}
	client := clientFor(t, func(w http.ResponseWriter, r *http.Request) { page(w, []map[string]any{row}, 1, nil) })
	lines, err := client.List(context.Background(), time.Now().Add(-time.Hour))
	if err != nil || len(lines) != 0 {
		t.Fatalf("lines=%+v err=%v", lines, err)
	}
}
