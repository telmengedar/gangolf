package core_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/telmengedar/gangolf/ai-core/internal/processor"
	"github.com/telmengedar/gangolf/ai-core/internal/window"
)

const (
	firstLine  = "hey gangolf"
	inRunLine  = "what about the pinkertons"
	afterLine  = "gangolf, you awake"
	echoMargin = time.Millisecond
)

func (r *rig) writeLater(key window.Key, speaker, text string) {
	r.advance(echoMargin)
	r.messages.add(key, speaker, text, r.clock)
	r.advance(echoMargin)
}

func (r *rig) duringRun(key window.Key, speaker, text string) {
	r.runner.during = func() {
		r.advance(3 * time.Second)
		r.writeLater(key, speaker, text)
	}
}

func (r *rig) armedWithFirstLine(t *testing.T) {
	t.Helper()
	r.bootstrap(t)
	r.advance(time.Second)
	r.messages.add(channel, "rugenforth", firstLine, r.clock)
	r.tick()
}

func (r *rig) runTurnThenIdle() {
	r.settle()
	r.advance(pollInterval)
	r.tick()
	r.settle()
	r.settle()
}

func TestALineWrittenDuringARunStartsNoFollowUpRun(t *testing.T) {
	r := newRig(t)
	r.armedWithFirstLine(t)
	r.duringRun(channel, "dapper", inRunLine)

	r.runTurnThenIdle()

	if len(r.runner.calls) != 1 {
		t.Fatalf("runs = %d, want the line written during the run to start nothing", len(r.runner.calls))
	}
	if len(r.messages.posts) != 1 {
		t.Fatalf("posts = %d, want the one reply", len(r.messages.posts))
	}
}

func TestTheNextRunStartsOnALineWrittenAfterTheRunAndKeepsTheInRunLineAsHistory(t *testing.T) {
	r := newRig(t)
	r.armedWithFirstLine(t)
	r.duringRun(channel, "dapper", inRunLine)
	r.runTurnThenIdle()

	r.writeLater(channel, "vesper", afterLine)
	r.tick()
	r.settle()

	if len(r.runner.calls) != 2 {
		t.Fatalf("runs = %d, want the later line to start a second run", len(r.runner.calls))
	}
	second := r.runner.calls[1]
	if second.input != "vesper: "+afterLine {
		t.Fatalf("second input = %q, want only the line written after the run", second.input)
	}
	kept := false
	for _, exchange := range second.history {
		kept = kept || strings.Contains(exchange.Input, inRunLine)
	}
	if !kept {
		t.Fatalf("second history = %+v, want the line written during the run kept as context", second.history)
	}
}

func TestALineWrittenRightAfterTheRunStillStartsTheNextRun(t *testing.T) {
	r := newRig(t)
	r.armedWithFirstLine(t)
	r.settle()

	r.writeLater(channel, "vesper", afterLine)
	r.tick()
	r.settle()

	if len(r.runner.calls) != 2 {
		t.Fatalf("runs = %d, want the line written right after the run answered", len(r.runner.calls))
	}
	if got := r.runner.calls[1].input; got != "vesper: "+afterLine {
		t.Fatalf("second input = %q", got)
	}
}

func TestAFailedRunStartsNoFollowUpRunForLinesWrittenDuringIt(t *testing.T) {
	r := newRig(t)
	r.runner.err = errors.New("model_unavailable")
	r.armedWithFirstLine(t)
	r.duringRun(channel, "dapper", inRunLine)

	r.runTurnThenIdle()

	if len(r.runner.calls) != 1 {
		t.Fatalf("runs = %d, want no follow-up after a failed run", len(r.runner.calls))
	}
}

func TestASilentRunStartsNoFollowUpRunForLinesWrittenDuringIt(t *testing.T) {
	r := newRig(t)
	r.runner.result = processor.Result{Silent: true, Chosen: true}
	r.armedWithFirstLine(t)
	r.duringRun(channel, "dapper", inRunLine)

	r.runTurnThenIdle()

	if len(r.runner.calls) != 1 {
		t.Fatalf("runs = %d, want no follow-up after a silent run", len(r.runner.calls))
	}
	if len(r.messages.posts) != 0 {
		t.Fatalf("posts = %+v, want nothing posted", r.messages.posts)
	}
}

func TestALineOnAnotherChannelWrittenDuringARunIsStillAnswered(t *testing.T) {
	other := window.Key{Service: "discord", Channel: "general"}
	r := newRig(t, channel, other)
	r.armedWithFirstLine(t)
	r.duringRun(other, "dapper", inRunLine)

	r.runTurnThenIdle()

	if len(r.runner.calls) != 2 {
		t.Fatalf("runs = %d, want the other channel answered", len(r.runner.calls))
	}
	if got := r.runner.calls[1].input; got != "dapper: "+inRunLine {
		t.Fatalf("second input = %q, want the other channel's line", got)
	}
	if last := r.messages.posts[len(r.messages.posts)-1]; last.key != other {
		t.Fatalf("last post went to %+v, want the other channel", last.key)
	}
}

func TestLinesWrittenDuringARunStayContextWhenTheClosingPollFails(t *testing.T) {
	r := newRig(t)
	r.armedWithFirstLine(t)
	r.runner.during = func() {
		r.advance(3 * time.Second)
		r.writeLater(channel, "dapper", inRunLine)
		r.messages.listErrs = 1
	}

	r.runTurnThenIdle()

	if len(r.runner.calls) != 1 {
		t.Fatalf("runs = %d, want the line kept as context after a failed closing poll", len(r.runner.calls))
	}
	if !strings.Contains(r.logs.String(), "poll failed") {
		t.Fatalf("logs = %s, want the failed poll logged", r.logs.String())
	}
}

func TestALineWrittenWhileTheReplyIsPostedStartsNoFollowUpRun(t *testing.T) {
	r := newRig(t)
	r.armedWithFirstLine(t)
	r.messages.onPost = func() {
		r.writeLater(channel, "dapper", inRunLine)
		r.messages.onPost = nil
	}

	r.runTurnThenIdle()

	if len(r.runner.calls) != 1 {
		t.Fatalf("runs = %d, want the line written during the post to start nothing", len(r.runner.calls))
	}
}

func TestLinesKeptAsContextAfterARunAreCountedInTheLogs(t *testing.T) {
	r := newRig(t)
	r.armedWithFirstLine(t)
	r.duringRun(channel, "dapper", inRunLine)

	r.runTurnThenIdle()

	found := false
	for _, line := range strings.Split(r.logs.String(), "\n") {
		if strings.Contains(line, "context") && strings.Contains(line, "lines=1") && strings.Contains(line, "channel=stream") {
			found = true
		}
	}
	if !found {
		t.Fatalf("logs = %s, want one line naming the channel and a count of one", r.logs.String())
	}
	if strings.Contains(r.logs.String(), inRunLine) {
		t.Fatalf("logs = %s, want no message text", r.logs.String())
	}
}
