package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"

	"github.com/omegaatt36/karakeep-tg/internal/app"
	"github.com/omegaatt36/karakeep-tg/internal/app/webhook"
	"github.com/omegaatt36/karakeep-tg/internal/repository/karakeep"
	"github.com/omegaatt36/karakeep-tg/internal/repository/telegram"
	"github.com/omegaatt36/karakeep-tg/internal/service/notify"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		if err := healthcheck(context.Background(), "http://127.0.0.1:"+portFrom(os.Getenv)+"/health"); err != nil {
			fmt.Fprintln(os.Stderr, "unhealthy:", err)
			os.Exit(1)
		}
		return
	}

	app.Run(run)
}

func run(ctx context.Context) error {
	cfg, err := loadConfig(os.Getenv)
	if err != nil {
		return err
	}

	source, err := karakeep.New(cfg.karakeepURL, cfg.karakeepAPIKey)
	if err != nil {
		return fmt.Errorf("create karakeep client: %w", err)
	}
	sender, err := telegram.New(cfg.telegramBotToken, cfg.telegramChatIDs)
	if err != nil {
		return fmt.Errorf("create telegram sender: %w", err)
	}
	if _, err := sender.WithKarakeepURL(cfg.karakeepPublicURL); err != nil {
		return fmt.Errorf("configure karakeep public url: %w", err)
	}
	notifier, err := notify.New(notify.Options{
		Source:          source,
		Notifier:        sender,
		PollInterval:    cfg.summaryPollInterval,
		SummaryTimeout:  cfg.summaryTimeout,
		FollowUpTimeout: cfg.summaryFollowUpTimeout,
		DedupeWindow:    cfg.dedupeWindow,
	})
	if err != nil {
		return fmt.Errorf("create notify service: %w", err)
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", ":"+cfg.port)
	if err != nil {
		return fmt.Errorf("listen on port %s: %w", cfg.port, err)
	}
	server, err := webhook.New(webhook.Options{
		Token:           cfg.webhookToken,
		Notifier:        notifier,
		WorkerCount:     cfg.webhookWorkers,
		QueueCapacity:   cfg.webhookQueueSize,
		ShutdownTimeout: cfg.shutdownTimeout,
	})
	if err != nil {
		_ = ln.Close()
		return fmt.Errorf("create webhook server: %w", err)
	}
	slog.InfoContext(ctx, "listening", "addr", ln.Addr().String())
	return server.Serve(ctx, ln)
}
