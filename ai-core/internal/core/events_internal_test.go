package core

import (
	"fmt"
	"io"
	"log/slog"
	"runtime"
	"testing"
	"time"

	"github.com/telmengedar/gangolf/ai-core/internal/messages"
	"github.com/telmengedar/gangolf/ai-core/internal/window"
)

func TestObservationAdmissionBoundAndFirstReadableUpdateLifetime(t *testing.T) {
	now := time.Now()
	key := window.Key{Service: "Twitch", Channel: "101"}
	c := New(Config{Routes: map[window.Key][]window.Key{key: {key}}}, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), func() time.Time { return now })
	c.observations = make(map[string]messageObservation)
	line := window.Line{ID: "1", Key: key, Time: now, Speaker: "Mara", Text: "first readable"}
	unknown := messages.Frame{ID: "1", Time: now, Line: &line, Updated: true}
	if err := c.observe(unknown, false); err != nil || len(c.observations) != 0 {
		t.Fatal("unknown update admitted as new work")
	}
	if err := c.observe(messages.Frame{ID: "1", Time: now}, false); err != nil {
		t.Fatal(err)
	}
	if err := c.observe(unknown, false); err != nil {
		t.Fatal(err)
	}
	if batch := c.windows.Take(key); batch.Pending != 1 {
		t.Fatal("retained unreadable row did not transition once")
	}
	now = now.Add(time.Hour)
	c.observe(unknown, false)
	if batch := c.windows.Take(key); batch.Pending != 0 {
		t.Fatal("old readable edit triggered again")
	}
	c.observe(messages.Frame{ID: "2", Time: now}, false)
	now = now.Add(window.MaxAge + time.Second)
	late := line
	late.ID = "2"
	c.observe(messages.Frame{ID: "2", Time: now.Add(-window.MaxAge - time.Second), Line: &late, Updated: true}, false)
	if batch := c.windows.Take(key); batch.Pending != 0 {
		t.Fatal("expired unreadable placeholder became public work")
	}
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for i := 3; i <= 32000; i++ {
		if err := c.observe(messages.Frame{ID: fmt.Sprint(i), Time: now}, false); err != nil {
			t.Fatal(err)
		}
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	err := c.observe(messages.Frame{ID: "32001", Time: now}, false)
	if len(c.observations) != 32000 || err == nil {
		t.Fatal("observation identities exceeded finite generation capacity")
	}
	t.Logf("observation_identities=%d allocated_bytes=%d observed_heap_delta_bytes=%d", len(c.observations), after.TotalAlloc-before.TotalAlloc, int64(after.HeapAlloc)-int64(before.HeapAlloc))
	runtime.KeepAlive(c)
}
func TestEventWindowEqualTimestampUsesNumericResourceIdentity(t *testing.T) {
	now := time.Now()
	key := window.Key{Service: "Twitch", Channel: "101"}
	w := window.New("")
	w.Ingest([]window.Line{{ID: "10", Key: key, Time: now, Speaker: "A", Text: "later"}}, now)
	w.Ingest([]window.Line{{ID: "2", Key: key, Time: now, Speaker: "B", Text: "earlier"}}, now)
	batch := w.Take(key)
	if batch.Input != "B: earlier\nA: later" {
		t.Fatalf("equal-time input=%q", batch.Input)
	}
}
