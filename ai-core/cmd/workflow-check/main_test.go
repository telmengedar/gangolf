package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestExecute_RealCoreProcessTurn_RecordsPrivateSink(t *testing.T) {
	for _, reply := range []bool{true, false} {
		t.Run(map[bool]string{true: "reply", false: "typed failure"}[reply], func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path != "/process-runs" {
					t.Error("unexpected path")
				}
				var req struct {
					Arguments struct {
						Message string              `json:"message"`
						History []map[string]string `json:"history"`
					} `json:"arguments"`
				}
				json.NewDecoder(r.Body).Decode(&req)
				if !strings.Contains(req.Arguments.Message, "Ada: correction") || len(req.Arguments.History) != 1 {
					t.Errorf("actual core input=%+v", req)
				}
				if !reply {
					w.WriteHeader(422)
					io.WriteString(w, `{"status":"failed","error":{"code":"invalid_transition","message":"private feedback"}}`)
					return
				}
				io.WriteString(w, `{"process":{"id":81,"name":"private","hash":"`+strings.Repeat("a", 64)+`"},"status":"completed","output":{"reply":"Gangolf: corrected answer","silent":false}}`)
			}))
			defer server.Close()
			input, _ := json.Marshal(request{BaseURL: server.URL, ProcessID: 81, PersonaID: 92, TimeoutSeconds: 10, History: []line{{Speaker: "Mara", Text: "old fact"}, {Speaker: "GangolfBot", Text: "old reply", Own: true}}, Messages: []line{{Speaker: "Ada", Text: "correction: current fact"}}})
			var out strings.Builder
			if err := execute(strings.NewReader(string(input)), &out); err != nil {
				t.Fatal(err)
			}
			var result struct {
				Completed bool   `json:"completed"`
				Calls     []call `json:"calls"`
				Posts     []post `json:"posts"`
				Logs      string `json:"logs"`
			}
			if err := json.Unmarshal([]byte(out.String()), &result); err != nil {
				t.Fatal(err)
			}
			if !result.Completed || calls != 1 || len(result.Calls) != 1 {
				t.Fatalf("result=%+v calls=%d", result, calls)
			}
			if reply {
				if len(result.Posts) != 1 || result.Posts[0].Text != "corrected answer" || result.Posts[0].Service != "private" {
					t.Fatal("private delivery sink lost")
				}
			} else if len(result.Posts) != 0 || result.Calls[0].ErrorCode != "invalid_transition" || strings.Contains(result.Logs, "private feedback") {
				t.Fatal("failed turn published or leaked")
			}
		})
	}
}

func TestExecute_InvalidOrUnboundedInput_RejectsBeforeNetwork(t *testing.T) {
	for _, raw := range []string{`{}`, `{"baseURL":"http://user:secret@localhost","processId":81,"personaId":92,"timeoutSeconds":1,"messages":[{"speaker":"Ada","text":"x"}]}`, `{"baseURL":"http://localhost","processId":81,"personaId":92,"timeoutSeconds":181,"messages":[{"speaker":"Ada","text":"x"}]}`, `{} {}`} {
		var out strings.Builder
		if err := execute(strings.NewReader(raw), &out); err == nil || out.Len() != 0 {
			t.Fatal("invalid input accepted")
		}
	}
}
