package core_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/telmengedar/gangolf/ai-core/internal/core"
	"github.com/telmengedar/gangolf/ai-core/internal/processor"
	"github.com/telmengedar/gangolf/ai-core/internal/window"
)

type blockingRunner struct{ entered func() }

func (b *blockingRunner) Run(ctx context.Context, _ string, _ []window.Exchange) (processor.Result, error) {
	b.entered()
	<-ctx.Done()
	return processor.Result{}, ctx.Err()
}

func finishes(t *testing.T, what string, run func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		run()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s", what)
	}
}

func TestUnlistedChannelIsNamedOnceInTheLogs(t *testing.T) {
	r := newRig(t)
	r.bootstrap(t)
	r.advance(time.Second)
	r.messages.add(window.Key{Service: "Twitch", Channel: "Stream"}, "vesper", "wrong case", r.clock)

	r.tick()
	r.advance(pollInterval)
	r.tick()

	logs := r.logs.String()
	if strings.Count(logs, "not in GANGOLF_CHANNELS") != 1 || !strings.Contains(logs, "service=Twitch") || !strings.Contains(logs, "channel=Stream") {
		t.Fatalf("logs = %s, want the unlisted channel named exactly once", logs)
	}
	if len(r.runner.calls) != 0 {
		t.Fatalf("runs = %d, want an unlisted channel never answered", len(r.runner.calls))
	}
}

func TestListedChannelIsNeverReportedAsUnlisted(t *testing.T) {
	r := newRig(t)
	r.bootstrap(t)
	r.advance(time.Second)
	r.messages.add(channel, "vesper", "hello", r.clock)

	r.tick()

	if strings.Contains(r.logs.String(), "not in GANGOLF_CHANNELS") {
		t.Fatalf("logs = %s, want no warning for a listed channel", r.logs.String())
	}
}

func TestRunFailureLogsTheDroppedSurplus(t *testing.T) {
	r := newRig(t)
	r.runner.err = errors.New("model_unavailable")
	r.bootstrap(t)
	r.advance(time.Second)
	for i := 0; i < 12; i++ {
		r.messages.add(channel, "vesper", "flood", r.clock.Add(time.Duration(i)*time.Millisecond))
	}
	r.tick()

	r.settle()

	if !strings.Contains(r.logs.String(), "run failed") || !strings.Contains(r.logs.String(), "dropped=2") {
		t.Fatalf("logs = %s, want the failed run to report the two dropped lines", r.logs.String())
	}
}

func TestShutdownDuringAFailingBootstrapStopsTheLoop(t *testing.T) {
	r := newRig(t)
	r.messages.listErr = errors.New("api down")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.messages.onList = func(n int) {
		if n == 3 {
			cancel()
		}
	}

	finishes(t, "run kept retrying the bootstrap after its context ended", func() { r.core.Run(ctx) })

	if len(r.messages.froms) != 3 {
		t.Fatalf("polls = %d, want the loop to stop right after the cancelled attempt", len(r.messages.froms))
	}
}

func TestShutdownDuringARunStopsTheLoop(t *testing.T) {
	r := newRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	blocker := &blockingRunner{entered: cancel}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := core.New(core.Config{Channels: []window.Key{channel}, BotName: "GangolfBot", Interval: time.Millisecond}, r.messages, blocker, log, func() time.Time { return r.clock })
	r.messages.onList = func(n int) {
		switch n {
		case 2:
			r.messages.add(channel, "vesper", "hello", r.clock)
		case 3:
			r.advance(3 * time.Second)
		}
	}

	finishes(t, "run kept waiting on the model after its context ended", func() { c.Run(ctx) })

	if len(r.messages.posts) != 0 {
		t.Fatalf("posts = %+v, want nothing posted for an interrupted run", r.messages.posts)
	}
}

func TestPollingWithoutAConfiguredIntervalHappensEveryTwoSeconds(t *testing.T) {
	r := newRig(t)
	c := core.New(core.Config{Channels: []window.Key{channel}, BotName: "GangolfBot"}, r.messages, r.runner, slog.New(slog.NewTextHandler(io.Discard, nil)), func() time.Time { return r.clock })
	ctx, cancel := context.WithTimeout(context.Background(), 2600*time.Millisecond)
	defer cancel()

	c.Run(ctx)

	if len(r.messages.froms) != 2 {
		t.Fatalf("polls in 2.6 s = %d, want the bootstrap and exactly one tick", len(r.messages.froms))
	}
}
