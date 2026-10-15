package window_test

import (
	"testing"
	"time"

	"github.com/telmengedar/gangolf/ai-core/internal/window"
)

func TestTakeCarriesWhetherALineCanStartARun(t *testing.T) {
	w := window.New("GangolfBot")
	quiet := say(channel, "rugenforth", "mhm", base)
	quiet.CannotStartRun = true
	w.Ingest([]window.Line{quiet, say(channel, "vesper", "hello", base.Add(time.Second))}, base)

	batch := w.Take(channel)

	if len(batch.Lines) != 2 {
		t.Fatalf("lines = %+v, want both pending lines", batch.Lines)
	}
	if !batch.Lines[0].CannotStartRun || batch.Lines[1].CannotStartRun {
		t.Fatalf("lines = %+v, want only the first marked", batch.Lines)
	}
}
