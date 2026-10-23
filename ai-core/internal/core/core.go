package core

import (
	"context"
	"fmt"
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
	// repeatHistoryExchangeLimit mirrors processor HistoryExchangeLimit from
	// DiVoid #16291/#16334. Processor may cut further for its byte safety limit.
	repeatHistoryExchangeLimit = 4

	outcomeAddressedElsewhere = "addressed-elsewhere"
	outcomeTooShort           = "too-short"
	repeatViolationWindow     = time.Hour
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
	Routes         map[window.Key][]window.Key
	MinWords       map[window.Key]int
	BotName        string
	Interval       time.Duration
	MemoryReviewer MemoryReviewer
}

// Core polls, windows, runs and posts.
type Core struct {
	routes         map[window.Key][]window.Key
	minWords       map[window.Key]int
	interval       time.Duration
	windows        *window.Windows
	messages       Messages
	runner         Runner
	log            *slog.Logger
	now            func() time.Time
	labels         []string
	cursor         time.Time
	fallback       time.Time
	unlisted       map[window.Key]bool
	closing        map[window.Key]bool
	violations     map[window.Key][]time.Time
	memoryReviewer MemoryReviewer
	memoryJobs     chan memoryJob
}

// New returns a core wired to its two collaborators; now supplies the clock.
func New(cfg Config, messages Messages, runner Runner, log *slog.Logger, now func() time.Time) *Core {
	if cfg.Interval == 0 {
		cfg.Interval = PollInterval
	}
	return &Core{
		unlisted:       make(map[window.Key]bool),
		closing:        make(map[window.Key]bool),
		violations:     make(map[window.Key][]time.Time),
		memoryReviewer: cfg.MemoryReviewer,
		routes:         cfg.Routes,
		minWords:       cfg.MinWords,
		interval:       cfg.Interval,
		labels:         []string{cfg.BotName, personaName},
		windows:        window.New(cfg.BotName),
		messages:       messages,
		runner:         runner,
		log:            log,
		now:            now,
	}
}

// Run bootstraps, retrying until it succeeds, and then ticks until the context ends.
func (c *Core) Run(ctx context.Context) {
	if c.memoryReviewer != nil {
		workerCtx, cancel := context.WithCancel(ctx)
		c.memoryJobs = make(chan memoryJob, 1)
		done := make(chan struct{})
		go func() {
			defer close(done)
			c.reviewMemory(workerCtx)
		}()
		defer func() {
			cancel()
			<-done
			c.memoryJobs = nil
		}()
	}
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
	var routed []window.Line
	for _, line := range lines {
		if line.Time.After(c.cursor) {
			c.cursor = line.Time
		}
		if targets, ok := c.routes[line.Key]; ok {
			minimum := c.minWords[line.Key]
			short := minimum > 0 && words(line.Text) < minimum
			for _, target := range targets {
				copied := line
				copied.Key = target
				copied.CannotStartRun = short
				routed = append(routed, copied)
			}
			continue
		}
		if !c.unlisted[line.Key] {
			c.unlisted[line.Key] = true
			c.log.Warn("ignoring a channel that is not in GANGOLF_CHANNELS", "service", line.Service, "channel", line.Channel)
		}
	}
	added := c.windows.Ingest(routed, c.now())
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
	if cannotStartRun(batch) {
		c.logTurn(key, batch, 0, 0, true, outcomeTooShort)
		return false
	}
	if c.addressedElsewhere(key, batch) {
		c.logTurn(key, batch, 0, 0, true, outcomeAddressedElsewhere)
		return false
	}
	now := c.now()
	c.pruneViolations(key, now)
	input := c.withMuteNotice(key, batch.Input)
	started := now
	result, err := c.runner.Run(ctx, input, batch.History)
	took := c.now().Sub(started)
	if err != nil {
		c.log.Error("run failed", "channel", key.Channel, "pending", batch.Pending, "dropped", batch.Dropped, "error", err.Error())
		return true
	}
	text := sanitize(result.Reply, c.labels)
	silent := result.Silent || text == ""
	if !silent && repeated(text, batch.History) {
		notice := fmt.Sprintf("You violated your contract and failed to behave like a natural conversation partner. Your answer %q was not sent: it repeats a reply you already gave earlier in this conversation. Reply to the request with something you have not said before.", text)
		retryInput := appendNotice(input, notice)
		c.log.Info("repeat detected; requesting a fresh answer", "service", key.Service, "channel", key.Channel, "outcome", "repeat-reask", "violations", len(c.violations[key]))
		retryStarted := c.now()
		retry, retryErr := c.runner.Run(ctx, retryInput, batch.History)
		took += c.now().Sub(retryStarted)
		if retryErr != nil {
			c.log.Error("repeat re-ask failed", "service", key.Service, "channel", key.Channel, "error", retryErr.Error())
			c.logTurn(key, batch, took, 0, true, "repeat-reask-failed")
			return true
		}
		text = sanitize(retry.Reply, c.labels)
		silent = retry.Silent || text == ""
		result = retry
		if !silent && repeated(text, batch.History) {
			c.violations[key] = append(c.violations[key], c.now())
			count := len(c.violations[key])
			c.log.Info("repeated retry withheld", "service", key.Service, "channel", key.Channel, "outcome", "repeat-withheld", "violations", count, "window", repeatViolationWindow)
			c.logTurn(key, batch, took, len(text), true, "repeat-withheld")
			return true
		}
	}
	c.logTurn(key, batch, took, len(text), silent, outcome(result, silent))
	if silent {
		return true
	}
	if err := c.messages.Post(ctx, key, text); err != nil {
		c.log.Error("post failed", "channel", key.Channel, "error", err.Error())
		return true
	}
	c.offerMemory(ctx, key, batch, text)
	return true
}

// Violations expire after one hour without another repeated retry.
func (c *Core) pruneViolations(key window.Key, now time.Time) {
	cutoff := now.Add(-repeatViolationWindow)
	items := c.violations[key]
	first := 0
	for first < len(items) && !items[first].After(cutoff) {
		first++
	}
	if first == len(items) {
		delete(c.violations, key)
	} else if first > 0 {
		c.violations[key] = append([]time.Time(nil), items[first:]...)
	}
}

func (c *Core) withMuteNotice(key window.Key, input string) string {
	count := len(c.violations[key])
	if count == 0 {
		return input
	}
	return appendNotice(input, fmt.Sprintf("You violated your contract %d times in the last hour and are currently muted. Your response is delivered only after you start following your task.", count))
}

func appendNotice(input, notice string) string {
	if input == "" {
		return notice
	}
	return input + "\n\n" + notice
}

// repeated applies the measured case-folded, whitespace-normalized predicate
// to each prior reply and to each line within a multiline prior reply.
func repeated(reply string, history []window.Exchange) bool {
	want := normalizeReply(reply)
	if want == "" {
		return false
	}
	// Processor renders only the newest four exchanges; its byte safety cut
	// can make the visible history smaller still.
	if len(history) > repeatHistoryExchangeLimit {
		history = history[len(history)-repeatHistoryExchangeLimit:]
	}
	for _, exchange := range history {
		for _, previous := range strings.Split(exchange.Reply, "\n") {
			if strings.EqualFold(normalizeReply(previous), want) {
				return true
			}
		}
		if strings.EqualFold(normalizeReply(exchange.Reply), want) {
			return true
		}
	}
	return false
}

func normalizeReply(reply string) string {
	text := strings.TrimSpace(reply)
	text = strings.TrimRightFunc(text, func(r rune) bool { return strings.ContainsRune(".!?…", r) })
	for _, pair := range [][2]string{{"\"", "\""}, {"'", "'"}, {"“", "”"}, {"‘", "’"}, {"`", "`"}} {
		if strings.HasPrefix(text, pair[0]) && strings.HasSuffix(text, pair[1]) && len(text) >= len(pair[0])+len(pair[1]) {
			text = strings.TrimSpace(text[len(pair[0]) : len(text)-len(pair[1])])
			break
		}
	}
	text = strings.TrimRightFunc(text, func(r rune) bool { return strings.ContainsRune(".!?…", r) })
	return strings.Join(strings.Fields(text), " ")
}

func cannotStartRun(batch window.Batch) bool {
	for _, line := range batch.Lines {
		if !line.CannotStartRun {
			return false
		}
	}
	return true
}

func words(text string) int {
	count := 0
	for _, token := range strings.Fields(text) {
		if strings.ContainsFunc(token, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) {
			count++
		}
	}
	return count
}

func (c *Core) addressedElsewhere(key window.Key, batch window.Batch) bool {
	candidates := append(c.windows.Roster(key), key.Channel)
	for _, line := range batch.Lines {
		if line.CannotStartRun {
			continue
		}
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
