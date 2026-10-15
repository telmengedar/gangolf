package window

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	// MaxLines is how many of the newest lines a channel window keeps.
	MaxLines = 10
	// MaxAge is how old a line may get before it leaves the window.
	MaxAge = 7 * time.Minute
	// PendingCap is how many pending lines one turn answers at most.
	PendingCap = 10
)

// Key identifies one chat channel on one service.
type Key struct {
	Service string
	Channel string
}

// Line is one chat message as the window stores it.
type Line struct {
	Key
	Speaker string
	Text    string
	Time    time.Time
}

// Exchange is one earlier turn: what was said and, when Gangolf answered, the answer.
type Exchange struct {
	Input string
	Reply string
}

// Utterance is one pending line as a speaker and a text.
type Utterance struct {
	Speaker string
	Text    string
}

// Batch is one turn's material: the lines to answer and the conversation before them.
type Batch struct {
	Input   string
	Lines   []Utterance
	History []Exchange
	Pending int
	Dropped int
}

type entry struct {
	Line
	arrived time.Time
	bot     bool
	pending bool
}

// Windows holds the per-channel line windows and the dedupe memory of one core.
type Windows struct {
	bot    string
	seen   map[string]time.Time
	lines  map[Key][]*entry
	roster map[Key]map[string]bool
}

// New returns empty windows that treat speaker botName as Gangolf's own lines.
func New(botName string) *Windows {
	return &Windows{
		bot:    botName,
		seen:   make(map[string]time.Time),
		lines:  make(map[Key][]*entry),
		roster: make(map[Key]map[string]bool),
	}
}

// Ingest adds lines not seen before, marks non-bot lines pending and returns how many were new.
func (w *Windows) Ingest(lines []Line, now time.Time) int {
	added := 0
	for _, line := range lines {
		fp := fingerprint(line)
		if _, known := w.seen[fp]; known {
			continue
		}
		w.seen[fp] = line.Time
		bot := strings.EqualFold(line.Speaker, w.bot)
		if !bot {
			w.remember(line.Key, line.Speaker)
		}
		w.lines[line.Key] = append(w.lines[line.Key], &entry{
			Line:    line,
			arrived: now,
			bot:     bot,
			pending: !bot,
		})
		added++
	}
	for _, entries := range w.lines {
		sort.SliceStable(entries, func(i, j int) bool {
			return entries[i].Time.Before(entries[j].Time)
		})
	}
	return added
}

func (w *Windows) remember(key Key, speaker string) {
	if w.roster[key] == nil {
		w.roster[key] = make(map[string]bool)
	}
	w.roster[key][strings.ToLower(speaker)] = true
}

// Roster returns the lower-cased names of every non-bot speaker ingested on a channel.
func (w *Windows) Roster(key Key) []string {
	names := make([]string, 0, len(w.roster[key]))
	for name := range w.roster[key] {
		names = append(names, name)
	}
	return names
}

// MarkAllAnswered clears every pending line.
func (w *Windows) MarkAllAnswered() {
	for _, entries := range w.lines {
		for _, e := range entries {
			e.pending = false
		}
	}
}

// MarkAnswered clears the pending lines of one channel and returns how many it cleared.
func (w *Windows) MarkAnswered(key Key) int {
	cleared := 0
	for _, e := range w.lines[key] {
		if e.pending {
			e.pending = false
			cleared++
		}
	}
	return cleared
}

// Forget drops dedupe memory of lines stamped before the given time.
func (w *Windows) Forget(before time.Time) {
	for fp, stamp := range w.seen {
		if stamp.Before(before) {
			delete(w.seen, fp)
		}
	}
}

// Evict applies the line-count and age limits; pending lines are exempt from both.
func (w *Windows) Evict(now time.Time) {
	for key, entries := range w.lines {
		kept := make([]*entry, 0, len(entries))
		rank := 0
		for i := len(entries) - 1; i >= 0; i-- {
			rank++
			e := entries[i]
			if e.pending || (rank <= MaxLines && now.Sub(e.Time) <= MaxAge) {
				kept = append(kept, e)
			}
		}
		for i, j := 0, len(kept)-1; i < j; i, j = i+1, j-1 {
			kept[i], kept[j] = kept[j], kept[i]
		}
		w.lines[key] = kept
	}
}

// Next picks the channel to answer: settled pending lines, oldest waiting first.
func (w *Windows) Next(now time.Time, settle time.Duration) (Key, bool) {
	var best Key
	var bestOldest time.Time
	found := false
	for key, entries := range w.lines {
		var oldest, newestArrival time.Time
		pending := false
		for _, e := range entries {
			if !e.pending {
				continue
			}
			if !pending || e.Time.Before(oldest) {
				oldest = e.Time
			}
			if !pending || e.arrived.After(newestArrival) {
				newestArrival = e.arrived
			}
			pending = true
		}
		if !pending || now.Sub(newestArrival) < settle {
			continue
		}
		if !found || oldest.Before(bestOldest) || (oldest.Equal(bestOldest) && keyLess(key, best)) {
			best, bestOldest, found = key, oldest, true
		}
	}
	return best, found
}

// Take builds the turn for a channel and marks its pending lines answered.
func (w *Windows) Take(key Key) Batch {
	entries := w.lines[key]
	var pending []*entry
	for _, e := range entries {
		if e.pending {
			pending = append(pending, e)
		}
	}
	if len(pending) == 0 {
		return Batch{}
	}
	dropped := 0
	if len(pending) > PendingCap {
		dropped = len(pending) - PendingCap
		for _, e := range pending[:dropped] {
			e.pending = false
		}
		pending = pending[dropped:]
	}
	lines := make([]string, len(pending))
	spoken := make([]Utterance, len(pending))
	for i, e := range pending {
		lines[i] = speak(e)
		spoken[i] = Utterance{Speaker: e.Speaker, Text: e.Text}
	}
	batch := Batch{
		Input:   strings.Join(lines, "\n"),
		Lines:   spoken,
		History: history(entries),
		Pending: len(pending),
		Dropped: dropped,
	}
	for _, e := range pending {
		e.pending = false
	}
	return batch
}

func history(entries []*entry) []Exchange {
	exchanges := []Exchange{}
	var user, bot []string
	flush := func() {
		if len(user) > 0 {
			exchanges = append(exchanges, Exchange{Input: strings.Join(user, "\n"), Reply: strings.Join(bot, "\n")})
		}
		user, bot = nil, nil
	}
	for _, e := range entries {
		if e.pending {
			continue
		}
		switch {
		case e.bot:
			bot = append(bot, e.Text)
		default:
			if len(bot) > 0 {
				flush()
			}
			user = append(user, speak(e))
		}
	}
	flush()
	return exchanges
}

func speak(e *entry) string {
	return e.Speaker + ": " + e.Text
}

func fingerprint(line Line) string {
	return fmt.Sprintf("%s\x00%s\x00%s\x00%d\x00%s", line.Service, line.Channel, line.Speaker, line.Time.UnixNano(), line.Text)
}

func keyLess(a, b Key) bool {
	if a.Service != b.Service {
		return a.Service < b.Service
	}
	return a.Channel < b.Channel
}
