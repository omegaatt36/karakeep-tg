package telegram_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/go-telegram/bot"

	"github.com/omegaatt36/karakeep-tg/domain"
	"github.com/omegaatt36/karakeep-tg/internal/repository/telegram"
)

type sentMessage struct {
	ChatID    string
	Text      string
	ParseMode string
}

type recordedRequest struct {
	Path        string
	ChatID      string
	MessageID   string
	Text        string
	ParseMode   string
	ReplyMarkup string
}

type spyBotAPI struct {
	mu         sync.Mutex
	sent       []sentMessage
	reqs       []recordedRequest
	nextMsgID  int
	failChat   string
	failText   string
	failures   int
	retryAfter int
	hook       func(*http.Request, sentMessage) error
}

func (a *spyBotAPI) Do(r *http.Request) (*http.Response, error) {
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		return nil, err
	}
	body := sentMessage{ChatID: r.FormValue("chat_id"), Text: r.FormValue("text"), ParseMode: r.FormValue("parse_mode")}
	req := recordedRequest{
		Path:        r.URL.Path,
		ChatID:      r.FormValue("chat_id"),
		MessageID:   r.FormValue("message_id"),
		Text:        r.FormValue("text"),
		ParseMode:   r.FormValue("parse_mode"),
		ReplyMarkup: r.FormValue("reply_markup"),
	}
	a.mu.Lock()
	a.sent = append(a.sent, body)
	a.reqs = append(a.reqs, req)
	a.nextMsgID++
	msgID := a.nextMsgID
	fail := body.ChatID == a.failChat && (a.failText == "" || body.Text == a.failText) && a.failures != 0
	if fail && a.failures > 0 {
		a.failures--
	}
	a.mu.Unlock()
	if a.hook != nil {
		if err := a.hook(r, body); err != nil {
			return nil, err
		}
	}
	status, response := http.StatusOK, fmt.Sprintf(`{"ok":true,"result":{"message_id":%d,"date":0,"chat":{"id":1,"type":"private"}}}`, msgID)
	if fail {
		status, response = http.StatusBadRequest, `{"ok":false,"error_code":400,"description":"Bad Request: chat not found"}`
		if a.retryAfter > 0 {
			status, response = http.StatusTooManyRequests, fmt.Sprintf(`{"ok":false,"error_code":429,"description":"Too Many Requests","parameters":{"retry_after":%d}}`, a.retryAfter)
		}
	}
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(response)), Header: make(http.Header)}, nil
}

func (a *spyBotAPI) messages() []sentMessage {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.sent)
}

func (a *spyBotAPI) requests() []recordedRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.reqs)
}

func (a *spyBotAPI) chats() []string {
	var chats []string
	for _, m := range a.messages() {
		chats = append(chats, m.ChatID)
	}
	slices.Sort(chats)
	return chats
}

func newSender(t *testing.T, api *spyBotAPI, chatIDs []string) *telegram.Sender {
	t.Helper()
	sender, err := telegram.New("token", chatIDs, bot.WithHTTPClient(time.Second, api), bot.WithSkipGetMe())
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
	if got, want := api.chats(), []string{"-1001234567890", "123456", "@channel"}; !slices.Equal(got, want) {
		t.Fatalf("chats = %v, want %v", got, want)
	}
	for _, m := range api.messages() {
		if m.ParseMode != "HTML" || m.Text != telegram.Render(testMessage)[0] {
			t.Fatalf("message = %+v, want HTML mode and rendered text", m)
		}
	}
}

func TestSendIsolatesPerChatFailures(t *testing.T) {
	api := &spyBotAPI{failChat: "222", failures: -1}
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
	if err := sender.Send(t.Context(), testMessage); err == nil {
		t.Fatal("failed chat unexpectedly succeeded")
	}
	if got, want := api.chats(), []string{"111", "222", "222", "333"}; !slices.Equal(got, want) {
		t.Fatalf("replay attempted chats = %v, want %v", got, want)
	}
}

func TestSendResumesFailedPartAndPreservesOriginalSnapshot(t *testing.T) {
	msg := testMessage
	msg.Summary = strings.Repeat("original", 1500)
	parts := telegram.Render(msg)
	api := &spyBotAPI{failChat: "222", failText: parts[1], failures: 1}
	sender := newSender(t, api, []string{"111", "222", "333"})
	if err := sender.Send(t.Context(), msg); err == nil {
		t.Fatal("Send succeeded, want failed second part")
	}
	first := api.messages()
	var expected []sentMessage
	for _, chat := range []string{"111", "222", "333"} {
		for i, text := range parts {
			expected = append(expected, sentMessage{ChatID: chat, Text: text, ParseMode: "HTML"})
			if chat == "222" && i == 1 {
				break
			}
		}
	}
	if !slices.Equal(first, expected) {
		t.Fatalf("initial parts differ: got %d attempts, want %d", len(first), len(expected))
	}
	msg.Summary = strings.Repeat("changed", 2000)
	if err := sender.Send(t.Context(), msg); err != nil {
		t.Fatalf("retry: %v", err)
	}
	for _, text := range parts[1:] {
		expected = append(expected, sentMessage{ChatID: "222", Text: text, ParseMode: "HTML"})
	}
	if got := api.messages(); !slices.Equal(got, expected) {
		t.Fatalf("retry did not resume original snapshot: got %d attempts, want %d", len(got), len(expected))
	}
	if err := sender.Send(t.Context(), msg); err != nil {
		t.Fatalf("complete replay: %v", err)
	}
	if got := len(api.messages()); got != len(expected) {
		t.Fatalf("complete replay sent %d extra parts", got-len(expected))
	}
}

func TestSendDedupeExpiresOneHourAfterSuccess(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		api := &spyBotAPI{}
		sender := newSender(t, api, []string{"111"})
		if err := sender.Send(t.Context(), testMessage); err != nil {
			t.Fatal(err)
		}
		time.Sleep(59 * time.Minute)
		if err := sender.Send(t.Context(), testMessage); err != nil {
			t.Fatal(err)
		}
		if got := len(api.messages()); got != 1 {
			t.Fatalf("before TTL: got %d attempts, want 1", got)
		}
		time.Sleep(time.Minute)
		if err := sender.Send(t.Context(), testMessage); err != nil {
			t.Fatal(err)
		}
		if got := len(api.messages()); got != 2 {
			t.Fatalf("after TTL: got %d attempts, want 2", got)
		}
	})
}

func TestSendFailedAttemptDoesNotRefreshTTL(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		msg := testMessage
		msg.Summary = strings.Repeat("x", 9000)
		parts := telegram.Render(msg)
		api := &spyBotAPI{failChat: "111", failText: parts[1], failures: -1}
		sender := newSender(t, api, []string{"111"})
		if err := sender.Send(t.Context(), msg); err == nil {
			t.Fatal("wanted failure")
		}
		time.Sleep(59 * time.Minute)
		if err := sender.Send(t.Context(), msg); err == nil {
			t.Fatal("wanted failure")
		}
		time.Sleep(time.Minute)
		if err := sender.Send(t.Context(), msg); err == nil {
			t.Fatal("wanted failure")
		}
		got := api.messages()
		if len(got) != 5 || got[3].Text != parts[0] {
			t.Fatalf("expired partial delivery did not restart at part zero: %d attempts", len(got))
		}
	})
}

func TestSendSuccessfulPartRefreshesPartialDeliveryTTL(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		msg := testMessage
		msg.Summary = strings.Repeat("x", 9000)
		parts := telegram.Render(msg)
		api := &spyBotAPI{failChat: "111", failText: parts[1], failures: 1}
		sender := newSender(t, api, []string{"111"})
		if err := sender.Send(t.Context(), msg); err == nil {
			t.Fatal("wanted failure")
		}
		time.Sleep(30 * time.Minute)
		api.mu.Lock()
		api.failText, api.failures = parts[2], 1
		api.mu.Unlock()
		if err := sender.Send(t.Context(), msg); err == nil {
			t.Fatal("wanted failure")
		}
		time.Sleep(31 * time.Minute)
		if err := sender.Send(t.Context(), msg); err != nil {
			t.Fatal(err)
		}
		got := api.messages()
		if len(got) != 5 || got[4].Text != parts[2] {
			t.Fatalf("successful second part did not refresh TTL: %d attempts", len(got))
		}
	})
}

func TestSendHonorsRetryAfterAndRetriesOnlyCurrentPart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		msg := testMessage
		msg.Summary = strings.Repeat("x", 9000)
		parts := telegram.Render(msg)
		api := &spyBotAPI{failChat: "111", failText: parts[1], failures: 1, retryAfter: 3}
		sender := newSender(t, api, []string{"111"})
		start := time.Now()
		if err := sender.Send(t.Context(), msg); err != nil {
			t.Fatal(err)
		}
		if elapsed := time.Since(start); elapsed < 3*time.Second {
			t.Fatalf("retry waited %s, want at least 3s", elapsed)
		}
		got := api.messages()
		if len(got) != len(parts)+1 || got[1].Text != parts[1] || got[2].Text != parts[1] {
			t.Fatalf("429 retry lost offset: %d attempts", len(got))
		}
	})
}

func TestSendRetryAfterIsBoundedAndCancelable(t *testing.T) {
	t.Run("bounded", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			api := &spyBotAPI{failChat: "111", failures: -1, retryAfter: 3}
			sender := newSender(t, api, []string{"111", "222"})
			if err := sender.Send(t.Context(), testMessage); err == nil {
				t.Fatal("wanted repeated 429 error")
			}
			if got, want := api.chats(), []string{"111", "111", "222"}; !slices.Equal(got, want) {
				t.Fatalf("got attempts %v, want %v", got, want)
			}
		})
	})
	t.Run("canceled", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			api := &spyBotAPI{failChat: "111", failures: 1, retryAfter: 30}
			sender := newSender(t, api, []string{"111"})
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() { done <- sender.Send(ctx, testMessage) }()
			time.Sleep(time.Second)
			cancel()
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Fatalf("got %v, want canceled", err)
			}
			if got := len(api.messages()); got != 1 {
				t.Fatalf("canceled wait retried: %d attempts", got)
			}
			if err := sender.Send(t.Context(), testMessage); err != nil {
				t.Fatal(err)
			}
			if got := len(api.messages()); got != 2 {
				t.Fatalf("replay after cancel: %d attempts", got)
			}
		})
	})
}

func TestSendConcurrentReplayIsDeduplicated(t *testing.T) {
	api := &spyBotAPI{}
	sender := newSender(t, api, []string{"111", "222"})
	var wg sync.WaitGroup
	for range 30 {
		wg.Go(func() {
			if err := sender.Send(t.Context(), testMessage); err != nil {
				t.Errorf("Send: %v", err)
			}
		})
	}
	wg.Wait()
	if got := len(api.messages()); got != 2 {
		t.Fatalf("got %d requests, want 2", got)
	}
}

func TestSendCancellationWhileWaitingDoesNotBlockOtherBookmarks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		started, release := make(chan struct{}), make(chan struct{})
		api := &spyBotAPI{hook: func(r *http.Request, m sentMessage) error {
			if strings.Contains(m.Text, "Title") {
				close(started)
				select {
				case <-release:
					return nil
				case <-r.Context().Done():
					return r.Context().Err()
				}
			}
			return nil
		}}
		sender := newSender(t, api, []string{"111"})
		firstDone := make(chan error, 1)
		go func() { firstDone <- sender.Send(t.Context(), testMessage) }()
		<-started
		ctx, cancel := context.WithCancel(t.Context())
		waitingDone := make(chan error, 1)
		go func() { waitingDone <- sender.Send(ctx, testMessage) }()
		synctest.Wait()
		cancel()
		if err := <-waitingDone; !errors.Is(err, context.Canceled) {
			t.Fatalf("waiting send: %v, want canceled", err)
		}
		other := testMessage
		other.BookmarkID, other.Title = "other", "Other"
		if err := sender.Send(t.Context(), other); err != nil {
			t.Fatal(err)
		}
		close(release)
		if err := <-firstDone; err != nil {
			t.Fatal(err)
		}
		if got := len(api.messages()); got != 2 {
			t.Fatalf("got %d requests, want 2", got)
		}
	})
}

func TestSendCanceledRequestResumesAtUnsentPart(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	api := &spyBotAPI{hook: func(r *http.Request, _ sentMessage) error {
		if r.Context() == ctx {
			cancel()
			return r.Context().Err()
		}
		return nil
	}}
	sender := newSender(t, api, []string{"111"})
	if err := sender.Send(ctx, testMessage); !errors.Is(err, context.Canceled) {
		t.Fatalf("Send: %v, want canceled", err)
	}
	if err := sender.Send(t.Context(), testMessage); err != nil {
		t.Fatal(err)
	}
	if got := len(api.messages()); got != 2 {
		t.Fatalf("got %d attempts, want 2", got)
	}
}

func TestSendActiveDeliverySurvivesCleanup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		started, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		api := &spyBotAPI{hook: func(_ *http.Request, m sentMessage) error {
			if strings.Contains(m.Text, "Title") {
				once.Do(func() { close(started); <-release })
			}
			return nil
		}}
		sender := newSender(t, api, []string{"111"})
		done := make(chan error, 1)
		go func() { done <- sender.Send(t.Context(), testMessage) }()
		<-started
		time.Sleep(2 * time.Hour)
		other := testMessage
		other.BookmarkID, other.Title = "other", "Other"
		if err := sender.Send(t.Context(), other); err != nil {
			t.Fatal(err)
		}
		close(release)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if err := sender.Send(t.Context(), testMessage); err != nil {
			t.Fatal(err)
		}
		if got := len(api.messages()); got != 2 {
			t.Fatalf("active delivery evicted: %d attempts", got)
		}
	})
}

func TestNewRejectsMissingSettings(t *testing.T) {
	if _, err := telegram.New("", []string{"1"}, bot.WithSkipGetMe()); err == nil {
		t.Error("New with empty token succeeded")
	}
	if _, err := telegram.New("token", nil, bot.WithSkipGetMe()); err == nil {
		t.Error("New without chat ids succeeded")
	}
}

func TestSendRejectsNoDeliverableParts(t *testing.T) {
	api := &spyBotAPI{}
	sender := newSender(t, api, []string{"111"})
	for _, msg := range []domain.Message{{}, {Title: " ", URL: " ", Summary: "\n\t"}} {
		if err := sender.Send(t.Context(), msg); err == nil {
			t.Fatal("empty rendered message silently succeeded")
		}
	}
	if got := len(api.messages()); got != 0 {
		t.Fatalf("sent %d empty messages", got)
	}
}

func TestSendAttachesInlineKeyboardOnFinalPart(t *testing.T) {
	api := &spyBotAPI{}
	sender := newSender(t, api, []string{"111"})
	if _, err := sender.WithKarakeepURL("https://keep.example.com"); err != nil {
		t.Fatalf("WithKarakeepURL: %v", err)
	}

	msg := testMessage
	msg.Summary = strings.Repeat("a", 5000)
	if err := sender.Send(t.Context(), msg); err != nil {
		t.Fatalf("Send: %v", err)
	}

	reqs := api.requests()
	if len(reqs) != 2 {
		t.Fatalf("requests = %d, want 2", len(reqs))
	}
	if reqs[0].ReplyMarkup != "" {
		t.Errorf("first part reply_markup = %q, want empty", reqs[0].ReplyMarkup)
	}
	if !strings.Contains(reqs[1].ReplyMarkup, "https://e.com/a") || !strings.Contains(reqs[1].ReplyMarkup, "https://keep.example.com/dashboard/preview/b1") {
		t.Errorf("final part reply_markup = %q, want article and karakeep buttons", reqs[1].ReplyMarkup)
	}
}

func TestWithKarakeepURLRejectsUnsafeURLs(t *testing.T) {
	api := &spyBotAPI{}
	sender := newSender(t, api, []string{"111"})
	for _, bad := range []string{"", "ftp://keep.example.com", "https://user:pass@keep.example.com", "https://keep.example.com?x=1", "https://keep.example.com#frag"} {
		if _, err := sender.WithKarakeepURL(bad); err == nil {
			t.Errorf("WithKarakeepURL(%q) succeeded, want error", bad)
		}
	}
}

func TestSendEditsPendingMessageInPlaceWhenSummaryResolves(t *testing.T) {
	api := &spyBotAPI{}
	sender := newSender(t, api, []string{"111", "222"})
	if _, err := sender.WithKarakeepURL("https://keep.example.com"); err != nil {
		t.Fatal(err)
	}

	pendingMsg := domain.Message{BookmarkID: "b1", Title: "Title", URL: "https://e.com/a", SummaryPending: true}
	if err := sender.Send(t.Context(), pendingMsg); err != nil {
		t.Fatalf("Send pending: %v", err)
	}
	// Duplicate pending replay should remain deduplicated.
	if err := sender.Send(t.Context(), pendingMsg); err != nil {
		t.Fatalf("Send duplicate pending: %v", err)
	}

	readyMsg := domain.Message{BookmarkID: "b1", Title: "Title", URL: "https://e.com/a", Summary: "final summary"}
	if err := sender.Send(t.Context(), readyMsg); err != nil {
		t.Fatalf("Send resolved: %v", err)
	}
	// Subsequent completed replay should also be deduplicated.
	if err := sender.Send(t.Context(), readyMsg); err != nil {
		t.Fatalf("Send duplicate resolved: %v", err)
	}

	reqs := api.requests()
	if len(reqs) != 4 {
		t.Fatalf("requests = %d, want 4 (2 initial sends + 2 in-place edits)", len(reqs))
	}
	if reqs[0].Path != "/bottoken/sendMessage" || reqs[1].Path != "/bottoken/sendMessage" {
		t.Fatalf("initial paths = %q, %q, want /bottoken/sendMessage", reqs[0].Path, reqs[1].Path)
	}
	if reqs[2].Path != "/bottoken/editMessageText" || reqs[2].ChatID != "111" || reqs[2].MessageID != "1" || !strings.Contains(reqs[2].Text, "final summary") {
		t.Fatalf("first edit = %+v", reqs[2])
	}
	if reqs[3].Path != "/bottoken/editMessageText" || reqs[3].ChatID != "222" || reqs[3].MessageID != "2" || !strings.Contains(reqs[3].Text, "final summary") {
		t.Fatalf("second edit = %+v", reqs[3])
	}
}

func TestSendEditsFirstPartAndSendsOverflowPartsWhenResolvedSummaryIsLong(t *testing.T) {
	api := &spyBotAPI{}
	sender := newSender(t, api, []string{"111"})

	pendingMsg := domain.Message{BookmarkID: "b1", Title: "Title", URL: "https://e.com/a", SummaryPending: true}
	if err := sender.Send(t.Context(), pendingMsg); err != nil {
		t.Fatalf("Send pending: %v", err)
	}

	readyMsg := domain.Message{BookmarkID: "b1", Title: "Title", URL: "https://e.com/a", Summary: strings.Repeat("x", 5000)}
	if err := sender.Send(t.Context(), readyMsg); err != nil {
		t.Fatalf("Send long resolved: %v", err)
	}

	reqs := api.requests()
	if len(reqs) != 3 {
		t.Fatalf("requests = %d, want 3 (1 initial send + 1 edit + 1 overflow send)", len(reqs))
	}
	if reqs[1].Path != "/bottoken/editMessageText" || reqs[1].MessageID != "1" || reqs[1].ReplyMarkup != "" {
		t.Fatalf("edited part 0 = %+v, want editMessageText with cleared reply_markup", reqs[1])
	}
	if reqs[2].Path != "/bottoken/sendMessage" || reqs[2].ReplyMarkup == "" {
		t.Fatalf("overflow part 1 = %+v, want sendMessage with reply_markup on final part", reqs[2])
	}
}
