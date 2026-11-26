package core_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/telmengedar/gangolf/ai-core/internal/processor"
	"github.com/telmengedar/gangolf/ai-core/internal/window"
)

func TestEventRun_ThoughtFailure_PreservesDeliveredChatWithoutRetry(t *testing.T) {
	var chatCalls, thoughtCalls atomic.Int64
	thoughtSeen := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			ProcessID int64                      `json:"processId"`
			Arguments map[string]json.RawMessage `json:"arguments"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		switch request.ProcessID {
		case 81:
			chatCalls.Add(1)
			io.WriteString(w, `{"process":{"id":81,"name":"chat","hash":"`+strings.Repeat("a", 64)+`"},"status":"completed","output":{"reply":"Gangolf: delivered answer","silent":false}}`)
		case 82:
			thoughtCalls.Add(1)
			var latest []struct{ ID, Speaker, Text string }
			json.Unmarshal(request.Arguments["latest"], &latest)
			if len(latest) != 1 || latest[0].ID != "1" || latest[0].Speaker != "Mara" || string(request.Arguments["assistantReply"]) != `"delivered answer"` {
				t.Error("actual postdelivery thought input lost source/ownreply")
			}
			io.WriteString(w, `{"process":{"id":82,"name":"thought","hash":"`+strings.Repeat("b", 64)+`"},"status":"failed","error":{"code":"memory_write_incomplete","message":"PRIVATE THOUGHT MUST NOT POST"},"trace":[{"kind":"action","repair":{"nodeId":501}}]}`)
			thoughtSeen <- struct{}{}
		default:
			t.Errorf("unexpected selector=%d", request.ProcessID)
		}
	}))
	defer server.Close()
	f := newLiveRig(t, processor.NewProcess(server.URL, 81, 92, server.Client()), processor.NewThoughtProcess(server.URL, 82, server.Client()))
	f.push(t, "message.created", liveRow("1", "please stop that joke"))
	select {
	case <-thoughtSeen:
	case <-time.After(4 * time.Second):
		t.Fatal("delivered turn did not invoke thought worker")
	}
	f.waitFence(t, 2)
	f.cancel()
	select {
	case <-f.done:
	case <-time.After(time.Second):
		t.Fatal("thought failure blocked shutdown")
	}
	if f.posts.Load() != 1 || chatCalls.Load() != 1 || thoughtCalls.Load() != 1 {
		t.Fatalf("posts=%d chat=%d thought=%d", f.posts.Load(), chatCalls.Load(), thoughtCalls.Load())
	}
}

func TestReview_CancelledThought_DoesNotRetryOrProduceReceipt(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := processor.NewThoughtProcess(server.URL, 82, server.Client()).Review(ctx, []window.Utterance{{ID: "121", Speaker: "Lena", Text: "reaction"}}, nil, "delivered")
	if !errors.Is(err, context.Canceled) || calls.Load() != 0 || result.NoteID != 0 || result.State != "" {
		t.Fatal("canceled review reached HTTP, retried or produced success")
	}
}
