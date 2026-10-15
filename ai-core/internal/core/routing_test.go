package core_test

import (
	"strings"
	"testing"
	"time"

	"github.com/telmengedar/gangolf/ai-core/internal/window"
)

var (
	hostKey    = window.Key{Service: "localhost", Channel: "host"}
	discordKey = window.Key{Service: "discord", Channel: "general"}
)

const (
	hostSpeaker = "rugenforth"
	hostLine    = "gangolf what do you think of this boss"
)

func hostToStream() map[window.Key][]window.Key {
	return map[window.Key][]window.Key{hostKey: {channel}, channel: {channel}}
}

func hostToStreamAndDiscord() map[window.Key][]window.Key {
	return map[window.Key][]window.Key{hostKey: {channel, discordKey}, channel: {channel}, discordKey: {discordKey}}
}

func (r *rig) quiet() {
	for i := 0; i < 4; i++ {
		r.settle()
		r.advance(pollInterval)
	}
}

func (r *rig) hostSpeaks(text string) {
	r.advance(time.Second)
	r.messages.add(hostKey, hostSpeaker, text, r.clock)
}

func postedKeys(posts []posted) []window.Key {
	keys := make([]window.Key, len(posts))
	for i, p := range posts {
		keys[i] = p.key
	}
	return keys
}

func TestAHostLineIsAnsweredInItsTargetChannel(t *testing.T) {
	r := newRoutedRig(t, hostToStream())
	r.bootstrap(t)
	r.hostSpeaks(hostLine)

	r.quiet()

	if len(r.runner.calls) != 1 || r.runner.calls[0].input != hostSpeaker+": "+hostLine {
		t.Fatalf("calls = %+v, want one run on the host line as the streamer's line", r.runner.calls)
	}
	if len(r.messages.posts) != 1 || r.messages.posts[0] != (posted{key: channel, text: "an answer"}) {
		t.Fatalf("posts = %+v, want the one reply in the target channel", r.messages.posts)
	}
}

func TestAHostLineAndAChatLineFormOneBatchInTheTargetChannel(t *testing.T) {
	r := newRoutedRig(t, hostToStream())
	r.bootstrap(t)
	r.hostSpeaks(hostLine)
	r.advance(100 * time.Millisecond)
	r.messages.add(channel, "vesper", "me too", r.clock)

	r.quiet()

	want := hostSpeaker + ": " + hostLine + "\nvesper: me too"
	if len(r.runner.calls) != 1 || r.runner.calls[0].input != want {
		t.Fatalf("calls = %+v, want one run with the input %q", r.runner.calls, want)
	}
}

func TestTheNextTurnInTheTargetSeesTheHostLineAndTheReplyAsHistory(t *testing.T) {
	r := newRoutedRig(t, hostToStream())
	r.bootstrap(t)
	r.hostSpeaks(hostLine)
	r.quiet()
	r.advance(time.Second)
	r.messages.add(channel, "vesper", "and then?", r.clock)

	r.quiet()

	if len(r.runner.calls) != 2 {
		t.Fatalf("runs = %d, want two", len(r.runner.calls))
	}
	history := r.runner.calls[1].history
	if len(history) != 1 || history[0].Input != hostSpeaker+": "+hostLine || history[0].Reply != "an answer" {
		t.Fatalf("history = %+v, want the host line paired with the reply it got", history)
	}
}

func TestARouteToTwoChannelsAnswersInBoth(t *testing.T) {
	r := newRoutedRig(t, hostToStreamAndDiscord())
	r.bootstrap(t)
	r.hostSpeaks(hostLine)

	r.quiet()

	if len(r.runner.calls) != 2 {
		t.Fatalf("runs = %d, want one per target", len(r.runner.calls))
	}
	for _, call := range r.runner.calls {
		if call.input != hostSpeaker+": "+hostLine {
			t.Fatalf("input = %q, want the line once", call.input)
		}
	}
	keys := postedKeys(r.messages.posts)
	if len(keys) != 2 || !(keys[0] == channel && keys[1] == discordKey || keys[0] == discordKey && keys[1] == channel) {
		t.Fatalf("posts to %v, want one reply in each target", keys)
	}
}

func TestASourceRoutedToItselfAndAnotherAnswersInBoth(t *testing.T) {
	routes := map[window.Key][]window.Key{channel: {channel, discordKey}, discordKey: {discordKey}}
	r := newRoutedRig(t, routes)
	r.bootstrap(t)
	r.advance(time.Second)
	r.messages.add(channel, "vesper", "hello everyone", r.clock)

	r.quiet()

	keys := postedKeys(r.messages.posts)
	if len(keys) != 2 || !(keys[0] == channel && keys[1] == discordKey || keys[0] == discordKey && keys[1] == channel) {
		t.Fatalf("posts to %v, want a reply in the source and in the other target", keys)
	}
}

func TestARoutedLineIsGatedAgainstTheTargetsChatters(t *testing.T) {
	r := newRoutedRig(t, hostToStream())
	r.messages.add(channel, quietChatter, "an old question", start.Add(-time.Minute))
	r.bootstrap(t)
	r.hostSpeaks(toVesperLine)

	r.quiet()

	if len(r.runner.calls) != 0 || len(r.messages.posts) != 0 {
		t.Fatalf("runs = %d, posts = %d, want a host line to a Twitch chatter gated like a typed one", len(r.runner.calls), len(r.messages.posts))
	}
}

func TestAHostLineGreetingTheTargetsStreamerIsGated(t *testing.T) {
	r := newRoutedRig(t, hostToStream())
	r.bootstrap(t)
	r.hostSpeaks("hi " + channel.Channel)

	r.quiet()

	if len(r.runner.calls) != 0 {
		t.Fatalf("runs = %d, want none for a line greeting the target's channel name", len(r.runner.calls))
	}
}

func TestTheSourceChannelNameIsNotAChatterInTheTarget(t *testing.T) {
	r := newRoutedRig(t, hostToStream())
	r.bootstrap(t)
	r.hostSpeaks("hi " + hostKey.Channel)

	r.quiet()

	if len(r.runner.calls) != 1 {
		t.Fatalf("runs = %d, want the line run because host is not a candidate name in the target", len(r.runner.calls))
	}
}

func TestARoutedAwayChannelNeverTakesATurn(t *testing.T) {
	r := newRoutedRig(t, hostToStream())
	r.bootstrap(t)
	r.hostSpeaks(hostLine)

	r.quiet()

	for _, p := range r.messages.posts {
		if p.key == hostKey {
			t.Fatalf("posts = %+v, want nothing posted to the routed-away channel", r.messages.posts)
		}
	}
	if strings.Contains(r.logs.String(), "channel="+hostKey.Channel) {
		t.Fatalf("logs = %s, want no log line naming the routed-away channel", r.logs.String())
	}
}

func TestAListedRoutedChannelIsNotWarnedAboutAsUnlisted(t *testing.T) {
	r := newRoutedRig(t, hostToStream())
	r.bootstrap(t)
	r.hostSpeaks(hostLine)

	r.quiet()

	if strings.Contains(r.logs.String(), "not in GANGOLF_CHANNELS") {
		t.Fatalf("logs = %s, want no unlisted warning for a configured source", r.logs.String())
	}
}

func TestARoutedLineWrittenDuringARunIsContextAndStartsNoFollowUpRun(t *testing.T) {
	r := newRoutedRig(t, hostToStream())
	r.bootstrap(t)
	r.advance(time.Second)
	r.messages.add(channel, "vesper", firstLine, r.clock)
	r.tick()
	r.runner.during = func() {
		r.advance(3 * time.Second)
		r.advance(echoMargin)
		r.messages.add(hostKey, hostSpeaker, inRunLine, r.clock)
		r.advance(echoMargin)
	}

	r.runTurnThenIdle()

	if len(r.runner.calls) != 1 {
		t.Fatalf("runs = %d, want the routed line written during the run to start nothing", len(r.runner.calls))
	}
	r.runner.during = nil
	r.writeLater(channel, "vesper", afterLine)
	r.quiet()
	if len(r.runner.calls) != 2 {
		t.Fatalf("runs = %d, want a second run on the line written after the first", len(r.runner.calls))
	}
	if !strings.Contains(r.runner.calls[1].history[0].Input, hostSpeaker+": "+inRunLine) {
		t.Fatalf("history = %+v, want the routed in-run line as context", r.runner.calls[1].history)
	}
}
