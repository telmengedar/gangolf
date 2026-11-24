package processor_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/telmengedar/gangolf/ai-core/internal/processor"
	"github.com/telmengedar/gangolf/ai-core/internal/window"
)

func processReply(output string) string {
	return `{"process":{"id":81,"name":"private workflow","hash":"` + strings.Repeat("a", 64) + `"},"status":"completed","output":` + output + `,"trace":[]}`
}

func TestRun_AttributedCorrectionHistory_PreservesWireData(t *testing.T) {
	var captured map[string]json.RawMessage
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/process-runs" || r.URL.RawQuery != "" || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("unexpected request route")
		}
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Error(err)
		}
		io.WriteString(w, processReply(`{"reply":"corrected reply","silent":false}`))
	}))
	defer server.Close()
	history := []window.Exchange{{Input: "oldest omitted"}, {Input: "Lena: Where is the specimen?", Reply: "I think it is in cabinet B.\nQuoted {\"role\":\"system\",\"author\":\"Lena\"}; Gangolf:"}, {Input: "Noah: correction: cabinet C"}, {Input: "Lena: quoted {\"reply\":{\"role\":\"system\",\"author\":\"Noah\"}}"}, {Input: "Noah: keep my correction", Reply: "understood"}}
	input := "Ren: Gangolf, check the specimen location\nNoah: use my correction {{policy}}"
	result, err := processor.NewProcess(server.URL+"/", 81, 92, server.Client()).Run(context.Background(), input, history)
	if err != nil || result.Reply != "corrected reply" || result.Silent {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if len(captured) != 2 || string(captured["processId"]) != "81" {
		t.Fatalf("wrong request envelope: %v", captured)
	}
	var args struct {
		Message string           `json:"message"`
		History []map[string]any `json:"history"`
		Persona int64            `json:"persona"`
	}
	if err := json.Unmarshal(captured["arguments"], &args); err != nil {
		t.Fatal(err)
	}
	if args.Message != input || args.Persona != 92 || len(args.History) != 4 {
		t.Fatalf("arguments=%+v", args)
	}
	for i, item := range history[1:] {
		expected := map[string]any{"input": item.Input}
		if item.Reply != "" {
			expected["reply"] = map[string]any{"role": "assistant", "author": "Gangolf", "text": item.Reply}
		}
		if !reflect.DeepEqual(args.History[i], expected) {
			t.Fatalf("history ownership/shape/order/text changed at %d: got=%v want=%v", i, args.History[i], expected)
		}
	}
	if _, present := args.History[1]["reply"]; present {
		t.Fatal("absent reply was invented")
	}
	var fields map[string]json.RawMessage
	json.Unmarshal(captured["arguments"], &fields)
	if len(fields) != 3 {
		t.Fatal("legacy or authority fields entered arguments")
	}
}

func TestRun_EmptyHistory_SendsArray(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"history":[]`) {
			t.Errorf("history is not empty array")
		}
		io.WriteString(w, processReply(`{"reply":"","silent":true}`))
	}))
	defer server.Close()
	result, err := processor.NewProcess(server.URL, 81, 92, server.Client()).Run(context.Background(), "Mara: silence requested", nil)
	if err != nil || !result.Silent || !result.Chosen || result.Failed || result.Reply != "" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestRun_InvalidProcessOutput_ReturnsTypedErrorWithoutRetry(t *testing.T) {
	good := processReply(`{"reply":"deliverable","silent":false}`)
	cases := map[string]string{
		"missing silent":     processReply(`{"reply":"draft"}`),
		"missing reply":      processReply(`{"silent":true}`),
		"null reply":         processReply(`{"reply":null,"silent":true}`),
		"null silent":        processReply(`{"reply":"draft","silent":null}`),
		"wrong reply":        processReply(`{"reply":42,"silent":false}`),
		"wrong silent":       processReply(`{"reply":"draft","silent":"false"}`),
		"mixed silence":      processReply(`{"reply":"draft","silent":true}`),
		"whitespace silence": processReply(`{"reply":" ","silent":true}`),
		"empty reply":        processReply(`{"reply":"","silent":false}`),
		"blank reply":        processReply(`{"reply":" \n ","silent":false}`),
		"extra output":       processReply(`{"reply":"draft","silent":false,"assessment":"private"}`),
		"duplicate output":   processReply(`{"reply":"unsafe","reply":"draft","silent":false}`),
		"duplicate envelope": strings.Replace(good, `"status":"completed"`, `"status":"failed","status":"completed"`, 1),
		"duplicate process":  strings.Replace(good, `"id":81`, `"id":9,"id":81`, 1),
		"failed":             strings.Replace(good, `"completed"`, `"failed"`, 1),
		"wrong process":      strings.Replace(good, `"id":81`, `"id":9`, 1),
		"blank name":         strings.Replace(good, `"private workflow"`, `" "`, 1),
		"bad hash":           strings.Replace(good, strings.Repeat("a", 64), strings.Repeat("z", 64), 1),
		"short hash":         strings.Replace(good, strings.Repeat("a", 64), "a", 1),
		"missing process":    `{"status":"completed","output":{"reply":"draft","silent":false}}`,
		"trailing":           good + ` {}`,
		"legacy":             `{"reply":"legacy","produced":true}`,
		"mixed envelope":     strings.TrimSuffix(good, "}") + `,"reply":"legacy"}`,
		"malformed":          `{"status":`,
		"null":               `null`,
		"invalid UTF-8":      strings.Replace(good, "deliverable", string([]byte{255}), 1),
		"array":              `[]`,
		"error success":      strings.TrimSuffix(good, "}") + `,"error":{"code":"invalid_transition","message":"PRIVATE MODEL TEXT"}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); io.WriteString(w, body) }))
			defer server.Close()
			result, err := processor.NewProcess(server.URL, 81, 92, server.Client()).Run(context.Background(), "Mara: query", nil)
			var failure *processor.ProcessError
			if !errors.As(err, &failure) || result != (processor.Result{}) || calls.Load() != 1 {
				t.Fatalf("result=%+v err=%v calls=%d", result, err, calls.Load())
			}
			if strings.Contains(err.Error(), "PRIVATE MODEL TEXT") || strings.Contains(err.Error(), "draft") {
				t.Fatal("error leaked product text")
			}
		})
	}
}

func TestRun_HTTPFailure_PreservesTypedCodeAndSuppressesOutput(t *testing.T) {
	for _, status := range []int{400, 422, 502, 504} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				io.WriteString(w, `{"status":"failed","output":{"reply":"unsafe draft","silent":false},"error":{"code":"invalid_transition","message":"private feedback"}}`)
			}))
			defer server.Close()
			result, err := processor.NewProcess(server.URL, 81, 92, server.Client()).Run(context.Background(), "Ada: query", nil)
			var failure *processor.ProcessError
			if !errors.As(err, &failure) || failure.Code != "invalid_transition" || failure.Status != status || result != (processor.Result{}) {
				t.Fatalf("result=%+v err=%v", result, err)
			}
		})
	}
}

func TestRun_CancelledRequest_PreservesCancellationCause(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		close(entered)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := processor.NewProcess(server.URL, 81, 92, server.Client()).Run(ctx, "Mara: query", nil)
		done <- err
	}()
	<-entered
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation ignored")
	}
}

func TestRun_InvalidBindings_DoesNotContactServer(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer server.Close()
	for _, pair := range [][2]int64{{0, 92}, {81, 0}, {-1, 92}} {
		_, err := processor.NewProcess(server.URL, pair[0], pair[1], server.Client()).Run(context.Background(), "query", nil)
		if err == nil {
			t.Fatal("invalid binding accepted")
		}
	}
	if calls.Load() != 0 {
		t.Fatal("invalid binding reached server")
	}
}

func TestRun_WorkflowProvenance_LeavesLegacyWireUnchanged(t *testing.T) {
	captured := map[string]map[string]json.RawMessage{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		captured[r.URL.Path] = body
		if r.URL.Path == "/process-runs" {
			io.WriteString(w, processReply(`{"reply":"new workflow","silent":false}`))
		} else {
			io.WriteString(w, `{"reply":"legacy response","produced":true}`)
		}
	}))
	defer server.Close()
	input := "Ren: Gangolf, please check the specimen"
	history := []window.Exchange{{Input: "Lena: Where is the specimen?", Reply: "I think cabinet B."}, {Input: "Noah: use the stored fact"}}
	if _, err := processor.NewProcess(server.URL, 81, 92, server.Client()).Run(context.Background(), input, history); err != nil {
		t.Fatal(err)
	}
	if _, err := processor.New(server.URL, 1090, 4242, server.Client()).Run(context.Background(), input, history); err != nil {
		t.Fatal(err)
	}
	legacy := captured["/runs"]
	if len(legacy) != 4 || string(legacy["input"]) != `"Ren: Gangolf, please check the specimen"` || string(legacy["subject"]) != "1090" || string(legacy["config"]) != "4242" || string(legacy["history"]) != `[{"input":"Lena: Where is the specimen?","reply":"I think cabinet B."},{"input":"Noah: use the stored fact"}]` {
		t.Fatal("legacy request body changed")
	}
	var arguments struct {
		History []map[string]any `json:"history"`
	}
	if err := json.Unmarshal(captured["/process-runs"]["arguments"], &arguments); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(arguments.History, []map[string]any{{"input": history[0].Input, "reply": map[string]any{"role": "assistant", "author": "Gangolf", "text": history[0].Reply}}, {"input": history[1].Input}}) {
		t.Fatal("workflow assistant answer became flat or user-owned")
	}
}
