package notify_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/omegaatt36/karakeep-tg/domain"
	"github.com/omegaatt36/karakeep-tg/internal/service/notify"
)

const (
	pollInterval   = 5 * time.Second
	summaryTimeout = 5 * time.Minute
	dedupeWindow   = 24 * time.Hour
)

type stubBookmarkSource struct {
	mu        sync.Mutex
	responses []domain.Bookmark
	err       error
	calls     int
	release   chan struct{}
}

func (s *stubBookmarkSource) GetBookmark(ctx context.Context, id string) (domain.Bookmark, error) {
	s.mu.Lock()
	i := min(s.calls, len(s.responses)-1)
	s.calls++
	release := s.release
	s.mu.Unlock()

	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return domain.Bookmark{}, ctx.Err()
		}
	}
	if s.err != nil {
		return domain.Bookmark{}, s.err
	}
	b := s.responses[i]
	b.ID = id
	return b, nil
}

func (s *stubBookmarkSource) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

type spyNotifier struct {
	mu   sync.Mutex
	sent []domain.Message
}

func (n *spyNotifier) Send(_ context.Context, msg domain.Message) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.sent = append(n.sent, msg)
	return nil
}

func (n *spyNotifier) messages() []domain.Message {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]domain.Message(nil), n.sent...)
}

func bookmark(status domain.SummaryStatus, summary string) domain.Bookmark {
	return domain.Bookmark{
		Title:         "Title",
		URL:           "https://example.com/a",
		Summary:       summary,
		SummaryStatus: status,
	}
}

func newService(t *testing.T, source notify.BookmarkSource, notifier notify.Notifier) *notify.Service {
	t.Helper()
	svc, err := notify.New(notify.Options{
		Source:         source,
		Notifier:       notifier,
		PollInterval:   pollInterval,
		SummaryTimeout: summaryTimeout,
		DedupeWindow:   dedupeWindow,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return svc
}

func TestHandleSendsSummaryOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		source := &stubBookmarkSource{responses: []domain.Bookmark{bookmark(domain.SummarySuccess, "short summary")}}
		notifier := &spyNotifier{}

		if err := newService(t, source, notifier).Handle(t.Context(), "b1"); err != nil {
			t.Fatalf("Handle: %v", err)
		}

		want := domain.Message{
			BookmarkID: "b1",
			Title:      "Title",
			URL:        "https://example.com/a",
			Summary:    "short summary",
		}
		got := notifier.messages()
		if len(got) != 1 || got[0] != want {
			t.Fatalf("sent = %+v, want [%+v]", got, want)
		}
	})
}

func TestHandlePollsWhilePending(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		source := &stubBookmarkSource{responses: []domain.Bookmark{
			bookmark(domain.SummaryPending, ""),
			bookmark(domain.SummaryPending, ""),
			bookmark(domain.SummarySuccess, "ready"),
		}}
		notifier := &spyNotifier{}

		start := time.Now()
		if err := newService(t, source, notifier).Handle(t.Context(), "b1"); err != nil {
			t.Fatalf("Handle: %v", err)
		}

		if got := source.callCount(); got != 3 {
			t.Fatalf("reads = %d, want 3", got)
		}
		if elapsed := time.Since(start); elapsed != 2*pollInterval {
			t.Fatalf("elapsed = %v, want %v", elapsed, 2*pollInterval)
		}
		got := notifier.messages()
		if len(got) != 1 || got[0].Summary != "ready" || got[0].SummaryPending {
			t.Fatalf("sent = %+v, want one message with summary", got)
		}
	})
}

func TestHandleTimeoutSendsPendingMarker(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		source := &stubBookmarkSource{responses: []domain.Bookmark{bookmark(domain.SummaryPending, "")}}
		notifier := &spyNotifier{}

		start := time.Now()
		if err := newService(t, source, notifier).Handle(t.Context(), "b1"); err != nil {
			t.Fatalf("Handle: %v", err)
		}

		if elapsed := time.Since(start); elapsed != summaryTimeout {
			t.Fatalf("elapsed = %v, want %v", elapsed, summaryTimeout)
		}
		got := notifier.messages()
		if len(got) != 1 {
			t.Fatalf("sent %d messages, want 1", len(got))
		}
		if !got[0].SummaryPending || got[0].Summary != "" || got[0].Title != "Title" || got[0].URL != "https://example.com/a" {
			t.Fatalf("message = %+v, want title and URL with pending marker and no summary", got[0])
		}
	})
}

func TestHandleTerminalWithoutSummarySendsImmediately(t *testing.T) {
	tests := []struct {
		name string
		got  domain.Bookmark
	}{
		{"failure", bookmark(domain.SummaryFailure, "")},
		{"success with empty summary", bookmark(domain.SummarySuccess, "")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				source := &stubBookmarkSource{responses: []domain.Bookmark{tt.got}}
				notifier := &spyNotifier{}

				start := time.Now()
				if err := newService(t, source, notifier).Handle(t.Context(), "b1"); err != nil {
					t.Fatalf("Handle: %v", err)
				}

				if elapsed := time.Since(start); elapsed != 0 {
					t.Fatalf("elapsed = %v, want 0", elapsed)
				}
				if got := source.callCount(); got != 1 {
					t.Fatalf("reads = %d, want 1", got)
				}
				got := notifier.messages()
				if len(got) != 1 || got[0].Summary != "" || got[0].SummaryPending {
					t.Fatalf("sent = %+v, want one message without summary or marker", got)
				}
			})
		})
	}
}

func TestHandleSuppressesDuplicates(t *testing.T) {
	t.Run("sequential within window", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			source := &stubBookmarkSource{responses: []domain.Bookmark{bookmark(domain.SummarySuccess, "s")}}
			notifier := &spyNotifier{}
			svc := newService(t, source, notifier)

			for range 2 {
				if err := svc.Handle(t.Context(), "b1"); err != nil {
					t.Fatalf("Handle: %v", err)
				}
			}
			if err := svc.Handle(t.Context(), "b2"); err != nil {
				t.Fatalf("Handle: %v", err)
			}

			if got := len(notifier.messages()); got != 2 {
				t.Fatalf("sent %d messages, want 2 (b1 once, b2 once)", got)
			}
		})
	})

	t.Run("concurrent while in flight", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			source := &stubBookmarkSource{
				responses: []domain.Bookmark{bookmark(domain.SummarySuccess, "s")},
				release:   make(chan struct{}),
			}
			notifier := &spyNotifier{}
			svc := newService(t, source, notifier)

			var wg sync.WaitGroup
			for range 2 {
				wg.Go(func() {
					if err := svc.Handle(t.Context(), "b1"); err != nil {
						t.Errorf("Handle: %v", err)
					}
				})
			}
			synctest.Wait()
			close(source.release)
			wg.Wait()

			if got := len(notifier.messages()); got != 1 {
				t.Fatalf("sent %d messages, want 1", got)
			}
			if got := source.callCount(); got != 1 {
				t.Fatalf("reads = %d, want 1", got)
			}
		})
	})

	t.Run("notifies again after window", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			source := &stubBookmarkSource{responses: []domain.Bookmark{bookmark(domain.SummarySuccess, "s")}}
			notifier := &spyNotifier{}
			svc := newService(t, source, notifier)

			if err := svc.Handle(t.Context(), "b1"); err != nil {
				t.Fatalf("Handle: %v", err)
			}
			time.Sleep(dedupeWindow - time.Second)
			if err := svc.Handle(t.Context(), "b1"); err != nil {
				t.Fatalf("Handle: %v", err)
			}
			if got := len(notifier.messages()); got != 1 {
				t.Fatalf("sent %d messages just inside window, want 1", got)
			}

			time.Sleep(2 * time.Second)
			if err := svc.Handle(t.Context(), "b1"); err != nil {
				t.Fatalf("Handle: %v", err)
			}
			if got := len(notifier.messages()); got != 2 {
				t.Fatalf("sent %d messages after window, want 2", got)
			}
		})
	})
}

func TestHandleStopsWhenContextCanceled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		source := &stubBookmarkSource{responses: []domain.Bookmark{bookmark(domain.SummaryPending, "")}}
		notifier := &spyNotifier{}
		svc := newService(t, source, notifier)

		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- svc.Handle(ctx, "b1") }()

		synctest.Wait()
		cancel()

		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("Handle error = %v, want context.Canceled", err)
		}
		if got := len(notifier.messages()); got != 0 {
			t.Fatalf("sent %d messages after cancel, want 0", got)
		}
	})
}

func TestHandleSourceErrorSendsNothingAndAllowsRetry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		boom := errors.New("boom")
		source := &stubBookmarkSource{responses: []domain.Bookmark{bookmark(domain.SummarySuccess, "s")}, err: boom}
		notifier := &spyNotifier{}
		svc := newService(t, source, notifier)

		if err := svc.Handle(t.Context(), "b1"); !errors.Is(err, boom) {
			t.Fatalf("Handle error = %v, want %v", err, boom)
		}

		source.mu.Lock()
		source.err = nil
		source.mu.Unlock()
		if err := svc.Handle(t.Context(), "b1"); err != nil {
			t.Fatalf("Handle retry: %v", err)
		}
		if got := len(notifier.messages()); got != 1 {
			t.Fatalf("sent %d messages, want 1", got)
		}
	})
}

func TestNewRejectsInvalidOptions(t *testing.T) {
	valid := notify.Options{
		Source:         &stubBookmarkSource{},
		Notifier:       &spyNotifier{},
		PollInterval:   pollInterval,
		SummaryTimeout: summaryTimeout,
		DedupeWindow:   dedupeWindow,
	}
	tests := map[string]func(*notify.Options){
		"missing source":   func(o *notify.Options) { o.Source = nil },
		"missing notifier": func(o *notify.Options) { o.Notifier = nil },
		"zero interval":    func(o *notify.Options) { o.PollInterval = 0 },
		"zero timeout":     func(o *notify.Options) { o.SummaryTimeout = 0 },
		"zero window":      func(o *notify.Options) { o.DedupeWindow = 0 },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			opts := valid
			mutate(&opts)
			if _, err := notify.New(opts); err == nil {
				t.Fatal("New succeeded, want error")
			}
		})
	}
}
