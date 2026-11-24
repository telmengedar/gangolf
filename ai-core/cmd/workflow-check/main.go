package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/telmengedar/gangolf/ai-core/internal/core"
	"github.com/telmengedar/gangolf/ai-core/internal/messages"
	"github.com/telmengedar/gangolf/ai-core/internal/processor"
	"github.com/telmengedar/gangolf/ai-core/internal/window"
)

type line struct {
	Speaker string `json:"speaker"`
	Text    string `json:"text"`
	Own     bool   `json:"own"`
}
type request struct {
	BaseURL        string `json:"baseURL"`
	ProcessID      int64  `json:"processId"`
	PersonaID      int64  `json:"personaId"`
	History        []line `json:"history"`
	Messages       []line `json:"messages"`
	TimeoutSeconds int    `json:"timeoutSeconds"`
}
type call struct {
	Input       string            `json:"input"`
	History     []window.Exchange `json:"history"`
	Result      map[string]any    `json:"result"`
	ErrorCode   string            `json:"errorCode,omitempty"`
	ErrorStatus int               `json:"errorStatus,omitempty"`
}
type post struct {
	Service string `json:"service"`
	Channel string `json:"channel"`
	Text    string `json:"text"`
}
type recordingRunner struct {
	client *processor.ProcessClient
	calls  []call
}

func (r *recordingRunner) Run(ctx context.Context, input string, history []window.Exchange) (processor.Result, error) {
	result, err := r.client.Run(ctx, input, history)
	record := call{Input: input, History: append([]window.Exchange{}, history...), Result: map[string]any{"reply": result.Reply, "silent": result.Silent, "chosen": result.Chosen, "failed": result.Failed}}
	var failure *processor.ProcessError
	if errors.As(err, &failure) {
		record.ErrorCode = failure.Code
		record.ErrorStatus = failure.Status
	}
	r.calls = append(r.calls, record)
	return result, err
}

type sink struct {
	ctx       context.Context
	cancel    context.CancelFunc
	history   []messages.Frame
	pending   []messages.Frame
	frames    chan messages.Frame
	fences    int
	completed bool
	posts     []post
}

func (s *sink) Post(_ context.Context, key window.Key, text string) error {
	s.posts = append(s.posts, post{key.Service, key.Channel, text})
	return nil
}
func (s *sink) Open(context.Context) (messages.Feed, error) { return s, nil }
func (s *sink) Sync(_ context.Context, _ time.Time, consume func(messages.Frame) error) error {
	for _, frame := range s.history {
		if err := consume(frame); err != nil {
			return err
		}
	}
	return nil
}
func (s *sink) Context() context.Context { return s.ctx }
func (s *sink) Close()                   {}
func (s *sink) Next(ctx context.Context) (messages.Frame, error) {
	select {
	case frame := <-s.frames:
		return frame, nil
	case <-ctx.Done():
		return messages.Frame{}, ctx.Err()
	}
}
func (s *sink) TakeAvailable() (messages.Frame, bool, error) {
	select {
	case frame := <-s.frames:
		return frame, true, nil
	default:
		return messages.Frame{}, false, nil
	}
}
func (s *sink) RequestFence(ctx context.Context, id string) error {
	s.fences++
	if s.fences == 2 {
		s.completed = true
		s.cancel()
		return nil
	}
	select {
	case s.frames <- messages.Frame{Fence: id}:
	case <-ctx.Done():
		return ctx.Err()
	}
	for _, frame := range s.pending {
		select {
		case s.frames <- frame:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func execute(input io.Reader, output io.Writer) error {
	var req request
	decoder := json.NewDecoder(input)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		return fmt.Errorf("invalid harness request")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return fmt.Errorf("invalid trailing harness input")
	}
	endpoint, err := url.Parse(req.BaseURL)
	if err != nil || endpoint.Host == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || req.ProcessID <= 0 || req.PersonaID <= 0 || req.TimeoutSeconds < 1 || req.TimeoutSeconds > 180 || len(req.Messages) == 0 || len(req.Messages) > 10 || len(req.History) > 10 {
		return fmt.Errorf("invalid harness binding or bound")
	}
	for _, row := range append(append([]line{}, req.History...), req.Messages...) {
		if strings.TrimSpace(row.Speaker) == "" || strings.TrimSpace(row.Text) == "" {
			return fmt.Errorf("invalid harness line")
		}
	}
	for _, row := range req.Messages {
		if row.Own {
			return fmt.Errorf("trigger cannot be an own echo")
		}
	}
	deadline, stop := context.WithTimeout(context.Background(), time.Duration(req.TimeoutSeconds)*time.Second)
	defer stop()
	ctx, cancel := context.WithCancel(deadline)
	defer cancel()
	s := &sink{ctx: ctx, cancel: cancel, frames: make(chan messages.Frame, 16), posts: []post{}}
	key := window.Key{Service: "private", Channel: "workflow-check"}
	now := time.Now()
	makeFrames := func(rows []line, offset int) []messages.Frame {
		frames := make([]messages.Frame, len(rows))
		for i, row := range rows {
			at := now.Add(time.Duration(offset+i) * time.Millisecond)
			id := fmt.Sprint(offset + i + 100)
			item := window.Line{ID: id, Key: key, Own: row.Own, Speaker: row.Speaker, Text: row.Text, Time: at}
			frames[i] = messages.Frame{ID: id, Time: at, Line: &item}
		}
		return frames
	}
	s.history = makeFrames(req.History, -50)
	s.pending = makeFrames(req.Messages, 0)
	capture := &captureTransport{base: http.DefaultTransport, exchanges: []processExchange{}}
	runner := &recordingRunner{client: processor.NewProcess(req.BaseURL, req.ProcessID, req.PersonaID, &http.Client{Transport: capture, Timeout: processor.RunTimeout}), calls: []call{}}
	var logs strings.Builder
	core.New(core.Config{Routes: map[window.Key][]window.Key{key: {key}}, BotName: "GangolfBot"}, s, runner, slog.New(slog.NewTextHandler(&logs, nil)), time.Now).Run(ctx)
	return json.NewEncoder(output).Encode(struct {
		Completed        bool              `json:"completed"`
		Calls            []call            `json:"calls"`
		Posts            []post            `json:"posts"`
		Logs             string            `json:"logs"`
		ProcessResponses []processExchange `json:"processResponses"`
	}{s.completed && deadline.Err() == nil, runner.calls, s.posts, logs.String(), capture.exchanges})
}

func main() {
	if err := execute(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
