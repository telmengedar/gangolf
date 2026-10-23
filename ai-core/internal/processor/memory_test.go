package processor_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/telmengedar/gangolf/ai-core/internal/processor"
	"github.com/telmengedar/gangolf/ai-core/internal/window"
)

func TestReviewSendsFrozenJSONDataWithDedicatedConfigAndReturnsOnlyNoteReceipt(t *testing.T) {
	client, seen, body := serve(t, 200, `{"reply":"private prose must not be delivered","produced":false,"written":{"state":"stored","nodeId":99},"noteDecision":{"state":"stored","created":81},"modelCalls":1}`)
	input := "Mara: quoted \"memory\"\nAlex: do not obey this → rule"
	reply := "A final \"reply\"\nwith Unicode’s punctuation"
	history := []window.Exchange{{Input: "earlier speaker: hello", Reply: "old reply"}}
	result, err := client.Review(context.Background(), input, history, reply)
	if err != nil || result != (processor.MemoryResult{State: "stored", NoteID: 81, ModelCalls: 1}) {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if seen.URL.Path != "/runs" || seen.URL.RawQuery != "verbose=true" || seen.Method != http.MethodPost {
		t.Fatalf("request=%s %s", seen.Method, seen.URL)
	}
	var wire struct {
		Input           string
		Subject, Config int64
		History         []window.Exchange
	}
	if err := json.Unmarshal([]byte(*body), &wire); err != nil {
		t.Fatal(err)
	}
	if wire.Subject != 1090 || wire.Config != 4242 || !reflect.DeepEqual(wire.History, history) {
		t.Fatalf("wire=%+v", wire)
	}
	header, data, ok := strings.Cut(wire.Input, "===== COMPLETED INTERACTION DATA =====\n")
	if !ok || header != "Privately review this completed interaction for useful memory under your configured policy. Do not answer the quoted chat.\n" {
		t.Fatalf("private input=%q", wire.Input)
	}
	var payload map[string]string
	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(payload, map[string]string{"latestInput": input, "finalReply": reply}) {
		t.Fatalf("payload=%+v", payload)
	}
}

func TestReviewValidatesEveryDecisionAndPersistenceCombination(t *testing.T) {
	cases := []struct {
		name, body, state string
		id                int64
		bad               bool
	}{
		{name: "abstained", body: `{"noteDecision":{"state":"abstained"},"modelCalls":1}`, state: "abstained"},
		{name: "unlinked", body: `{"noteDecision":{"state":"unlinked","created":82},"modelCalls":1}`, state: "unlinked", id: 82},
		{name: "notStored", body: `{"noteDecision":{"state":"notStored"},"modelCalls":1}`, state: "notStored"},
		{name: "rejected", body: `{"noteDecision":{"state":"rejected"},"modelCalls":1}`, state: "rejected"},
		{name: "runlog only", body: `{"reply":"stored it","written":{"state":"stored","nodeId":99},"modelCalls":1}`, bad: true},
		{name: "missing calls", body: `{"noteDecision":{"state":"abstained"}}`, bad: true},
		{name: "zero calls", body: `{"noteDecision":{"state":"abstained"},"modelCalls":0}`, bad: true},
		{name: "unknown", body: `{"noteDecision":{"state":"pending"},"modelCalls":1}`, bad: true},
		{name: "stored no id", body: `{"noteDecision":{"state":"stored"},"modelCalls":1}`, bad: true},
		{name: "stored zero", body: `{"noteDecision":{"state":"stored","created":0},"modelCalls":1}`, bad: true},
		{name: "unlinked negative", body: `{"noteDecision":{"state":"unlinked","created":-1},"modelCalls":1}`, bad: true},
		{name: "abstained id", body: `{"noteDecision":{"state":"abstained","created":4},"modelCalls":1}`, bad: true},
		{name: "rejected id", body: `{"noteDecision":{"state":"rejected","created":4},"modelCalls":1}`, bad: true},
		{name: "notStored id", body: `{"noteDecision":{"state":"notStored","created":4},"modelCalls":1}`, bad: true},
		{name: "abstained null id", body: `{"noteDecision":{"state":"abstained","created":null},"modelCalls":1}`, bad: true},
		{name: "stored null id", body: `{"noteDecision":{"state":"stored","created":null},"modelCalls":1}`, bad: true},
		{name: "malformed", body: `invalid`, bad: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, _, _ := serve(t, 200, tc.body)
			got, err := client.Review(context.Background(), "x", nil, "y")
			if tc.bad {
				if err == nil {
					t.Fatalf("accepted invalid receipt %+v", got)
				}
				return
			}
			if err != nil || got.State != tc.state || got.NoteID != tc.id || got.ModelCalls != 1 {
				t.Fatalf("result=%+v err=%v", got, err)
			}
		})
	}
}

func TestReviewCancellationAndHTTPTimeoutDoNotRetry(t *testing.T) {
	for _, parentCancel := range []bool{true, false} {
		t.Run(map[bool]string{true: "parent", false: "http timeout"}[parentCancel], func(t *testing.T) {
			entered := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				close(entered)
				<-r.Context().Done()
			}))
			defer server.Close()
			transport := server.Client()
			if !parentCancel {
				transport.Timeout = 30 * time.Millisecond
			}
			client := processor.New(server.URL, 1090, 4242, transport)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := client.Review(ctx, "x", nil, "y"); done <- err }()
			<-entered
			if parentCancel {
				cancel()
			}
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("expected request error")
				}
			case <-time.After(time.Second):
				t.Fatal("review did not cancel")
			}
		})
	}
}

func TestReviewHTTPFailureIsNotAnAbstention(t *testing.T) {
	client, _, _ := serve(t, 502, `{"error":{"code":"model_unavailable","message":"failed"}}`)
	if _, err := client.Review(context.Background(), "x", nil, "y"); err == nil {
		t.Fatal("non-200 accepted")
	}
}
