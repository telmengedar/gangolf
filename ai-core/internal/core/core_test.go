package core_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/telmengedar/gangolf/ai-core/internal/core"
	"github.com/telmengedar/gangolf/ai-core/internal/messages"
	"github.com/telmengedar/gangolf/ai-core/internal/processor"
	"github.com/telmengedar/gangolf/ai-core/internal/window"
)

const (
	settleTime   = 2 * time.Second
	overlapTime  = 10 * time.Second
	sevenMinutes = 7 * time.Minute
	pollInterval = 2 * time.Second
)

var (
	start   = time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	channel = window.Key{Service: "twitch", Channel: "stream"}
)

type posted struct {
	key  window.Key
	text string
}

type call struct {
	input   string
	history []window.Exchange
}

type fakeMessages struct {
	clock     *time.Time
	store     []window.Line
	froms     []time.Time
	posts     []posted
	listErr   error
	listErrs  int
	postErr   error
	onList    func(n int)
	echoPosts bool
}

func (f *fakeMessages) List(_ context.Context, from time.Time) ([]window.Line, error) {
	f.froms = append(f.froms, from)
	if f.onList != nil {
		f.onList(len(f.froms))
	}
	if f.listErrs > 0 {
		f.listErrs--
		return nil, errors.New("list failed")
	}
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []window.Line
	for _, line := range f.store {
		if !line.Time.Before(from) {
			out = append(out, line)
		}
	}
	return out, nil
}

func (f *fakeMessages) Post(_ context.Context, key window.Key, text string) error {
	f.posts = append(f.posts, posted{key: key, text: text})
	if f.postErr != nil {
		return f.postErr
	}
	if f.echoPosts {
		f.store = append(f.store, window.Line{Key: key, Speaker: "GangolfBot", Text: text, Time: *f.clock})
	}
	return nil
}

func (f *fakeMessages) add(key window.Key, speaker, text string, at time.Time) {
	f.store = append(f.store, window.Line{Key: key, Speaker: speaker, Text: text, Time: at})
}

type fakeRunner struct {
	calls  []call
	result processor.Result
	err    error
	during func()
}

func (f *fakeRunner) Run(_ context.Context, input string, history []window.Exchange) (processor.Result, error) {
	f.calls = append(f.calls, call{input: input, history: history})
	if f.during != nil {
		f.during()
	}
	if f.err != nil {
		return processor.Result{}, f.err
	}
	return f.result, nil
}

type rig struct {
	clock    time.Time
	messages *fakeMessages
	runner   *fakeRunner
	core     *core.Core
	logs     *bytes.Buffer
}

func newRig(t *testing.T, channels ...window.Key) *rig {
	t.Helper()
	if len(channels) == 0 {
		channels = []window.Key{channel}
	}
	r := &rig{clock: start, runner: &fakeRunner{result: processor.Result{Reply: "an answer"}}, logs: &bytes.Buffer{}}
	r.messages = &fakeMessages{clock: &r.clock, echoPosts: true}
	log := slog.New(slog.NewTextHandler(r.logs, nil))
	r.core = core.New(core.Config{Channels: channels, BotName: "GangolfBot", Interval: time.Millisecond}, r.messages, r.runner, log, func() time.Time { return r.clock })
	return r
}

func (r *rig) advance(d time.Duration) { r.clock = r.clock.Add(d) }

func (r *rig) bootstrap(t *testing.T) {
	t.Helper()
	if err := r.core.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func (r *rig) tick() { r.core.Tick(context.Background()) }

func (r *rig) settle() {
	r.advance(settleTime)
	r.tick()
}

func TestBurstYieldsOneTurn(t *testing.T) {
	r := newRig(t)
	r.bootstrap(t)
	r.advance(time.Second)
	r.messages.add(channel, "rugenforth", "hey gangolf", r.clock)
	r.messages.add(channel, "vesper", "he is asleep", r.clock)
	r.messages.add(channel, "dapper", "no he is not", r.clock)

	r.tick()
	r.settle()
	r.advance(pollInterval)
	r.tick()

	if len(r.runner.calls) != 1 {
		t.Fatalf("runs = %d, want one turn for the whole burst", len(r.runner.calls))
	}
	if got := r.runner.calls[0].input; got != "rugenforth: hey gangolf\nvesper: he is asleep\ndapper: no he is not" {
		t.Fatalf("input = %q", got)
	}
	if len(r.messages.posts) != 1 || r.messages.posts[0] != (posted{key: channel, text: "an answer"}) {
		t.Fatalf("posts = %+v, want the reply in the turn's channel", r.messages.posts)
	}
}

func TestNothingIsAnsweredBeforeTheChannelHasSettled(t *testing.T) {
	r := newRig(t)
	r.bootstrap(t)
	r.advance(time.Second)
	r.messages.add(channel, "vesper", "hello", r.clock)

	r.tick()
	r.advance(settleTime - time.Nanosecond)
	r.tick()

	if len(r.runner.calls) != 0 {
		t.Fatalf("runs = %d, want none before the settle time", len(r.runner.calls))
	}
	r.advance(time.Nanosecond)
	r.tick()
	if len(r.runner.calls) != 1 {
		t.Fatalf("runs = %d, want one at the settle time", len(r.runner.calls))
	}
}

func TestLinesDuringATurnBecomeTheNextInputAndNeverHistory(t *testing.T) {
	r := newRig(t)
	r.bootstrap(t)
	r.advance(time.Second)
	r.messages.add(channel, "rugenforth", "hey gangolf", r.clock)
	r.tick()
	r.runner.during = func() {
		r.advance(18 * time.Second)
		r.messages.add(channel, "dapper", "what about the pinkertons", r.clock)
		r.runner.during = nil
	}

	r.settle()
	r.advance(pollInterval)
	r.tick()
	r.settle()

	if len(r.runner.calls) != 2 {
		t.Fatalf("runs = %d, want two turns", len(r.runner.calls))
	}
	second := r.runner.calls[1]
	if second.input != "dapper: what about the pinkertons" {
		t.Fatalf("second input = %q, want only the late line", second.input)
	}
	if len(second.history) != 1 || second.history[0].Input != "rugenforth: hey gangolf" || second.history[0].Reply != "an answer" {
		t.Fatalf("second history = %+v, want the first exchange with its reply", second.history)
	}
}

func TestBootstrapMarksEverythingAnsweredAndKeepsItAsContext(t *testing.T) {
	r := newRig(t)
	r.messages.add(channel, "vesper", "an old question", start.Add(-2*time.Minute))
	r.messages.add(channel, "dapper", "another one", start.Add(-time.Minute))
	r.bootstrap(t)

	r.settle()
	r.settle()
	if len(r.runner.calls) != 0 {
		t.Fatalf("runs = %d, want old lines never answered", len(r.runner.calls))
	}
	r.messages.add(channel, "rugenforth", "fresh question", r.clock)
	r.tick()
	r.settle()

	if len(r.runner.calls) != 1 {
		t.Fatalf("runs = %d, want one", len(r.runner.calls))
	}
	call := r.runner.calls[0]
	if call.input != "rugenforth: fresh question" {
		t.Fatalf("input = %q", call.input)
	}
	if len(call.history) != 1 || call.history[0].Input != "vesper: an old question\ndapper: another one" {
		t.Fatalf("history = %+v, want the old lines as context", call.history)
	}
}

func TestLinesOlderThanSevenMinutesLeaveTheContext(t *testing.T) {
	r := newRig(t)
	r.messages.add(channel, "vesper", "stale chatter", start.Add(-6*time.Minute))
	r.bootstrap(t)
	r.advance(5 * time.Minute)
	r.messages.add(channel, "dapper", "fresh question", r.clock)

	r.tick()
	r.settle()

	if len(r.runner.calls) != 1 || len(r.runner.calls[0].history) != 0 {
		t.Fatalf("calls = %+v, want the stale line gone from the context", r.runner.calls)
	}
}

func TestBootstrapReachesBackSevenMinutes(t *testing.T) {
	r := newRig(t)

	r.bootstrap(t)

	if len(r.messages.froms) != 1 || !r.messages.froms[0].Equal(start.Add(-sevenMinutes)) {
		t.Fatalf("bootstrap from = %v, want seven minutes before now", r.messages.froms)
	}
}

func TestRunRetriesTheBootstrapUntilItSucceedsBeforeAnsweringAnything(t *testing.T) {
	r := newRig(t)
	r.messages.add(channel, "vesper", "an old question", start.Add(-time.Minute))
	r.messages.listErrs = 2
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.messages.onList = func(n int) {
		r.advance(3 * time.Second)
		if n >= 7 {
			cancel()
		}
	}

	done := make(chan struct{})
	go func() {
		r.core.Run(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("run did not stop when its context ended")
	}

	if len(r.messages.froms) < 7 {
		t.Fatalf("polls = %d, want the bootstrap retried and ticks to follow", len(r.messages.froms))
	}
	if len(r.runner.calls) != 0 {
		t.Fatalf("runs = %d, want the old line never answered after a retried bootstrap", len(r.runner.calls))
	}
}

func TestCursorFollowsTheNewestServerTimeWithAnOverlap(t *testing.T) {
	r := newRig(t)
	r.bootstrap(t)
	newest := start.Add(30 * time.Second)
	r.messages.add(channel, "vesper", "hello", newest)
	r.advance(time.Minute)

	r.tick()
	r.tick()

	if got := r.messages.froms[len(r.messages.froms)-1]; !got.Equal(newest.Add(-overlapTime)) {
		t.Fatalf("from = %v, want the newest line time minus the overlap", got)
	}
	if got := r.messages.froms[1]; !got.Equal(start.Add(-sevenMinutes)) {
		t.Fatalf("from before any line = %v, want the bootstrap start", got)
	}
}

func TestRepolledLineIsNotAnsweredTwice(t *testing.T) {
	r := newRig(t)
	r.bootstrap(t)
	r.advance(time.Second)
	r.messages.add(channel, "vesper", "hello", r.clock)

	r.tick()
	r.settle()
	for i := 0; i < 4; i++ {
		r.advance(pollInterval)
		r.tick()
	}

	if len(r.runner.calls) != 1 {
		t.Fatalf("runs = %d, want the re-polled line answered once", len(r.runner.calls))
	}
	r.advance(20 * time.Second)
	r.messages.add(channel, "rugenforth", "fresh question", r.clock)
	r.tick()
	r.settle()
	if len(r.runner.calls) != 2 || r.runner.calls[1].input != "rugenforth: fresh question" {
		t.Fatalf("calls = %+v, want the next turn to hold only the fresh line", r.runner.calls)
	}
}

func TestLateCommittedRowInsideTheOverlapIsStillAnswered(t *testing.T) {
	r := newRig(t)
	r.bootstrap(t)
	r.advance(5 * time.Second)
	r.messages.add(channel, "vesper", "newer row", r.clock)
	r.tick()
	r.settle()
	r.messages.add(channel, "dapper", "late row", r.clock.Add(-9*time.Second))

	r.tick()
	r.settle()

	if len(r.runner.calls) != 2 || r.runner.calls[1].input != "dapper: late row" {
		t.Fatalf("calls = %+v, want the late row answered in its own turn", r.runner.calls)
	}
}

func TestCursorNeverMovesBackwards(t *testing.T) {
	r := newRig(t)
	r.bootstrap(t)
	newest := start.Add(30 * time.Second)
	r.messages.add(channel, "vesper", "newest", newest)
	r.messages.add(channel, "dapper", "older", newest.Add(-5*time.Second))
	r.advance(time.Minute)
	r.tick()

	r.tick()

	if got := r.messages.froms[len(r.messages.froms)-1]; !got.Equal(newest.Add(-overlapTime)) {
		t.Fatalf("from = %v, want the cursor to stay at the newest time seen", got)
	}
}

func TestPollFailureKeepsTheCursor(t *testing.T) {
	r := newRig(t)
	r.bootstrap(t)
	newest := start.Add(30 * time.Second)
	r.messages.add(channel, "vesper", "hello", newest)
	r.advance(time.Minute)
	r.tick()
	r.messages.listErr = errors.New("api down")

	r.tick()
	r.messages.listErr = nil
	r.tick()

	if got := r.messages.froms[len(r.messages.froms)-1]; !got.Equal(newest.Add(-overlapTime)) {
		t.Fatalf("from = %v, want the cursor unchanged by the failed poll", got)
	}
	if !strings.Contains(r.logs.String(), "poll failed") {
		t.Fatalf("logs = %s, want the failure logged", r.logs.String())
	}
}

func TestPollFailureStillAnswersWhatHasAlreadySettled(t *testing.T) {
	r := newRig(t)
	r.bootstrap(t)
	r.advance(time.Second)
	r.messages.add(channel, "vesper", "hello", r.clock)
	r.tick()
	r.messages.listErr = errors.New("api down")

	r.settle()

	if len(r.runner.calls) != 1 {
		t.Fatalf("runs = %d, want the settled line answered despite the failed poll", len(r.runner.calls))
	}
}

func TestOnlyAllowListedChannelsAreAnswered(t *testing.T) {
	r := newRig(t)
	r.bootstrap(t)
	r.advance(time.Second)
	r.messages.add(window.Key{Service: "twitch", Channel: "elsewhere"}, "vesper", "not for you", r.clock)
	r.messages.add(window.Key{Service: "discord", Channel: "stream"}, "vesper", "wrong service", r.clock)

	r.tick()
	r.settle()

	if len(r.runner.calls) != 0 || len(r.messages.posts) != 0 {
		t.Fatalf("runs = %d posts = %d, want none outside the allow-list", len(r.runner.calls), len(r.messages.posts))
	}
}

func TestOtherChannelsStillAdvanceTheCursor(t *testing.T) {
	r := newRig(t)
	r.bootstrap(t)
	newest := start.Add(40 * time.Second)
	r.messages.add(window.Key{Service: "twitch", Channel: "elsewhere"}, "vesper", "busy elsewhere", newest)
	r.advance(time.Minute)
	r.tick()

	r.tick()

	if got := r.messages.froms[len(r.messages.froms)-1]; !got.Equal(newest.Add(-overlapTime)) {
		t.Fatalf("from = %v, want the cursor to follow every channel", got)
	}
}

func TestReplyGoesBackToTheChannelOfTheTurn(t *testing.T) {
	other := window.Key{Service: "discord", Channel: "general"}
	r := newRig(t, channel, other)
	r.bootstrap(t)
	r.advance(time.Second)
	r.messages.add(other, "vesper", "hello from discord", r.clock)

	r.tick()
	r.settle()

	if len(r.messages.posts) != 1 || r.messages.posts[0].key != other {
		t.Fatalf("posts = %+v, want the reply in the discord channel", r.messages.posts)
	}
}

func TestOneChannelIsAnsweredPerTick(t *testing.T) {
	other := window.Key{Service: "discord", Channel: "general"}
	r := newRig(t, channel, other)
	r.bootstrap(t)
	r.advance(time.Second)
	r.messages.add(channel, "vesper", "first channel", r.clock)
	r.messages.add(other, "dapper", "second channel", r.clock.Add(time.Millisecond))
	r.tick()

	r.settle()
	if len(r.runner.calls) != 1 {
		t.Fatalf("runs after one tick = %d, want one", len(r.runner.calls))
	}
	r.advance(pollInterval)
	r.tick()

	if len(r.runner.calls) != 2 {
		t.Fatalf("runs after two ticks = %d, want both channels answered", len(r.runner.calls))
	}
}

func TestRunFailureMarksTheBatchAnsweredAndNeverRetries(t *testing.T) {
	r := newRig(t)
	r.bootstrap(t)
	r.advance(time.Second)
	r.messages.add(channel, "vesper", "hello", r.clock)
	r.runner.err = errors.New("model_unavailable")
	r.tick()

	r.settle()
	r.advance(pollInterval)
	r.tick()
	r.settle()

	if len(r.runner.calls) != 1 {
		t.Fatalf("runs = %d, want no retry of a failed turn", len(r.runner.calls))
	}
	if len(r.messages.posts) != 0 {
		t.Fatalf("posts = %+v, want nothing posted after a failed run", r.messages.posts)
	}
	if !strings.Contains(r.logs.String(), "run failed") {
		t.Fatalf("logs = %s, want the failure logged", r.logs.String())
	}
}

func TestPostFailureIsLoggedAndNeverRetried(t *testing.T) {
	r := newRig(t)
	r.bootstrap(t)
	r.advance(time.Second)
	r.messages.add(channel, "vesper", "hello", r.clock)
	r.messages.postErr = errors.New("post timed out")
	r.tick()

	r.settle()
	r.advance(pollInterval)
	r.tick()
	r.settle()

	if len(r.messages.posts) != 1 {
		t.Fatalf("posts = %d, want exactly one attempt", len(r.messages.posts))
	}
	if !strings.Contains(r.logs.String(), "post failed") {
		t.Fatalf("logs = %s, want the failure logged", r.logs.String())
	}
}

func TestLeadingBangsAreStrippedFromTheReply(t *testing.T) {
	r := newRig(t)
	r.runner.result = processor.Result{Reply: "  !! !shout hello there  "}
	r.bootstrap(t)
	r.advance(time.Second)
	r.messages.add(channel, "vesper", "say it", r.clock)

	r.tick()
	r.settle()

	if len(r.messages.posts) != 1 || r.messages.posts[0].text != "shout hello there" {
		t.Fatalf("posts = %+v, want the reply without leading bangs", r.messages.posts)
	}
}

func TestBangsInsideTheReplyAreKept(t *testing.T) {
	r := newRig(t)
	r.runner.result = processor.Result{Reply: "Hello! Nice to see you!"}
	r.bootstrap(t)
	r.advance(time.Second)
	r.messages.add(channel, "vesper", "hi", r.clock)

	r.tick()
	r.settle()

	if len(r.messages.posts) != 1 || r.messages.posts[0].text != "Hello! Nice to see you!" {
		t.Fatalf("posts = %+v, want inner and trailing bangs untouched", r.messages.posts)
	}
}

func TestAReplyOfOnlyBangsIsNotPosted(t *testing.T) {
	r := newRig(t)
	r.runner.result = processor.Result{Reply: "!!!"}
	r.bootstrap(t)
	r.advance(time.Second)
	r.messages.add(channel, "vesper", "hi", r.clock)

	r.tick()
	r.settle()

	if len(r.messages.posts) != 0 {
		t.Fatalf("posts = %+v, want nothing posted for an empty reply", r.messages.posts)
	}
}

func postedFor(t *testing.T, reply string) []posted {
	t.Helper()
	return postedAs(t, "GangolfBot", reply)
}

func postedAs(t *testing.T, botName, reply string) []posted {
	t.Helper()
	r := newRig(t)
	log := slog.New(slog.NewTextHandler(r.logs, nil))
	r.core = core.New(core.Config{Channels: []window.Key{channel}, BotName: botName, Interval: time.Millisecond}, r.messages, r.runner, log, func() time.Time { return r.clock })
	r.runner.result = processor.Result{Reply: reply}
	r.bootstrap(t)
	r.advance(time.Second)
	r.messages.add(channel, "vesper", "hi", r.clock)
	r.tick()
	r.settle()
	return r.messages.posts
}

func TestOwnSpeakerLabelIsStrippedFromTheStartOfTheReply(t *testing.T) {
	cases := map[string]string{
		"Gangolf: what's the point?": "what's the point?",
		"GangolfBot: hello":          "hello",
		"gangolf:hello":              "hello",
		"GANGOLFBOT:   hello":        "hello",
		"  Gangolf: hello  ":         "hello",
		"Gangolf: GangolfBot: hello": "hello",
		"Gangolf: !shout hello":      "shout hello",
		"!Gangolf: hello":            "hello",
		"Gangolf:\n\nhello":          "hello",
	}
	for reply, want := range cases {
		posts := postedFor(t, reply)
		if len(posts) != 1 || posts[0].text != want {
			t.Errorf("reply %q: posts = %+v, want %q", reply, posts, want)
		}
	}
}

func TestSpeakerLabelInsideTheReplyIsKept(t *testing.T) {
	cases := []string{
		"He said Gangolf: hello",
		"Hello\nGangolf: hello",
		"Gangolf is here: hello",
		"Gangolfs: hello",
		"Gangolf",
		"Another: Gangolf: hello",
	}
	for _, reply := range cases {
		posts := postedFor(t, reply)
		if len(posts) != 1 || posts[0].text != reply {
			t.Errorf("reply %q: posts = %+v, want it untouched", reply, posts)
		}
	}
}

func TestAReplyOfOnlyTheSpeakerLabelIsNotPosted(t *testing.T) {
	for _, reply := range []string{"Gangolf:", "GangolfBot:  ", "Gangolf: GangolfBot:\n", "Gangolf: !!"} {
		if posts := postedFor(t, reply); len(posts) != 0 {
			t.Errorf("reply %q: posts = %+v, want nothing posted", reply, posts)
		}
	}
}

func TestConfiguredBotNameLabelIsStripped(t *testing.T) {
	cases := map[string]string{
		"Gandalf: hi":          "hi",
		"gANDALF:hi":           "hi",
		"Gangolf: Gandalf: hi": "hi",
		"GangolfBot: hi":       "GangolfBot: hi",
		"He said Gandalf: hi":  "He said Gandalf: hi",
	}
	for reply, want := range cases {
		posts := postedAs(t, "Gandalf", reply)
		if len(posts) != 1 || posts[0].text != want {
			t.Errorf("reply %q: posts = %+v, want %q", reply, posts, want)
		}
	}
}

func TestLabelMatchIgnoresCaseInAnyPosition(t *testing.T) {
	for _, reply := range []string{"gangolfBot: hi", "GangolfBOT: hi", "gAngolf: hi"} {
		posts := postedFor(t, reply)
		if len(posts) != 1 || posts[0].text != "hi" {
			t.Errorf("reply %q: posts = %+v, want %q", reply, posts, "hi")
		}
	}
}

func TestEmptyBotNameNeverStripsALeadingColon(t *testing.T) {
	posts := postedAs(t, "", ":) hi")
	if len(posts) != 1 || posts[0].text != ":) hi" {
		t.Fatalf("posts = %+v, want the reply untouched", posts)
	}
}

func TestSilentResultPostsNothingAndStillConsumesTheBatch(t *testing.T) {
	r := newRig(t)
	r.runner.result = processor.Result{Reply: "Sorry, I could not put an answer together.", Silent: true}
	r.bootstrap(t)
	r.advance(time.Second)
	r.messages.add(channel, "vesper", "hi", r.clock)
	r.tick()

	r.settle()
	r.advance(pollInterval)
	r.tick()
	r.settle()

	if len(r.messages.posts) != 0 {
		t.Fatalf("posts = %+v, want nothing posted when gangolf has nothing to add", r.messages.posts)
	}
	if len(r.runner.calls) != 1 {
		t.Fatalf("runs = %d, want the silent batch not asked again", len(r.runner.calls))
	}
}

func turnLogFor(t *testing.T, result processor.Result) (*rig, string) {
	t.Helper()
	r := newRig(t)
	r.runner.result = result
	r.bootstrap(t)
	r.advance(time.Second)
	r.messages.add(channel, "vesper", "hi", r.clock)
	r.tick()
	r.settle()
	for _, line := range strings.Split(r.logs.String(), "\n") {
		if strings.Contains(line, "msg=turn") {
			return r, line
		}
	}
	t.Fatalf("logs = %s, want a turn line", r.logs.String())
	return r, ""
}

func TestTurnLogTellsEveryOutcomeApart(t *testing.T) {
	cases := []struct {
		name   string
		result processor.Result
		want   string
		silent string
		posts  int
	}{
		{"chosen silence", processor.Result{Silent: true, Chosen: true}, "outcome=chose-silence", "silent=true", 0},
		{"failed turn", processor.Result{Reply: "Sorry, I could not put an answer together.", Silent: true, Failed: true}, "outcome=failed", "silent=true", 0},
		{"blank reply", processor.Result{Silent: true}, "outcome=empty", "silent=true", 0},
		{"reply that sanitises to nothing", processor.Result{Reply: "GangolfBot:"}, "outcome=empty", "silent=true", 0},
		{"reply", processor.Result{Reply: "an answer"}, "outcome=replied", "silent=false", 1},
	}
	for _, c := range cases {
		r, line := turnLogFor(t, c.result)
		if !strings.Contains(line, c.want) || !strings.Contains(line, c.silent) {
			t.Errorf("%s: turn line = %s, want %s and %s", c.name, line, c.want, c.silent)
		}
		if len(r.messages.posts) != c.posts {
			t.Errorf("%s: posts = %+v, want %d", c.name, r.messages.posts, c.posts)
		}
	}
}

func TestChosenSilenceIsNotPostedEvenWithAReplyAttached(t *testing.T) {
	r, line := turnLogFor(t, processor.Result{Reply: "left over text", Silent: true, Chosen: true})

	if len(r.messages.posts) != 0 || !strings.Contains(line, "outcome=chose-silence") {
		t.Fatalf("posts = %+v line = %s, want nothing posted and a chosen silence", r.messages.posts, line)
	}
}

func TestSilentTurnLinesStayInTheNextTurnsHistory(t *testing.T) {
	r := newRig(t)
	r.runner.result = processor.Result{Silent: true}
	r.bootstrap(t)
	r.advance(time.Second)
	r.messages.add(channel, "vesper", "quiet chatter", r.clock)
	r.tick()
	r.settle()
	r.runner.result = processor.Result{Reply: "now I speak"}
	r.advance(time.Second)
	r.messages.add(channel, "dapper", "gangolf?", r.clock)
	r.tick()

	r.settle()

	if len(r.runner.calls) != 2 {
		t.Fatalf("runs = %d, want two", len(r.runner.calls))
	}
	history := r.runner.calls[1].history
	if len(history) != 1 || history[0].Input != "vesper: quiet chatter" || history[0].Reply != "" {
		t.Fatalf("history = %+v, want the unanswered line as a reply-less exchange", history)
	}
}

func TestKeyNeverReachesTheLogs(t *testing.T) {
	const key = "keyid.topsecretvalue"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, "rejected "+r.Header.Get("Authorization"))
	}))
	defer server.Close()
	logs := &bytes.Buffer{}
	log := slog.New(slog.NewTextHandler(logs, nil))
	client := messages.New(server.URL, key, "GangolfBot", server.Client(), log)
	runner := &fakeRunner{}
	c := core.New(core.Config{Channels: []window.Key{channel}, BotName: "GangolfBot", Interval: time.Millisecond}, client, runner, log, func() time.Time { return start })

	c.Tick(context.Background())
	_ = c.Bootstrap(context.Background())

	if logs.Len() == 0 {
		t.Fatal("expected the failed poll to be logged")
	}
	if strings.Contains(logs.String(), key) || strings.Contains(logs.String(), "topsecretvalue") {
		t.Fatalf("logs leaked the key: %s", logs.String())
	}
}

func TestTurnLogCarriesCountsButNoMessageText(t *testing.T) {
	r := newRig(t)
	r.bootstrap(t)
	r.advance(time.Second)
	r.messages.add(channel, "vesper", "a very private sentence", r.clock)

	r.tick()
	r.settle()

	logs := r.logs.String()
	if !strings.Contains(logs, "inputBytes") || strings.Contains(logs, "private sentence") || strings.Contains(logs, "an answer") {
		t.Fatalf("logs = %s, want sizes only and no message text", logs)
	}
}
