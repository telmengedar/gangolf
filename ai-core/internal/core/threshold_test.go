package core_test

import (
	"strings"
	"testing"
	"time"

	"github.com/telmengedar/gangolf/ai-core/internal/core"
	"github.com/telmengedar/gangolf/ai-core/internal/window"
)

const (
	hostMinimum = 4
	threeWords  = "yes that is"
	fourWords   = "yes that is right"
	shortLine   = "mhm"
)

func newThresholdRig(t *testing.T) *rig {
	t.Helper()
	return newMinimumRig(t, hostToStream(), map[window.Key]int{hostKey: hostMinimum})
}

func newMinimumRig(t *testing.T, routes map[window.Key][]window.Key, minimums map[window.Key]int) *rig {
	t.Helper()
	return newConfiguredRig(t, core.Config{Routes: routes, MinWords: minimums})
}

func (r *rig) turnLines() []string {
	var out []string
	for _, line := range strings.Split(r.logs.String(), "\n") {
		if strings.Contains(line, "msg=turn") {
			out = append(out, line)
		}
	}
	return out
}

func TestABatchOfOnlyShortHostLinesStartsNoRun(t *testing.T) {
	r := newThresholdRig(t)
	r.bootstrap(t)
	r.hostSpeaks(shortLine)

	r.quiet()

	if len(r.runner.calls) != 0 || len(r.messages.posts) != 0 {
		t.Fatalf("runs = %d, posts = %d, want neither for a short host line", len(r.runner.calls), len(r.messages.posts))
	}
	turns := r.turnLines()
	if len(turns) != 1 || !strings.Contains(turns[0], "outcome=too-short") || !strings.Contains(turns[0], "tookMs=0") || !strings.Contains(turns[0], "replyBytes=0") || !strings.Contains(turns[0], "silent=true") {
		t.Fatalf("turn lines = %v, want one too-short line with zero duration and reply size", turns)
	}
}

func TestAHostLineOfExactlyTheMinimumWordsStartsARun(t *testing.T) {
	r := newThresholdRig(t)
	r.bootstrap(t)
	r.hostSpeaks(fourWords)
	r.quiet()
	if len(r.runner.calls) != 1 {
		t.Fatalf("runs = %d, want a line of exactly the minimum to run", len(r.runner.calls))
	}

	r.hostSpeaks(threeWords)
	r.quiet()

	if len(r.runner.calls) != 1 {
		t.Fatalf("runs = %d, want a line one word below the minimum to start nothing", len(r.runner.calls))
	}
}

func TestPunctuationIsNotAWord(t *testing.T) {
	r := newThresholdRig(t)
	r.bootstrap(t)
	r.hostSpeaks("uh ... ... ---")

	r.quiet()

	if len(r.runner.calls) != 0 {
		t.Fatalf("runs = %d, want punctuation tokens not to count as words", len(r.runner.calls))
	}
}

func TestWordsWithLettersOrDigitsInsideCount(t *testing.T) {
	r := newThresholdRig(t)
	r.bootstrap(t)
	r.hostSpeaks("uh... don't 42 über")

	r.quiet()

	if len(r.runner.calls) != 1 {
		t.Fatalf("runs = %d, want four tokens with a letter or digit to be four words", len(r.runner.calls))
	}
}

func TestAShortLineRidesAlongWithALongOne(t *testing.T) {
	r := newThresholdRig(t)
	r.bootstrap(t)
	r.hostSpeaks(shortLine)
	r.advance(100 * time.Millisecond)
	r.messages.add(hostKey, hostSpeaker, fourWords, r.clock)

	r.quiet()

	if len(r.runner.calls) != 1 {
		t.Fatalf("runs = %d, want one", len(r.runner.calls))
	}
	if got, want := r.runner.calls[0].input, hostSpeaker+": "+shortLine+"\n"+hostSpeaker+": "+fourWords; got != want {
		t.Fatalf("input = %q, want %q", got, want)
	}
}

func TestAShortChatLineStillStartsARun(t *testing.T) {
	r := newThresholdRig(t)
	r.bootstrap(t)
	r.advance(time.Second)
	r.messages.add(channel, "vesper", "ok", r.clock)

	r.quiet()

	if len(r.runner.calls) != 1 || r.runner.calls[0].input != "vesper: ok" {
		t.Fatalf("calls = %+v, want a one word chat line to run", r.runner.calls)
	}
}

func TestAShortChatLineRunsWithShortHostLines(t *testing.T) {
	r := newThresholdRig(t)
	r.bootstrap(t)
	r.hostSpeaks(shortLine)
	r.advance(100 * time.Millisecond)
	r.messages.add(channel, "vesper", "ok", r.clock)

	r.quiet()

	if len(r.runner.calls) != 1 || r.runner.calls[0].input != hostSpeaker+": "+shortLine+"\nvesper: ok" {
		t.Fatalf("calls = %+v, want the chat line to carry the short host line into a run", r.runner.calls)
	}
}

func TestTheThresholdIsTheSourcesNotTheTargets(t *testing.T) {
	r := newMinimumRig(t, hostToStream(), map[window.Key]int{channel: 100})
	r.bootstrap(t)
	r.hostSpeaks(shortLine)

	r.quiet()

	if len(r.runner.calls) != 1 {
		t.Fatalf("runs = %d, want the host line judged by the host's threshold (none), not the target's", len(r.runner.calls))
	}
}

func TestTheSourcesThresholdFollowsItsLinesIntoEveryTarget(t *testing.T) {
	r := newMinimumRig(t, hostToStreamAndDiscord(), map[window.Key]int{hostKey: hostMinimum})
	r.bootstrap(t)
	r.hostSpeaks(shortLine)

	r.quiet()

	if len(r.runner.calls) != 0 {
		t.Fatalf("runs = %d, want the short line to start nothing in either target", len(r.runner.calls))
	}
}

func TestALineIngestedRightAfterATooShortSkipIsAnswered(t *testing.T) {
	r := newThresholdRig(t)
	r.bootstrap(t)
	r.hostSpeaks(shortLine)
	r.tick()
	skipPollsDone := len(r.messages.froms) + 1
	r.messages.onList = func(n int) {
		if n == skipPollsDone+1 {
			r.messages.add(channel, "vesper", "gangolf are you there", r.clock)
		}
	}

	r.settle()
	r.quiet()

	if len(r.runner.calls) != 1 {
		t.Fatalf("runs = %d, want the line read in right after the skip answered", len(r.runner.calls))
	}
	if strings.Contains(r.logs.String(), "kept lines written during the run as context") {
		t.Fatalf("logs = %s, want no closing step after a too-short skip", r.logs.String())
	}
}

func TestTheLengthCheckRunsBeforeTheAddresseeGate(t *testing.T) {
	r := newMinimumRig(t, hostToStream(), map[window.Key]int{hostKey: 10})
	r.messages.add(channel, quietChatter, "an old question", start.Add(-time.Minute))
	r.bootstrap(t)
	r.hostSpeaks(toVesperLine)

	r.quiet()

	turns := r.turnLines()
	if len(turns) != 1 || !strings.Contains(turns[0], "outcome=too-short") {
		t.Fatalf("turn lines = %v, want too-short and not addressed-elsewhere", turns)
	}
}

func TestShortLinesAppearInTheNextTurnsHistory(t *testing.T) {
	r := newThresholdRig(t)
	r.bootstrap(t)
	r.hostSpeaks(shortLine)
	r.quiet()
	r.advance(time.Second)
	r.messages.add(channel, "vesper", "gangolf are you there", r.clock)

	r.quiet()

	if len(r.runner.calls) != 1 {
		t.Fatalf("runs = %d, want one", len(r.runner.calls))
	}
	history := r.runner.calls[0].history
	if len(history) != 1 || history[0].Input != hostSpeaker+": "+shortLine {
		t.Fatalf("history = %+v, want the skipped short line as context", history)
	}
}

func TestWithoutAThresholdAHostLineOfAnyLengthRuns(t *testing.T) {
	r := newRoutedRig(t, hostToStream())
	r.bootstrap(t)
	r.hostSpeaks(shortLine)

	r.quiet()

	if len(r.runner.calls) != 1 {
		t.Fatalf("runs = %d, want no threshold unless configured", len(r.runner.calls))
	}
}

func (r *rig) vesperIsInTheRoom(t *testing.T) {
	t.Helper()
	r.messages.add(channel, quietChatter, "an old question", start.Add(-time.Minute))
	r.bootstrap(t)
}

func (r *rig) requireSkippedAs(t *testing.T, outcome string) {
	t.Helper()
	turns := r.turnLines()
	if len(r.runner.calls) != 0 || len(r.messages.posts) != 0 {
		t.Fatalf("runs = %d, posts = %d, want neither", len(r.runner.calls), len(r.messages.posts))
	}
	if len(turns) != 1 || !strings.Contains(turns[0], "outcome="+outcome) {
		t.Fatalf("turn lines = %v, want one %s line", turns, outcome)
	}
}

func TestAShortHostLineDoesNotCarryAChatLineToAnotherChatterIntoARun(t *testing.T) {
	r := newThresholdRig(t)
	r.vesperIsInTheRoom(t)
	r.hostSpeaks(shortLine)
	r.advance(100 * time.Millisecond)
	r.messages.add(channel, "dapper", toVesperLine, r.clock)

	r.quiet()

	r.requireSkippedAs(t, "addressed-elsewhere")
}

func TestAShortHostLineDoesNotCarryALongHostLineToAnotherChatterIntoARun(t *testing.T) {
	r := newThresholdRig(t)
	r.vesperIsInTheRoom(t)
	r.hostSpeaks(shortLine)
	r.advance(100 * time.Millisecond)
	r.messages.add(hostKey, hostSpeaker, "vesper did you see that boss", r.clock)

	r.quiet()

	r.requireSkippedAs(t, "addressed-elsewhere")
}

func TestALineIngestedRightAfterAMixedBatchWasSkippedAsAddressedElsewhereIsAnswered(t *testing.T) {
	r := newThresholdRig(t)
	r.vesperIsInTheRoom(t)
	r.hostSpeaks(shortLine)
	r.advance(100 * time.Millisecond)
	r.messages.add(channel, "dapper", toVesperLine, r.clock)
	r.tick()
	skipPollsDone := len(r.messages.froms) + 1
	r.messages.onList = func(n int) {
		if n == skipPollsDone+1 {
			r.messages.add(channel, "vesper", "gangolf are you there", r.clock)
		}
	}

	r.settle()
	r.quiet()

	if len(r.runner.calls) != 1 || r.runner.calls[0].input != "vesper: gangolf are you there" {
		t.Fatalf("calls = %+v, want only the line read in right after the skip answered", r.runner.calls)
	}
	if strings.Contains(r.logs.String(), "kept lines written during the run as context") {
		t.Fatalf("logs = %s, want no closing step after the skip", r.logs.String())
	}
}

func TestAShortHostLineRunsWithAChatLineThatIsNotForSomeoneElse(t *testing.T) {
	r := newThresholdRig(t)
	r.vesperIsInTheRoom(t)
	r.hostSpeaks(shortLine)
	r.advance(100 * time.Millisecond)
	r.messages.add(channel, "vesper", "lol", r.clock)

	r.quiet()

	if len(r.runner.calls) != 1 || r.runner.calls[0].input != hostSpeaker+": "+shortLine+"\nvesper: lol" {
		t.Fatalf("calls = %+v, want one run with both lines in order", r.runner.calls)
	}
}

func TestAShortHostLineRunsWithALongHostLineToTheRoom(t *testing.T) {
	r := newThresholdRig(t)
	r.vesperIsInTheRoom(t)
	r.hostSpeaks(shortLine)
	r.advance(100 * time.Millisecond)
	r.messages.add(hostKey, hostSpeaker, "gangolf what do you think of this boss", r.clock)

	r.quiet()

	want := hostSpeaker + ": " + shortLine + "\n" + hostSpeaker + ": gangolf what do you think of this boss"
	if len(r.runner.calls) != 1 || r.runner.calls[0].input != want {
		t.Fatalf("calls = %+v, want one run with both lines in order", r.runner.calls)
	}
}
