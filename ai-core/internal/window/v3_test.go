package window_test

import (
	"github.com/telmengedar/gangolf/ai-core/internal/window"
	"testing"
	"time"
)

func TestV3DistinctMessageIDsAndOverlap(t *testing.T) {
	w := window.New("GangolfBot")
	a := window.Line{Key: channel, ID: "1", Speaker: "viewer", Text: "identical", Time: base}
	b := a
	b.ID = "2"
	if got := w.Ingest([]window.Line{a, b, a}, base); got != 2 {
		t.Fatalf("added=%d", got)
	}
	if got := w.Ingest([]window.Line{b, a}, base.Add(time.Second)); got != 0 {
		t.Fatalf("overlap added=%d", got)
	}
	target := a
	target.Key = other
	if got := w.Ingest([]window.Line{target}, base); got != 1 {
		t.Fatalf("routed added=%d", got)
	}
	if batch := w.Take(channel); batch.Pending != 2 {
		t.Fatalf("batch=%+v", batch)
	}
}
func TestV3OwnUsesIdentityInsteadOfDisplayName(t *testing.T) {
	w := window.New("GangolfBot")
	w.Ingest([]window.Line{{Key: channel, ID: "1", Own: true, Speaker: "newname", Text: "my echo", Time: base}}, base)
	if _, ready := w.Next(base.Add(time.Hour), time.Second); ready {
		t.Fatal("own echo became pending")
	}
	if roster := w.Roster(channel); len(roster) != 0 {
		t.Fatalf("roster=%v", roster)
	}
	w.Ingest([]window.Line{{Key: channel, ID: "2", Speaker: "GangolfBot", Text: "human", Time: base}}, base)
	if batch := w.Take(channel); batch.Pending != 1 || batch.Input != "GangolfBot: human" {
		t.Fatalf("batch=%+v", batch)
	}
}
