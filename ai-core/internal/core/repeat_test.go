package core_test

import (
	"strings"
	"testing"
	"time"

	"github.com/telmengedar/gangolf/ai-core/internal/processor"
)

const (
	earlierQuestion = "what is going on"
	earlierReply    = "what's the deal with that again?"
)

func (r *rig) hadAnExchange(t *testing.T, input, reply string) {
	t.Helper()
	r.messages.add(channel, "vesper", input, start.Add(-2*time.Minute))
	r.messages.add(channel, "GangolfBot", reply, start.Add(-time.Minute))
	r.bootstrap(t)
	r.advance(time.Second)
}

func (r *rig) turnLine(t *testing.T) string {
	t.Helper()
	var turn string
	for _, line := range strings.Split(r.logs.String(), "\n") {
		if strings.Contains(line, "msg=turn") {
			turn = line
		}
	}
	if turn == "" {
		t.Fatalf("logs = %s, want a turn line", r.logs.String())
	}
	return turn
}

func TestAReplyIdenticalToAnEarlierReplyInHistoryIsNotPostedAndLogsRepeat(t *testing.T) {
	r := newRig(t)
	r.hadAnExchange(t, earlierQuestion, earlierReply)
	r.runner.result = processor.Result{Reply: earlierReply}
	r.runner.during = func() { r.advance(1500 * time.Millisecond) }
	r.messages.add(channel, "rugenforth", toGangolfLine, r.clock)

	r.turns()

	if len(r.runner.calls) != 1 {
		t.Fatalf("runs = %d, want the run to happen", len(r.runner.calls))
	}
	if len(r.messages.posts) != 0 {
		t.Fatalf("posts = %+v, want the repeated reply withheld", r.messages.posts)
	}
	turn := r.turnLine(t)
	for _, want := range []string{"outcome=repeat", "silent=true", "tookMs=1500", "replyBytes=32"} {
		if !strings.Contains(turn, want) {
			t.Fatalf("turn log line %q lacks %q", turn, want)
		}
	}
}

func TestARepeatIgnoresCasing(t *testing.T) {
	r := newRig(t)
	r.hadAnExchange(t, earlierQuestion, earlierReply)
	r.runner.result = processor.Result{Reply: "WHAT'S the Deal With THAT again?"}
	r.messages.add(channel, "rugenforth", toGangolfLine, r.clock)

	r.turns()

	if len(r.messages.posts) != 0 {
		t.Fatalf("posts = %+v, want the differently cased repeat withheld", r.messages.posts)
	}
	if turn := r.turnLine(t); !strings.Contains(turn, "outcome=repeat") {
		t.Fatalf("turn log line %q, want outcome=repeat", turn)
	}
}

func TestARepeatIgnoresSurroundingWhitespace(t *testing.T) {
	r := newRig(t)
	r.hadAnExchange(t, earlierQuestion, "  "+earlierReply+" \t")
	r.runner.result = processor.Result{Reply: earlierReply + "  "}
	r.messages.add(channel, "rugenforth", toGangolfLine, r.clock)

	r.turns()

	if len(r.messages.posts) != 0 {
		t.Fatalf("posts = %+v, want the repeat withheld despite surrounding whitespace", r.messages.posts)
	}
	if turn := r.turnLine(t); !strings.Contains(turn, "outcome=repeat") {
		t.Fatalf("turn log line %q, want outcome=repeat", turn)
	}
}

func TestAReplyEqualToOneOfSeveralConsecutiveEarlierRepliesIsARepeat(t *testing.T) {
	r := newRig(t)
	r.messages.add(channel, "vesper", earlierQuestion, start.Add(-3*time.Minute))
	r.messages.add(channel, "GangolfBot", "first thing", start.Add(-2*time.Minute))
	r.messages.add(channel, "GangolfBot", earlierReply, start.Add(-time.Minute))
	r.bootstrap(t)
	r.advance(time.Second)
	r.runner.result = processor.Result{Reply: earlierReply}
	r.messages.add(channel, "rugenforth", toGangolfLine, r.clock)

	r.turns()

	if len(r.messages.posts) != 0 {
		t.Fatalf("posts = %+v, want the repeat of the second consecutive reply withheld", r.messages.posts)
	}
}

func TestADifferentReplyIsPosted(t *testing.T) {
	r := newRig(t)
	r.hadAnExchange(t, earlierQuestion, earlierReply)
	r.runner.result = processor.Result{Reply: "what's the actual question you wanna know about?"}
	r.messages.add(channel, "rugenforth", toGangolfLine, r.clock)

	r.turns()

	if len(r.messages.posts) != 1 || r.messages.posts[0].text != "what's the actual question you wanna know about?" {
		t.Fatalf("posts = %+v, want the different reply posted", r.messages.posts)
	}
	if turn := r.turnLine(t); !strings.Contains(turn, "outcome=replied") || !strings.Contains(turn, "silent=false") {
		t.Fatalf("turn log line %q, want outcome=replied and silent=false", turn)
	}
}

func TestAReplyIdenticalOnlyToAnEarlierInputLineIsPosted(t *testing.T) {
	r := newRig(t)
	r.hadAnExchange(t, earlierQuestion, earlierReply)
	r.runner.result = processor.Result{Reply: "vesper: " + earlierQuestion}
	r.messages.add(channel, "rugenforth", toGangolfLine, r.clock)

	r.turns()

	if len(r.messages.posts) != 1 {
		t.Fatalf("posts = %+v, want a reply equal to an earlier input line posted", r.messages.posts)
	}
	if turn := r.turnLine(t); !strings.Contains(turn, "outcome=replied") {
		t.Fatalf("turn log line %q, want outcome=replied", turn)
	}
}

func TestTheClosingStepStillRunsAfterARepeatTurn(t *testing.T) {
	r := newRig(t)
	r.hadAnExchange(t, earlierQuestion, earlierReply)
	r.runner.result = processor.Result{Reply: earlierReply}
	r.messages.add(channel, "rugenforth", toGangolfLine, r.clock)
	r.tick()
	r.duringRun(channel, "dapper", inRunLine)

	r.runTurnThenIdle()

	if len(r.runner.calls) != 1 {
		t.Fatalf("runs = %d, want the line written during a repeat turn to start no follow-up run", len(r.runner.calls))
	}
	if len(r.messages.posts) != 0 {
		t.Fatalf("posts = %+v, want nothing posted", r.messages.posts)
	}
	if !strings.Contains(r.logs.String(), "kept lines written during the run as context") {
		t.Fatalf("logs = %s, want the closing step to have kept the in-run line as context", r.logs.String())
	}
}
