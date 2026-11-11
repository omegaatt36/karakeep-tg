package main

import (
	"maps"
	"reflect"
	"strings"
	"testing"
	"time"
)

func env(overrides map[string]string) func(string) string {
	values := map[string]string{
		"KARAKEEP_URL":       "https://keep.example.com",
		"KARAKEEP_API_KEY":   "api-key",
		"TELEGRAM_BOT_TOKEN": "bot-token",
		"TELEGRAM_CHAT_IDS":  "123, -1001234567890 ,@channel",
		"WEBHOOK_TOKEN":      "webhook-token",
	}
	maps.Copy(values, overrides)
	return func(key string) string { return values[key] }
}

func TestLoadConfigDefaults(t *testing.T) {
	got, err := loadConfig(env(nil))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}

	want := config{
		karakeepURL:            "https://keep.example.com",
		karakeepPublicURL:      "https://keep.example.com",
		karakeepAPIKey:         "api-key",
		telegramBotToken:       "bot-token",
		telegramChatIDs:        []string{"123", "-1001234567890", "@channel"},
		webhookToken:           "webhook-token",
		summaryPollInterval:    5 * time.Second,
		summaryTimeout:         5 * time.Minute,
		summaryFollowUpTimeout: 15 * time.Minute,
		dedupeWindow:           time.Hour,
		webhookWorkers:         4,
		webhookQueueSize:       64,
		shutdownTimeout:        10 * time.Second,
		port:                   "8080",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("config = %+v, want %+v", got, want)
	}
}

func TestLoadConfigOverrides(t *testing.T) {
	got, err := loadConfig(env(map[string]string{
		"KARAKEEP_PUBLIC_URL":      "https://public.example.com",
		"SUMMARY_POLL_INTERVAL":    "2s",
		"SUMMARY_TIMEOUT":          "90s",
		"SUMMARY_FOLLOWUP_TIMEOUT": "0s",
		"DEDUPE_WINDOW":            "30m",
		"WEBHOOK_WORKERS":          "8",
		"WEBHOOK_QUEUE_SIZE":       "128",
		"SHUTDOWN_TIMEOUT":         "20s",
		"PORT":                     "9000",
	}))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}

	if got.karakeepPublicURL != "https://public.example.com" ||
		got.summaryPollInterval != 2*time.Second ||
		got.summaryTimeout != 90*time.Second ||
		got.summaryFollowUpTimeout != 0 ||
		got.dedupeWindow != 30*time.Minute ||
		got.webhookWorkers != 8 ||
		got.webhookQueueSize != 128 ||
		got.shutdownTimeout != 20*time.Second ||
		got.port != "9000" {
		t.Fatalf("config = %+v", got)
	}
}

func TestLoadConfigRejectsInvalidSettings(t *testing.T) {
	tests := []struct {
		name      string
		overrides map[string]string
		wantInErr string
	}{
		{"missing karakeep url", map[string]string{"KARAKEEP_URL": ""}, "KARAKEEP_URL"},
		{"missing api key", map[string]string{"KARAKEEP_API_KEY": ""}, "KARAKEEP_API_KEY"},
		{"missing bot token", map[string]string{"TELEGRAM_BOT_TOKEN": ""}, "TELEGRAM_BOT_TOKEN"},
		{"missing chat ids", map[string]string{"TELEGRAM_CHAT_IDS": ""}, "TELEGRAM_CHAT_IDS"},
		{"blank chat ids", map[string]string{"TELEGRAM_CHAT_IDS": " , ,"}, "TELEGRAM_CHAT_IDS"},
		{"missing webhook token", map[string]string{"WEBHOOK_TOKEN": ""}, "WEBHOOK_TOKEN"},
		{"malformed interval", map[string]string{"SUMMARY_POLL_INTERVAL": "soon"}, "SUMMARY_POLL_INTERVAL"},
		{"zero interval", map[string]string{"SUMMARY_POLL_INTERVAL": "0s"}, "SUMMARY_POLL_INTERVAL"},
		{"negative timeout", map[string]string{"SUMMARY_TIMEOUT": "-1m"}, "SUMMARY_TIMEOUT"},
		{"negative follow-up timeout", map[string]string{"SUMMARY_FOLLOWUP_TIMEOUT": "-1m"}, "SUMMARY_FOLLOWUP_TIMEOUT"},
		{"malformed follow-up timeout", map[string]string{"SUMMARY_FOLLOWUP_TIMEOUT": "later"}, "SUMMARY_FOLLOWUP_TIMEOUT"},
		{"zero dedupe window", map[string]string{"DEDUPE_WINDOW": "0s"}, "DEDUPE_WINDOW"},
		{"zero workers", map[string]string{"WEBHOOK_WORKERS": "0"}, "WEBHOOK_WORKERS"},
		{"malformed workers", map[string]string{"WEBHOOK_WORKERS": "many"}, "WEBHOOK_WORKERS"},
		{"negative queue size", map[string]string{"WEBHOOK_QUEUE_SIZE": "-5"}, "WEBHOOK_QUEUE_SIZE"},
		{"zero shutdown timeout", map[string]string{"SHUTDOWN_TIMEOUT": "0s"}, "SHUTDOWN_TIMEOUT"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadConfig(env(tt.overrides))

			if err == nil || !strings.Contains(err.Error(), tt.wantInErr) {
				t.Fatalf("error = %v, want it to name %s", err, tt.wantInErr)
			}
		})
	}
}

func TestLoadConfigReportsEveryMissingSetting(t *testing.T) {
	_, err := loadConfig(func(string) string { return "" })

	if err == nil {
		t.Fatal("loadConfig succeeded, want error")
	}
	for _, name := range []string{"KARAKEEP_URL", "KARAKEEP_API_KEY", "TELEGRAM_BOT_TOKEN", "TELEGRAM_CHAT_IDS", "WEBHOOK_TOKEN"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error %q does not name %s", err, name)
		}
	}
}
