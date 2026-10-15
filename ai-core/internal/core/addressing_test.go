package core_test

import (
	"strings"
	"testing"
	"time"
)

const (
	toVesperLine   = "vesper what time do you start tomorrow?"
	toGangolfLine  = "hey gangolf"
	namedBothLine  = "vesper what would gangolf say"
	quietChatter   = "vesper"
	quietChatLine  = "i will be back later"
	greetQuietLine = "hi vesper"
)

func (r *rig) turns() {
	r.tick()
	r.settle()
	r.advance(pollInterval)
	r.tick()
}

func (r *rig) vesperWasHereBefore(t *testing.T) {
	t.Helper()
	r.messages.add(channel, quietChatter, "an old question", start.Add(-time.Minute))
	r.bootstrap(t)
	r.advance(time.Second)
}

func TestABatchWrittenEntirelyToOtherChattersStartsNoRun(t *testing.T) {
	r := newRig(t)
	r.vesperWasHereBefore(t)
	r.messages.add(channel, "dapper", toVesperLine, r.clock)

	r.turns()

	if len(r.runner.calls) != 0 || len(r.messages.posts) != 0 {
		t.Fatalf("runs = %d, posts = %d, want neither for a batch written to another chatter", len(r.runner.calls), len(r.messages.posts))
	}
}

func TestAMixedBatchRunsWithEveryLine(t *testing.T) {
	r := newRig(t)
	r.vesperWasHereBefore(t)
	r.messages.add(channel, "dapper", toVesperLine, r.clock)
	r.messages.add(channel, "rugenforth", toGangolfLine, r.clock)

	r.turns()

	if len(r.runner.calls) != 1 {
		t.Fatalf("runs = %d, want one for the mixed batch", len(r.runner.calls))
	}
	if got, want := r.runner.calls[0].input, "dapper: "+toVesperLine+"\nrugenforth: "+toGangolfLine; got != want {
		t.Fatalf("input = %q, want %q", got, want)
	}
}

func TestTheChannelNameCountsAsAChatter(t *testing.T) {
	r := newRig(t)
	r.bootstrap(t)
	r.advance(time.Second)
	r.messages.add(channel, "vesper", "hi "+channel.Channel, r.clock)

	r.turns()

	if len(r.runner.calls) != 0 {
		t.Fatalf("runs = %d, want none for a greeting to the channel's streamer", len(r.runner.calls))
	}
}

func TestAChattersNameIsRememberedAfterTheirLinesLeaveTheWindow(t *testing.T) {
	r := newRig(t)
	r.bootstrap(t)
	r.advance(time.Second)
	r.messages.add(channel, quietChatter, quietChatLine, r.clock)
	r.turns()
	r.advance(sevenMinutes + time.Minute)
	r.tick()
	r.messages.add(channel, "dapper", greetQuietLine, r.clock)

	r.turns()

	if len(r.runner.calls) != 1 {
		t.Fatalf("runs = %d, want only the first line's run and none for the greeting to the quiet chatter", len(r.runner.calls))
	}
}

func TestALineNamingGangolfAfterAnotherChattersNameStillRuns(t *testing.T) {
	r := newRig(t)
	r.vesperWasHereBefore(t)
	r.messages.add(channel, "dapper", namedBothLine, r.clock)

	r.turns()

	if len(r.runner.calls) != 1 {
		t.Fatalf("runs = %d, want the line that names Gangolf answered", len(r.runner.calls))
	}
}

func TestASkippedTurnIsLoggedAsAddressedElsewhere(t *testing.T) {
	r := newRig(t)
	r.vesperWasHereBefore(t)
	r.messages.add(channel, "dapper", toVesperLine, r.clock)
	r.messages.add(channel, "dapper", "@vesper are you there", r.clock)

	r.turns()

	var turn string
	for _, line := range strings.Split(r.logs.String(), "\n") {
		if strings.Contains(line, "msg=turn") {
			turn = line
		}
	}
	for _, want := range []string{"outcome=addressed-elsewhere", "pending=2", "tookMs=0", "replyBytes=0", "silent=true", "channel=stream"} {
		if !strings.Contains(turn, want) {
			t.Fatalf("turn log line %q lacks %q", turn, want)
		}
	}
}

func (r *rig) afterAGatedLine(t *testing.T) {
	t.Helper()
	r.vesperWasHereBefore(t)
	r.messages.add(channel, "dapper", toVesperLine, r.clock)
	r.turns()
	r.writeLater(channel, "rugenforth", toGangolfLine)
	r.turns()
}

func TestLinesWrittenToOthersAppearInTheNextTurnsHistory(t *testing.T) {
	r := newRig(t)
	r.afterAGatedLine(t)

	if len(r.runner.calls) != 1 {
		t.Fatalf("runs = %d, want one", len(r.runner.calls))
	}
	want := "vesper: an old question\ndapper: " + toVesperLine
	if got := r.runner.calls[0].history; len(got) != 1 || got[0].Input != want {
		t.Fatalf("history = %+v, want the skipped line as context after the old question", got)
	}
}

func TestALineIngestedRightAfterASkippedTurnIsAnswered(t *testing.T) {
	r := newRig(t)
	r.vesperWasHereBefore(t)
	r.messages.add(channel, "dapper", toVesperLine, r.clock)
	r.tick()
	listsBeforeSkip := len(r.messages.froms)
	r.messages.onList = func(n int) {
		if n == listsBeforeSkip+2 {
			r.messages.add(channel, "rugenforth", toGangolfLine, r.clock)
		}
	}

	r.settle()
	r.advance(pollInterval)
	r.tick()
	r.settle()

	if len(r.runner.calls) != 1 || r.runner.calls[0].input != "rugenforth: "+toGangolfLine {
		t.Fatalf("calls = %+v, want one run on the line that appeared right after the skipped turn", r.runner.calls)
	}
	if strings.Contains(r.logs.String(), "kept lines written during the run") {
		t.Fatalf("log %q reports in-run lines although no run happened", r.logs.String())
	}
}

func TestALineAddressedToGangolfAfterAGatedLineIsAnswered(t *testing.T) {
	r := newRig(t)
	r.afterAGatedLine(t)

	if len(r.runner.calls) != 1 || r.runner.calls[0].input != "rugenforth: "+toGangolfLine {
		t.Fatalf("calls = %+v, want one run on the addressed line alone", r.runner.calls)
	}
	if len(r.messages.posts) != 1 {
		t.Fatalf("posts = %d, want the reply", len(r.messages.posts))
	}
}
