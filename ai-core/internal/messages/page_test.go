package messages_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/telmengedar/gangolf/ai-core/internal/messages"
)

func serveWithLogs(t *testing.T, body string) (*messages.Client, *strings.Builder) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	var logs strings.Builder
	client := messages.New(server.URL, testKey, "GangolfBot", server.Client(), slog.New(slog.NewTextHandler(&logs, nil)))
	return client, &logs
}

const oneRow = `{"service":"s","channel":"c","user":{"name":"a"},"time":"2026-10-13T12:00:00","chunks":[{"type":0,"data":"one"}]}`

func TestTruncatedPageIsWarnedAbout(t *testing.T) {
	client, logs := serveWithLogs(t, `{"result":[`+oneRow+`],"total":5}`)

	lines, err := client.List(context.Background(), time.Now())

	if err != nil || len(lines) != 1 {
		t.Fatalf("lines = %d err = %v", len(lines), err)
	}
	if !strings.Contains(logs.String(), "truncated") || !strings.Contains(logs.String(), "total=5") || !strings.Contains(logs.String(), "returned=1") {
		t.Fatalf("logs = %s, want a warning with total 5 and 1 returned", logs.String())
	}
}

func TestCompletePageIsNotWarnedAbout(t *testing.T) {
	client, logs := serveWithLogs(t, `{"result":[`+oneRow+`],"total":1}`)

	if _, err := client.List(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}

	if strings.Contains(logs.String(), "truncated") {
		t.Fatalf("logs = %s, want no warning for a complete page", logs.String())
	}
}

func TestSpaceSeparatedTimeIsNotAccepted(t *testing.T) {
	client, _ := serveWithLogs(t, `{"result":[{"service":"s","channel":"c","user":{"name":"a"},"time":"2026-10-13 12:00:00","chunks":[{"type":0,"data":"x"}]}],"total":1}`)

	lines, err := client.List(context.Background(), time.Now())

	if err != nil || len(lines) != 0 {
		t.Fatalf("lines = %+v err = %v, want the unknown time format skipped", lines, err)
	}
}
