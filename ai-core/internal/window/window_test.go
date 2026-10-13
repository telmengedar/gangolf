package window_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/telmengedar/gangolf/ai-core/internal/window"
)

const (
	tenLines     = 10
	tenPending   = 10
	sevenMinutes = 7 * time.Minute
)

var (
	base    = time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	channel = window.Key{Service: "twitch", Channel: "stream"}
	other   = window.Key{Service: "discord", Channel: "general"}
)

func say(key window.Key, speaker, text string, at time.Time) window.Line {
	return window.Line{Key: key, Speaker: speaker, Text: text, Time: at}
}

func TestDedupeAcrossOverlapIngestsTheSameRowOnce(t *testing.T) {
	w := window.New("GangolfBot")
	row := say(channel, "vesper", "hello there", base)

	first := w.Ingest([]window.Line{row}, base)
	second := w.Ingest([]window.Line{row}, base.Add(2*time.Second))

	if first != 1 || second != 0 {
		t.Fatalf("ingest counts = %d then %d, want 1 then 0", first, second)
	}
	batch := w.Take(channel)
	if batch.Input != "vesper: hello there" {
		t.Fatalf("input = %q, want the row exactly once", batch.Input)
	}
}

func TestForgetReleasesOnlyFingerprintsOlderThanTheBound(t *testing.T) {
	w := window.New("GangolfBot")
	old := say(channel, "vesper", "old", base)
	recent := say(channel, "vesper", "recent", base.Add(time.Minute))
	w.Ingest([]window.Line{old, recent}, base)

	w.Forget(base.Add(30 * time.Second))

	if got := w.Ingest([]window.Line{old}, base); got != 1 {
		t.Fatalf("forgotten row ingested %d times, want 1", got)
	}
	if got := w.Ingest([]window.Line{recent}, base); got != 0 {
		t.Fatalf("remembered row ingested %d times, want 0", got)
	}
}

func TestFingerprintDistinguishesSpeakerTimeTextAndChannel(t *testing.T) {
	w := window.New("GangolfBot")
	row := say(channel, "vesper", "same", base)
	variants := []window.Line{
		say(channel, "dapper", "same", base),
		say(channel, "vesper", "different", base),
		say(channel, "vesper", "same", base.Add(time.Nanosecond)),
		say(other, "vesper", "same", base),
	}
	w.Ingest([]window.Line{row}, base)

	if got := w.Ingest(variants, base); got != len(variants) {
		t.Fatalf("variants ingested = %d, want %d", got, len(variants))
	}
}

func TestLateCommittedRowIsInsertedInTimeOrderAndPending(t *testing.T) {
	w := window.New("GangolfBot")
	w.Ingest([]window.Line{
		say(channel, "vesper", "first", base),
		say(channel, "dapper", "third", base.Add(20*time.Second)),
	}, base)
	w.MarkAllAnswered()

	w.Ingest([]window.Line{say(channel, "rugen", "second", base.Add(10*time.Second))}, base.Add(30*time.Second))
	batch := w.Take(channel)

	if batch.Input != "rugen: second" {
		t.Fatalf("input = %q, want only the late row", batch.Input)
	}
	if len(batch.History) != 1 || strings.Contains(batch.History[0].Input, "rugen") {
		t.Fatalf("history = %+v, want the late row kept out of history", batch.History)
	}
	if batch.History[0].Input != "vesper: first\ndapper: third" {
		t.Fatalf("history input = %q, want first then third in time order", batch.History[0].Input)
	}
	w.Ingest([]window.Line{say(channel, "vesper", "fourth", base.Add(40*time.Second))}, base.Add(40*time.Second))
	next := w.Take(channel)
	if len(next.History) != 1 || next.History[0].Input != "vesper: first\nrugen: second\ndapper: third" {
		t.Fatalf("next history = %+v, want the late row sorted between first and third", next.History)
	}
}

func TestRowsAtTheSameTimeKeepArrivalOrder(t *testing.T) {
	w := window.New("GangolfBot")
	w.Ingest([]window.Line{say(channel, "a", "one", base)}, base)
	w.Ingest([]window.Line{say(channel, "b", "two", base)}, base)

	if got := w.Take(channel).Input; got != "a: one\nb: two" {
		t.Fatalf("input = %q, want arrival order for equal times", got)
	}
}

func TestPendingLineSurvivesEvictionByAgeAndCount(t *testing.T) {
	w := window.New("GangolfBot")
	now := base.Add(20 * time.Minute)
	var later []window.Line
	for i := 0; i < tenLines+2; i++ {
		later = append(later, say(channel, "dapper", fmt.Sprintf("line %d", i), now.Add(-time.Duration(i)*time.Second)))
	}
	w.Ingest(later, now)
	w.MarkAllAnswered()
	w.Ingest([]window.Line{say(channel, "vesper", "ancient pending", base)}, now)

	w.Evict(now)
	batch := w.Take(channel)

	if batch.Input != "vesper: ancient pending" {
		t.Fatalf("input = %q, want the old pending line kept", batch.Input)
	}
	if len(batch.History) != 1 || strings.Count(batch.History[0].Input, "\n")+1 != tenLines {
		t.Fatalf("history = %+v, want the ten newest answered lines", batch.History)
	}
}

func TestEvictKeepsOnlyTheNewestTenAnsweredLines(t *testing.T) {
	w := window.New("GangolfBot")
	var lines []window.Line
	for i := 0; i < tenLines+2; i++ {
		lines = append(lines, say(channel, "dapper", fmt.Sprintf("line %02d", i), base.Add(time.Duration(i)*time.Second)))
	}
	w.Ingest(lines, base)
	w.MarkAllAnswered()

	w.Evict(base.Add(time.Minute))
	w.Ingest([]window.Line{say(channel, "vesper", "now", base.Add(time.Minute))}, base.Add(time.Minute))
	batch := w.Take(channel)

	if len(batch.History) != 1 {
		t.Fatalf("history exchanges = %d, want 1", len(batch.History))
	}
	kept := strings.Split(batch.History[0].Input, "\n")
	if len(kept) != tenLines {
		t.Fatalf("kept %d lines, want %d", len(kept), tenLines)
	}
	if kept[0] != "dapper: line 02" {
		t.Fatalf("oldest kept = %q, want the two oldest dropped", kept[0])
	}
}

func TestEvictDropsAnsweredLinesOlderThanSevenMinutes(t *testing.T) {
	w := window.New("GangolfBot")
	now := base.Add(time.Hour)
	w.Ingest([]window.Line{
		say(channel, "vesper", "exactly at the limit", now.Add(-sevenMinutes)),
		say(channel, "vesper", "just past the limit", now.Add(-sevenMinutes-time.Nanosecond)),
	}, now)
	w.MarkAllAnswered()

	w.Evict(now)
	w.Ingest([]window.Line{say(channel, "dapper", "now", now)}, now)
	batch := w.Take(channel)

	if len(batch.History) != 1 || batch.History[0].Input != "vesper: exactly at the limit" {
		t.Fatalf("history = %+v, want only the line exactly at the age limit", batch.History)
	}
}

func TestPendingCapDropsOldestSurplusIntoHistory(t *testing.T) {
	w := window.New("GangolfBot")
	var lines []window.Line
	for i := 0; i < tenPending+2; i++ {
		lines = append(lines, say(channel, "dapper", fmt.Sprintf("line %02d", i), base.Add(time.Duration(i)*time.Second)))
	}
	w.Ingest(lines, base)

	batch := w.Take(channel)

	if batch.Pending != tenPending || batch.Dropped != 2 {
		t.Fatalf("pending = %d dropped = %d, want %d and 2", batch.Pending, batch.Dropped, tenPending)
	}
	if strings.Contains(batch.Input, "line 00") || strings.Contains(batch.Input, "line 01") || !strings.Contains(batch.Input, "line 11") {
		t.Fatalf("input = %q, want the two oldest lines cut and the newest kept", batch.Input)
	}
	if len(batch.History) != 1 || batch.History[0].Input != "dapper: line 00\ndapper: line 01" {
		t.Fatalf("history = %+v, want the dropped surplus as earlier conversation", batch.History)
	}
}

func TestHistoryGroupsSpeakersAndBotRuns(t *testing.T) {
	w := window.New("GangolfBot")
	at := func(s int) time.Time { return base.Add(time.Duration(s) * time.Second) }
	w.Ingest([]window.Line{
		say(channel, "GangolfBot", "leading bot line", at(0)),
		say(channel, "vesper", "hi", at(1)),
		say(channel, "dapper", "hello", at(2)),
		say(channel, "GangolfBot", "first part", at(3)),
		say(channel, "GangolfBot", "second part", at(4)),
		say(channel, "rugen", "unanswered", at(5)),
	}, base)
	w.MarkAllAnswered()
	w.Ingest([]window.Line{say(channel, "vesper", "now", at(6))}, base)

	batch := w.Take(channel)

	want := []window.Exchange{
		{Input: "vesper: hi\ndapper: hello", Reply: "first part\nsecond part"},
		{Input: "rugen: unanswered", Reply: ""},
	}
	if len(batch.History) != len(want) {
		t.Fatalf("history = %+v, want %+v", batch.History, want)
	}
	for i := range want {
		if batch.History[i] != want[i] {
			t.Fatalf("exchange %d = %+v, want %+v", i, batch.History[i], want[i])
		}
	}
}

func TestHistoryIsAnEmptySliceWhenNothingCameBefore(t *testing.T) {
	w := window.New("GangolfBot")
	w.Ingest([]window.Line{say(channel, "vesper", "first ever", base)}, base)

	if batch := w.Take(channel); batch.History == nil {
		t.Fatalf("history must be an empty slice, never nil")
	}
}

func TestBotLineIsNeverPendingWhateverItsCase(t *testing.T) {
	w := window.New("GangolfBot")
	w.Ingest([]window.Line{say(channel, "gangolfbot", "my own echo", base)}, base)

	if _, ok := w.Next(base.Add(time.Hour), 0); ok {
		t.Fatalf("a bot line must not make a channel a candidate")
	}
	if batch := w.Take(channel); batch.Input != "" || batch.Pending != 0 {
		t.Fatalf("batch = %+v, want nothing to answer", batch)
	}
}

func TestNoTurnWhenOnlyBotLinesAreNew(t *testing.T) {
	w := window.New("GangolfBot")
	w.Ingest([]window.Line{say(channel, "vesper", "question", base)}, base)
	w.MarkAllAnswered()
	w.Ingest([]window.Line{say(channel, "GangolfBot", "answer", base.Add(time.Second))}, base)

	if _, ok := w.Next(base.Add(time.Hour), 0); ok {
		t.Fatalf("a window whose newest line is the bot's own has nothing to answer")
	}
}

func TestPendingLineStaysAnswerableWhenTheBotEchoIsNewer(t *testing.T) {
	w := window.New("GangolfBot")
	w.Ingest([]window.Line{
		say(channel, "dapper", "late question", base),
		say(channel, "GangolfBot", "echo of the earlier answer", base.Add(time.Second)),
	}, base)

	key, ok := w.Next(base.Add(time.Hour), 0)

	if !ok || key != channel {
		t.Fatalf("next = %v %v, want the channel with the pending line", key, ok)
	}
}

func TestBootstrapMarksEverythingAnsweredButKeepsItAsContext(t *testing.T) {
	w := window.New("GangolfBot")
	w.Ingest([]window.Line{
		say(channel, "vesper", "old question", base),
		say(channel, "GangolfBot", "old answer", base.Add(time.Second)),
		say(channel, "dapper", "older unanswered", base.Add(2*time.Second)),
	}, base)

	w.MarkAllAnswered()

	if _, ok := w.Next(base.Add(time.Hour), 0); ok {
		t.Fatalf("nothing may be pending after the bootstrap")
	}
	w.Ingest([]window.Line{say(channel, "rugen", "fresh", base.Add(time.Minute))}, base.Add(time.Minute))
	batch := w.Take(channel)
	if batch.Input != "rugen: fresh" || len(batch.History) != 2 {
		t.Fatalf("batch = %+v, want only the fresh line as input and both old exchanges as context", batch)
	}
}

func TestTakeMarksItsLinesAnswered(t *testing.T) {
	w := window.New("GangolfBot")
	w.Ingest([]window.Line{say(channel, "vesper", "once", base)}, base)

	w.Take(channel)

	if _, ok := w.Next(base.Add(time.Hour), 0); ok {
		t.Fatalf("a taken batch must not be pending again")
	}
	if again := w.Take(channel); again.Input != "" {
		t.Fatalf("second take = %+v, want empty", again)
	}
}

func TestNextWaitsUntilTheNewestPendingLineHasSettled(t *testing.T) {
	w := window.New("GangolfBot")
	settle := 2 * time.Second
	w.Ingest([]window.Line{say(channel, "vesper", "first", base)}, base)
	w.Ingest([]window.Line{say(channel, "dapper", "second", base)}, base.Add(time.Second))

	if _, ok := w.Next(base.Add(time.Second+settle-time.Nanosecond), settle); ok {
		t.Fatalf("must not settle before the newest arrival has been quiet for the settle time")
	}
	if _, ok := w.Next(base.Add(time.Second+settle), settle); !ok {
		t.Fatalf("must settle once the newest arrival has been quiet for the settle time")
	}
}

func TestNextPrefersTheChannelWhoseOldestPendingLineWaitedLongest(t *testing.T) {
	w := window.New("GangolfBot")
	w.Ingest([]window.Line{
		say(channel, "vesper", "newer", base.Add(5*time.Second)),
		say(other, "dapper", "older", base),
	}, base)

	key, ok := w.Next(base.Add(time.Hour), 0)

	if !ok || key != other {
		t.Fatalf("next = %v %v, want the channel that waited longest", key, ok)
	}
}

func TestPendingInputCarriesTheSpeakerOfEachLine(t *testing.T) {
	w := window.New("GangolfBot")
	w.Ingest([]window.Line{
		say(channel, "vesper", "he is asleep", base),
		say(channel, "dapper", "what about the pinkertons", base.Add(time.Second)),
	}, base)

	if got := w.Take(channel).Input; got != "vesper: he is asleep\ndapper: what about the pinkertons" {
		t.Fatalf("input = %q", got)
	}
}
