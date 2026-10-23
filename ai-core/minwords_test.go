package main

import (
	"strings"
	"testing"
)

func minWordsOf(channels, minimums string) (config, error) {
	values := map[string]string{"GANGOLF_API_KEY": "k.s", "PROCESSOR_MEMORY_CONFIG": "88", "GANGOLF_CHANNELS": channels}
	if minimums != "" {
		values["GANGOLF_MIN_WORDS"] = minimums
	}
	return loadConfig(env(values))
}

func requireMinWordsError(t *testing.T, channels, minimums, phrase string) {
	t.Helper()
	_, err := minWordsOf(channels, minimums)
	if err == nil || !strings.Contains(err.Error(), "GANGOLF_MIN_WORDS") || !strings.Contains(err.Error(), phrase) {
		t.Fatalf("min words %q: err = %v, want GANGOLF_MIN_WORDS named with %q", minimums, err, phrase)
	}
}

func TestWithoutMinWordsNoSourceHasAThreshold(t *testing.T) {
	cfg, err := minWordsOf("localhost/host,twitch/stream", "")

	if err != nil {
		t.Fatal(err)
	}
	if cfg.minWords == nil || len(cfg.minWords) != 0 {
		t.Fatalf("minWords = %v, want an empty map", cfg.minWords)
	}
}

func TestBlankMinWordsCountAsUnset(t *testing.T) {
	cfg, err := loadConfig(env(map[string]string{"GANGOLF_API_KEY": "k.s", "PROCESSOR_MEMORY_CONFIG": "88", "GANGOLF_CHANNELS": "twitch/stream", "GANGOLF_MIN_WORDS": "  "}))

	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.minWords) != 0 {
		t.Fatalf("minWords = %v, want none", cfg.minWords)
	}
}

func TestMinWordsAppliesOnlyToItsSource(t *testing.T) {
	cfg, err := minWordsOf("localhost/host,twitch/stream,discord/general", "localhost/host=4, discord/general = 2")

	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.minWords) != 2 || cfg.minWords[hostKey] != 4 || cfg.minWords[discordKey] != 2 {
		t.Fatalf("minWords = %v, want host 4 and discord 2 and nothing for twitch", cfg.minWords)
	}
	if _, ok := cfg.minWords[streamKey]; ok {
		t.Fatalf("minWords = %v, want no threshold for twitch", cfg.minWords)
	}
}

func TestAMinimumOfOneIsAccepted(t *testing.T) {
	cfg, err := minWordsOf("localhost/host", "localhost/host=1")

	if err != nil {
		t.Fatal(err)
	}
	if cfg.minWords[hostKey] != 1 {
		t.Fatalf("minWords = %v", cfg.minWords)
	}
}

func TestAMalformedMinWordsEntryIsRejected(t *testing.T) {
	for _, bad := range []string{"justaname", "localhost/host", "localhost=4", "=4", "localhost/host=4,", ",localhost/host=4"} {
		requireMinWordsError(t, "localhost/host,twitch/stream", bad, "must look like service/channel=n")
	}
}

func TestAMinWordsEntryForAnUnlistedChannelIsRejected(t *testing.T) {
	requireMinWordsError(t, "twitch/stream", "localhost/host=4", "source is not in GANGOLF_CHANNELS")
}

func TestAMinWordsEntryListedTwiceIsRejected(t *testing.T) {
	requireMinWordsError(t, "localhost/host", "localhost/host=4,localhost/host=5", "source is listed twice")
}

func TestAMinimumBelowOneOrNotANumberIsRejected(t *testing.T) {
	for _, bad := range []string{"localhost/host=0", "localhost/host=-3", "localhost/host=abc", "localhost/host=", "localhost/host=2.5", "localhost/host=99999999999999999999"} {
		requireMinWordsError(t, "localhost/host,twitch/stream", bad, "needs a whole number of at least 1")
	}
}
