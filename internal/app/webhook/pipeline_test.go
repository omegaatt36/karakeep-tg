package webhook_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-telegram/bot"

	"github.com/omegaatt36/karakeep-tg/internal/app/webhook"
	"github.com/omegaatt36/karakeep-tg/internal/repository/karakeep"
	"github.com/omegaatt36/karakeep-tg/internal/repository/telegram"
	"github.com/omegaatt36/karakeep-tg/internal/service/notify"
)

func TestWebhookPipelinePollsRetriesAndDeduplicates(t *testing.T) {
	releaseSource := make(chan struct{})
	var releaseOnce sync.Once
	var mu sync.Mutex
	reads := 0
	attempts := make(map[string]int)
	successes := make(map[string]int)
	delivered := make(chan struct{})
	sourceAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/bookmarks/b1" || r.Header.Get("Authorization") != "Bearer source-key" {
			t.Errorf("unexpected source request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "bad source request", http.StatusBadRequest)
			return
		}
		select {
		case <-releaseSource:
		case <-r.Context().Done():
			return
		}
		mu.Lock()
		reads++
		pending := reads == 1
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if pending {
			_, _ = w.Write([]byte(`{"id":"b1","summarizationStatus":"pending","content":{"url":"https://e.com/a"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"b1","title":"Article","summary":"Ready summary","summarizationStatus":"success","content":{"url":"https://e.com/a"}}`))
	}))
	t.Cleanup(sourceAPI.Close)
	botAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		chat := r.FormValue("chat_id")
		if r.URL.Path != "/botsynthetic-token/sendMessage" || r.FormValue("parse_mode") != "HTML" || !strings.Contains(r.FormValue("text"), "Ready summary") {
			t.Error("unexpected Telegram request or message content")
		}
		mu.Lock()
		defer mu.Unlock()
		attempts[chat]++
		w.Header().Set("Content-Type", "application/json")
		if chat == "222" && attempts[chat] == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"ok":false,"error_code":500,"description":"temporary failure"}`))
			return
		}
		successes[chat]++
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1,"date":0,"chat":{"id":1,"type":"private"}}}`))
		if successes["111"] == 1 && successes["222"] == 1 {
			close(delivered)
		}
	}))
	t.Cleanup(botAPI.Close)
	// Register before setup can fail: cleanup releases any blocked source request
	// before httptest closes the API servers.
	t.Cleanup(func() { releaseOnce.Do(func() { close(releaseSource) }) })
	source, err := karakeep.New(sourceAPI.URL, "source-key")
	if err != nil {
		t.Fatal(err)
	}
	sender, err := telegram.New("synthetic-token", []string{"111", "222"}, bot.WithServerURL(botAPI.URL), bot.WithSkipGetMe())
	if err != nil {
		t.Fatal(err)
	}
	service, err := notify.New(notify.Options{
		Source: source, Notifier: sender, PollInterval: time.Millisecond,
		SummaryTimeout: time.Second, DedupeWindow: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	barrier := make(chan struct{})
	server := newServerWithOptions(t, webhook.Options{
		Notifier: notifierFunc(func(ctx context.Context, id string) error {
			if id == "barrier" {
				close(barrier)
				return nil
			}
			return service.Handle(ctx, id)
		}),
		WorkerCount: 1, QueueCapacity: 2,
	})
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	// This cleanup precedes the worker cleanup even if an assertion fails.
	t.Cleanup(func() { releaseOnce.Do(func() { close(releaseSource) }) })
	postEvent := func(id string) {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, httpServer.URL+"/webhook/karakeep", strings.NewReader(crawledBody(id)))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := (&http.Client{Timeout: time.Second}).Do(req)
		if err != nil {
			t.Fatalf("webhook did not acknowledge while source was blocked: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("webhook status = %d, want 202", resp.StatusCode)
		}
	}
	postEvent("b1")
	postEvent("b1")
	releaseOnce.Do(func() { close(releaseSource) })
	select {
	case <-delivered:
	case <-time.After(5 * time.Second):
		t.Fatal("pipeline did not recover the failed chat")
	}
	// The single worker reaches this barrier only after b1 completed and left
	// the pending set. The following replay must exercise completed deduplication.
	postEvent("barrier")
	select {
	case <-barrier:
	case <-time.After(time.Second):
		t.Fatal("worker did not finish the first bookmark")
	}
	postEvent("b1")
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := server.Close(ctx); err != nil {
		t.Fatalf("drain pipeline: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if reads != 3 || attempts["111"] != 1 || attempts["222"] != 2 || successes["111"] != 1 || successes["222"] != 1 {
		t.Fatalf("reads=%d attempts=%v successes=%v, want 3 reads and one successful delivery per chat", reads, attempts, successes)
	}
}
