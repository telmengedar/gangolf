package core

import (
	"context"
	"github.com/telmengedar/gangolf/ai-core/internal/window"
	"time"
)

type fixtureCursor struct{ cursor, fallback time.Time }

var fixtureCursors = make(map[*Core]*fixtureCursor)

func fixtureState(c *Core) *fixtureCursor {
	if fixtureCursors[c] == nil {
		fixtureCursors[c] = &fixtureCursor{}
	}
	return fixtureCursors[c]
}

const Overlap = 10 * time.Second

// RunFixture drives finite legacy scenarios without a production polling path.
func (c *Core) RunFixture(ctx context.Context) {
	c.withMemoryWorker(ctx, func() {
		for {
			if ctx.Err() != nil {
				return
			}
			if c.Bootstrap(ctx) == nil {
				break
			}
			if !sleep(ctx, time.Nanosecond) {
				return
			}
		}
		for sleep(ctx, time.Nanosecond) {
			if ctx.Err() != nil {
				return
			}
			c.Tick(ctx)
		}
	})
}

// Bootstrap loads the recent conversation as context and marks all of it answered.
func (c *Core) Bootstrap(ctx context.Context) error {
	fixtureState(c).fallback = c.now().Add(-window.MaxAge)
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
	if fixtureState(c).cursor.IsZero() {
		return fixtureState(c).fallback
	}
	return fixtureState(c).cursor.Add(-Overlap)
}

func (c *Core) poll(ctx context.Context) error {
	lines, err := c.messages.(interface {
		List(context.Context, time.Time) ([]window.Line, error)
	}).List(ctx, c.from())
	if err != nil {
		return err
	}
	var routed []window.Line
	for _, line := range lines {
		if line.Time.After(fixtureState(c).cursor) {
			fixtureState(c).cursor = line.Time
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
	c.windows.Forget(fixtureState(c).cursor.Add(-2 * Overlap))
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
