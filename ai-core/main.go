package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/telmengedar/gangolf/ai-core/internal/core"
	"github.com/telmengedar/gangolf/ai-core/internal/messages"
	"github.com/telmengedar/gangolf/ai-core/internal/processor"
)

func logStartup(log *slog.Logger, cfg config) {
	log.Info("starting", "set", cfg.set, "defaulted", cfg.defaulted, "channels", len(cfg.channels), "routed", cfg.routed(), "memoryEnabled", true)
}

func run(ctx context.Context, lookup func(string) (string, bool), log *slog.Logger) error {
	cfg, err := loadConfig(lookup)
	if err != nil {
		return err
	}
	logStartup(log, cfg)

	messageClient := messages.New(cfg.apiURL, cfg.apiKey, cfg.botName, nil, log)
	processorClient := processor.New(cfg.processorURL, cfg.subject, cfg.processorConfig, nil)
	reviewer := processor.New(cfg.processorURL, cfg.subject, cfg.memoryConfig, &http.Client{Timeout: processor.MemoryReviewTimeout})
	core.New(core.Config{
		MemoryReviewer: reviewer,
		Routes:         cfg.routes,
		MinWords:       cfg.minWords,
		BotName:        cfg.botName,
	}, messageClient, processorClient, log, time.Now).Run(ctx)
	return nil
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, os.LookupEnv, log); err != nil {
		log.Error("invalid configuration", "error", err.Error())
		os.Exit(1)
	}
	log.Info("stopped")
}
