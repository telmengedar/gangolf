package processor

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/telmengedar/gangolf/ai-core/internal/window"
)

type noteSource struct {
	Speaker string `json:"speaker"`
	Text    string `json:"text"`
}

func sourceEvidence(source window.Utterance) (*noteSource, error) {
	if !utf8.ValidString(source.Speaker) || !utf8.ValidString(source.Text) {
		return nil, fmt.Errorf("processor memory: source must contain valid UTF-8")
	}
	if strings.TrimSpace(source.Speaker) == "" || strings.TrimSpace(source.Text) == "" {
		return nil, fmt.Errorf("processor memory: source speaker and text must not be blank")
	}
	return &noteSource{Speaker: source.Speaker, Text: source.Text}, nil
}
