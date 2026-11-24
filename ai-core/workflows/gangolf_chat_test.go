package workflows_test

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

type blueprint struct {
	Entry  string `json:"entry"`
	Inputs map[string]struct {
		Type     string `json:"type"`
		NodeType string `json:"nodeType"`
	} `json:"inputs"`
	Outputs map[string]struct {
		Type string `json:"type"`
	} `json:"outputs"`
	Nodes []struct {
		Symbol string          `json:"symbol"`
		Type   string          `json:"type"`
		Body   json.RawMessage `json:"body"`
	} `json:"nodes"`
	Edges []struct {
		From    string `json:"from"`
		To      string `json:"to"`
		Role    string `json:"role"`
		Default bool   `json:"default"`
		When    *struct {
			Path   string `json:"path"`
			Equals any    `json:"equals"`
		} `json:"when"`
	} `json:"edges"`
}

func readGraph(t *testing.T) blueprint {
	t.Helper()
	raw, err := os.ReadFile("gangolf-chat-v2.json")
	if err != nil {
		t.Fatal(err)
	}
	var b blueprint
	if err := json.Unmarshal(raw, &b); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestBlueprint_StaticTaskAndSelectedHistory_PreservesAuthorityBoundary(t *testing.T) {
	b := readGraph(t)
	judgements := 0
	templates := 0
	for _, node := range b.Nodes {
		if node.Type == "process-prompt-template" {
			templates++
			var text string
			if err := json.Unmarshal(node.Body, &text); err != nil || !strings.HasPrefix(text, "# ") || strings.Contains(text, "{{") || !strings.Contains(text, "user JSON data") {
				t.Fatalf("unsafe/raw template=%s", node.Symbol)
			}
		}
		if node.Type != "process-step" {
			continue
		}
		var step struct {
			Kind    string  `json:"kind"`
			Tools   []int64 `json:"tools"`
			Context []struct {
				Key      string `json:"key"`
				Required bool   `json:"required"`
				Value    struct {
					Source string `json:"source"`
					Path   string `json:"path"`
				} `json:"value"`
			} `json:"context"`
		}
		json.Unmarshal(node.Body, &step)
		if step.Kind != "judgement" {
			continue
		}
		judgements++
		if len(step.Tools) != 0 {
			t.Fatalf("judgement offered tools=%s", node.Symbol)
		}
		history := false
		for _, item := range step.Context {
			if item.Key == "history" {
				history = item.Required && item.Value.Source == "argument" && item.Value.Path == "history"
			}
			if item.Key == "policy" && node.Symbol != "decide_response" {
				t.Fatal("policy leaked beyond decision context")
			}
			if item.Key == "persona" && node.Symbol != "compose_response" {
				t.Fatal("persona leaked beyond composition")
			}
		}
		if !history {
			t.Fatalf("history lost=%s", node.Symbol)
		}
	}
	if judgements != 7 || templates != 6 {
		t.Fatalf("judgements=%d templates=%d", judgements, templates)
	}
	if len(b.Inputs) != 3 || b.Inputs["history"].Type != "array" || b.Inputs["persona"].NodeType != "gangolf-chat-policy" || len(b.Outputs) != 2 || b.Outputs["reply"].Type != "string" || b.Outputs["silent"].Type != "boolean" {
		t.Fatal("caller declarations changed")
	}
}

func TestBlueprint_ExactDecisionAndBoundedRepair_RejectsFallbacks(t *testing.T) {
	b := readGraph(t)
	if b.Entry != "load_decision_policy" {
		t.Fatal("policy not loaded first")
	}
	controls := map[string]map[string]string{}
	defaults := 0
	for _, edge := range b.Edges {
		if edge.Role != "" {
			continue
		}
		if edge.Default {
			defaults++
			if edge.From != "check_draft" || edge.To != "repair_response" {
				t.Fatal("unexpected fallback edge")
			}
		}
		if edge.When != nil {
			if controls[edge.From] == nil {
				controls[edge.From] = map[string]string{}
			}
			value, _ := edge.When.Equals.(string)
			controls[edge.From][edge.To] = edge.When.Path + "=" + value
		}
	}
	if defaults != 1 || !reflect.DeepEqual(controls["decide_response"], map[string]string{"return_silent": "reply=SILENCE", "prepare_query": "reply=REPLY"}) || !reflect.DeepEqual(controls["check_repair"], map[string]string{"return_repaired": "reply=ACCEPT"}) || controls["check_draft"]["return_draft"] != "reply=ACCEPT" {
		t.Fatal("exact decisions or final rejection lost")
	}
	for _, symbol := range []string{"load_decision_policy", "recall_context", "load_persona"} {
		count := 0
		for _, edge := range b.Edges {
			if edge.From == symbol && edge.Role == "" {
				count++
				if edge.When == nil || edge.When.Path != "ok" || edge.When.Equals != true {
					t.Fatalf("action failure bypass=%s", symbol)
				}
			}
		}
		if count != 1 {
			t.Fatal("action transition ambiguity")
		}
	}
}

func TestBlueprint_RecallAndTransientExports_ExcludeWritesAndStalePolicy(t *testing.T) {
	b := readGraph(t)
	exports := map[string]map[string]any{}
	foundRecall := false
	returns := 0
	for _, node := range b.Nodes {
		var body map[string]any
		if node.Type == "process-prompt-template" {
			continue
		}
		json.Unmarshal(node.Body, &body)
		if builtin, _ := body["builtin"].(string); builtin != "" && builtin != "read_node" && builtin != "recall" {
			t.Fatalf("unexpected tool capability=%s", builtin)
		}
		if node.Symbol == "recall_context" {
			foundRecall = true
			filter := body["input"].(map[string]any)["filter"]
			want := map[string]any{"type": []any{"thought", "knowledge"}}
			if !reflect.DeepEqual(filter, want) {
				t.Fatalf("recall scope=%v", filter)
			}
		}
		if value, ok := body["exports"].(map[string]any); ok {
			exports[node.Symbol] = value
		}
		if body["kind"] == "return" {
			returns++
			output := body["output"].(map[string]any)
			if len(output) != 2 {
				t.Fatal("ambiguous output")
			}
			if node.Symbol == "return_silent" {
				if output["reply"] != "" || output["silent"] != true {
					t.Fatal("silence carrier changed")
				}
			} else if output["silent"] != false {
				t.Fatal("reply carrier changed")
			}
		}
	}
	if !foundRecall || returns != 3 || len(exports["decide_response"]) != 0 || len(exports["check_repair"]) != 1 {
		t.Fatal("scope/return/export contract lost")
	}
	for _, symbol := range []string{"compose_response", "check_draft", "repair_response", "check_repair"} {
		for key := range exports[symbol] {
			if key == "persona" || key == "policy" || key == "recalled" {
				t.Fatalf("transient authority retained=%s", symbol)
			}
		}
	}
}
