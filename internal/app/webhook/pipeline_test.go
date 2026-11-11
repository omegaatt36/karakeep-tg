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

func TestWebhookPipelineSendsPendingThenEditsInPlaceOnFollowUp(t *testing.T) {
	var mu sync.Mutex
	reads := 0
	var paths, texts, messageIDs, markups []string
	edited := make(chan struct{})

	sourceAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		reads++
		ready := reads >= 4
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if !ready {
			_, _ = w.Write([]byte(`{"id":"b1","title":"Article","summarizationStatus":"pending","content":{"url":"https://e.com/a"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"b1","title":"Article","summary":"Delayed AI summary","summarizationStatus":"success","content":{"url":"https://e.com/a"}}`))
	}))
	t.Cleanup(sourceAPI.Close)

	botAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		paths = append(paths, r.URL.Path)
		texts = append(texts, r.FormValue("text"))
		messageIDs = append(messageIDs, r.FormValue("message_id"))
		markups = append(markups, r.FormValue("reply_markup"))
		done := r.URL.Path == "/botsynthetic-token/editMessageText"
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":42,"date":0,"chat":{"id":111,"type":"private"}}}`))
		if done {
			close(edited)
		}
	}))
	t.Cleanup(botAPI.Close)

	source, err := karakeep.New(sourceAPI.URL, "source-key")
	if err != nil {
		t.Fatal(err)
	}
	sender, err := telegram.New("synthetic-token", []string{"111"}, bot.WithServerURL(botAPI.URL), bot.WithSkipGetMe())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sender.WithKarakeepURL("https://keep.example.com"); err != nil {
		t.Fatal(err)
	}
	service, err := notify.New(notify.Options{
		Source:          source,
		Notifier:        sender,
		PollInterval:    5 * time.Millisecond,
		SummaryTimeout:  8 * time.Millisecond,
		FollowUpTimeout: time.Second,
		DedupeWindow:    time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := newServerWithOptions(t, webhook.Options{Notifier: service, WorkerCount: 1, QueueCapacity: 2})
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, httpServer.URL+"/webhook/karakeep", strings.NewReader(crawledBody("b1")))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := (&http.Client{Timeout: time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	select {
	case <-edited:
	case <-time.After(5 * time.Second):
		t.Fatal("pipeline did not edit pending message after follow-up summary resolved")
	}

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := server.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(paths) != 2 || paths[0] != "/botsynthetic-token/sendMessage" || paths[1] != "/botsynthetic-token/editMessageText" {
		t.Fatalf("paths = %v, want [sendMessage editMessageText]", paths)
	}
	if !strings.Contains(texts[0], "⏳ 摘要尚未就緒") || !strings.Contains(texts[1], "Delayed AI summary") {
		t.Fatalf("texts = %q, want pending marker then delayed summary", texts)
	}
	if messageIDs[1] != "42" {
		t.Fatalf("edited message_id = %q, want 42", messageIDs[1])
	}
	for i, m := range markups {
		if !strings.Contains(m, "https://e.com/a") || !strings.Contains(m, "https://keep.example.com/dashboard/preview/b1") {
			t.Errorf("request %d reply_markup = %q, want article and karakeep preview buttons", i, m)
		}
	}
}
