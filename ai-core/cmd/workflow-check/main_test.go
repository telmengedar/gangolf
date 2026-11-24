package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/telmengedar/gangolf/ai-core/internal/processor"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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
						Message string                       `json:"message"`
						History []map[string]json.RawMessage `json:"history"`
					} `json:"arguments"`
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
				}
				if !strings.Contains(req.Arguments.Message, "Ada: correction") || len(req.Arguments.History) != 1 {
					t.Errorf("actual core input=%+v", req)
				}
				var assistant map[string]string
				if len(req.Arguments.History) != 1 || json.Unmarshal(req.Arguments.History[0]["reply"], &assistant) != nil || assistant["role"] != "assistant" || assistant["author"] != "Gangolf" || assistant["text"] != "old reply" {
					t.Error("actual private adapter lost assistant provenance")
				}
				if !reply {
					w.WriteHeader(422)
					io.WriteString(w, `{"status":"failed","error":{"code":"invalid_transition","message":"private feedback"}}`)
					return
				}
				io.WriteString(w, `{"process":{"id":81,"name":"private","hash":"`+strings.Repeat("a", 64)+`"},"status":"completed","output":{"reply":"Gangolf: corrected answer","silent":false},"trace":[{"key":"check_draft","outcome":"ACCEPT","private":"CAPTURE_ONLY"}]}`)
			}))
			defer server.Close()
			input, _ := json.Marshal(request{BaseURL: server.URL, ProcessID: 81, PersonaID: 92, TimeoutSeconds: 10, History: []line{{Speaker: "Mara", Text: "old fact"}, {Speaker: "GangolfBot", Text: "old reply", Own: true}}, Messages: []line{{Speaker: "Ada", Text: "correction: current fact"}}})
			var out strings.Builder
			if err := execute(strings.NewReader(string(input)), &out); err != nil {
				t.Fatal(err)
			}
			var result struct {
				Completed        bool              `json:"completed"`
				Calls            []call            `json:"calls"`
				Posts            []post            `json:"posts"`
				Logs             string            `json:"logs"`
				ProcessResponses []processExchange `json:"processResponses"`
			}
			if err := json.Unmarshal([]byte(out.String()), &result); err != nil {
				t.Fatal(err)
			}
			if !result.Completed || calls != 1 || len(result.Calls) != 1 {
				t.Fatalf("result=%+v calls=%d", result, calls)
			}
			if len(result.ProcessResponses) != 1 || !json.Valid(result.ProcessResponses[0].Request) || len(result.ProcessResponses[0].Response) == 0 || result.ProcessResponses[0].RequestSHA256 == "" || result.ProcessResponses[0].ResponseSHA256 == "" {
				t.Fatal("same actual HTTP exchange was not captured")
			}
			if reply && (!bytes.Contains(result.ProcessResponses[0].Response, []byte("CAPTURE_ONLY")) || strings.Contains(result.Logs, "CAPTURE_ONLY") || strings.Contains(fmt.Sprint(result.Posts), "CAPTURE_ONLY")) {
				t.Fatal("actual trace missing or leaked to log/delivery")
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

type responseTransport func(*http.Request) (*http.Response, error)

func (f responseTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRoundTrip_CapturedBody_ReplaysExactBytesWithoutAnotherRequest(t *testing.T) {
	raw := " {\n\t\"process\":{\"id\":81},\"trace\":[{\"key\":\"actual_step\"}] } \n"
	calls := 0
	capture := &captureTransport{base: responseTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		body, _ := io.ReadAll(r.Body)
		if string(body) != `{"actual":"request"}` {
			t.Fatal("actual adapter request altered")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(raw)), Header: http.Header{}}, nil
	})}
	req, _ := http.NewRequest("POST", "http://private.invalid/process-runs", strings.NewReader(`{"actual":"request"}`))
	response, err := capture.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(raw))
	if string(body) != raw || calls != 1 || len(capture.exchanges) != 1 || string(capture.exchanges[0].Response) != raw || capture.exchanges[0].ResponseSHA256 != hex.EncodeToString(sum[:]) {
		t.Fatal("capture changed bytes, lost trace, or replayed network request")
	}
}

func TestExecute_CapturedMalformedOrOversizedResponse_FailsWithoutPost(t *testing.T) {
	for _, tc := range []struct{ name, body, captureError string }{
		{"malformed", `{"status":`, ""},
		{"duplicate", `{"process":{"id":81,"name":"private","hash":"` + strings.Repeat("a", 64) + `"},"status":"completed","output":{"reply":"unsafe","reply":"safe","silent":false}}`, ""},
		{"limit", strings.Repeat("x", captureLimit+1), "response_limit"},
		{"truncated", `{"process":{"id":81,"name":"private","hash":"` + strings.Repeat("a", 64) + `"},"status":"completed","output":{"reply":"unsafe truncated","silent":false}}`, "response_read"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				io.Copy(io.Discard, r.Body)
				if tc.name == "truncated" {
					w.Header().Set("Content-Length", fmt.Sprint(len(tc.body)+10))
				}
				io.WriteString(w, tc.body)
			}))
			defer server.Close()
			input, _ := json.Marshal(request{BaseURL: server.URL, ProcessID: 81, PersonaID: 92, TimeoutSeconds: 10, Messages: []line{{Speaker: "Ada", Text: "answer this"}}})
			var out strings.Builder
			if err := execute(bytes.NewReader(input), &out); err != nil {
				t.Fatal(err)
			}
			var result struct {
				Completed        bool              `json:"completed"`
				Calls            []call            `json:"calls"`
				Posts            []post            `json:"posts"`
				ProcessResponses []processExchange `json:"processResponses"`
			}
			if err := json.Unmarshal([]byte(out.String()), &result); err != nil {
				t.Fatal(err)
			}
			if calls != 1 || !result.Completed || len(result.Calls) != 1 || result.Calls[0].ErrorCode == "" || len(result.Posts) != 0 || len(result.ProcessResponses) != 1 {
				t.Fatal("malformed response was accepted or replayed")
			}
			record := result.ProcessResponses[0]
			if record.CaptureError != tc.captureError {
				t.Fatal("capture bound/error evidence missing")
			}
			if tc.name == "limit" || tc.name == "truncated" {
				if len(record.Response) != 0 || len(record.MalformedResponse) != 0 {
					t.Fatal("over-limit body retained")
				}
			} else if tc.name == "malformed" {
				if string(record.MalformedResponse) != tc.body {
					t.Fatal("malformed response bytes lost")
				}
			} else if string(record.Response) != tc.body {
				t.Fatal("duplicate body normalized before strict decoder")
			}
		})
	}
}

func TestExecute_CancelledResponse_CapturesFailureWithoutPost(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}))
	defer server.Close()
	input, _ := json.Marshal(request{BaseURL: server.URL, ProcessID: 81, PersonaID: 92, TimeoutSeconds: 3, Messages: []line{{Speaker: "Ada", Text: "answer this"}}})
	var out strings.Builder
	if err := execute(bytes.NewReader(input), &out); err != nil {
		t.Fatal(err)
	}
	var result struct {
		Completed        bool              `json:"completed"`
		Calls            []call            `json:"calls"`
		Posts            []post            `json:"posts"`
		ProcessResponses []processExchange `json:"processResponses"`
	}
	json.Unmarshal([]byte(out.String()), &result)
	if calls.Load() != 1 || result.Completed || len(result.Calls) != 1 || result.Calls[0].ErrorCode != "transport" || len(result.Posts) != 0 || len(result.ProcessResponses) != 1 || result.ProcessResponses[0].CaptureError != "transport" {
		t.Fatal("cancellation lost, retried or posted")
	}
}

func TestRoundTrip_CancelledTransport_PreservesAdapterCause(t *testing.T) {
	calls := 0
	capture := &captureTransport{base: responseTransport(func(r *http.Request) (*http.Response, error) { calls++; return nil, context.Canceled })}
	result, err := processor.NewProcess("http://private.invalid", 81, 92, &http.Client{Transport: capture}).Run(context.Background(), "Ada: query", nil)
	if calls != 1 || !errors.Is(err, context.Canceled) || result != (processor.Result{}) || len(capture.exchanges) != 1 || capture.exchanges[0].CaptureError != "transport" {
		t.Fatal("transport cancellation changed")
	}
}

func TestRoundTrip_NonUTF8Response_PreservesRawBytesPrivately(t *testing.T) {
	raw := []byte{'"', 255, '"'}
	capture := &captureTransport{base: responseTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(raw)), Header: http.Header{}}, nil
	})}
	req, _ := http.NewRequest("POST", "http://private.invalid/process-runs", strings.NewReader(`{}`))
	response, err := capture.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	replay, _ := io.ReadAll(response.Body)
	if !bytes.Equal(replay, raw) || len(capture.exchanges) != 1 || !bytes.Equal(capture.exchanges[0].MalformedResponse, raw) || len(capture.exchanges[0].Response) != 0 {
		t.Fatal("non-UTF8 response normalized or exposed as JSON")
	}
	encoded, err := json.Marshal(capture.exchanges)
	if err != nil || !json.Valid(encoded) {
		t.Fatal("private raw-byte capture is not serializable")
	}
}
