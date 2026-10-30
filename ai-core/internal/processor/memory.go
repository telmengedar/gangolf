package processor

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/telmengedar/gangolf/ai-core/internal/window"
)

// MemoryReviewTimeout bounds each private review independently of chat runs.
const MemoryReviewTimeout = 60 * time.Second

// MemoryResult reports a private decision and its persistence receipt.
type MemoryResult struct {
	State      string
	NoteID     int64
	ModelCalls int
}

type memoryResponse struct {
	Decision   *noteDecision `json:"noteDecision"`
	ModelCalls *int          `json:"modelCalls"`
}

type noteDecision struct {
	State   string          `json:"state"`
	Created json.RawMessage `json:"created"`
}

// Review submits speaker evidence and returns only the typed memory receipt.
func (c *Client) Review(ctx context.Context, lines []window.Utterance, history []window.Exchange) (MemoryResult, error) {
	source, err := transcriptEvidence(lines, history)
	if err != nil {
		return MemoryResult{}, err
	}
	body, err := c.request(ctx, source.Text, nil, "/runs?verbose=true", source)
	if err != nil {
		return MemoryResult{}, err
	}
	var response memoryResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return MemoryResult{}, fmt.Errorf("processor memory: decode response: %w", err)
	}
	if response.Decision == nil || response.ModelCalls == nil || *response.ModelCalls < 1 {
		return MemoryResult{}, fmt.Errorf("processor memory: missing or invalid decision metadata")
	}
	decision := response.Decision
	result := MemoryResult{State: decision.State, ModelCalls: *response.ModelCalls}
	switch decision.State {
	case "stored", "unlinked":
		var noteID int64
		if json.Unmarshal(decision.Created, &noteID) != nil || noteID <= 0 {
			return MemoryResult{}, fmt.Errorf("processor memory: persisted decision lacks a positive note ID")
		}
		result.NoteID = noteID
	case "abstained", "rejected", "notStored":
		if len(decision.Created) != 0 {
			return MemoryResult{}, fmt.Errorf("processor memory: nonpersisted decision carries a note ID")
		}
	default:
		return MemoryResult{}, fmt.Errorf("processor memory: unknown decision state")
	}
	return result, nil
}
