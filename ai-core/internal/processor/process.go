package processor

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/telmengedar/gangolf/ai-core/internal/window"
)

// ProcessClient invokes one explicitly selected, published chat workflow.
type ProcessClient struct {
	base      string
	processID int64
	personaID int64
	http      *http.Client
}

// ProcessError identifies a failed workflow without exposing model or history text.
type ProcessError struct {
	Code   string
	Status int
	Cause  error
}

func (e *ProcessError) Error() string {
	return fmt.Sprintf("processor process: %s (HTTP %d)", e.Code, e.Status)
}

func (e *ProcessError) Unwrap() error { return e.Cause }

// NewProcess binds the chat workflow and its current, separately published policy.
func NewProcess(base string, processID, personaID int64, client *http.Client) *ProcessClient {
	if client == nil {
		client = &http.Client{Timeout: RunTimeout}
	}
	return &ProcessClient{base: strings.TrimRight(base, "/"), processID: processID, personaID: personaID, http: client}
}

// Run retains attributed conversation as data and returns only declared final output.
func (c *ProcessClient) Run(ctx context.Context, input string, history []window.Exchange) (Result, error) {
	if c.processID <= 0 || c.personaID <= 0 {
		return Result{}, &ProcessError{Code: "invalid_binding"}
	}
	if len(history) > 4 {
		history = history[len(history)-4:]
	}
	exchanges := make([]wireExchange, len(history))
	for i, item := range history {
		exchanges[i] = wireExchange{Input: item.Input, Reply: item.Reply}
	}
	encoded, err := json.Marshal(map[string]any{
		"processId": c.processID,
		"arguments": map[string]any{"message": input, "history": exchanges, "persona": c.personaID},
	})
	if err != nil {
		return Result{}, &ProcessError{Code: "invalid_request", Cause: err}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/process-runs", bytes.NewReader(encoded))
	if err != nil {
		return Result{}, &ProcessError{Code: "invalid_request", Cause: err}
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		return Result{}, &ProcessError{Code: "transport", Cause: err}
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return Result{}, &ProcessError{Code: "read_response", Status: response.StatusCode, Cause: err}
	}
	return c.decode(response.StatusCode, body)
}

func (c *ProcessClient) decode(status int, body []byte) (Result, error) {
	invalid := func(code string) (Result, error) { return Result{}, &ProcessError{Code: code, Status: status} }
	envelope, err := processObject(body)
	if err != nil {
		return invalid("invalid_response")
	}
	for key := range envelope {
		if key != "process" && key != "status" && key != "output" && key != "trace" && key != "error" {
			return invalid("invalid_response")
		}
	}
	if raw, ok := envelope["error"]; ok && string(raw) != "null" {
		failure, err := processObject(raw)
		var code string
		if err != nil || json.Unmarshal(failure["code"], &code) != nil || code == "" || len(code) > 64 {
			return invalid("invalid_response")
		}
		for _, r := range code {
			if r != '_' && (r < 'a' || r > 'z') {
				return invalid("invalid_response")
			}
		}
		return invalid(code)
	}
	var state string
	if status != http.StatusOK || json.Unmarshal(envelope["status"], &state) != nil || state != "completed" {
		return invalid("invalid_response")
	}
	identity, err := processObject(envelope["process"])
	var id int64
	var name, hash string
	if err != nil || len(identity) != 3 || json.Unmarshal(identity["id"], &id) != nil || id != c.processID || json.Unmarshal(identity["name"], &name) != nil || strings.TrimSpace(name) == "" || json.Unmarshal(identity["hash"], &hash) != nil || len(hash) != 64 {
		return invalid("invalid_response")
	}
	if _, err := hex.DecodeString(hash); err != nil {
		return invalid("invalid_response")
	}
	output, err := processObject(envelope["output"])
	var reply *string
	var silent *bool
	if err != nil || len(output) != 2 || json.Unmarshal(output["reply"], &reply) != nil || json.Unmarshal(output["silent"], &silent) != nil || reply == nil || silent == nil {
		return invalid("invalid_output")
	}
	if *silent {
		if *reply != "" {
			return invalid("invalid_output")
		}
		return Result{Silent: true, Chosen: true}, nil
	}
	if strings.TrimSpace(*reply) == "" {
		return invalid("invalid_output")
	}
	return Result{Reply: *reply}, nil
}

func processObject(body []byte) (map[string]json.RawMessage, error) {
	if !utf8.Valid(body) {
		return nil, fmt.Errorf("invalid UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return nil, fmt.Errorf("expected object")
	}
	fields := map[string]json.RawMessage{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		key, ok := token.(string)
		if !ok {
			return nil, fmt.Errorf("expected field")
		}
		if _, exists := fields[key]; exists {
			return nil, fmt.Errorf("duplicate field")
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		fields[key] = value
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("trailing JSON")
	}
	return fields, nil
}
