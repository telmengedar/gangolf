package messages

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/telmengedar/gangolf/ai-core/internal/window"
)

// BotServiceID is the service-side id every Gangolf reply is posted under.
const BotServiceID = "GangolfBot"

const directService = "localhost"

const (
	requestTimeout = 30 * time.Second
	bodyPrefixSize = 200
	fromLayout     = "2006-01-02T15:04:05.000Z"
)

const zonelessLayout = "2006-01-02T15:04:05.999999999"

// Client talks to the online message API.
type Client struct {
	base string
	key  string
	bot  string
	http *http.Client
	log  *slog.Logger
}

// New returns a client for the API at base authenticating with key and posting as botName.
func New(base, key, botName string, httpClient *http.Client, log *slog.Logger) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: requestTimeout}
	}
	return &Client{base: strings.TrimRight(base, "/"), key: key, bot: botName, http: httpClient, log: log}
}

type page struct {
	Result []wireMessage `json:"result"`
	Total  int           `json:"total"`
}

type wireMessage struct {
	Service string      `json:"service"`
	Channel string      `json:"channel"`
	User    wireUser    `json:"user"`
	Time    string      `json:"time"`
	Chunks  []wireChunk `json:"chunks"`
}

type wireUser struct {
	Name string `json:"name"`
}

type wireChunk struct {
	Type json.RawMessage `json:"type"`
	Data string          `json:"data"`
	Code string          `json:"code"`
}

type postMessage struct {
	Service string       `json:"service"`
	Channel string       `json:"channel"`
	User    postUser     `json:"user"`
	Chunks  []postChunk  `json:"chunks"`
	Tts     bool         `json:"tts"`
	PostTo  []postTarget `json:"postTo,omitempty"`
}

type postTarget struct {
	Service string `json:"service"`
	Name    string `json:"name"`
}

type postUser struct {
	Service   string `json:"service"`
	ServiceID string `json:"serviceId"`
	Name      string `json:"name"`
}

type postChunk struct {
	Type int    `json:"type"`
	Data string `json:"data"`
}

// List returns the messages stamped at or after from.
func (c *Client) List(ctx context.Context, from time.Time) ([]window.Line, error) {
	target := c.base + "/v1/messages?from=" + url.QueryEscape(from.UTC().Format(fromLayout))
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, fmt.Errorf("list messages: %w", err)
	}
	body, err := c.do(request)
	if err != nil {
		return nil, fmt.Errorf("list messages: %w", err)
	}
	var result page
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("list messages: decode page: %w", err)
	}
	if result.Total > len(result.Result) {
		c.log.Warn("message page is truncated", "total", result.Total, "returned", len(result.Result))
	}
	lines := make([]window.Line, 0, len(result.Result))
	for _, message := range result.Result {
		stamp, ok := parseTime(message.Time)
		text := textOf(message.Chunks)
		if !ok || text == "" {
			c.log.Warn("skipped an unreadable message", "timeReadable", ok, "hasText", text != "")
			continue
		}
		lines = append(lines, window.Line{
			Key:     window.Key{Service: message.Service, Channel: message.Channel},
			Speaker: message.User.Name,
			Text:    text,
			Time:    stamp,
		})
	}
	return lines, nil
}

// Post sends one Gangolf reply to the channel; on a chat platform it is stored when that platform reads it back.
func (c *Client) Post(ctx context.Context, key window.Key, text string) error {
	payload := []postMessage{{
		Service: key.Service,
		Channel: key.Channel,
		User:    postUser{Service: key.Service, ServiceID: BotServiceID, Name: c.bot},
		Chunks:  []postChunk{{Type: 0, Data: text}},
		Tts:     true,
		PostTo:  targetsFor(key),
	}}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("post message: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/v1/messages", bytes.NewReader(encoded))
	if err != nil {
		return fmt.Errorf("post message: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	if _, err := c.do(request); err != nil {
		return fmt.Errorf("post message: %w", err)
	}
	return nil
}

func targetsFor(key window.Key) []postTarget {
	if strings.EqualFold(key.Service, directService) {
		return nil
	}
	return []postTarget{{Service: key.Service, Name: key.Channel}}
}

func (c *Client) do(request *http.Request) ([]byte, error) {
	request.Header.Set("Authorization", "Bearer "+c.key)
	response, err := c.http.Do(request)
	if err != nil {
		return nil, errors.New(c.redact(err.Error()))
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		hint := ""
		if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
			hint = " (check GANGOLF_API_KEY)"
		}
		return nil, fmt.Errorf("status %d%s: %s", response.StatusCode, hint, c.prefix(body))
	}
	return body, nil
}

func (c *Client) prefix(body []byte) string {
	text := c.redact(string(body))
	if len(text) > bodyPrefixSize {
		text = text[:bodyPrefixSize]
	}
	return text
}

func (c *Client) redact(text string) string {
	if c.key == "" {
		return text
	}
	return strings.ReplaceAll(text, c.key, "[redacted]")
}

func textOf(chunks []wireChunk) string {
	var text strings.Builder
	for _, chunk := range chunks {
		switch chunkKind(chunk.Type) {
		case kindText:
			text.WriteString(chunk.Data)
		case kindEmote:
			text.WriteString(chunk.Code)
		}
	}
	return strings.Join(strings.Fields(text.String()), " ")
}

type kind int

const (
	kindOther kind = iota
	kindText
	kindEmote
)

func chunkKind(raw json.RawMessage) kind {
	var number int
	if json.Unmarshal(raw, &number) == nil {
		switch number {
		case 0:
			return kindText
		case 1:
			return kindEmote
		}
		return kindOther
	}
	var name string
	if json.Unmarshal(raw, &name) == nil {
		switch strings.ToLower(name) {
		case "text":
			return kindText
		case "emote":
			return kindEmote
		}
	}
	return kindOther
}

func parseTime(value string) (time.Time, bool) {
	if stamp, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return stamp.UTC(), true
	}
	if stamp, err := time.ParseInLocation(zonelessLayout, value, time.UTC); err == nil {
		return stamp, true
	}
	return time.Time{}, false
}
