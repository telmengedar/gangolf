package processor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/telmengedar/gangolf/ai-core/internal/window"
)

// ThoughtReceipt describes a confirmed native memory operation outside thought prose.
type ThoughtReceipt struct {
	Operation   string `json:"operation"`
	NodeID      int64  `json:"nodeId"`
	ContentHash string `json:"contentHash"`
	StepID      int64  `json:"stepId"`
	Visit       int    `json:"visit"`
}

// ThoughtMetadata correlates one private worker request with its observed persistence outcome.
type ThoughtMetadata struct {
	ProcessID   int64
	ProcessHash string
	InputHash   string
	SourceIDs   []string
	Receipt     *ThoughtReceipt
	RepairIDs   []int64
}

// ThoughtClient invokes an explicitly selected companion process after delivery.
type ThoughtClient struct{ process *ProcessClient }

// NewThoughtProcess binds the companion to the existing private review deadline.
func NewThoughtProcess(base string, processID int64, client *http.Client) *ThoughtClient {
	if client == nil {
		client = &http.Client{Timeout: MemoryReviewTimeout}
	}
	return &ThoughtClient{process: NewProcess(base, processID, 0, client)}
}

// Review retains attributed evidence and accepts only a matching native persistence receipt.
func (c *ThoughtClient) Review(ctx context.Context, lines []window.Utterance, history []window.Exchange, deliveredReply string) (MemoryResult, error) {
	metadata := &ThoughtMetadata{ProcessID: c.process.processID, SourceIDs: []string{}, RepairIDs: []int64{}}
	result := MemoryResult{Thought: metadata}
	httpStatus := 0
	nativeIDs := []int64{}
	remember := func() {
		for _, id := range nativeIDs {
			if !slices.Contains(metadata.RepairIDs, id) {
				metadata.RepairIDs = append(metadata.RepairIDs, id)
			}
		}
	}
	fail := func(code string) (MemoryResult, error) {
		remember()
		return result, &ProcessError{Code: code, Status: httpStatus}
	}
	if c.process.processID <= 0 || len(lines) == 0 || len(lines) > window.PendingCap || !utf8.ValidString(deliveredReply) || strings.TrimSpace(deliveredReply) == "" {
		return fail("invalid_thought_input")
	}
	latest := make([]struct {
		ID      string `json:"id,omitempty"`
		Speaker string `json:"speaker"`
		Text    string `json:"text"`
	}, len(lines))
	for i, line := range lines {
		if _, err := sourceEvidence(line); err != nil {
			return fail("invalid_thought_input")
		}
		if line.ID != "" {
			id, err := strconv.ParseInt(line.ID, 10, 64)
			if err != nil || id <= 0 || strconv.FormatInt(id, 10) != line.ID {
				return fail("invalid_thought_source")
			}
			metadata.SourceIDs = append(metadata.SourceIDs, line.ID)
		}
		latest[i].ID = line.ID
		latest[i].Speaker = line.Speaker
		latest[i].Text = line.Text
	}
	exchanges := workflowHistory(history)
	for _, exchange := range exchanges {
		if !utf8.ValidString(exchange.Input) || exchange.Reply != nil && !utf8.ValidString(exchange.Reply.Text) {
			return fail("invalid_thought_input")
		}
	}
	arguments := map[string]any{"latest": latest, "history": exchanges, "assistantReply": deliveredReply}
	encoded, err := json.Marshal(arguments)
	if err != nil || len(encoded) > 8*1024 {
		return fail("thought_input_bound")
	}
	sum := sha256.Sum256(encoded)
	metadata.InputHash = hex.EncodeToString(sum[:])
	status, body, err := c.process.requestProcess(ctx, arguments)
	httpStatus = status
	if err != nil {
		return result, err
	}
	raw, err := processObject(body)
	if err != nil {
		return result, &ProcessError{Code: "invalid_response", Status: status}
	}
	var traces []struct {
		Ordinal  int              `json:"ordinal"`
		StepID   int64            `json:"stepId"`
		Kind     string           `json:"kind"`
		Receipts []ThoughtReceipt `json:"receipts"`
		Repair   *struct {
			NodeID int64 `json:"nodeId"`
		} `json:"repair"`
	}
	if json.Unmarshal(raw["trace"], &traces) != nil || len(traces) > 32 {
		return result, &ProcessError{Code: "invalid_thought_trace", Status: status}
	}
	partialWrite := false
	for _, trace := range traces {
		for _, receipt := range trace.Receipts {
			if receipt.Operation == "memorize" && receipt.NodeID > 0 {
				nativeIDs = append(nativeIDs, receipt.NodeID)
			}
		}
		if trace.Kind == "judgement" {
			result.ModelCalls++
		}
		if trace.Repair != nil {
			partialWrite = true
			if trace.Repair.NodeID > 0 {
				metadata.RepairIDs = append(metadata.RepairIDs, trace.Repair.NodeID)
			}
		}
	}
	envelope, err := c.process.envelope(status, body)
	if err != nil {
		remember()
		return result, err
	}
	var identity struct {
		Hash string `json:"hash"`
	}
	json.Unmarshal(envelope["process"], &identity)
	metadata.ProcessHash = identity.Hash
	if partialWrite {
		return fail("memory_write_incomplete")
	}
	output, err := processObject(envelope["output"])
	if err != nil || len(output) != 1 {
		return fail("invalid_thought_output")
	}
	thought, err := processObject(output["thought"])
	if err != nil || len(thought) != 2 {
		return fail("invalid_thought_output")
	}
	var state string
	if json.Unmarshal(thought["state"], &state) != nil {
		return fail("invalid_thought_output")
	}
	if result.ModelCalls < 1 || result.ModelCalls > 3 {
		return fail("invalid_thought_trace")
	}
	receipt, err := processObject(thought["receipt"])
	if err != nil {
		return fail("invalid_thought_receipt")
	}
	receipts := 0
	for _, trace := range traces {
		receipts += len(trace.Receipts)
	}
	if state == "abstained" {
		if len(receipt) != 0 || receipts != 0 {
			return fail("invalid_thought_receipt")
		}
		result.State = state
		return result, nil
	}
	if state != "stored" || len(receipt) != 5 || receipts != 1 {
		return fail("invalid_thought_receipt")
	}
	var observed ThoughtReceipt
	if json.Unmarshal(thought["receipt"], &observed) != nil || observed.Operation != "memorize" || observed.NodeID <= 0 || observed.StepID <= 0 || observed.Visit <= 0 || len(observed.ContentHash) != 64 {
		return fail("invalid_thought_receipt")
	}
	if _, err := hex.DecodeString(observed.ContentHash); err != nil {
		return fail("invalid_thought_receipt")
	}
	matched := false
	for _, trace := range traces {
		for _, native := range trace.Receipts {
			if native == observed && trace.Kind == "action" && trace.StepID == observed.StepID && trace.Ordinal == observed.Visit {
				matched = true
			}
		}
	}
	if !matched {
		return fail("invalid_thought_receipt")
	}
	metadata.Receipt = &observed
	result.State = "stored"
	result.NoteID = observed.NodeID
	return result, nil
}
