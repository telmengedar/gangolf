package messages_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestV3MessageListPagesAtomically(t *testing.T) {
	for _, mode := range []string{"complete", "401", "503", "malformed", "repeated", "missing-continue", "missing-result", "null-result", "wrong-total", "fractional", "empty-continuing"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			var cutoff, from string
			client := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				q := r.URL.Query()
				if r.Method != "GET" || r.URL.Path != "/api/v3/messages" || r.Header.Get("Authorization") != "Bearer "+testKey || q.Get("sort") != "time" || q.Get("count") != "500" || q.Get("descending") != "false" || q.Has("apiKey") {
					t.Errorf("request=%s", r.URL)
				}
				if calls == 1 {
					cutoff = q.Get("to")
					from = q.Get("from")
					if _, err := time.Parse(time.RFC3339Nano, cutoff); err != nil {
						t.Error(err)
					}
					if q.Has("continue") {
						t.Error("initial continuation")
					}
					page(w, []map[string]any{inbound("1", "11", "viewer")}, 2, 1)
					return
				}
				if q.Get("to") != cutoff || q.Get("from") != from || q.Get("continue") != "1" {
					t.Errorf("changed scan=%s", r.URL)
				}
				switch mode {
				case "401":
					w.WriteHeader(401)
				case "503":
					w.WriteHeader(503)
				case "malformed":
					w.Write([]byte(`not json`))
				case "repeated":
					page(w, []map[string]any{inbound("2", "11", "viewer")}, 3, 1)
				case "missing-continue":
					w.Write([]byte(`{"result":[],"total":1}`))
				case "missing-result":
					w.Write([]byte(`{"total":1,"continue":null}`))
				case "null-result":
					w.Write([]byte(`{"result":null,"total":1,"continue":null}`))
				case "wrong-total":
					page(w, []map[string]any{inbound("2", "11", "viewer")}, 3, nil)
				case "fractional":
					json.NewEncoder(w).Encode(map[string]any{"result": []any{}, "total": 2, "continue": 1.5})
				case "empty-continuing":
					page(w, []map[string]any{}, 3, 2)
				default:
					page(w, []map[string]any{inbound("2", "11", "viewer")}, 2, nil)
				}
			})
			instant := time.Date(2026, 10, 13, 11, 0, 0, 123456789, time.UTC)
			lines, err := client.List(context.Background(), instant)
			if calls != 2 || from != instant.Format(time.RFC3339Nano) {
				t.Fatalf("calls=%d from=%s", calls, from)
			}
			if mode == "complete" {
				if err != nil || len(lines) != 2 || lines[0].ID != "1" || lines[1].ID != "2" || !lines[0].Time.Equal(lines[1].Time) {
					t.Fatalf("lines=%+v err=%v", lines, err)
				}
			} else if err == nil || lines != nil || strings.Contains(err.Error(), testKey) {
				t.Fatalf("partial lines=%+v err=%v", lines, err)
			}
		})
	}
}
