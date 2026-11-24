package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/telmengedar/gangolf/ai-core/internal/processor"
)

func TestLoadConfig_WorkflowPair_SelectsProcessWithoutMemoryWriter(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/process-runs" {
			t.Errorf("path=%s", r.URL.Path)
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["processId"] != float64(81) {
			t.Error("process binding lost")
		}
		args, _ := body["arguments"].(map[string]any)
		if args["persona"] != float64(92) {
			t.Error("policy binding lost")
		}
		io.WriteString(w, `{"process":{"id":81,"name":"workflow","hash":"`+strings.Repeat("a", 64)+`"},"status":"completed","output":{"reply":"selected reply","silent":false}}`)
	}))
	defer server.Close()
	cfg, err := loadConfig(env(map[string]string{"GANGOLF_API_KEY": "test.key", "GANGOLF_TWITCH_BOT_USER_ID": "99", "GANGOLF_CHANNELS": "Twitch/123", "PROCESSOR_URL": server.URL, "PROCESSOR_PROCESS_ID": "81", "PROCESSOR_PERSONA_ID": "92"}))
	if err != nil {
		t.Fatal(err)
	}
	runner, reviewer := processors(cfg)
	if _, ok := runner.(*processor.ProcessClient); !ok || reviewer != nil {
		t.Fatal("workflow not selected or memory reviewer instantiated")
	}
	result, err := runner.Run(context.Background(), "Ada: actual trigger", nil)
	if err != nil || result.Reply != "selected reply" || calls != 1 {
		t.Fatalf("result=%+v err=%v calls=%d", result, err, calls)
	}
}

func TestLoadConfig_InvalidWorkflowPair_RejectsStartup(t *testing.T) {
	for _, pair := range [][2]string{{"81", ""}, {"", "92"}, {"0", "92"}, {"81", "-1"}, {"bad", "92"}, {"81", "1.2"}, {"01", "92"}, {"+81", "92"}} {
		values := map[string]string{"GANGOLF_API_KEY": "test.key", "GANGOLF_TWITCH_BOT_USER_ID": "99", "GANGOLF_CHANNELS": "Twitch/123", "PROCESSOR_MEMORY_CONFIG": "7", "PROCESSOR_PROCESS_ID": pair[0], "PROCESSOR_PERSONA_ID": pair[1]}
		if _, err := loadConfig(env(values)); err == nil {
			t.Fatalf("pair=%v accepted", pair)
		}
	}
}

func TestProcessors_AbsentWorkflowPair_PreservesLegacyReviewer(t *testing.T) {
	cfg, err := loadConfig(env(map[string]string{"GANGOLF_API_KEY": "test.key", "GANGOLF_TWITCH_BOT_USER_ID": "99", "GANGOLF_CHANNELS": "Twitch/123", "PROCESSOR_MEMORY_CONFIG": "7"}))
	if err != nil {
		t.Fatal(err)
	}
	runner, reviewer := processors(cfg)
	if _, ok := runner.(*processor.Client); !ok || reviewer == nil {
		t.Fatal("legacy runner/reviewer changed")
	}
}
