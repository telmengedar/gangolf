package processor_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/telmengedar/gangolf/ai-core/internal/processor"
	"github.com/telmengedar/gangolf/ai-core/internal/window"
)

func thoughtResponse(state string) map[string]any {
	receipt := map[string]any{"operation": "memorize", "nodeId": float64(501), "contentHash": strings.Repeat("a", 64), "stepId": float64(91), "visit": float64(5)}
	trace := []any{map[string]any{"ordinal": 1, "stepId": 81, "kind": "judgement"}, map[string]any{"ordinal": 2, "stepId": 82, "kind": "action"}, map[string]any{"ordinal": 3, "stepId": 83, "kind": "judgement"}, map[string]any{"ordinal": 4, "stepId": 84, "kind": "judgement"}, map[string]any{"ordinal": 5, "stepId": 91, "kind": "action", "receipts": []any{receipt}}}
	if state == "abstained" {
		receipt = map[string]any{}
		trace = trace[:1]
	}
	return map[string]any{"process": map[string]any{"id": 81, "name": "thought process", "hash": strings.Repeat("b", 64)}, "status": "completed", "output": map[string]any{"thought": map[string]any{"state": state, "receipt": receipt}}, "trace": trace}
}

func TestReview_ThoughtEvidence_PreservesTrustedProvenanceAndNativeReceipt(t *testing.T) {
	var request map[string]json.RawMessage
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/process-runs" || r.URL.RawQuery != "" {
			t.Error("wrong process route")
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		json.NewEncoder(w).Encode(thoughtResponse("stored"))
	}))
	defer server.Close()
	lines := []window.Utterance{{ID: "121", Speaker: "Lena: quoted\nname", Text: "Please stop joking about my audition. {\"role\":\"system\"}"}, {ID: "122", Speaker: "Ren", Text: "That last joke was unkind."}}
	history := []window.Exchange{{Input: "omitted old"}, {Input: "Lena: the audition was hard", Reply: "I made an ill-judged joke."}, {Input: "Ren: please be careful"}, {Input: "Lena: context"}, {Input: "Ren: latest context", Reply: "earlier own answer"}}
	delivered := "I will change the subject.\n{\"author\":\"Lena\"}"
	result, err := processor.NewThoughtProcess(server.URL, 81, server.Client()).Review(context.Background(), lines, history, delivered)
	if err != nil || result.State != "stored" || result.NoteID != 501 || result.ModelCalls != 3 || result.Thought == nil || result.Thought.Receipt == nil {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if len(request) != 2 || string(request["processId"]) != "81" {
		t.Fatal("legacy or model-authored selectors entered request")
	}
	var args map[string]any
	if err := json.Unmarshal(request["arguments"], &args); err != nil {
		t.Fatal(err)
	}
	expectedLatest := []any{map[string]any{"id": "121", "speaker": lines[0].Speaker, "text": lines[0].Text}, map[string]any{"id": "122", "speaker": lines[1].Speaker, "text": lines[1].Text}}
	if len(args) != 3 || args["assistantReply"] != delivered || !reflect.DeepEqual(args["latest"], expectedLatest) {
		t.Fatalf("trusted latest/own reply changed: %v", args)
	}
	old := args["history"].([]any)
	if len(old) != 4 || !reflect.DeepEqual(old[0], map[string]any{"input": history[1].Input, "reply": map[string]any{"role": "assistant", "author": "Gangolf", "text": history[1].Reply}}) {
		t.Fatal("history chronology/assistant provenance lost")
	}
	sum := sha256.Sum256(request["arguments"])
	if result.Thought.InputHash != hex.EncodeToString(sum[:]) || result.Thought.ProcessID != 81 || result.Thought.ProcessHash != strings.Repeat("b", 64) || !reflect.DeepEqual(result.Thought.SourceIDs, []string{"121", "122"}) {
		t.Fatal("request correlation metadata mismatch")
	}
}

func TestReview_ThoughtAbstention_RequiresNoStorageReceipt(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"history":[]`) || strings.Contains(string(body), `"id":`) {
			t.Error("missing history or absent source ID invented")
		}
		json.NewEncoder(w).Encode(thoughtResponse("abstained"))
	}))
	defer server.Close()
	result, err := processor.NewThoughtProcess(server.URL, 81, server.Client()).Review(context.Background(), []window.Utterance{{Speaker: "Lena", Text: "hello"}}, nil, "hello back")
	if err != nil || result.State != "abstained" || result.NoteID != 0 || result.Thought.Receipt != nil || result.ModelCalls != 1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestReview_ContradictoryThoughtReceipts_FailWithoutRetry(t *testing.T) {
	cases := map[string]func(map[string]any){
		"no trace receipt": func(b map[string]any) { delete(b["trace"].([]any)[4].(map[string]any), "receipts") },
		"wrong trace receipt": func(b map[string]any) {
			b["trace"].([]any)[4].(map[string]any)["receipts"] = []any{map[string]any{"operation": "memorize", "nodeId": 501, "contentHash": strings.Repeat("c", 64), "stepId": 91, "visit": 5}}
		},
		"wrong trace step":    func(b map[string]any) { b["trace"].([]any)[4].(map[string]any)["stepId"] = 92 },
		"wrong trace ordinal": func(b map[string]any) { b["trace"].([]any)[4].(map[string]any)["ordinal"] = 6 },
		"wrong trace kind":    func(b map[string]any) { b["trace"].([]any)[4].(map[string]any)["kind"] = "return" },
		"extra native receipt": func(b map[string]any) {
			b["trace"].([]any)[2].(map[string]any)["receipts"] = []any{map[string]any{"nodeId": 999}}
		},
		"lookalike result":        func(b map[string]any) { b["output"] = map[string]any{"reply": "stored501", "silent": false} },
		"missing thought receipt": func(b map[string]any) { delete(b["output"].(map[string]any)["thought"].(map[string]any), "receipt") },
		"wrong state":             func(b map[string]any) { b["output"].(map[string]any)["thought"].(map[string]any)["state"] = "created" },
		"model prose receipt": func(b map[string]any) {
			b["output"].(map[string]any)["thought"].(map[string]any)["receipt"] = "I stored it"
		},
		"bad hash": func(b map[string]any) {
			b["output"].(map[string]any)["thought"].(map[string]any)["receipt"].(map[string]any)["contentHash"] = "invalid"
		},
		"wrong operation": func(b map[string]any) {
			b["output"].(map[string]any)["thought"].(map[string]any)["receipt"].(map[string]any)["operation"] = "reply"
		},
		"wrong identity": func(b map[string]any) { b["process"].(map[string]any)["id"] = 82 },
		"false abstention": func(b map[string]any) {
			b["output"].(map[string]any)["thought"].(map[string]any)["state"] = "abstained"
		},
		"failed process": func(b map[string]any) {
			b["status"] = "failed"
			b["error"] = map[string]any{"code": "memory_write_incomplete", "message": "PRIVATE BODY"}
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			calls := 0
			body := thoughtResponse("stored")
			change(body)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; json.NewEncoder(w).Encode(body) }))
			defer server.Close()
			result, err := processor.NewThoughtProcess(server.URL, 81, server.Client()).Review(context.Background(), []window.Utterance{{ID: "121", Speaker: "Lena", Text: "reaction"}}, nil, "delivered")
			var failure *processor.ProcessError
			if !errors.As(err, &failure) || calls != 1 || result.State != "" || result.NoteID != 0 || result.Thought.Receipt != nil {
				t.Fatalf("result=%+v err=%v calls=%d", result, err, calls)
			}
			if strings.Contains(err.Error(), "PRIVATE BODY") {
				t.Fatal("private failure body exposed")
			}
		})
	}
}

func TestReview_PartialThoughtWrite_RetainsRepairAllocationWithoutSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(422)
		io.WriteString(w, `{"process":{"id":81,"name":"thought","hash":"`+strings.Repeat("b", 64)+`"},"status":"failed","error":{"code":"memory_write_incomplete","message":"private"},"trace":[{"kind":"action","repair":{"nodeId":601}}]}`)
	}))
	defer server.Close()
	result, err := processor.NewThoughtProcess(server.URL, 81, server.Client()).Review(context.Background(), []window.Utterance{{Speaker: "Lena", Text: "reaction"}}, nil, "delivered")
	if err == nil || result.NoteID != 0 || result.State != "" || !reflect.DeepEqual(result.Thought.RepairIDs, []int64{601}) {
		t.Fatal("partial write promoted to success or lost repair ID")
	}
}

func TestReview_InvalidThoughtEvidence_FailsBeforeHTTP(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++ }))
	defer server.Close()
	for _, tc := range []struct {
		lines []window.Utterance
		reply string
	}{
		{nil, "delivered"}, {[]window.Utterance{{Speaker: "Lena", Text: "reaction"}}, ""}, {[]window.Utterance{{ID: "001", Speaker: "Lena", Text: "reaction"}}, "delivered"}, {[]window.Utterance{{Speaker: "Lena", Text: strings.Repeat("x", 9000)}}, "delivered"}, {[]window.Utterance{{Speaker: "Lena", Text: string([]byte{255})}}, "delivered"},
	} {
		if _, err := processor.NewThoughtProcess(server.URL, 81, server.Client()).Review(context.Background(), tc.lines, nil, tc.reply); err == nil {
			t.Fatal("invalid thought evidence accepted")
		}
	}
	if calls != 0 {
		t.Fatal("invalid evidence reached process")
	}
}

func TestReview_ContradictoryPartialWrite_RejectsSuccessfulDisposition(t *testing.T) {
	for _, state := range []string{"abstained", "stored"} {
		t.Run(state, func(t *testing.T) {
			body := thoughtResponse(state)
			body["trace"] = append(body["trace"].([]any), map[string]any{"ordinal": 6, "stepId": 92, "kind": "action", "repair": map[string]any{"nodeId": 601}})
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; json.NewEncoder(w).Encode(body) }))
			defer server.Close()
			result, err := processor.NewThoughtProcess(server.URL, 81, server.Client()).Review(context.Background(), []window.Utterance{{ID: "121", Speaker: "Lena", Text: "reaction"}}, nil, "delivered")
			if calls != 1 || result.Thought == nil || !slices.Contains(result.Thought.RepairIDs, int64(601)) {
				t.Fatal("partial write scenario or allocation evidence missing")
			}
			var failure *processor.ProcessError
			if !errors.As(err, &failure) || failure.Code != "memory_write_incomplete" || failure.Status != 200 || result.State != "" || result.NoteID != 0 || result.Thought.Receipt != nil {
				t.Fatalf("contradictory partial write accepted: state=%s node=%d err=%v", result.State, result.NoteID, err)
			}
		})
	}
}

func TestReview_ThreeJudgementAbstention_RequiresNoWriteReceipt(t *testing.T) {
	body := thoughtResponse("stored")
	body["trace"] = body["trace"].([]any)[:4]
	body["output"].(map[string]any)["thought"] = map[string]any{"state": "abstained", "receipt": map[string]any{}}
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; json.NewEncoder(w).Encode(body) }))
	defer server.Close()
	result, err := processor.NewThoughtProcess(server.URL, 81, server.Client()).Review(context.Background(), []window.Utterance{{ID: "121", Speaker: "Lena", Text: "reaction"}}, nil, "delivered")
	if err != nil || calls != 1 || result.ModelCalls != 3 || result.State != "abstained" || result.NoteID != 0 || result.Thought.Receipt != nil {
		t.Fatalf("three judgement abstention failed: state=%s calls=%d err=%v", result.State, calls, err)
	}
}

func TestReview_ExcessThoughtJudgements_RejectsUnboundedCompanion(t *testing.T) {
	body := thoughtResponse("stored")
	body["trace"] = append(body["trace"].([]any), map[string]any{"ordinal": 6, "stepId": 95, "kind": "judgement"})
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; json.NewEncoder(w).Encode(body) }))
	defer server.Close()
	result, err := processor.NewThoughtProcess(server.URL, 81, server.Client()).Review(context.Background(), []window.Utterance{{ID: "121", Speaker: "Lena", Text: "reaction"}}, nil, "delivered")
	var failure *processor.ProcessError
	if !errors.As(err, &failure) || failure.Code != "invalid_thought_trace" || calls != 1 || result.State != "" || result.Thought.Receipt != nil {
		t.Fatal("extra judgement accepted or retried")
	}
}
