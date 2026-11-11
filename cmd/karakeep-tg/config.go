package main

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	defaultPort                   = "8080"
	defaultSummaryPollInterval    = 5 * time.Second
	defaultSummaryTimeout         = 5 * time.Minute
	defaultSummaryFollowUpTimeout = 15 * time.Minute
	defaultDedupeWindow           = time.Hour
	defaultWebhookWorkers         = 4
	defaultWebhookQueueSize       = 64
	defaultShutdownTimeout        = 10 * time.Second
)

type config struct {
	karakeepURL            string
	karakeepPublicURL      string
	karakeepAPIKey         string
	telegramBotToken       string
	telegramChatIDs        []string
	webhookToken           string
	summaryPollInterval    time.Duration
	summaryTimeout         time.Duration
	summaryFollowUpTimeout time.Duration
	dedupeWindow           time.Duration
	webhookWorkers         int
	webhookQueueSize       int
	shutdownTimeout        time.Duration
	port                   string
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
	nonNegativeDuration := func(key string, fallback time.Duration) time.Duration {
		raw := getenv(key)
		if raw == "" {
			return fallback
		}
		value, err := time.ParseDuration(raw)
		if err != nil || value < 0 {
			errs = append(errs, fmt.Errorf("%s must be a non-negative duration, got %q", key, raw))
		}
		return value
	}
	positiveInt := func(key string, fallback int) int {
		raw := getenv(key)
		if raw == "" {
			return fallback
		}
		value, err := strconv.Atoi(raw)
		if err != nil || value <= 0 {
			errs = append(errs, fmt.Errorf("%s must be a positive integer, got %q", key, raw))
		}
		return value
	}

	karakeepURL := required("KARAKEEP_URL")
	karakeepPublicURL := getenv("KARAKEEP_PUBLIC_URL")
	if karakeepPublicURL == "" {
		karakeepPublicURL = karakeepURL
	}

	cfg := config{
		karakeepURL:            karakeepURL,
		karakeepPublicURL:      karakeepPublicURL,
		karakeepAPIKey:         required("KARAKEEP_API_KEY"),
		telegramBotToken:       required("TELEGRAM_BOT_TOKEN"),
		telegramChatIDs:        splitList(getenv("TELEGRAM_CHAT_IDS")),
		webhookToken:           required("WEBHOOK_TOKEN"),
		summaryPollInterval:    duration("SUMMARY_POLL_INTERVAL", defaultSummaryPollInterval),
		summaryTimeout:         duration("SUMMARY_TIMEOUT", defaultSummaryTimeout),
		summaryFollowUpTimeout: nonNegativeDuration("SUMMARY_FOLLOWUP_TIMEOUT", defaultSummaryFollowUpTimeout),
		dedupeWindow:           duration("DEDUPE_WINDOW", defaultDedupeWindow),
		webhookWorkers:         positiveInt("WEBHOOK_WORKERS", defaultWebhookWorkers),
		webhookQueueSize:       positiveInt("WEBHOOK_QUEUE_SIZE", defaultWebhookQueueSize),
		shutdownTimeout:        duration("SHUTDOWN_TIMEOUT", defaultShutdownTimeout),
		port:                   portFrom(getenv),
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
