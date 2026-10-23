package core_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/telmengedar/gangolf/ai-core/internal/core"
	"github.com/telmengedar/gangolf/ai-core/internal/processor"
	"github.com/telmengedar/gangolf/ai-core/internal/window"
)

type reviewCall struct {
	input    string
	history  []window.Exchange
	reply    string
	deadline time.Time
}
type fakeReviewer struct {
	calls  chan reviewCall
	review func(context.Context) (processor.MemoryResult, error)
}

func (f *fakeReviewer) Review(ctx context.Context, input string, history []window.Exchange, reply string) (processor.MemoryResult, error) {
	deadline, _ := ctx.Deadline()
	f.calls <- reviewCall{input, history, reply, deadline}
	if f.review != nil {
		return f.review(ctx)
	}
	return processor.MemoryResult{State: "abstained", ModelCalls: 1}, nil
}

type memoryLog struct {
	mu   sync.Mutex
	text strings.Builder
}

func (l *memoryLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.text.Write(p)
}
func (l *memoryLog) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.text.String() }

func receiveReview(t *testing.T, calls <-chan reviewCall) reviewCall {
	t.Helper()
	select {
	case call := <-calls:
		return call
	case <-time.After(time.Second):
		t.Fatal("review not dispatched")
		return reviewCall{}
	}
}

func TestMemoryReviewUsesOriginalInputHistoryAndFinalSanitizedRetryOnly(t *testing.T) {
	r := newRig(t)
	reviewer := &fakeReviewer{calls: make(chan reviewCall, 4)}
	c := core.New(core.Config{Routes: selfRoutes(channel), BotName: "GangolfBot", Interval: time.Nanosecond, MemoryReviewer: reviewer}, r.messages, r.runner, slog.New(slog.NewTextHandler(io.Discard, nil)), func() time.Time { return r.clock })
	r.messages.add(channel, "Mara", "earlier input", start.Add(-time.Minute))
	r.messages.add(channel, "GangolfBot", "old answer", start.Add(-time.Minute+time.Second))
	r.runner.results = []processor.Result{{Reply: "old answer"}, {Reply: " !! Gangolf: final accepted answer "}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var call reviewCall
	r.messages.onList = func(n int) {
		switch n {
		case 2:
			r.messages.add(channel, "Mara", "new input", r.clock)
		case 3:
			r.advance(3 * time.Second)
		case 4:
			call = receiveReview(t, reviewer.calls)
			cancel()
		}
	}
	finishes(t, "core did not stop", func() { c.Run(ctx) })
	if call.input != "Mara: new input" || call.reply != "final accepted answer" || len(r.runner.calls) != 2 {
		t.Fatalf("review=%+v chatruns=%d", call, len(r.runner.calls))
	}
	if !reflect.DeepEqual(call.history, r.runner.calls[0].history) {
		t.Fatalf("review history=%+v, chat=%+v", call.history, r.runner.calls[0].history)
	}
	if len(r.messages.posts) != 1 || r.messages.posts[0].text != call.reply {
		t.Fatalf("posts=%+v", r.messages.posts)
	}
	if time.Until(call.deadline) > processor.MemoryReviewTimeout || time.Until(call.deadline) < processor.MemoryReviewTimeout-time.Second {
		t.Fatalf("review deadline=%v", call.deadline)
	}
	select {
	case extra := <-reviewer.calls:
		t.Fatalf("unexpected extra review=%+v", extra)
	default:
	}
}

func TestUndeliveredTurnsNeverQueueMemoryReview(t *testing.T) {
	cases := []struct {
		name            string
		result          processor.Result
		runErr, postErr error
		input           string
		minimum         int
		retry           bool
	}{
		{name: "silent", result: processor.Result{Silent: true, Chosen: true}},
		{name: "withheld", result: processor.Result{Silent: true, Failed: true, Reply: "fallback"}},
		{name: "empty", result: processor.Result{Reply: " !! Gangolf: "}},
		{name: "run failure", runErr: errors.New("failed")},
		{name: "post failure", result: processor.Result{Reply: "a reply"}, postErr: errors.New("failed")},
		{name: "repeat withheld", result: processor.Result{Reply: "old answer"}, retry: true},
		{name: "too short", input: "hi", minimum: 4, result: processor.Result{Reply: "a reply"}},
		{name: "addressed elsewhere", input: "Mara, how are you?", result: processor.Result{Reply: "a reply"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			reviewer := &fakeReviewer{calls: make(chan reviewCall, 4)}
			r.runner.result = tc.result
			r.runner.err = tc.runErr
			r.messages.postErr = tc.postErr
			r.messages.add(channel, "Mara", "old question", start.Add(-time.Minute))
			r.messages.add(channel, "GangolfBot", "old answer", start.Add(-time.Minute+time.Second))
			input := tc.input
			if input == "" {
				input = "hello Gangolf"
			}
			c := core.New(core.Config{Routes: selfRoutes(channel), BotName: "GangolfBot", Interval: time.Nanosecond, MinWords: map[window.Key]int{channel: tc.minimum}, MemoryReviewer: reviewer}, r.messages, r.runner, slog.New(slog.NewTextHandler(io.Discard, nil)), func() time.Time { return r.clock })
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r.messages.onList = func(n int) {
				switch n {
				case 2:
					r.messages.add(channel, "Alex", input, r.clock)
				case 3:
					r.advance(3 * time.Second)
				case 4:
					cancel()
				}
			}
			finishes(t, "core did not stop", func() { c.Run(ctx) })
			select {
			case got := <-reviewer.calls:
				t.Fatalf("undelivered turn reviewed=%+v", got)
			default:
			}
		})
	}
}

func TestBlockedMemoryReviewPreservesPollingAndBoundsQueueAndShutdown(t *testing.T) {
	r := newRig(t)
	reviewer := &fakeReviewer{calls: make(chan reviewCall, 4), review: func(ctx context.Context) (processor.MemoryResult, error) {
		<-ctx.Done()
		return processor.MemoryResult{}, ctx.Err()
	}}
	logs := &memoryLog{}
	c := core.New(core.Config{Routes: selfRoutes(channel), BotName: "GangolfBot", Interval: time.Nanosecond, MemoryReviewer: reviewer}, r.messages, r.runner, slog.New(slog.NewTextHandler(logs, nil)), func() time.Time { return r.clock })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var first reviewCall
	r.messages.onList = func(n int) {
		r.advance(3 * time.Second)
		switch n {
		case 2:
			r.messages.add(channel, "Mara", "first turn", r.clock)
		case 4:
			first = receiveReview(t, reviewer.calls)
		case 5:
			r.messages.add(channel, "Mara", "second turn", r.clock)
		case 8:
			r.messages.add(channel, "Mara", "third turn", r.clock)
		case 10:
			cancel()
		}
	}
	finishes(t, "blocked memory stopped polling or shutdown", func() { c.Run(ctx) })
	if len(r.messages.froms) < 10 || len(r.messages.posts) != 3 || first.input != "Mara: first turn" {
		t.Fatalf("polls=%d posts=%+v first=%+v", len(r.messages.froms), r.messages.posts, first)
	}
	select {
	case got := <-reviewer.calls:
		t.Fatalf("queued review dispatched after cancellation: %+v", got)
	default:
	}
	if strings.Count(logs.String(), "outcome=queued") != 2 || strings.Count(logs.String(), "outcome=busy") != 1 || !strings.Contains(logs.String(), "outcome=unknown") {
		t.Fatalf("logs=%s", logs.String())
	}
	if strings.Contains(logs.String(), "first turn") || strings.Contains(logs.String(), "an answer") {
		t.Fatalf("logs contain conversation text: %s", logs.String())
	}
}

func TestMemoryReviewFailureIsNotRetriedOrLoggedAsRawText(t *testing.T) {
	r := newRig(t)
	reviewer := &fakeReviewer{calls: make(chan reviewCall, 4), review: func(context.Context) (processor.MemoryResult, error) {
		return processor.MemoryResult{}, errors.New("private raw response and secret")
	}}
	logs := &memoryLog{}
	c := core.New(core.Config{Routes: selfRoutes(channel), BotName: "GangolfBot", Interval: time.Nanosecond, MemoryReviewer: reviewer}, r.messages, r.runner, slog.New(slog.NewTextHandler(logs, nil)), func() time.Time { return r.clock })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.messages.onList = func(n int) {
		switch n {
		case 2:
			r.messages.add(channel, "Mara", "hello", r.clock)
		case 3:
			r.advance(3 * time.Second)
		case 4:
			receiveReview(t, reviewer.calls)
		case 6:
			cancel()
		}
	}
	finishes(t, "core did not stop", func() { c.Run(ctx) })
	if !strings.Contains(logs.String(), "outcome=unknown") || strings.Contains(logs.String(), "secret") || strings.Contains(logs.String(), "private raw") {
		t.Fatalf("logs=%s", logs.String())
	}
	select {
	case got := <-reviewer.calls:
		t.Fatalf("failed review retried: %+v", got)
	default:
	}
}

func TestQueuedReviewFreezesHistoryBeforeTheWorkerCanDispatchIt(t *testing.T) {
	r := newRig(t)
	release := make(chan struct{})
	reviewer := &fakeReviewer{calls: make(chan reviewCall, 4), review: func(ctx context.Context) (processor.MemoryResult, error) {
		select {
		case <-release:
			return processor.MemoryResult{State: "abstained", ModelCalls: 1}, nil
		case <-ctx.Done():
			return processor.MemoryResult{}, ctx.Err()
		}
	}}
	c := core.New(core.Config{Routes: selfRoutes(channel), BotName: "GangolfBot", Interval: time.Nanosecond, MemoryReviewer: reviewer}, r.messages, r.runner, slog.New(slog.NewTextHandler(io.Discard, nil)), func() time.Time { return r.clock })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var second reviewCall
	var original string
	r.messages.onList = func(n int) {
		r.advance(3 * time.Second)
		switch n {
		case 2:
			r.messages.add(channel, "Mara", "first input", r.clock)
		case 4:
			receiveReview(t, reviewer.calls)
		case 5:
			r.messages.add(channel, "Mara", "second input", r.clock)
		case 7:
			if len(r.runner.calls[1].history) == 0 {
				t.Fatal("second turn has no history")
			}
			original = r.runner.calls[1].history[0].Input
			r.runner.calls[1].history[0].Input = "mutated after enqueue"
			close(release)
			second = receiveReview(t, reviewer.calls)
			cancel()
		}
	}
	finishes(t, "core did not stop", func() { c.Run(ctx) })
	if len(second.history) == 0 || second.history[0].Input != original {
		t.Fatalf("review history=%+v want original=%q", second.history, original)
	}
}

func TestCancellationAfterPublicPostPreventsReviewDispatch(t *testing.T) {
	r := newRig(t)
	reviewer := &fakeReviewer{calls: make(chan reviewCall, 4)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.messages.onPost = cancel
	c := core.New(core.Config{Routes: selfRoutes(channel), BotName: "GangolfBot", Interval: time.Nanosecond, MemoryReviewer: reviewer}, r.messages, r.runner, slog.New(slog.NewTextHandler(io.Discard, nil)), func() time.Time { return r.clock })
	r.messages.onList = func(n int) {
		switch n {
		case 2:
			r.messages.add(channel, "Mara", "hello", r.clock)
		case 3:
			r.advance(3 * time.Second)
		}
	}
	finishes(t, "core did not stop", func() { c.Run(ctx) })
	if len(r.messages.posts) != 1 {
		t.Fatalf("posts=%+v", r.messages.posts)
	}
	select {
	case got := <-reviewer.calls:
		t.Fatalf("review dispatched after cancellation: %+v", got)
	default:
	}
}
