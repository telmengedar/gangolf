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
	mode := "legacy"
	if cfg.processID > 0 {
		mode = "workflow"
	}
	log.Info("starting", "set", cfg.set, "defaulted", cfg.defaulted, "channels", len(cfg.channels), "routed", cfg.routed(), "memoryEnabled", cfg.processID == 0, "processorMode", mode, "processID", cfg.processID, "personaID", cfg.personaID)
}

func run(ctx context.Context, lookup func(string) (string, bool), log *slog.Logger) error {
	cfg, err := loadConfig(lookup)
	if err != nil {
		return err
	}
	logStartup(log, cfg)

	messageClient := messages.New(cfg.apiURL, cfg.apiKey, cfg.twitchBotUserID, nil, log)
	processorClient, reviewer := processors(cfg)
	core.New(core.Config{
		MemoryReviewer: reviewer,
		Routes:         cfg.routes,
		MinWords:       cfg.minWords,
		BotName:        cfg.botName,
	}, messageClient, processorClient, log, time.Now).Run(ctx)
	return nil
}

func processors(cfg config) (core.Runner, core.MemoryReviewer) {
	if cfg.processID > 0 {
		return processor.NewProcess(cfg.processorURL, cfg.processID, cfg.personaID, nil), nil
	}
	return processor.New(cfg.processorURL, cfg.subject, cfg.processorConfig, nil), processor.New(cfg.processorURL, cfg.subject, cfg.memoryConfig, &http.Client{Timeout: processor.MemoryReviewTimeout})
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
