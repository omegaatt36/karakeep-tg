package telegram_test

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/go-telegram/bot"

	"github.com/omegaatt36/karakeep-tg/domain"
	"github.com/omegaatt36/karakeep-tg/internal/repository/telegram"
)

type sentMessage struct {
	ChatID    string
	Text      string
	ParseMode string
}

type spyBotAPI struct {
	mu       sync.Mutex
	sent     []sentMessage
	failChat string
}

func (a *spyBotAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	chat := r.FormValue("chat_id")
	body := sentMessage{ChatID: chat, Text: r.FormValue("text"), ParseMode: r.FormValue("parse_mode")}

	a.mu.Lock()
	a.sent = append(a.sent, body)
	a.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	if chat == a.failChat {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"ok":false,"error_code":400,"description":"Bad Request: chat not found"}`))
		return
	}
	_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1,"date":0,"chat":{"id":1,"type":"private"}}}`))
}

func (a *spyBotAPI) chats() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var chats []string
	for _, m := range a.sent {
		chats = append(chats, m.ChatID)
	}
	slices.Sort(chats)
	return chats
}

func newSender(t *testing.T, api *spyBotAPI, chatIDs []string) *telegram.Sender {
	t.Helper()
	server := httptest.NewServer(api)
	t.Cleanup(server.Close)
	sender, err := telegram.New("token", chatIDs, bot.WithServerURL(server.URL), bot.WithSkipGetMe())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return sender
}

var testMessage = domain.Message{BookmarkID: "b1", Title: "Title", URL: "https://e.com/a", Summary: "summary"}

func TestSendDeliversToEveryChat(t *testing.T) {
	api := &spyBotAPI{}
	sender := newSender(t, api, []string{"123456", "-1001234567890", "@channel"})

	if err := sender.Send(t.Context(), testMessage); err != nil {
		t.Fatalf("Send: %v", err)
	}

	want := []string{"-1001234567890", "123456", "@channel"}
	if got := api.chats(); !slices.Equal(got, want) {
		t.Fatalf("chats = %v, want %v", got, want)
	}
	for _, m := range api.sent {
		if m.ParseMode != "HTML" || m.Text != telegram.Render(testMessage) {
			t.Fatalf("message = %+v, want HTML mode and rendered text", m)
		}
	}
}

func TestSendIsolatesPerChatFailures(t *testing.T) {
	api := &spyBotAPI{failChat: "222"}
	sender := newSender(t, api, []string{"111", "222", "333"})

	err := sender.Send(t.Context(), testMessage)

	if err == nil {
		t.Fatal("Send succeeded, want joined error")
	}
	if !strings.Contains(err.Error(), "222") || strings.Contains(err.Error(), "111") {
		t.Fatalf("error = %v, want it to name only chat 222", err)
	}
	if got, want := api.chats(), []string{"111", "222", "333"}; !slices.Equal(got, want) {
		t.Fatalf("attempted chats = %v, want %v", got, want)
	}
}

func TestNewRejectsMissingSettings(t *testing.T) {
	if _, err := telegram.New("", []string{"1"}, bot.WithSkipGetMe()); err == nil {
		t.Error("New with empty token succeeded, want error")
	}
	if _, err := telegram.New("token", nil, bot.WithSkipGetMe()); err == nil {
		t.Error("New without chat ids succeeded, want error")
	}
}
