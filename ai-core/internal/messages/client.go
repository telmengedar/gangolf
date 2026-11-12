package messages

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/telmengedar/gangolf/ai-core/internal/window"
)

// BotServiceID is the reserved API author marker for outgoing replies.
const BotServiceID = "GangolfBot"
const requestTimeout = 30 * time.Second
const zonelessLayout = "2006-01-02T15:04:05.999999999"

// Client talks to the v3 message API.
type Client struct {
	base, key, bot string
	http           *http.Client
	log            *slog.Logger
}

// New returns a client identifying its own inbound messages by numeric botID.
func New(base, key, botID string, httpClient *http.Client, log *slog.Logger) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: requestTimeout}
	}
	bounded := *httpClient
	bounded.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{base: strings.TrimRight(base, "/"), key: key, bot: botID, http: &bounded, log: log}
}

type page struct {
	Result   *[]wireMessage  `json:"result"`
	Total    *int            `json:"total"`
	Continue json.RawMessage `json:"continue"`
}

type wireMessage struct {
	ID       string          `json:"id"`
	Service  string          `json:"service"`
	Channel  string          `json:"channel"`
	User     wireUser        `json:"user"`
	Time     string          `json:"time"`
	Chunks   []wireChunk     `json:"chunks"`
	Source   *wireSource     `json:"source"`
	Delivery *wireDelivery   `json:"delivery"`
	TTS      *bool           `json:"tts"`
	ReplyTo  json.RawMessage `json:"replyTo"`
}

type wireSource struct {
	Kind      string  `json:"kind"`
	Transport *string `json:"transport"`
}

type wireDelivery struct {
	State      string          `json:"state"`
	MessageID  json.RawMessage `json:"messageId"`
	Transport  string          `json:"transport"`
	RequestKey string          `json:"requestKey"`
}

type wireUser struct {
	Service   string `json:"service"`
	ServiceID string `json:"serviceId"`
	Name      string `json:"name"`
}

type wireChunk struct {
	Type json.RawMessage `json:"type"`
	Data string          `json:"data"`
	Code string          `json:"code"`
}

type postMessage struct {
	Service string      `json:"service"`
	Channel string      `json:"channel"`
	User    wireUser    `json:"user"`
	Chunks  []postChunk `json:"chunks"`
	TTS     bool        `json:"tts"`
}

type postChunk struct {
	Type int    `json:"type"`
	Data string `json:"data"`
}

func resourceID(value string) bool {
	id, err := strconv.ParseInt(value, 10, 64)
	return err == nil && id > 0 && strconv.FormatInt(id, 10) == value
}

func decimal(value string) bool {
	if value == "" || value[0] == '0' {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// List returns a complete bounded scan of inbound Twitch observations.
func (c *Client) List(ctx context.Context, from time.Time) ([]window.Line, error) {
	q := url.Values{"from": {from.UTC().Format(time.RFC3339Nano)}, "to": {time.Now().UTC().Format(time.RFC3339Nano)}, "sort": {"time"}, "descending": {"false"}, "count": {"500"}}
	lines := []window.Line{}
	offset := 0
	for {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/v3/messages?"+q.Encode(), nil)
		if err != nil {
			return nil, errors.New("list messages: invalid request")
		}
		body, err := c.do(request)
		if err != nil {
			return nil, fmt.Errorf("list messages: %w", err)
		}
		var result page
		if json.Unmarshal(body, &result) != nil || result.Result == nil || result.Total == nil || *result.Total < 0 || len(result.Continue) == 0 {
			return nil, errors.New("list messages: invalid page")
		}
		rows := *result.Result
		if len(rows) > 500 || offset+len(rows) > *result.Total {
			return nil, errors.New("list messages: inconsistent page")
		}
		for _, message := range rows {
			if !resourceID(message.ID) {
				return nil, errors.New("list messages: invalid resource ID")
			}
			if message.Service != "Twitch" || message.User.Service != "Twitch" || !decimal(message.User.ServiceID) || !decimal(message.Channel) || message.Source == nil || message.Source.Kind != "twitch" || message.Source.Transport != nil && *message.Source.Transport != "irc" || message.Delivery != nil {
				continue
			}
			stamp, ok := parseTime(message.Time)
			text := textOf(message.Chunks)
			if !ok || text == "" {
				c.log.Warn("skipped an unreadable message", "timeReadable", ok, "hasText", text != "")
				continue
			}
			lines = append(lines, window.Line{ID: message.ID, Own: message.User.ServiceID == c.bot, Key: window.Key{Service: message.Service, Channel: message.Channel}, Speaker: message.User.Name, Text: text, Time: stamp})
		}
		if string(result.Continue) == "null" {
			if offset+len(rows) != *result.Total {
				return nil, errors.New("list messages: incomplete page")
			}
			return lines, nil
		}
		var next int
		if json.Unmarshal(result.Continue, &next) != nil || next <= offset || next != offset+len(rows) || next >= *result.Total {
			return nil, errors.New("list messages: invalid continuation")
		}
		offset = next
		q.Set("continue", strconv.Itoa(next))
	}
}

// Post makes one delivery attempt and succeeds only on a matching confirmed sent resource.
func (c *Client) Post(ctx context.Context, key window.Key, text string) error {
	if key.Service != "Twitch" || !decimal(key.Channel) {
		return errors.New("post message: unsupported channel")
	}
	marker := wireUser{Service: "Twitch", ServiceID: BotServiceID, Name: BotServiceID}
	payload := postMessage{Service: key.Service, Channel: key.Channel, User: marker, Chunks: []postChunk{{Type: 0, Data: text}}}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return errors.New("post message: invalid body")
	}
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return errors.New("post message: request key unavailable")
	}
	requestKey := hex.EncodeToString(random)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/v3/messages", bytes.NewReader(encoded))
	if err != nil {
		return errors.New("post message: invalid request")
	}
	request.GetBody = nil
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", requestKey)
	body, err := c.do(request)
	if err != nil {
		return fmt.Errorf("post message requestKey=%s: %w", requestKey, err)
	}
	var result wireMessage
	if json.Unmarshal(body, &result) != nil || !resourceID(result.ID) || result.Service != key.Service || result.Channel != key.Channel || result.User != marker || result.TTS == nil || *result.TTS || len(result.ReplyTo) != 0 && string(result.ReplyTo) != "null" || len(result.Chunks) != 1 || chunkKind(result.Chunks[0].Type) != kindText || result.Chunks[0].Data != text || result.Source != nil || result.Delivery == nil || result.Delivery.RequestKey != requestKey {
		return fmt.Errorf("post message requestKey=%s: delivery unconfirmed", requestKey)
	}
	if result.Delivery.Transport == "irc" && result.Delivery.State == "submitted" && len(result.Delivery.MessageID) == 0 {
		return &UnconfirmedSubmission{ID: result.ID, RequestKey: requestKey}
	}
	var messageID string
	if result.Delivery.Transport != "" || result.Delivery.State != "sent" || json.Unmarshal(result.Delivery.MessageID, &messageID) != nil || messageID == "" {
		return fmt.Errorf("post message requestKey=%s: delivery unconfirmed", requestKey)
	}
	c.log.Info("message delivery confirmed", "id", result.ID, "requestKey", requestKey)
	return nil
}

func (c *Client) do(request *http.Request) ([]byte, error) {
	request.Header.Set("Authorization", "Bearer "+c.key)
	response, err := c.http.Do(request)
	if err != nil {
		return nil, errors.New("message API transport failure")
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, errors.New("message API response read failed")
	}
	if response.StatusCode != http.StatusOK && (request.Method != http.MethodPost || response.StatusCode != http.StatusCreated) {
		hint := ""
		if response.StatusCode == 401 || response.StatusCode == 403 {
			hint = " (check GANGOLF_API_KEY)"
		}
		var detail struct {
			ID   string `json:"id"`
			Code string `json:"code"`
		}
		if json.Unmarshal(body, &detail) == nil {
			if resourceID(detail.ID) {
				hint += " id=" + detail.ID
			}
			switch detail.Code {
			case "delivery_outcome_unknown", "delivery_failed", "delivery_rate_limited", "message_too_large", "delivery_not_supported", "idempotency_conflict", "message_deleted":
				hint += " code=" + detail.Code
			}
		}
		return nil, fmt.Errorf("status %d%s", response.StatusCode, hint)
	}
	return body, nil
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
