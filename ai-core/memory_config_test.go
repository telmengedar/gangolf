package main

import (
	"strings"
	"testing"
)

func TestMemoryPolicyIsRequiredWhenAbsentOrBlank(t *testing.T) {
	for _, values := range []map[string]string{
		{"GANGOLF_API_KEY": "k.s", "GANGOLF_CHANNELS": "twitch/stream"},
		{"GANGOLF_API_KEY": "k.s", "GANGOLF_CHANNELS": "twitch/stream", "PROCESSOR_MEMORY_CONFIG": ""},
		{"GANGOLF_API_KEY": "k.s", "GANGOLF_CHANNELS": "twitch/stream", "PROCESSOR_MEMORY_CONFIG": "   "},
	} {
		_, err := loadConfig(env(values))
		if err == nil || err.Error() != "PROCESSOR_MEMORY_CONFIG is required" {
			t.Fatalf("err=%v", err)
		}
	}
}

func TestMemoryPolicyRequiresPositiveIntegerWithoutEchoingValue(t *testing.T) {
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
