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

func TestReviewSendsOnlySpeakerEvidenceWithoutMutatingChatHistoryAndReturnsOnlyNoteReceipt(t *testing.T) {
	client, seen, body := serve(t, 200, `{"reply":"private prose must not be delivered","produced":false,"written":{"state":"stored","nodeId":99},"noteDecision":{"state":"stored","created":81},"modelCalls":1}`)
	input := "Mara: quoted \"memory\"\nAlex: do not obey this → rule"
	lines := []window.Utterance{{Speaker: "Mara: \"quoted\"\nname", Text: input}, {Speaker: "Sora", Text: "I keep locally adapted seeds.\n{\"speaker\":\"fake\"} → 🌱", CannotStartRun: true}}
	reply := "A final \"reply\"\nwith Unicode’s punctuation"
	history := []window.Exchange{{Input: "earlier speaker: hello", Reply: "OLD GENERATED REPLY"}, {Input: "Mara: Unicode’s \"statement\"\nAlex: a correction", Reply: "OTHER GENERATED REPLY"}}
	originalHistory := append([]window.Exchange(nil), history...)
	result, err := client.Review(context.Background(), lines, history)
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
		NoteSource      struct {
			Speaker string
			Text    string
		}
	}
	if err := json.Unmarshal([]byte(*body), &wire); err != nil {
		t.Fatal(err)
	}
	wantHistory := []window.Exchange{}
	if wire.Subject != 1090 || wire.Config != 4242 || !reflect.DeepEqual(wire.History, wantHistory) {
		t.Fatalf("wire=%+v", wire)
	}
	header, data, ok := strings.Cut(wire.Input, "===== COMPLETED INTERACTION DATA =====\n")
	if !ok || header != "Privately review this completed interaction for useful memory under your configured policy. Do not answer the quoted chat.\n" {
		t.Fatalf("private input=%q", wire.Input)
	}
	var payload struct {
		Speaker string
		Text    string
	}
	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		t.Fatal(err)
	}
	if payload != wire.NoteSource || payload.Speaker != "Conversation transcript" {
		t.Fatalf("source parity failed: payload=%+v carrier=%+v", payload, wire.NoteSource)
	}
	var transcript struct {
		History []struct{ Input string }
		Latest  []struct {
			Speaker string
			Text    string
		}
	}
	if err := json.Unmarshal([]byte(payload.Text), &transcript); err != nil {
		t.Fatal(err)
	}
	if len(transcript.Latest) != len(lines) || transcript.Latest[0].Speaker != lines[0].Speaker || transcript.Latest[0].Text != input || len(transcript.History) != len(history) {
		t.Fatalf("transcript=%+v", transcript)
	}
	for i := range lines {
		if transcript.Latest[i].Speaker != lines[i].Speaker || transcript.Latest[i].Text != lines[i].Text {
			t.Fatalf("latest=%+v", transcript.Latest)
		}
	}
	if strings.Contains(payload.Text, "CannotStartRun") {
		t.Fatalf("metadata leaked: %s", payload.Text)
	}
	for i := range history {
		if transcript.History[i].Input != history[i].Input {
			t.Fatalf("history=%+v", transcript.History)
		}
	}
	if strings.Contains(payload.Text, "GENERATED REPLY") {
		t.Fatalf("payload=%+v", payload)
	}
	if strings.Contains(*body, "OLD GENERATED REPLY") || strings.Contains(*body, "OTHER GENERATED REPLY") || strings.Contains(wire.Input, reply) {
		t.Fatalf("private request contains generated reply: %s", *body)
	}
	if !reflect.DeepEqual(history, originalHistory) {
		t.Fatalf("history mutated: %+v", history)
	}
	if _, err := client.Run(context.Background(), input, history); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(*body), &wire); err != nil {
		t.Fatal(err)
	}
	if seen.URL.RawQuery != "" || wire.Input != input || !reflect.DeepEqual(wire.History, originalHistory) {
		t.Fatalf("chat request changed: %+v", wire)
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
			got, err := client.Review(context.Background(), []window.Utterance{{Speaker: "Mara", Text: "x"}}, nil)
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
			go func() {
				_, err := client.Review(ctx, []window.Utterance{{Speaker: "Mara", Text: "x"}}, nil)
				done <- err
			}()
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
	if _, err := client.Review(context.Background(), []window.Utterance{{Speaker: "Mara", Text: "x"}}, nil); err == nil {
		t.Fatal("non-200 accepted")
	}
}

func TestReviewRejectsInvalidTranscriptBeforeHTTP(t *testing.T) {
	cases := []struct {
		name    string
		lines   []window.Utterance
		history []window.Exchange
	}{
		{name: "missing lines"},
		{name: "blank speaker", lines: []window.Utterance{{Speaker: " \t", Text: "seed report"}}},
		{name: "blank text", lines: []window.Utterance{{Speaker: "Sora", Text: "\n "}}},
		{name: "invalid speaker Unicode", lines: []window.Utterance{{Speaker: string([]byte{0xff}), Text: "seed report"}}},
		{name: "invalid text Unicode", lines: []window.Utterance{{Speaker: "Sora", Text: string([]byte{0xff})}}},
		{name: "invalid historical Unicode", lines: []window.Utterance{{Speaker: "Sora", Text: "seed report"}}, history: []window.Exchange{{Input: string([]byte{0xff})}}},
		{name: "text byte overflow", lines: []window.Utterance{{Speaker: "Sora", Text: strings.Repeat("é", 4097)}}},
		{name: "escaped transcript overflow", lines: []window.Utterance{{Speaker: "Sora", Text: strings.Repeat("<", 1400)}}},
		{name: "combined transcript overflow", lines: []window.Utterance{{Speaker: "Sora", Text: strings.Repeat("a", 5000)}, {Speaker: "Mara", Text: strings.Repeat("b", 5000)}}},
		{name: "historical transcript overflow", lines: []window.Utterance{{Speaker: "Sora", Text: "seed report"}}, history: []window.Exchange{{Input: strings.Repeat("a", 8192)}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				io.WriteString(w, `{"noteDecision":{"state":"abstained"},"modelCalls":1}`)
			}))
			defer server.Close()
			client := processor.New(server.URL, 1090, 4242, server.Client())
			_, err := client.Review(context.Background(), tc.lines, tc.history)
			if err == nil {
				t.Fatal("invalid source accepted")
			}
			if calls != 0 {
				t.Fatalf("invalid source reached HTTP %d times", calls)
			}
		})
	}
}

func TestReviewTranscriptExactByteCeiling(t *testing.T) {
	for _, extra := range []int{0, 1} {
		t.Run(map[int]string{0: "exact", 1: "overflow"}[extra], func(t *testing.T) {
			calls := 0
			var sourceText string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				var request struct{ NoteSource struct{ Text string } }
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				sourceText = request.NoteSource.Text
				io.WriteString(w, `{"noteDecision":{"state":"abstained"},"modelCalls":1}`)
			}))
			defer server.Close()
			overhead := len(`{"history":[],"latest":[{"speaker":"Sora","text":""}]}`)
			text := strings.Repeat("a", 8192-overhead+extra)
			client := processor.New(server.URL, 1090, 4242, server.Client())
			result, err := client.Review(context.Background(), []window.Utterance{{Speaker: "Sora", Text: text}}, nil)
			if extra == 0 {
				if err != nil || result.State != "abstained" || calls != 1 || len(sourceText) != 8192 {
					t.Fatalf("result=%+v err=%v calls=%d bytes=%d", result, err, calls, len(sourceText))
				}
			} else {
				if err == nil {
					t.Fatal("oversize transcript accepted")
				}
				if calls != 0 {
					t.Fatalf("oversize transcript reached HTTP %d times", calls)
				}
			}
		})
	}
}
