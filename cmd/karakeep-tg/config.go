package main

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	defaultPort                = "8080"
	defaultSummaryPollInterval = 5 * time.Second
	defaultSummaryTimeout      = 5 * time.Minute
)

type config struct {
	karakeepURL         string
	karakeepAPIKey      string
	telegramBotToken    string
	telegramChatIDs     []string
	webhookToken        string
	summaryPollInterval time.Duration
	summaryTimeout      time.Duration
	port                string
}

func loadConfig(getenv func(string) string) (config, error) {
	var errs []error
	required := func(key string) string {
		value := getenv(key)
		if value == "" {
			errs = append(errs, fmt.Errorf("%s is required", key))
		}
		return value
	}
	duration := func(key string, fallback time.Duration) time.Duration {
		raw := getenv(key)
		if raw == "" {
			return fallback
		}
		value, err := time.ParseDuration(raw)
		if err != nil || value <= 0 {
			errs = append(errs, fmt.Errorf("%s must be a positive duration, got %q", key, raw))
		}
		return value
	}

	cfg := config{
		karakeepURL:         required("KARAKEEP_URL"),
		karakeepAPIKey:      required("KARAKEEP_API_KEY"),
		telegramBotToken:    required("TELEGRAM_BOT_TOKEN"),
		telegramChatIDs:     splitList(getenv("TELEGRAM_CHAT_IDS")),
		webhookToken:        required("WEBHOOK_TOKEN"),
		summaryPollInterval: duration("SUMMARY_POLL_INTERVAL", defaultSummaryPollInterval),
		summaryTimeout:      duration("SUMMARY_TIMEOUT", defaultSummaryTimeout),
		port:                portFrom(getenv),
	}
	if len(cfg.telegramChatIDs) == 0 {
		errs = append(errs, errors.New("TELEGRAM_CHAT_IDS is required"))
	}

	if err := errors.Join(errs...); err != nil {
		return config{}, fmt.Errorf("invalid configuration: %w", err)
	}
	return cfg, nil
}

func portFrom(getenv func(string) string) string {
	if port := getenv("PORT"); port != "" {
		return port
	}
	return defaultPort
}

func splitList(raw string) []string {
	var items []string
	for item := range strings.SplitSeq(raw, ",") {
		if item = strings.TrimSpace(item); item != "" {
			items = append(items, item)
		}
	}
	return items
}
