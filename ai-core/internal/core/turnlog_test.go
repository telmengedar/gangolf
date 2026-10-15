package core_test

import (
	"strings"
	"testing"
	"time"
)

func TestARunTurnLogCarriesItsSizesAndTiming(t *testing.T) {
	r := newRig(t)
	r.vesperWasHereBefore(t)
	r.runner.during = func() { r.advance(1500 * time.Millisecond) }
	r.messages.add(channel, "rugenforth", toGangolfLine, r.clock)

	r.turns()

	var turn string
	for _, line := range strings.Split(r.logs.String(), "\n") {
		if strings.Contains(line, "msg=turn") {
			turn = line
		}
	}
	for _, want := range []string{"exchanges=1", "inputBytes=23", "tookMs=1500", "replyBytes=9", "pending=1", "outcome=replied", "silent=false"} {
		if !strings.Contains(turn, want) {
			t.Fatalf("turn log line %q lacks %q", turn, want)
		}
	}
}
