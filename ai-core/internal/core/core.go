package core

import (
	"context"
	"log/slog"
	"strings"
	"time"
	"unicode"

	"github.com/telmengedar/gangolf/ai-core/internal/address"
	"github.com/telmengedar/gangolf/ai-core/internal/processor"
	"github.com/telmengedar/gangolf/ai-core/internal/window"
)

const (
	// PollInterval is the pause between two polls of the message API.
	PollInterval = 2 * time.Second
	// Overlap is how far each poll reaches back before the newest line seen.
	Overlap = 10 * time.Second
	// Settle is how long a channel must stay quiet before its pending lines are answered.
	Settle = 2 * time.Second

	personaName = "Gangolf"

	outcomeAddressedElsewhere = "addressed-elsewhere"
)

// Messages is the online message API as the core uses it.
type Messages interface {
	List(ctx context.Context, from time.Time) ([]window.Line, error)
	Post(ctx context.Context, key window.Key, text string) error
}

// Runner is processor's run endpoint as the core uses it.
type Runner interface {
	Run(ctx context.Context, input string, history []window.Exchange) (processor.Result, error)
}

// Config carries what the core needs to know about its deployment.
type Config struct {
	Channels []window.Key
	BotName  string
	Interval time.Duration
}

// Core polls, windows, runs and posts.
type Core struct {
	allowed  map[window.Key]bool
	interval time.Duration
	windows  *window.Windows
	messages Messages
	runner   Runner
	log      *slog.Logger
	now      func() time.Time
	labels   []string
	cursor   time.Time
	fallback time.Time
	unlisted map[window.Key]bool
	closing  map[window.Key]bool
}

// New returns a core wired to its two collaborators; now supplies the clock.
func New(cfg Config, messages Messages, runner Runner, log *slog.Logger, now func() time.Time) *Core {
	allowed := make(map[window.Key]bool, len(cfg.Channels))
	for _, key := range cfg.Channels {
		allowed[key] = true
	}
	if cfg.Interval == 0 {
		cfg.Interval = PollInterval
	}
	return &Core{
		unlisted: make(map[window.Key]bool),
		closing:  make(map[window.Key]bool),
		allowed:  allowed,
		interval: cfg.Interval,
		labels:   []string{cfg.BotName, personaName},
		windows:  window.New(cfg.BotName),
		messages: messages,
		runner:   runner,
		log:      log,
		now:      now,
	}
}

// Run bootstraps, retrying until it succeeds, and then ticks until the context ends.
func (c *Core) Run(ctx context.Context) {
	for {
		err := c.Bootstrap(ctx)
		if err == nil {
			break
		}
		c.log.Error("bootstrap poll failed", "error", err.Error())
		if !sleep(ctx, c.interval) {
			return
		}
	}
	for sleep(ctx, c.interval) {
		c.Tick(ctx)
	}
}

// Bootstrap loads the recent conversation as context and marks all of it answered.
func (c *Core) Bootstrap(ctx context.Context) error {
	c.fallback = c.now().Add(-window.MaxAge)
	if err := c.poll(ctx); err != nil {
		return err
	}
	c.windows.MarkAllAnswered()
	return nil
}

// Tick polls once and answers at most one settled channel.
func (c *Core) Tick(ctx context.Context) {
	c.refresh(ctx)
	now := c.now()
	c.windows.Evict(now)
	if key, ok := c.windows.Next(now, Settle); ok {
		if c.turn(ctx, key) {
			c.closing[key] = true
			c.refresh(ctx)
		}
	}
}

func (c *Core) refresh(ctx context.Context) {
	if err := c.poll(ctx); err != nil {
		c.log.Error("poll failed", "error", err.Error())
	}
}

func (c *Core) from() time.Time {
	if c.cursor.IsZero() {
		return c.fallback
	}
	return c.cursor.Add(-Overlap)
}

func (c *Core) poll(ctx context.Context) error {
	lines, err := c.messages.List(ctx, c.from())
	if err != nil {
		return err
	}
	var allowed []window.Line
	for _, line := range lines {
		if line.Time.After(c.cursor) {
			c.cursor = line.Time
		}
		if c.allowed[line.Key] {
			allowed = append(allowed, line)
			continue
		}
		if !c.unlisted[line.Key] {
			c.unlisted[line.Key] = true
			c.log.Warn("ignoring a channel that is not in GANGOLF_CHANNELS", "service", line.Service, "channel", line.Channel)
		}
	}
	added := c.windows.Ingest(allowed, c.now())
	c.windows.Forget(c.cursor.Add(-2 * Overlap))
	if added > 0 {
		c.log.Info("ingested", "lines", added)
	}
	for key := range c.closing {
		if kept := c.windows.MarkAnswered(key); kept > 0 {
			c.log.Info("kept lines written during the run as context", "service", key.Service, "channel", key.Channel, "lines", kept)
		}
		delete(c.closing, key)
	}
	return nil
}

func (c *Core) turn(ctx context.Context, key window.Key) bool {
	batch := c.windows.Take(key)
	if c.addressedElsewhere(key, batch) {
		c.logTurn(key, batch, 0, 0, true, outcomeAddressedElsewhere)
		return false
	}
	started := c.now()
	result, err := c.runner.Run(ctx, batch.Input, batch.History)
	took := c.now().Sub(started)
	if err != nil {
		c.log.Error("run failed", "channel", key.Channel, "pending", batch.Pending, "dropped", batch.Dropped, "error", err.Error())
		return true
	}
	text := sanitize(result.Reply, c.labels)
	silent := result.Silent || text == ""
	c.logTurn(key, batch, took, len(text), silent, outcome(result, silent))
	if silent {
		return true
	}
	if err := c.messages.Post(ctx, key, text); err != nil {
		c.log.Error("post failed", "channel", key.Channel, "error", err.Error())
	}
	return true
}

func (c *Core) addressedElsewhere(key window.Key, batch window.Batch) bool {
	candidates := append(c.windows.Roster(key), key.Channel)
	for _, line := range batch.Lines {
		if !address.ForSomeoneElse(line.Speaker, line.Text, candidates, c.labels) {
			return false
		}
	}
	return true
}

func (c *Core) logTurn(key window.Key, batch window.Batch, took time.Duration, replyBytes int, silent bool, kind string) {
	c.log.Info("turn",
		"service", key.Service,
		"channel", key.Channel,
		"pending", batch.Pending,
		"dropped", batch.Dropped,
		"exchanges", len(batch.History),
		"inputBytes", len(batch.Input),
		"tookMs", took.Milliseconds(),
		"replyBytes", replyBytes,
		"silent", silent,
		"outcome", kind)
}

func outcome(result processor.Result, silent bool) string {
	switch {
	case result.Chosen:
		return "chose-silence"
	case result.Failed:
		return "failed"
	case silent:
		return "empty"
	default:
		return "replied"
	}
}

func sanitize(reply string, labels []string) string {
	text := trimLead(reply)
	for {
		stripped, ok := stripLabel(text, labels)
		if !ok {
			return strings.TrimRightFunc(text, unicode.IsSpace)
		}
		text = trimLead(stripped)
	}
}

func trimLead(text string) string {
	return strings.TrimLeftFunc(text, func(r rune) bool { return r == '!' || unicode.IsSpace(r) })
}

func stripLabel(text string, labels []string) (string, bool) {
	for _, label := range labels {
		n := len(label)
		if n == 0 || len(text) <= n || text[n] != ':' || !strings.EqualFold(text[:n], label) {
			continue
		}
		return text[n+1:], true
	}
	return text, false
}

func sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
