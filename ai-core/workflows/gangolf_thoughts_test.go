package workflows_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

func readThoughtGraph(t *testing.T) blueprint {
	t.Helper()
	raw, err := os.ReadFile("gangolf-thoughts-v2.json")
	if err != nil {
		t.Fatal(err)
	}
	var b blueprint
	if err := json.Unmarshal(raw, &b); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestThoughtGraph_FrozenPrompts_SelectsOnlyAttributedEvidence(t *testing.T) {
	b := readThoughtGraph(t)
	expectedHashes := map[string]string{"query_template": "3e60a467e8141b216f9f383aeb825502a6f30b0f7bf4d28be266a0e059865892", "insight_template": "783507f08a95f0ae7b8a82e919fa1f72829e231d7190bf4eb66873eaad4dc6b3", "guard_template": "6d4525438a1ebec849e03eb4c0874c3e44603dfc3b18699f8edc1cbedffb0cac"}
	templates, judgements := 0, 0
	for _, node := range b.Nodes {
		if node.Type == "process-prompt-template" {
			templates++
			var text string
			if json.Unmarshal(node.Body, &text) != nil || strings.Contains(text, "{{") {
				t.Fatal("dynamic template promoted evidence")
			}
			sum := sha256.Sum256([]byte(text))
			if hex.EncodeToString(sum[:]) != expectedHashes[node.Symbol] {
				t.Fatalf("frozen thought prompt changed=%s", node.Symbol)
			}
		}
		if node.Type != "process-step" {
			continue
		}
		var body map[string]any
		json.Unmarshal(node.Body, &body)
		if body["kind"] != "judgement" {
			continue
		}
		judgements++
		if _, present := body["tools"]; present {
			t.Fatal("thought judgement can select writes")
		}
		expected := []any{map[string]any{"key": "latest", "required": true, "value": map[string]any{"source": "argument", "path": "latest"}}, map[string]any{"key": "history", "required": true, "value": map[string]any{"source": "argument", "path": "history"}}, map[string]any{"key": "assistantReply", "required": true, "value": map[string]any{"source": "argument", "path": "assistantReply"}}}
		if node.Symbol == "create_insight" || node.Symbol == "check_insight" {
			expected = append(expected, map[string]any{"key": "existingThoughts", "required": true, "value": map[string]any{"source": "transport", "path": "existingThoughts"}})
			if node.Symbol == "check_insight" {
				expected = append(expected, map[string]any{"key": "candidate", "required": true, "value": map[string]any{"source": "transport", "path": "candidate"}})
			}
		} else if node.Symbol != "prepare_insight_query" {
			t.Fatal("extra inference stage")
		}
		if !reflect.DeepEqual(body["context"], expected) {
			t.Fatalf("thought context authority/scope changed=%s", node.Symbol)
		}
	}
	if templates != 3 || judgements != 3 || len(b.Inputs) != 3 || b.Inputs["latest"].Type != "array" || b.Inputs["history"].Type != "array" || b.Inputs["assistantReply"].Type != "string" || len(b.Outputs) != 1 || b.Outputs["thought"].Type != "object" {
		t.Fatal("companion interface or inference budget changed")
	}
}

func TestThoughtGraph_PrivateOwnedTarget_PreventsModelPlacementAndProvenanceGuess(t *testing.T) {
	b := readThoughtGraph(t)
	members := map[string]bool{}
	tools := 0
	for _, node := range b.Nodes {
		members[node.Symbol] = true
	}
	for _, node := range b.Nodes {
		if node.Type == "process-tool" {
			tools++
			var body map[string]any
			json.Unmarshal(node.Body, &body)
			switch body["builtin"] {
			case "memorize":
				expected := map[string]any{"root": map[string]any{"nodeRef": "thought_namespace"}, "anchor": map[string]any{"nodeRef": "thought_owner"}, "type": "thought", "access": float64(0)}
				if !reflect.DeepEqual(body["memory"], expected) || !members["thought_namespace"] || !members["thought_owner"] {
					t.Fatal("private owned target changed or inherited visibility assumed")
				}
				schema := body["schema"].(map[string]any)
				if !reflect.DeepEqual(schema["required"], []any{"text"}) || len(schema["properties"].(map[string]any)) != 1 || schema["additionalProperties"] != false {
					t.Fatal("model can supply placement/access/provenance")
				}
			case "recall":
			default:
				t.Fatal("unexpected thought capability")
			}
		}
		if node.Type != "process-step" {
			continue
		}
		var body map[string]any
		json.Unmarshal(node.Body, &body)
		if node.Symbol == "recall_thoughts" {
			filter := body["input"].(map[string]any)["filter"]
			expected := map[string]any{"type": []any{"thought"}, "rootNodeId": map[string]any{"nodeRef": "thought_namespace"}, "linkedto": []any{map[string]any{"nodeRef": "thought_owner"}}}
			if !reflect.DeepEqual(filter, expected) {
				t.Fatal("existing thought recall escaped namespace/author")
			}
		}
		if node.Symbol == "persist_insight" {
			if !reflect.DeepEqual(body["input"], map[string]any{"text": map[string]any{"source": "transport", "path": "candidate"}}) {
				t.Fatal("thought body gains transcript/provenance or model placement")
			}
		}
	}
	if tools != 2 {
		t.Fatal("unexpected thought tool budget")
	}
}

func TestThoughtGraph_SkipAndStorageBranches_RequireExactControlAndRuntimeReceipt(t *testing.T) {
	b := readThoughtGraph(t)
	skips := 0
	storedEdges := 0
	for _, edge := range b.Edges {
		if edge.From == "prepare_insight_query" || edge.From == "create_insight" {
			if edge.To == "return_abstained" {
				skips++
				if edge.When == nil || edge.When.Path != "reply" || edge.When.Equals != "SKIP" || edge.Default {
					t.Fatal("abstention is not exact SKIP")
				}
			}
		}
		if edge.From == "persist_insight" {
			storedEdges++
			if edge.To != "return_stored" || edge.When == nil || edge.When.Path != "ok" || edge.When.Equals != true || edge.Default {
				t.Fatal("failed write can claim success or retry")
			}
		}
	}
	if skips != 2 || storedEdges != 1 {
		t.Fatal("skip/persistence control changed")
	}
	returns := 0
	for _, node := range b.Nodes {
		if node.Type != "process-step" {
			continue
		}
		var body map[string]any
		json.Unmarshal(node.Body, &body)
		if node.Symbol == "create_insight" {
			if !reflect.DeepEqual(body["exports"], map[string]any{"candidate": map[string]any{"source": "result", "path": "reply"}, "existingThoughts": map[string]any{"source": "transport", "path": "existingThoughts"}}) {
				t.Fatal("persisted thought not exact generated prose")
			}
		}
		if body["kind"] != "return" {
			continue
		}
		returns++
		expected := map[string]any{"thought": map[string]any{"state": "abstained", "receipt": map[string]any{}}}
		if node.Symbol == "return_stored" {
			expected = map[string]any{"thought": map[string]any{"state": "stored", "receipt": map[string]any{"source": "receipt", "path": "latest"}}}
		} else if node.Symbol != "return_abstained" && node.Symbol != "return_unsupported" && node.Symbol != "return_duplicate" {
			t.Fatal("unexpected return")
		}
		if !reflect.DeepEqual(body["output"], expected) {
			t.Fatal("thought receipt synthesized from prose or abstention ambiguous")
		}
	}
	if returns != 4 {
		t.Fatal("extra success fallback")
	}
}

func TestThoughtGraph_RecalledProse_ReachesNoveltyConsumerWithoutProjection(t *testing.T) {
	b := readThoughtGraph(t)
	producer, consumer := false, false
	for _, node := range b.Nodes {
		if node.Type != "process-step" {
			continue
		}
		var body map[string]any
		if err := json.Unmarshal(node.Body, &body); err != nil {
			t.Fatal(err)
		}
		if node.Symbol == "recall_thoughts" {
			producer = true
			expected := map[string]any{"existingThoughts": map[string]any{"source": "result", "path": "value"}}
			if !reflect.DeepEqual(body["exports"], expected) {
				t.Fatalf("recalled prose export changed=%v", body["exports"])
			}
		}
		if node.Symbol == "create_insight" {
			contexts, ok := body["context"].([]any)
			if !ok {
				t.Fatal("novelty consumer context missing")
			}
			for _, entry := range contexts {
				selected, ok := entry.(map[string]any)
				if !ok {
					t.Fatal("invalid selected context")
				}
				if selected["key"] == "existingThoughts" {
					consumer = true
					expected := map[string]any{"key": "existingThoughts", "required": true, "value": map[string]any{"source": "transport", "path": "existingThoughts"}}
					if !reflect.DeepEqual(selected, expected) {
						t.Fatal("novelty consumer does not require complete recalled data")
					}
				}
			}
		}
	}
	if !producer || !consumer {
		t.Fatal("recalled prose producer or novelty consumer missing")
	}
}

func TestThoughtGraph_AdmissionGate_PersistsOnlyOriginalApprovedCandidate(t *testing.T) {
	b := readThoughtGraph(t)
	bodies := map[string]map[string]any{}
	for _, node := range b.Nodes {
		if node.Type != "process-step" {
			continue
		}
		var body map[string]any
		if json.Unmarshal(node.Body, &body) != nil {
			t.Fatal("invalid step")
		}
		bodies[node.Symbol] = body
	}
	creation, exists := bodies["create_insight"]
	if !exists {
		t.Fatal("candidate producer missing")
	}
	expectedCreation := map[string]any{"candidate": map[string]any{"source": "result", "path": "reply"}, "existingThoughts": map[string]any{"source": "transport", "path": "existingThoughts"}}
	if !reflect.DeepEqual(creation["exports"], expectedCreation) {
		t.Fatal("admission lost original candidate or actual prior thought prose")
	}
	guard, exists := bodies["check_insight"]
	if !exists {
		t.Fatal("candidate can bypass missing admission gate")
	}
	if !reflect.DeepEqual(guard["exports"], map[string]any{"candidate": map[string]any{"source": "transport", "path": "candidate"}}) {
		t.Fatal("guard verdict or rewritten text can replace original candidate")
	}
	persist, exists := bodies["persist_insight"]
	if !exists || !reflect.DeepEqual(persist["input"], map[string]any{"text": map[string]any{"source": "transport", "path": "candidate"}}) {
		t.Fatal("persist does not consume the original approved candidate")
	}
	incoming := 0
	routes := map[string]string{}
	createdGate := false
	for _, edge := range b.Edges {
		if edge.Role != "" {
			continue
		}
		if edge.From == "create_insight" && edge.Default {
			createdGate = edge.To == "check_insight"
		}
		if edge.To == "persist_insight" {
			incoming++
			if edge.From != "check_insight" || edge.Default || edge.When == nil || edge.When.Path != "reply" || edge.When.Equals != "INSIGHT" {
				t.Fatal("candidate can bypass exact admission INSIGHT")
			}
		}
		if edge.From == "check_insight" {
			if edge.Default || edge.When == nil || edge.When.Path != "reply" {
				t.Fatal("malformed guard output can fall back")
			}
			token, ok := edge.When.Equals.(string)
			if !ok {
				t.Fatal("nontext guard decision")
			}
			if _, duplicate := routes[token]; duplicate {
				t.Fatal("ambiguous guard decision")
			}
			routes[token] = edge.To
		}
	}
	if !createdGate || incoming != 1 || !reflect.DeepEqual(routes, map[string]string{"INSIGHT": "persist_insight", "REPORT": "return_abstained", "UNSUPPORTED": "return_unsupported", "DUPLICATE": "return_duplicate"}) {
		t.Fatal("strict admission route changed")
	}
	for _, rejected := range []string{"REPORT", "UNSUPPORTED", "DUPLICATE"} {
		destination := routes[rejected]
		if bodies[destination]["kind"] != "return" || !reflect.DeepEqual(bodies[destination]["output"], map[string]any{"thought": map[string]any{"state": "abstained", "receipt": map[string]any{}}}) {
			t.Fatalf("rejected category can claim storage=%s", rejected)
		}
		for _, edge := range b.Edges {
			if edge.From == destination && edge.Role == "" {
				t.Fatalf("rejected category can continue to a write=%s", rejected)
			}
		}
	}
	actualBadCandidate := "Ada stated her favorite color is indigo."
	sum := sha256.Sum256([]byte(actualBadCandidate))
	if hex.EncodeToString(sum[:]) != "da575599d5ae9750feea0def5e1968dbe67ad30e5d0424d3907984498a2e809a" {
		t.Fatal("observed counterexample bytes changed")
	}
	for _, invalid := range []string{"STORE", "SKIP", "INSIGHT ", "insight", "INSIGHT\n", `{"decision":"INSIGHT"}`, actualBadCandidate, "No new insight should be stored."} {
		if _, allowed := routes[invalid]; allowed {
			t.Fatal("guard explanation/nonexact control can authorize persistence")
		}
	}
}
