package processor

import (
	"testing"
	"time"
)

func TestDefaultClientWaitsSevenHundredSecondsForARun(t *testing.T) {
	if got := New("http://example.invalid", 1090, 17607, nil).http.Timeout; got != 700*time.Second {
		t.Fatalf("default timeout = %v, want 700s", got)
	}
}
