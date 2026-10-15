package window_test

import (
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/telmengedar/gangolf/ai-core/internal/window"
)

func rosterOf(w *window.Windows, key window.Key) []string {
	names := w.Roster(key)
	sort.Strings(names)
	return names
}

func TestSpeakersStayInTheRosterAfterTheirLinesAreEvicted(t *testing.T) {
	w := window.New("GangolfBot")
	w.Ingest([]window.Line{say(channel, "vesper", "see you later", base)}, base)
	w.Take(channel)

	later := base.Add(sevenMinutes + time.Minute)
	w.Evict(later)
	w.Ingest([]window.Line{say(channel, "dapper", "anyone here", later)}, later)

	if got := w.Take(channel); len(got.History) != 0 {
		t.Fatalf("history = %+v, want the old line evicted before the roster is read", got.History)
	}
	if got := rosterOf(w, channel); !reflect.DeepEqual(got, []string{"dapper", "vesper"}) {
		t.Fatalf("roster = %v, want both speakers kept", got)
	}
}

func TestBotLinesDoNotJoinTheRoster(t *testing.T) {
	w := window.New("GangolfBot")
	w.Ingest([]window.Line{
		say(channel, "GangolfBot", "an answer", base),
		say(channel, "gangolfbot", "another answer", base.Add(time.Second)),
	}, base)

	if got := w.Roster(channel); len(got) != 0 {
		t.Fatalf("roster = %v, want the bot left out", got)
	}
}

func TestTheRosterHoldsEachLowerCasedNameOnceAndKeepsChannelsApart(t *testing.T) {
	w := window.New("GangolfBot")
	w.Ingest([]window.Line{
		say(channel, "Vesper", "one", base),
		say(channel, "dapper", "two", base.Add(time.Second)),
		say(channel, "VESPER", "three", base.Add(2*time.Second)),
		say(other, "toni", "four", base.Add(3*time.Second)),
	}, base)

	if got := rosterOf(w, channel); !reflect.DeepEqual(got, []string{"dapper", "vesper"}) {
		t.Fatalf("roster = %v, want each name once and lower-cased", got)
	}
	if got := w.Roster(other); !reflect.DeepEqual(got, []string{"toni"}) {
		t.Fatalf("other roster = %v, want only its own speaker", got)
	}
	if got := w.Roster(window.Key{Service: "twitch", Channel: "nobody"}); len(got) != 0 {
		t.Fatalf("unknown channel roster = %v, want empty", got)
	}
}

func TestTheBatchCarriesItsPendingLinesAsSpeakerAndText(t *testing.T) {
	w := window.New("GangolfBot")
	w.Ingest([]window.Line{
		say(channel, "Vesper", "hello", base),
		say(channel, "dapper", "bye: now", base.Add(time.Second)),
	}, base)

	got := w.Take(channel).Lines

	want := []window.Utterance{{Speaker: "Vesper", Text: "hello"}, {Speaker: "dapper", Text: "bye: now"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("lines = %+v, want %+v", got, want)
	}
}

func TestTheBatchLinesHoldOnlyTheLinesTheInputKept(t *testing.T) {
	w := window.New("GangolfBot")
	var lines []window.Line
	for i := 0; i < tenPending+2; i++ {
		lines = append(lines, say(channel, "vesper", string(rune('a'+i)), base.Add(time.Duration(i)*time.Second)))
	}
	w.Ingest(lines, base)

	batch := w.Take(channel)

	if len(batch.Lines) != tenPending || batch.Lines[0].Text != "c" || batch.Dropped != 2 {
		t.Fatalf("lines = %d starting %+v with %d dropped, want the newest %d", len(batch.Lines), batch.Lines[0], batch.Dropped, tenPending)
	}
}
