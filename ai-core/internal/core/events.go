package core

import (
	"context"
	"errors"
	"time"

	"github.com/telmengedar/gangolf/ai-core/internal/messages"
	"github.com/telmengedar/gangolf/ai-core/internal/window"
)

type eventMessages interface {
	Open(context.Context) (messages.Feed, error)
	Sync(context.Context, time.Time, func(messages.Frame) error) error
}

func (c *Core) runEvents(ctx context.Context) {
	source, ok := c.messages.(eventMessages)
	if !ok {
		c.log.Error("message source does not support producer events")
		return
	}
	delay := time.Second
	for ctx.Err() == nil {
		feed, err := source.Open(ctx)
		if err == nil {
			err = c.eventGeneration(feed, source)
			feed.Close()
		}
		if ctx.Err() != nil {
			return
		}
		var denied *messages.AuthorizationError
		if errors.As(err, &denied) {
			c.log.Error("message synchronization stopped", "reason", "authorization_denied")
			return
		}
		c.log.Warn("message synchronization unavailable", "error", err)
		if !sleep(ctx, delay) {
			return
		}
		delay = min(delay*2, 30*time.Second)
	}
}
func (c *Core) eventGeneration(feed messages.Feed, source eventMessages) error {
	ctx := feed.Context()
	c.observations = make(map[string]messageObservation)
	c.windows = window.New("")
	c.closing = make(map[window.Key]bool)
	if err := source.Sync(ctx, c.now().Add(-window.MaxAge), func(frame messages.Frame) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return c.observe(frame, true)
	}); err != nil {
		return err
	}
	if err := c.consumeFence(feed, true); err != nil {
		return err
	}
	c.log.Info("message synchronization ready")
	for ctx.Err() == nil {
		for {
			frame, ok, err := feed.TakeAvailable()
			if err != nil {
				return err
			}
			if !ok {
				break
			}
			if frame.ID != "" {
				if err := c.observe(frame, false); err != nil {
					return err
				}
			} else if frame.Fence != "" {
				return errors.New("unexpected live fence")
			}
		}
		c.windows.Evict(c.now())
		if key, ok := c.windows.Next(c.now(), Settle); ok {
			if c.turn(ctx, key) {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				c.closing[key] = true
				if err := c.consumeFence(feed, false); err != nil {
					return err
				}
				delete(c.closing, key)
			}
			continue
		}
		var timer *time.Timer
		var due <-chan time.Time
		if at, ok := c.windows.PendingAt(Settle); ok {
			timer = time.NewTimer(max(time.Duration(0), at.Sub(c.now())))
			due = timer.C
		}
		event, err := nextEventOrDeadline(feed, due)
		if timer != nil {
			timer.Stop()
		}
		if err != nil {
			return err
		}
		if event.ID != "" {
			if err := c.observe(event, false); err != nil {
				return err
			}
		}
	}
	return ctx.Err()
}
func nextEventOrDeadline(feed messages.Feed, due <-chan time.Time) (messages.Frame, error) {
	ctx, cancel := context.WithCancel(feed.Context())
	defer cancel()
	if due == nil {
		return feed.Next(ctx)
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-due:
			cancel()
		case <-done:
		case <-ctx.Done():
		}
	}()
	frame, err := feed.Next(ctx)
	close(done)
	if err != nil && feed.Context().Err() == nil && ctx.Err() != nil {
		return messages.Frame{}, nil
	}
	return frame, err
}
func (c *Core) consumeFence(feed messages.Feed, all bool) error {
	ctx, cancel := context.WithTimeout(feed.Context(), 30*time.Second)
	defer cancel()
	id, err := messages.NewFenceID()
	if err != nil {
		return err
	}
	requestDone := make(chan error, 1)
	go func() {
		err := feed.RequestFence(ctx, id)
		var denied *messages.AuthorizationError
		if errors.As(err, &denied) {
			cancel()
		}
		requestDone <- err
	}()
	defer func() { cancel(); <-requestDone }()
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		frame, err := feed.Next(ctx)
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if frame.ID != "" {
			if err := c.observe(frame, all); err != nil {
				return err
			}
		}
		if frame.Fence == id {
			if all {
				c.windows.MarkAllAnswered()
			}
			return nil
		}
		if frame.Fence != "" {
			return errors.New("unexpected message synchronization fence")
		}
	}
}
func (c *Core) ingestEvent(line window.Line, all bool) {
	if targets, ok := c.routes[line.Key]; ok {
		short := c.minWords[line.Key] > 0 && words(line.Text) < c.minWords[line.Key]
		for _, target := range targets {
			copied := line
			copied.Key = target
			copied.CannotStartRun = short
			c.windows.Ingest([]window.Line{copied}, c.now())
			if all || c.closing[target] {
				c.windows.MarkAnswered(target)
			}
		}
	} else if !c.unlisted[line.Key] {
		c.unlisted[line.Key] = true
		c.log.Warn("ignoring a channel that is not in GANGOLF_CHANNELS", "service", line.Service, "channel", line.Channel)
	}
}

type messageObservation struct {
	time, observed time.Time
	readable       bool
}

func (c *Core) observe(frame messages.Frame, contextOnly bool) error {
	state, known := c.observations[frame.ID]
	if known && state.readable {
		return nil
	}
	if frame.Updated {
		if !known || !state.time.Equal(frame.Time) || c.now().Sub(state.time) > window.MaxAge || c.now().Sub(state.observed) > window.MaxAge {
			return nil
		}
	} else if !known {
		if len(c.observations) >= 32000 {
			return errors.New("message observation capacity exceeded")
		}
		state = messageObservation{time: frame.Time, observed: c.now()}
	}
	if frame.Line != nil {
		state.readable = true
		c.ingestEvent(*frame.Line, contextOnly)
	}
	c.observations[frame.ID] = state
	return nil
}
