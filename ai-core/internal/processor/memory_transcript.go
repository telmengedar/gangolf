package processor

import (
	"encoding/json"
	"fmt"
	"unicode/utf8"

	"github.com/telmengedar/gangolf/ai-core/internal/window"
)

type memoryTranscript struct {
	History []wireExchange `json:"history"`
	Latest  []noteSource   `json:"latest"`
}

func transcriptEvidence(lines []window.Utterance, history []window.Exchange) (*noteSource, error) {
	if len(lines) == 0 {
		return nil, fmt.Errorf("processor memory: missing trusted source lines")
	}
	transcript := memoryTranscript{History: make([]wireExchange, len(history)), Latest: make([]noteSource, len(lines))}
	for i, exchange := range history {
		if !utf8.ValidString(exchange.Input) {
			return nil, fmt.Errorf("processor memory: historical source must contain valid UTF-8")
		}
		transcript.History[i] = wireExchange{Input: exchange.Input}
	}
	for i, line := range lines {
		source, err := sourceEvidence(line)
		if err != nil {
			return nil, err
		}
		transcript.Latest[i] = *source
	}
	data, err := json.Marshal(transcript)
	if err != nil {
		return nil, fmt.Errorf("processor memory: encode transcript: %w", err)
	}
	if len(data) > 8*1024 {
		return nil, fmt.Errorf("processor memory: source transcript exceeds byte limit")
	}
	return &noteSource{Speaker: "Conversation transcript", Text: string(data)}, nil
}
