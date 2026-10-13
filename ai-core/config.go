package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/telmengedar/gangolf/ai-core/internal/window"
)

const (
	defaultAPIURL       = "https://gangolf.mamgo.io/api"
	defaultBotName      = "GangolfBot"
	defaultProcessorURL = "http://gangolf:8080"
	defaultSubject      = 1090
)

type config struct {
	apiURL       string
	apiKey       string
	channels     []window.Key
	botName      string
	processorURL string
	subject      int64
	set          []string
	defaulted    []string
}

func loadConfig(lookup func(string) (string, bool)) (config, error) {
	var set, defaulted []string
	get := func(name, fallback string) string {
		if value, ok := lookup(name); ok && strings.TrimSpace(value) != "" {
			set = append(set, name)
			return strings.TrimSpace(value)
		}
		defaulted = append(defaulted, name)
		return fallback
	}
	cfg := config{
		apiURL:       get("GANGOLF_API_URL", defaultAPIURL),
		apiKey:       get("GANGOLF_API_KEY", ""),
		botName:      get("GANGOLF_BOT_NAME", defaultBotName),
		processorURL: get("PROCESSOR_URL", defaultProcessorURL),
		subject:      defaultSubject,
	}
	if cfg.apiKey == "" {
		return config{}, fmt.Errorf("GANGOLF_API_KEY is required")
	}
	channels, err := parseChannels(get("GANGOLF_CHANNELS", ""))
	if err != nil {
		return config{}, err
	}
	cfg.channels = channels
	if raw := get("PROCESSOR_SUBJECT", ""); raw != "" {
		subject, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || subject <= 0 {
			return config{}, fmt.Errorf("PROCESSOR_SUBJECT must be a positive integer")
		}
		cfg.subject = subject
	}
	cfg.set, cfg.defaulted = set, defaulted
	return cfg, nil
}

func parseChannels(raw string) ([]window.Key, error) {
	if raw == "" {
		return nil, fmt.Errorf("GANGOLF_CHANNELS is required")
	}
	var keys []window.Key
	for _, item := range strings.Split(raw, ",") {
		service, channel, found := strings.Cut(strings.TrimSpace(item), "/")
		if !found || service == "" || channel == "" {
			return nil, fmt.Errorf("GANGOLF_CHANNELS entries must look like service/channel")
		}
		keys = append(keys, window.Key{Service: service, Channel: channel})
	}
	return keys, nil
}
