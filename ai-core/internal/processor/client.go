package processor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/telmengedar/gangolf/ai-core/internal/window"
)

// RunTimeout is how long one run may take before the client gives up.
const RunTimeout = 700 * time.Second

const bodyPrefixSize = 200

// Client calls processor's run endpoint.
type Client struct {
	base    string
	subject int64
	http    *http.Client
}

// Result is what a run produced: a reply, or the decision to stay silent.
type Result struct {
	Reply  string
	Silent bool
}

// New returns a client for the processor at base that anchors every run on subject.
func New(base string, subject int64, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: RunTimeout}
	}
	return &Client{base: strings.TrimRight(base, "/"), subject: subject, http: httpClient}
}

type runRequest struct {
	Input   string         `json:"input"`
	Subject int64          `json:"subject"`
	History []wireExchange `json:"history"`
}

type wireExchange struct {
	Input string `json:"input"`
	Reply string `json:"reply,omitempty"`
}

type runResponse struct {
	Reply    string `json:"reply"`
	Produced *bool  `json:"produced"`
}

type errorEnvelope struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// Run asks processor to answer input given the earlier conversation.
func (c *Client) Run(ctx context.Context, input string, history []window.Exchange) (Result, error) {
	exchanges := make([]wireExchange, len(history))
	for i, exchange := range history {
		exchanges[i] = wireExchange{Input: exchange.Input, Reply: exchange.Reply}
	}
	encoded, err := json.Marshal(runRequest{Input: input, Subject: c.subject, History: exchanges})
	if err != nil {
		return Result{}, fmt.Errorf("processor run: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/runs", bytes.NewReader(encoded))
	if err != nil {
		return Result{}, fmt.Errorf("processor run: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		return Result{}, fmt.Errorf("processor run: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return Result{}, fmt.Errorf("processor run: read response: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return Result{}, failure(response.StatusCode, body)
	}
	var decoded runResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return Result{}, fmt.Errorf("processor run: decode response: %w", err)
	}
	silent := (decoded.Produced != nil && !*decoded.Produced) || strings.TrimSpace(decoded.Reply) == ""
	return Result{Reply: decoded.Reply, Silent: silent}, nil
}

func failure(status int, body []byte) error {
	var envelope errorEnvelope
	if json.Unmarshal(body, &envelope) == nil && envelope.Error.Code != "" {
		return fmt.Errorf("processor run: status %d: %s: %s", status, envelope.Error.Code, envelope.Error.Message)
	}
	text := string(body)
	if len(text) > bodyPrefixSize {
		text = text[:bodyPrefixSize]
	}
	return fmt.Errorf("processor run: status %d: %s", status, text)
}
