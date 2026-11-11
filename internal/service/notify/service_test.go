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
	dedupeWindow   = time.Hour
)

type stubBookmarkSource struct {
	mu        sync.Mutex
	responses []domain.Bookmark
	errs      map[int]error
	err       error
	calls     int
	release   chan struct{}
}

func (s *stubBookmarkSource) GetBookmark(ctx context.Context, id string) (domain.Bookmark, error) {
	s.mu.Lock()
	callIndex := s.calls
	i := min(s.calls, max(len(s.responses)-1, 0))
	s.calls++
	release := s.release
	callErr := s.err
	if s.errs != nil {
		if specific, ok := s.errs[callIndex]; ok {
			callErr = specific
		}
	}
	s.mu.Unlock()

	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return domain.Bookmark{}, ctx.Err()
		}
	}
	if callErr != nil {
		return domain.Bookmark{}, callErr
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

func TestHandleDedupeWindowStartsAfterDelivery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		source := &stubBookmarkSource{
			responses: []domain.Bookmark{bookmark(domain.SummarySuccess, "s")},
			release:   make(chan struct{}),
		}
		notifier := &spyNotifier{}
		svc := newService(t, source, notifier)
		var wg sync.WaitGroup
		wg.Go(func() {
			if err := svc.Handle(t.Context(), "b1"); err != nil {
				t.Errorf("Handle: %v", err)
			}
		})
		synctest.Wait()
		time.Sleep(dedupeWindow + time.Second)
		wg.Go(func() {
			if err := svc.Handle(t.Context(), "b1"); err != nil {
				t.Errorf("duplicate Handle: %v", err)
			}
		})
		synctest.Wait()
		close(source.release)
		wg.Wait()
		if got := source.callCount(); got != 1 {
			t.Fatalf("active bookmark fetched %d times, want 1", got)
		}
		time.Sleep(dedupeWindow - time.Second)
		if err := svc.Handle(t.Context(), "b1"); err != nil {
			t.Fatal(err)
		}
		if got := len(notifier.messages()); got != 1 {
			t.Fatalf("sent %d within completion TTL, want 1", got)
		}
		time.Sleep(2 * time.Second)
		if err := svc.Handle(t.Context(), "b1"); err != nil {
			t.Fatal(err)
		}
		if got := len(notifier.messages()); got != 2 {
			t.Fatalf("sent %d after completion TTL, want 2", got)
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
		"missing source":    func(o *notify.Options) { o.Source = nil },
		"missing notifier":  func(o *notify.Options) { o.Notifier = nil },
		"zero interval":     func(o *notify.Options) { o.PollInterval = 0 },
		"zero timeout":      func(o *notify.Options) { o.SummaryTimeout = 0 },
		"negative followup": func(o *notify.Options) { o.FollowUpTimeout = -time.Second },
		"zero window":       func(o *notify.Options) { o.DedupeWindow = 0 },
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

func TestHandleFollowUpUpdatesPendingMessageWhenSummaryResolves(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		source := &stubBookmarkSource{responses: []domain.Bookmark{
			bookmark(domain.SummaryPending, ""),
			bookmark(domain.SummaryPending, ""),
			bookmark(domain.SummaryPending, ""),
			bookmark(domain.SummarySuccess, "late summary"),
		}}
		notifier := &spyNotifier{}
		svc, err := notify.New(notify.Options{
			Source:          source,
			Notifier:        notifier,
			PollInterval:    pollInterval,
			SummaryTimeout:  2 * pollInterval,
			FollowUpTimeout: 3 * pollInterval,
			DedupeWindow:    dedupeWindow,
		})
		if err != nil {
			t.Fatal(err)
		}

		if err := svc.Handle(t.Context(), "b1"); err != nil {
			t.Fatalf("Handle: %v", err)
		}

		got := notifier.messages()
		if len(got) != 2 {
			t.Fatalf("sent = %+v, want 2 messages (pending then resolved)", got)
		}
		if !got[0].SummaryPending || got[0].Summary != "" {
			t.Errorf("first message = %+v, want pending placeholder", got[0])
		}
		if got[1].SummaryPending || got[1].Summary != "late summary" {
			t.Errorf("second message = %+v, want resolved summary", got[1])
		}

		// Subsequent Handle within dedupe window should now be suppressed.
		if err := svc.Handle(t.Context(), "b1"); err != nil {
			t.Fatalf("duplicate Handle: %v", err)
		}
		if len(notifier.messages()) != 2 {
			t.Fatalf("sent %d messages after completion, want 2", len(notifier.messages()))
		}
	})
}

func TestHandleFollowUpTimeoutAndRetryPreservesPendingState(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		source := &stubBookmarkSource{responses: []domain.Bookmark{
			bookmark(domain.SummaryPending, ""),
		}}
		notifier := &spyNotifier{}
		svc, err := notify.New(notify.Options{
			Source:          source,
			Notifier:        notifier,
			PollInterval:    pollInterval,
			SummaryTimeout:  pollInterval,
			FollowUpTimeout: pollInterval,
			DedupeWindow:    dedupeWindow,
		})
		if err != nil {
			t.Fatal(err)
		}

		// Both initial and follow-up timeouts expire with pending status; only 1 pending message is sent.
		if err := svc.Handle(t.Context(), "b1"); err != nil {
			t.Fatalf("first Handle: %v", err)
		}
		if got := len(notifier.messages()); got != 1 || !notifier.messages()[0].SummaryPending {
			t.Fatalf("after follow-up timeout sent = %+v, want 1 pending message", notifier.messages())
		}

		// A second Handle encounters ErrBookmarkNotFound during follow-up, releasing while preserving pending state.
		source.mu.Lock()
		source.errs = map[int]error{2: domain.ErrBookmarkNotFound}
		source.calls = 0
		source.mu.Unlock()
		if err := svc.Handle(t.Context(), "b1"); !errors.Is(err, domain.ErrBookmarkNotFound) {
			t.Fatalf("second Handle error = %v, want ErrBookmarkNotFound", err)
		}
		if got := len(notifier.messages()); got != 1 {
			t.Fatalf("second Handle sent extra pending message: %+v", notifier.messages())
		}
	})
}

func TestHandleSubsequentCallUpgradesPreviouslyTimedOutPendingBookmark(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		source := &stubBookmarkSource{responses: []domain.Bookmark{bookmark(domain.SummaryPending, "")}}
		notifier := &spyNotifier{}
		svc := newService(t, source, notifier)

		if err := svc.Handle(t.Context(), "b1"); err != nil {
			t.Fatalf("first Handle: %v", err)
		}
		if got := len(notifier.messages()); got != 1 || !notifier.messages()[0].SummaryPending {
			t.Fatalf("after timeout sent = %+v, want 1 pending message", notifier.messages())
		}

		source.mu.Lock()
		source.responses = []domain.Bookmark{bookmark(domain.SummarySuccess, "resolved on replay")}
		source.calls = 0
		source.mu.Unlock()

		if err := svc.Handle(t.Context(), "b1"); err != nil {
			t.Fatalf("second Handle: %v", err)
		}
		got := notifier.messages()
		if len(got) != 2 || got[1].SummaryPending || got[1].Summary != "resolved on replay" {
			t.Fatalf("after replay sent = %+v, want pending followed by resolved", got)
		}

		// Third call within dedupeWindow should be suppressed now that it resolved.
		if err := svc.Handle(t.Context(), "b1"); err != nil {
			t.Fatalf("third Handle: %v", err)
		}
		if len(notifier.messages()) != 2 {
			t.Fatalf("third Handle sent extra message: %+v", notifier.messages())
		}
	})
}

func TestHandleToleratesTransientErrorsDuringPollingAndStopsOnNotFound(t *testing.T) {
	t.Run("recovers from transient poll error", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			source := &stubBookmarkSource{
				responses: []domain.Bookmark{
					bookmark(domain.SummaryPending, ""),
					bookmark(domain.SummaryPending, ""),
					bookmark(domain.SummarySuccess, "recovered summary"),
				},
				errs: map[int]error{
					1: errors.New("temporary 502 bad gateway"),
				},
			}
			notifier := &spyNotifier{}
			if err := newService(t, source, notifier).Handle(t.Context(), "b1"); err != nil {
				t.Fatalf("Handle: %v", err)
			}
			got := notifier.messages()
			if len(got) != 1 || got[0].Summary != "recovered summary" {
				t.Fatalf("sent = %+v, want recovered summary", got)
			}
		})
	})

	t.Run("stops immediately when bookmark is deleted during polling", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			source := &stubBookmarkSource{
				responses: []domain.Bookmark{
					bookmark(domain.SummaryPending, ""),
					bookmark(domain.SummarySuccess, "unreachable"),
				},
				errs: map[int]error{
					1: domain.ErrBookmarkNotFound,
				},
			}
			notifier := &spyNotifier{}
			err := newService(t, source, notifier).Handle(t.Context(), "b1")
			if !errors.Is(err, domain.ErrBookmarkNotFound) {
				t.Fatalf("Handle error = %v, want ErrBookmarkNotFound", err)
			}
			if got := len(notifier.messages()); got != 0 {
				t.Fatalf("sent = %d, want 0", got)
			}
		})
	})
}
