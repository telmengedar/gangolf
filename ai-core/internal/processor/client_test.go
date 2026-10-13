package processor_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/telmengedar/gangolf/ai-core/internal/processor"
	"github.com/telmengedar/gangolf/ai-core/internal/window"
)

func serve(t *testing.T, status int, body string) (*processor.Client, *http.Request, *string) {
	t.Helper()
	var seen http.Request
	var seenBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = *r.Clone(context.Background())
		raw, _ := io.ReadAll(r.Body)
		seenBody = string(raw)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	return processor.New(server.URL+"/", 1090, server.Client()), &seen, &seenBody
}

func TestRunsRequestShapeCarriesNoIdentityOrConfig(t *testing.T) {
	client, seen, body := serve(t, 200, `{"reply":"hi"}`)
	history := []window.Exchange{{Input: "vesper: hello", Reply: "hey"}, {Input: "dapper: anyone there"}}

	_, err := client.Run(context.Background(), "rugen: gangolf?", history)

	if err != nil {
		t.Fatal(err)
	}
	if seen.Method != http.MethodPost || seen.URL.Path != "/runs" || seen.URL.RawQuery != "" {
		t.Fatalf("request = %s %s?%s, want a plain POST /runs", seen.Method, seen.URL.Path, seen.URL.RawQuery)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal([]byte(*body), &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded) != 3 {
		t.Fatalf("body keys = %v, want exactly input, subject and history", keys(decoded))
	}
	if string(decoded["input"]) != `"rugen: gangolf?"` || string(decoded["subject"]) != "1090" {
		t.Fatalf("body = %s", *body)
	}
	if string(decoded["history"]) != `[{"input":"vesper: hello","reply":"hey"},{"input":"dapper: anyone there"}]` {
		t.Fatalf("history = %s, want reply omitted when there was none", decoded["history"])
	}
}

func TestRunsRequestSendsAnEmptyHistoryArrayNeverNull(t *testing.T) {
	client, _, body := serve(t, 200, `{"reply":"hi"}`)

	if _, err := client.Run(context.Background(), "vesper: first", nil); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(*body, `"history":[]`) {
		t.Fatalf("body = %s, want an empty history array", *body)
	}
}

func TestRunReturnsTheReply(t *testing.T) {
	client, _, _ := serve(t, 200, `{"reply":"well met","written":{}}`)

	result, err := client.Run(context.Background(), "x", nil)

	if err != nil || result.Reply != "well met" || result.Silent {
		t.Fatalf("result = %+v err = %v", result, err)
	}
}

func TestRunWithProducedFalseIsSilent(t *testing.T) {
	client, _, _ := serve(t, 200, `{"reply":"Sorry, I could not put an answer together.","produced":false}`)

	result, err := client.Run(context.Background(), "x", nil)

	if err != nil || !result.Silent {
		t.Fatalf("result = %+v err = %v, want silence", result, err)
	}
}

func TestRunWithProducedTrueIsNotSilent(t *testing.T) {
	client, _, _ := serve(t, 200, `{"reply":"fine","produced":true}`)

	result, err := client.Run(context.Background(), "x", nil)

	if err != nil || result.Silent {
		t.Fatalf("result = %+v err = %v, want a reply", result, err)
	}
}

func TestRunWithABlankReplyIsSilent(t *testing.T) {
	client, _, _ := serve(t, 200, `{"reply":"  \n "}`)

	result, err := client.Run(context.Background(), "x", nil)

	if err != nil || !result.Silent {
		t.Fatalf("result = %+v err = %v, want silence", result, err)
	}
}

func TestErrorBodyIsDecodedIntoCodeAndMessage(t *testing.T) {
	client, _, _ := serve(t, 502, `{"error":{"code":"model_unavailable","message":"the model call did not complete"}}`)

	_, err := client.Run(context.Background(), "x", nil)

	if err == nil || !strings.Contains(err.Error(), "502") || !strings.Contains(err.Error(), "model_unavailable") || !strings.Contains(err.Error(), "the model call did not complete") {
		t.Fatalf("err = %v, want status, code and message", err)
	}
}

func TestClientErrorStatusIsAnError(t *testing.T) {
	client, _, _ := serve(t, 400, `{"error":{"code":"invalid_request","message":"input must not be empty"}}`)

	_, err := client.Run(context.Background(), "x", nil)

	if err == nil || !strings.Contains(err.Error(), "invalid_request") {
		t.Fatalf("err = %v, want the invalid_request code", err)
	}
}

func TestUnstructuredErrorBodyIsKeptAsAPrefix(t *testing.T) {
	client, _, _ := serve(t, 500, strings.Repeat("x", 500))

	_, err := client.Run(context.Background(), "x", nil)

	if err == nil || !strings.Contains(err.Error(), "500") || len(err.Error()) > 300 {
		t.Fatalf("err = %v, want the status and a bounded body prefix", err)
	}
}

func TestMalformedSuccessBodyIsAnError(t *testing.T) {
	client, _, _ := serve(t, 200, `not json`)

	if _, err := client.Run(context.Background(), "x", nil); err == nil {
		t.Fatal("an undecodable 200 must be an error")
	}
}

func TestTransportFailureIsAnError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	client := processor.New(server.URL, 1090, server.Client())
	server.Close()

	if _, err := client.Run(context.Background(), "x", nil); err == nil {
		t.Fatal("a refused connection must be an error")
	}
}

func keys(m map[string]json.RawMessage) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
