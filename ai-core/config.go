package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/telmengedar/gangolf/ai-core/internal/window"
)

const (
	routesVariable   = "GANGOLF_ROUTES"
	minWordsVariable = "GANGOLF_MIN_WORDS"

	defaultAPIURL       = "https://gangolf.mamgo.io/api"
	defaultBotName      = "GangolfBot"
	defaultProcessorURL = "http://gangolf:8080"
	defaultSubject      = 1090
	defaultConfig       = 17607
)

type config struct {
	apiURL          string
	apiKey          string
	channels        []window.Key
	routes          map[window.Key][]window.Key
	minWords        map[window.Key]int
	botName         string
	processorURL    string
	subject         int64
	processorConfig int64
	memoryConfig    int64
	set             []string
	defaulted       []string
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
		apiURL:          get("GANGOLF_API_URL", defaultAPIURL),
		apiKey:          get("GANGOLF_API_KEY", ""),
		botName:         get("GANGOLF_BOT_NAME", defaultBotName),
		processorURL:    get("PROCESSOR_URL", defaultProcessorURL),
		subject:         defaultSubject,
		processorConfig: defaultConfig,
	}
	if cfg.apiKey == "" {
		return config{}, fmt.Errorf("GANGOLF_API_KEY is required")
	}
	channels, err := parseChannels(get("GANGOLF_CHANNELS", ""))
	if err != nil {
		return config{}, err
	}
	cfg.channels = channels
	routes, err := parseRoutes(get(routesVariable, ""), channels)
	if err != nil {
		return config{}, err
	}
	cfg.routes = routes
	minWords, err := parseMinWords(get(minWordsVariable, ""), channels)
	if err != nil {
		return config{}, err
	}
	cfg.minWords = minWords
	if raw := get("PROCESSOR_SUBJECT", ""); raw != "" {
		subject, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || subject <= 0 {
			return config{}, fmt.Errorf("PROCESSOR_SUBJECT must be a positive integer")
		}
		cfg.subject = subject
	}
	if raw := get("PROCESSOR_CONFIG", ""); raw != "" {
		processorConfig, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || processorConfig <= 0 {
			return config{}, fmt.Errorf("PROCESSOR_CONFIG must be a positive integer")
		}
		cfg.processorConfig = processorConfig
	}
	raw := get("PROCESSOR_MEMORY_CONFIG", "")
	if raw == "" {
		return config{}, fmt.Errorf("PROCESSOR_MEMORY_CONFIG is required")
	}
	memoryConfig, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || memoryConfig <= 0 {
		return config{}, fmt.Errorf("PROCESSOR_MEMORY_CONFIG must be a positive integer")
	}
	cfg.memoryConfig = memoryConfig
	cfg.set, cfg.defaulted = set, defaulted
	return cfg, nil
}

func parseChannels(raw string) ([]window.Key, error) {
	if raw == "" {
		return nil, fmt.Errorf("GANGOLF_CHANNELS is required")
	}
	var keys []window.Key
	for _, item := range strings.Split(raw, ",") {
		key, ok := parseKey(item)
		if !ok {
			return nil, fmt.Errorf("GANGOLF_CHANNELS entries must look like service/channel")
		}
		keys = append(keys, key)
	}
	return keys, nil
}

func parseKey(item string) (window.Key, bool) {
	service, channel, found := strings.Cut(strings.TrimSpace(item), "/")
	if !found || service == "" || channel == "" {
		return window.Key{}, false
	}
	return window.Key{Service: service, Channel: channel}, true
}

type routeEntry struct {
	text    string
	source  window.Key
	targets []window.Key
	star    bool
}

func entryError(variable, entry, problem string) error {
	return fmt.Errorf("%s entry %q %s", variable, entry, problem)
}

func parseRouteEntry(item string) (routeEntry, error) {
	text := strings.TrimSpace(item)
	entry := routeEntry{text: text}
	left, right, found := strings.Cut(text, "=")
	source, ok := parseKey(left)
	if !found || !ok {
		return entry, entryError(routesVariable, text, "must look like service/channel=targets")
	}
	entry.source = source
	if right == "" {
		return entry, entryError(routesVariable, text, "has no targets")
	}
	parts := strings.Split(right, "|")
	for _, part := range parts {
		if strings.TrimSpace(part) == "*" {
			entry.star = true
			continue
		}
		target, ok := parseKey(part)
		if !ok {
			return entry, entryError(routesVariable, text, "targets must be * or service/channel joined by |")
		}
		for _, known := range entry.targets {
			if known == target {
				return entry, entryError(routesVariable, text, "lists a target twice")
			}
		}
		entry.targets = append(entry.targets, target)
	}
	if entry.star && len(parts) > 1 {
		return entry, entryError(routesVariable, text, "mixes * with explicit targets")
	}
	return entry, nil
}

func parseRoutes(raw string, channels []window.Key) (map[window.Key][]window.Key, error) {
	routes := make(map[window.Key][]window.Key, len(channels))
	for _, key := range channels {
		routes[key] = []window.Key{key}
	}
	if raw == "" {
		return routes, nil
	}
	var entries []routeEntry
	given := make(map[window.Key]bool)
	for _, item := range strings.Split(raw, ",") {
		entry, err := parseRouteEntry(item)
		if err != nil {
			return nil, err
		}
		if _, listed := routes[entry.source]; !listed {
			return nil, entryError(routesVariable, entry.text, "source is not in GANGOLF_CHANNELS")
		}
		if given[entry.source] {
			return nil, entryError(routesVariable, entry.text, "source is listed twice")
		}
		given[entry.source] = true
		entries = append(entries, entry)
	}
	for _, entry := range entries {
		routes[entry.source] = entry.targets
	}
	answering := answeringChannels(channels, routes)
	for _, entry := range entries {
		for _, target := range entry.targets {
			if _, listed := routes[target]; !listed {
				return nil, entryError(routesVariable, entry.text, "target is not in GANGOLF_CHANNELS")
			}
			if !answering[target] {
				return nil, entryError(routesVariable, entry.text, "target is routed to other channels and has no conversation of its own")
			}
		}
	}
	var everyone []window.Key
	for _, key := range channels {
		if answering[key] && !contains(everyone, key) {
			everyone = append(everyone, key)
		}
	}
	for _, entry := range entries {
		if !entry.star {
			continue
		}
		if len(everyone) == 0 {
			return nil, entryError(routesVariable, entry.text, "* resolves to no channel")
		}
		routes[entry.source] = everyone
	}
	return routes, nil
}

func answeringChannels(channels []window.Key, routes map[window.Key][]window.Key) map[window.Key]bool {
	answering := make(map[window.Key]bool, len(channels))
	for _, key := range channels {
		answering[key] = contains(routes[key], key)
	}
	return answering
}

func contains(keys []window.Key, key window.Key) bool {
	for _, known := range keys {
		if known == key {
			return true
		}
	}
	return false
}

func (c config) routed() int {
	count := 0
	for key, targets := range c.routes {
		if len(targets) != 1 || targets[0] != key {
			count++
		}
	}
	return count
}

func parseMinWords(raw string, channels []window.Key) (map[window.Key]int, error) {
	minimums := make(map[window.Key]int)
	if raw == "" {
		return minimums, nil
	}
	listed := make(map[window.Key]bool, len(channels))
	for _, key := range channels {
		listed[key] = true
	}
	for _, item := range strings.Split(raw, ",") {
		text := strings.TrimSpace(item)
		left, right, found := strings.Cut(text, "=")
		source, ok := parseKey(left)
		if !found || !ok {
			return nil, entryError(minWordsVariable, text, "must look like service/channel=n")
		}
		if !listed[source] {
			return nil, entryError(minWordsVariable, text, "source is not in GANGOLF_CHANNELS")
		}
		if _, given := minimums[source]; given {
			return nil, entryError(minWordsVariable, text, "source is listed twice")
		}
		minimum, err := strconv.Atoi(strings.TrimSpace(right))
		if err != nil || minimum < 1 {
			return nil, entryError(minWordsVariable, text, "needs a whole number of at least 1")
		}
		minimums[source] = minimum
	}
	return minimums, nil
}
