package core

import (
	"context"
	"time"

	"github.com/telmengedar/gangolf/ai-core/internal/processor"
	"github.com/telmengedar/gangolf/ai-core/internal/window"
)

// MemoryReviewer privately considers a completed, delivered interaction.
type MemoryReviewer interface {
	Review(context.Context, string, []window.Exchange, string) (processor.MemoryResult, error)
}

type memoryJob struct {
	key     window.Key
	input   string
	history []window.Exchange
	reply   string
}

func (c *Core) offerMemory(ctx context.Context, key window.Key, batch window.Batch, reply string) {
	if c.memoryJobs == nil || ctx.Err() != nil {
		return
	}
	job := memoryJob{key: key, input: batch.Input, history: append([]window.Exchange(nil), batch.History...), reply: reply}
	outcome := "queued"
	select {
	case c.memoryJobs <- job:
	default:
		outcome = "busy"
	}
	c.log.Info("memory review", "service", key.Service, "channel", key.Channel, "outcome", outcome,
		"inputBytes", len(job.input), "exchanges", len(job.history), "replyBytes", len(job.reply))
}

func (c *Core) reviewMemory(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-c.memoryJobs:
			if ctx.Err() != nil {
				return
			}
			started := time.Now()
			reviewCtx, cancel := context.WithTimeout(ctx, processor.MemoryReviewTimeout)
			result, err := c.memoryReviewer.Review(reviewCtx, job.input, job.history, job.reply)
			cancel()
			state := result.State
			if err != nil {
				state = "unknown"
			}
			c.log.Info("memory review finished", "service", job.key.Service, "channel", job.key.Channel,
				"outcome", state, "noteID", result.NoteID, "modelCalls", result.ModelCalls,
				"inputBytes", len(job.input), "exchanges", len(job.history), "replyBytes", len(job.reply),
				"tookMs", time.Since(started).Milliseconds())
		}
	}
}
