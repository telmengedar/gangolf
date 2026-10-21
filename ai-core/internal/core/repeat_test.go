package core

import (
	"testing"

	"github.com/telmengedar/gangolf/ai-core/internal/window"
)

func TestRepeatedChecksOnlyNewestFourExchanges(t *testing.T) {
	history := []window.Exchange{
		{Reply: "fifth-oldest reply"},
		{Reply: "fourth-oldest reply"},
		{Reply: "third-oldest reply"},
		{Reply: "second-oldest reply"},
		{Reply: "newest reply"},
	}

	if repeated("fifth-oldest reply", history) {
		t.Fatal("older-than-fourth exchange should not count as visible history")
	}
	if !repeated("newest reply", history) {
		t.Fatal("newest exchange reply should count as visible history")
	}
}
