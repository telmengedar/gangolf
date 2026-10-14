package messages

import (
	"testing"
	"time"
)

func TestDefaultClientIsBoundedToThirtySeconds(t *testing.T) {
	if got := New("http://example.invalid", "k", "b", nil, nil).http.Timeout; got != 30*time.Second {
		t.Fatalf("default timeout = %v, want 30s", got)
	}
}
