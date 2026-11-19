package messages

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/telmengedar/gangolf/ai-core/internal/window"
)

const frameLimit = 8 << 20
const retainedFrames = 64

// AuthorizationError stops synchronization until credentials are repaired.
type AuthorizationError struct{ Status int }

func (e *AuthorizationError) Error() string { return "message API authorization denied" }

// Frame is one ordered live message or producer fence.
type Frame struct {
	ID      string
	Time    time.Time
	Updated bool
	Line    *window.Line
	Fence   string
	bytes   int
}

// Feed owns one bounded generation of the authenticated event stream.
type Feed interface {
	Context() context.Context
	Next(context.Context) (Frame, error)
	TakeAvailable() (Frame, bool, error)
	RequestFence(context.Context, string) error
	Close()
}

type stream struct {
	client   *Client
	ctx      context.Context
	cancel   context.CancelFunc
	frames   chan Frame
	ready    chan string
	finished chan struct{}
	id       string
	mu       sync.Mutex
	bytes    int
	err      error
}

func (c *Client) decode(message wireMessage) (window.Line, bool, error) {
	if !resourceID(message.ID) {
		return window.Line{}, false, errors.New("message API invalid resource identity")
	}
	if message.Service == "" || message.Channel == "" || message.User.Service == "" || message.User.ServiceID == "" || message.Time == "" || message.Chunks == nil {
		return window.Line{}, false, errors.New("message API malformed resource")
	}

	if message.Service != "Twitch" || message.User.Service != "Twitch" || !decimal(message.User.ServiceID) || !decimal(message.Channel) || message.Source == nil || message.Source.Kind != "twitch" || message.Source.Transport != nil && *message.Source.Transport != "irc" || message.Delivery != nil {
		return window.Line{}, false, nil
	}
	stamp, ok := parseTime(message.Time)
	text := textOf(message.Chunks)
	if !ok {
		return window.Line{}, false, nil
	}
	if text == "" {
		return window.Line{}, false, nil
	}
	return window.Line{ID: message.ID, Own: message.User.ServiceID == c.bot, Key: window.Key{Service: message.Service, Channel: message.Channel}, Speaker: message.User.Name, Text: text, Time: stamp}, true, nil
}

func (c *Client) observation(message wireMessage) (Frame, bool, error) {
	line, readable, err := c.decode(message)
	if err != nil {
		return Frame{}, false, err
	}
	stamp, ok := parseTime(message.Time)
	observed := ok && message.Service == "Twitch" && message.User.Service == "Twitch" && decimal(message.User.ServiceID) && decimal(message.Channel) && message.Source != nil && message.Source.Kind == "twitch" && (message.Source.Transport == nil || *message.Source.Transport == "irc") && message.Delivery == nil
	if !observed {
		return Frame{}, false, nil
	}
	frame := Frame{ID: message.ID, Time: stamp}
	if readable {
		frame.Line = &line
	}
	return frame, true, nil
}

// Open subscribes before permitting any finite history reconciliation.
func (c *Client) Open(ctx context.Context) (Feed, error) {
	ctx, cancel := context.WithCancel(ctx)
	s := &stream{client: c, ctx: ctx, cancel: cancel, frames: make(chan Frame, retainedFrames), ready: make(chan string, 1), finished: make(chan struct{})}
	go s.read()
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	select {
	case s.id = <-s.ready:
		return s, nil
	case <-ctx.Done():
		s.Close()
		return nil, s.failure()
	case <-timer.C:
		s.fail(errors.New("message event handshake deadline"))
		s.Close()
		return nil, s.failure()
	}
}
func (s *stream) Context() context.Context { return s.ctx }
func (s *stream) Close() {
	s.cancel()
	<-s.finished
	for {
		select {
		case <-s.frames:
		default:
			s.mu.Lock()
			s.bytes = 0
			s.mu.Unlock()
			return
		}
	}
}
func (s *stream) failure() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	return s.ctx.Err()
}
func (s *stream) fail(err error) {
	s.mu.Lock()
	if s.err == nil {
		s.err = err
	}
	s.mu.Unlock()
	s.cancel()
}
func (s *stream) Next(ctx context.Context) (Frame, error) {
	if s.ctx.Err() != nil {
		return Frame{}, s.failure()
	}
	select {
	case <-ctx.Done():
		return Frame{}, ctx.Err()
	case <-s.ctx.Done():
		return Frame{}, s.failure()
	case frame := <-s.frames:
		s.mu.Lock()
		s.bytes -= frame.bytes
		s.mu.Unlock()
		if s.ctx.Err() != nil {
			return Frame{}, s.failure()
		}
		return frame, nil
	}
}
func (s *stream) enqueue(frame Frame) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.bytes+frame.bytes > frameLimit {
		return errors.New("message event byte capacity exceeded")
	}
	select {
	case s.frames <- frame:
		s.bytes += frame.bytes
		return nil
	default:
		return errors.New("message event count capacity exceeded")
	}
}
func (s *stream) TakeAvailable() (Frame, bool, error) {
	if s.ctx.Err() != nil {
		return Frame{}, false, s.failure()
	}
	select {
	case frame := <-s.frames:
		s.mu.Lock()
		s.bytes -= frame.bytes
		s.mu.Unlock()
		return frame, true, nil
	default:
		return Frame{}, false, nil
	}
}

type streamProgressReader struct {
	io.Reader
	progress func()
}

func (r streamProgressReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if n > 0 {
		r.progress()
	}
	return n, err
}

func eventLine(data []byte, atEOF bool) (advance int, token []byte, err error) {
	for i, b := range data {
		if b == '\n' {
			return i + 1, data[:i+1], nil
		}
		if b == '\r' {
			if i+1 == len(data) && !atEOF {
				return 0, nil, nil
			}
			if i+1 < len(data) && data[i+1] == '\n' {
				return i + 2, data[:i+2], nil
			}
			return i + 1, data[:i+1], nil
		}
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}
func (s *stream) read() {
	defer close(s.finished)
	defer s.cancel()
	request, err := http.NewRequestWithContext(s.ctx, "GET", s.client.base+"/v3/events?types=message.created%2Cmessage.updated&service=Twitch", nil)
	if err != nil {
		s.fail(errors.New("message event invalid request"))
		return
	}
	request.Header.Set("Authorization", "Bearer "+s.client.key)
	request.Header.Set("Accept", "text/event-stream")
	client := *s.client.http
	client.Timeout = 0
	response, err := client.Do(request)
	if err != nil {
		s.fail(errors.New("message event transport unavailable"))
		return
	}
	defer response.Body.Close()
	if response.StatusCode == 401 || response.StatusCode == 403 {
		s.fail(&AuthorizationError{Status: response.StatusCode})
		return
	}
	if response.StatusCode != 200 || !bytes.HasPrefix([]byte(response.Header.Get("Content-Type")), []byte("text/event-stream")) {
		s.fail(errors.New("message event stream unavailable"))
		return
	}
	idle := time.AfterFunc(45*time.Second, func() { s.fail(errors.New("message event idle deadline")) })
	defer idle.Stop()
	scanner := bufio.NewScanner(streamProgressReader{Reader: response.Body, progress: func() { idle.Reset(45 * time.Second) }})
	scanner.Buffer(make([]byte, 4096), frameLimit+1)
	scanner.Split(eventLine)
	event, data := "", []byte(nil)
	size := 0
	ready := false
	for scanner.Scan() {
		raw := scanner.Bytes()
		size += len(raw)
		line := bytes.TrimSuffix(bytes.TrimSuffix(raw, []byte("\n")), []byte("\r"))
		if size > frameLimit || !utf8.Valid(line) {
			s.fail(errors.New("message event invalid frame"))
			return
		}
		if len(line) == 0 {
			if len(data) > 0 {
				if err := s.deliver(event, data, &ready, size); err != nil {
					s.fail(err)
					return
				}
			}
			event = ""
			data = nil
			size = 0
			continue
		}
		if line[0] == ':' {
			continue
		}
		name, value, found := bytes.Cut(line, []byte(":"))
		if !found {
			value = nil
		}
		value = bytes.TrimPrefix(value, []byte(" "))
		switch string(name) {
		case "event":
			event = string(value)
		case "data":
			if data != nil {
				data = append(data, '\n')
			}
			data = append(data, value...)
		}
	}
	if s.ctx.Err() == nil {
		s.fail(errors.New("message event stream ended"))
	}
}
func (s *stream) deliver(event string, data []byte, ready *bool, size int) error {
	if event == "stream.ready" {
		var value struct {
			SchemaVersion  int
			Replay         *bool
			ResyncRequired bool
			StreamID       string
		}
		if *ready || json.Unmarshal(data, &value) != nil || value.SchemaVersion != 1 || value.Replay == nil || *value.Replay || !value.ResyncRequired || !correlation(value.StreamID) {
			return errors.New("message event invalid ready")
		}
		*ready = true
		s.ready <- value.StreamID
		return nil
	}
	if !*ready {
		return errors.New("message event before ready")
	}
	if event == "stream.reset" {
		return errors.New("message event synchronization reset")
	}
	if event == "stream.fence" {
		var value struct {
			SchemaVersion int
			RequestID     string
		}
		if json.Unmarshal(data, &value) != nil || value.SchemaVersion != 1 || !correlation(value.RequestID) {
			return errors.New("message event invalid fence")
		}
		return s.enqueue(Frame{Fence: value.RequestID, bytes: size})
	}
	if event != "message.created" && event != "message.updated" {
		return nil
	}
	var value struct {
		SchemaVersion int
		ID            string
		Type          string
		Data          struct {
			ResourceVersion int
			Message         wireMessage
		}
	}
	if json.Unmarshal(data, &value) != nil || value.SchemaVersion != 1 || value.ID == "" || len(value.ID) > 128 || value.Type != event {
		return errors.New("message event invalid envelope")
	}
	if value.Data.ResourceVersion == 2 {
		return nil
	}
	if value.Data.ResourceVersion != 3 {
		return errors.New("message event invalid resource version")
	}
	frame, observed, err := s.client.observation(value.Data.Message)
	if err != nil {
		return err
	}
	if !observed {
		return nil
	}
	frame.Updated = event == "message.updated"
	frame.bytes = size
	return s.enqueue(frame)
}
func correlation(value string) bool {
	data, err := hex.DecodeString(value)
	return err == nil && len(data) == 16 && hex.EncodeToString(data) == value
}

// NewFenceID generates one opaque correlation for an ordered boundary.
func NewFenceID() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", errors.New("message event correlation unavailable")
	}
	return hex.EncodeToString(random[:]), nil
}
func (s *stream) RequestFence(ctx context.Context, id string) error {
	if !correlation(id) {
		return errors.New("message event invalid fence correlation")
	}
	body, _ := json.Marshal(map[string]string{"streamId": s.id, "requestId": id})
	request, err := http.NewRequestWithContext(ctx, "POST", s.client.base+"/v3/events/fence", bytes.NewReader(body))
	if err != nil {
		return errors.New("message event invalid fence request")
	}
	request.Header.Set("Content-Type", "application/json")
	data, err := s.client.do(request)
	if err != nil {
		return err
	}
	var result struct {
		StreamID  string
		RequestID string
	}
	if json.Unmarshal(data, &result) != nil || result.StreamID != s.id || result.RequestID != id {
		return errors.New("message event invalid fence response")
	}
	return nil
}

// Sync reads one bounded recent-context episode after a stream is ready.
func (c *Client) Sync(ctx context.Context, from time.Time, consume func(Frame) error) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	query := url.Values{"from": {from.UTC().Format(time.RFC3339Nano)}, "to": {time.Now().UTC().Format(time.RFC3339Nano)}, "sort": {"time"}, "descending": {"false"}, "count": {"500"}}
	offset := 0
	seen := make(map[string]bool)
	total := -1
	for pages := 0; pages < 64; pages++ {
		request, err := http.NewRequestWithContext(ctx, "GET", c.base+"/v3/messages?"+query.Encode(), nil)
		if err != nil {
			return errors.New("message sync invalid request")
		}
		body, err := c.do(request)
		if err != nil {
			return err
		}
		var value page
		if json.Unmarshal(body, &value) != nil || value.Result == nil || value.Total == nil || *value.Total < 0 || *value.Total > 32000 || total >= 0 && *value.Total != total {
			return errors.New("message sync inconsistent page")
		}
		total = *value.Total
		rows := *value.Result
		if len(rows) > 500 || offset+len(rows) > total {
			return errors.New("message sync invalid page size")
		}
		for _, message := range rows {
			if !resourceID(message.ID) {
				return errors.New("message sync invalid identity")
			}
			seen[message.ID] = true
			if len(seen) > 32000 {
				return errors.New("message sync identity capacity exceeded")
			}
			frame, observed, err := c.observation(message)
			if err != nil {
				return err
			}
			if observed {
				if err := consume(frame); err != nil {
					return err
				}
			}
		}
		if string(value.Continue) == "null" {
			if offset+len(rows) != total {
				return errors.New("message sync incomplete page")
			}
			return nil
		}
		var next int
		if json.Unmarshal(value.Continue, &next) != nil || next != offset+len(rows) || next <= offset || next >= total {
			return errors.New("message sync invalid continuation")
		}
		offset = next
		query.Set("continue", json.Number(string(value.Continue)).String())
	}
	return errors.New("message sync page capacity exceeded")
}
