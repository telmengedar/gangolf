package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/telmengedar/gangolf/ai-core/internal/core"
	"github.com/telmengedar/gangolf/ai-core/internal/messages"
	"github.com/telmengedar/gangolf/ai-core/internal/processor"
)

func logStartup(log *slog.Logger, cfg config) {
	log.Info("starting", "set", cfg.set, "defaulted", cfg.defaulted, "channels", len(cfg.channels))
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg, err := loadConfig(os.LookupEnv)
	if err != nil {
		log.Error("invalid configuration", "error", err.Error())
		os.Exit(1)
	}
	logStartup(log, cfg)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	messageClient := messages.New(cfg.apiURL, cfg.apiKey, cfg.botName, nil, log)
	processorClient := processor.New(cfg.processorURL, cfg.subject, nil)
	core.New(core.Config{
		Channels: cfg.channels,
		BotName:  cfg.botName,
	}, messageClient, processorClient, log, time.Now).Run(ctx)
	log.Info("stopped")
}
