package main

import (
	"strings"
	"testing"
)

func TestOptionalMemoryConfigIsDisabledWhenAbsentOrBlank(t *testing.T) {
	for _, value := range []string{"", "   "} {
		cfg, err := loadConfig(env(map[string]string{"GANGOLF_API_KEY": "k.s", "GANGOLF_CHANNELS": "twitch/stream", "PROCESSOR_MEMORY_CONFIG": value}))
		if err != nil || cfg.memoryConfig != 0 {
			t.Fatalf("config=%+v err=%v", cfg, err)
		}
	}
}

func TestOptionalMemoryConfigRequiresPositiveIntegerWithoutEchoingValue(t *testing.T) {
	for _, bad := range []string{"0", "-5", "garbage", "9223372036854775808"} {
		_, err := loadConfig(env(map[string]string{"GANGOLF_API_KEY": "k.s", "GANGOLF_CHANNELS": "twitch/stream", "PROCESSOR_MEMORY_CONFIG": bad}))
		if err == nil || err.Error() != "PROCESSOR_MEMORY_CONFIG must be a positive integer" {
			t.Fatalf("err=%v", err)
		}
	}
	cfg, err := loadConfig(env(map[string]string{"GANGOLF_API_KEY": "k.s", "GANGOLF_CHANNELS": "twitch/stream", "PROCESSOR_MEMORY_CONFIG": " 90210 "}))
	if err != nil || cfg.memoryConfig != 90210 || !strings.Contains(strings.Join(cfg.set, ","), "PROCESSOR_MEMORY_CONFIG") {
		t.Fatalf("config=%+v err=%v", cfg, err)
	}
}
