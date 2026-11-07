package notify_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/go-telegram/bot"

	"github.com/omegaatt36/karakeep-tg/domain"
	"github.com/omegaatt36/karakeep-tg/internal/repository/telegram"
)

func TestHandleResumesPartialChatDelivery(t *testing.T) {
	var mu sync.Mutex
	successes := make(map[string]int)
	attempts := make(map[string]int)
	chatAttempts := make(map[string]int)
	failedKey := ""
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		chat := r.FormValue("chat_id")
		key := chat + ":" + r.FormValue("text")
		mu.Lock()
		defer mu.Unlock()
		attempts[key]++
		chatAttempts[chat]++
		w.Header().Set("Content-Type", "application/json")
		if chat == "222" && chatAttempts[chat] == 2 {
			failedKey = key
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"ok":false,"error_code":500,"description":"temporary failure"}`))
			return
		}
		successes[key]++
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1,"date":0,"chat":{"id":1,"type":"private"}}}`))
	}))
	defer api.Close()
	sender, err := telegram.New("synthetic-token", []string{"111", "222"}, bot.WithServerURL(api.URL), bot.WithSkipGetMe())
	if err != nil {
		t.Fatal(err)
	}
	// Distinct content in each segment makes duplicate sends observable.
	summary := strings.Repeat("a", 3500) + strings.Repeat("b", 3500) + strings.Repeat("c", 3500)
	source := &stubBookmarkSource{responses: []domain.Bookmark{bookmark(domain.SummarySuccess, summary)}}
	svc := newService(t, source, sender)
	if err := svc.Handle(t.Context(), "b1"); err == nil {
		t.Fatal("first delivery succeeded despite the failed segment")
	}
	if err := svc.Handle(t.Context(), "b1"); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if err := svc.Handle(t.Context(), "b1"); err != nil {
		t.Fatalf("duplicate: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if failedKey == "" || attempts[failedKey] != 2 {
		t.Fatalf("failed segment attempts = %d, want 2", attempts[failedKey])
	}
	if len(successes) < 6 {
		t.Fatalf("successful distinct segments = %d, want at least 6", len(successes))
	}
	for key, count := range successes {
		if count != 1 {
			t.Errorf("segment %.40q delivered %d times", key, count)
		}
	}
	if source.callCount() != 2 {
		t.Errorf("source calls = %d, want 2", source.callCount())
	}
}
