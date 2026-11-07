package app

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

func Run(main func(ctx context.Context) error) {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := main(ctx); err != nil {
		slog.Error("terminated with error", "error", err)
		stop()
		os.Exit(1)
	}
	slog.Info("terminated")
}
