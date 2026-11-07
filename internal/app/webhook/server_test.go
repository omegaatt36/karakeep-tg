package webhook_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/omegaatt36/karakeep-tg/internal/app/webhook"
)

const token = "s3cret-token"

type spyBookmarkNotifier struct {
	mu      sync.Mutex
	handled []string
	block   chan struct{}
	started chan struct{}
	ctxDone chan struct{}
}

func (n *spyBookmarkNotifier) Handle(ctx context.Context, id string) error {
	n.mu.Lock()
	n.handled = append(n.handled, id)
	n.mu.Unlock()

	if n.started != nil {
		close(n.started)
	}
	if n.block != nil {
		select {
		case <-n.block:
		case <-ctx.Done():
			close(n.ctxDone)
			return ctx.Err()
		}
	}
	return nil
}

func (n *spyBookmarkNotifier) ids() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.handled...)
}

func newServer(t *testing.T, notifier webhook.BookmarkNotifier) *webhook.Server {
	return newServerWithOptions(t, webhook.Options{Notifier: notifier})
}

func newServerWithOptions(t *testing.T, opts webhook.Options) *webhook.Server {
	t.Helper()
	opts.Token = token
	srv, err := webhook.New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := srv.Close(ctx); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return srv
}

func post(h http.Handler, authorization, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/webhook/karakeep", strings.NewReader(body))
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func crawledBody(id string) string {
	return `{"jobId":"j1","bookmarkId":"` + id + `","userId":"u1","url":"https://e.com","type":"link","operation":"crawled"}`
}

func TestWebhookRejectsUnauthenticatedRequests(t *testing.T) {
	tests := []struct {
		name          string
		authorization string
		body          string
	}{
		{"missing header", "", crawledBody("b1")},
		{"wrong scheme", "Basic " + token, crawledBody("b1")},
		{"lowercase scheme", "bearer " + token, crawledBody("b1")},
		{"bare token", token, crawledBody("b1")},
		{"wrong token", "Bearer wrong", crawledBody("b1")},
		{"token prefix", "Bearer " + token[:4], crawledBody("b1")},
		{"token with suffix", "Bearer " + token + "x", crawledBody("b1")},
		{"token with trailing space", "Bearer " + token + " ", crawledBody("b1")},
		{"double space", "Bearer  " + token, crawledBody("b1")},
		{"empty bearer", "Bearer ", crawledBody("b1")},
		{"malformed body", "Bearer wrong", `{`},
		{"non crawled operation", "Bearer wrong", `{"bookmarkId":"b1","operation":"edited"}`},
		{"empty body", "", ``},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				notifier := &spyBookmarkNotifier{}
				srv := newServer(t, notifier)

				rec := post(srv.Handler(), tt.authorization, tt.body)
				synctest.Wait()

				if rec.Code != http.StatusUnauthorized {
					t.Fatalf("status = %d, want 401", rec.Code)
				}
				if got := notifier.ids(); len(got) != 0 {
					t.Fatalf("notifier handled %v, want nothing", got)
				}
			})
		})
	}
}

func TestWebhookAcceptsCorrectToken(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		notifier := &spyBookmarkNotifier{}
		srv := newServer(t, notifier)

		rec := post(srv.Handler(), "Bearer "+token, crawledBody("b1"))
		synctest.Wait()

		if rec.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202", rec.Code)
		}
		if got := notifier.ids(); len(got) != 1 || got[0] != "b1" {
			t.Fatalf("handled = %v, want [b1]", got)
		}
	})
}

func TestWebhookFiltersOperationsAndMalformedBodies(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		wantStatus  int
		wantHandled int
	}{
		{"crawled", crawledBody("b1"), http.StatusAccepted, 1},
		{"created", `{"bookmarkId":"b1","operation":"created"}`, http.StatusOK, 0},
		{"edited", `{"bookmarkId":"b1","operation":"edited"}`, http.StatusOK, 0},
		{"ai tagged", `{"bookmarkId":"b1","operation":"ai tagged"}`, http.StatusOK, 0},
		{"deleted", `{"bookmarkId":"b1","operation":"deleted"}`, http.StatusOK, 0},
		{"invalid json", `{`, http.StatusBadRequest, 0},
		{"empty body", ``, http.StatusBadRequest, 0},
		{"missing bookmark id", `{"operation":"crawled"}`, http.StatusBadRequest, 0},
		{"empty bookmark id", `{"bookmarkId":"","operation":"crawled"}`, http.StatusBadRequest, 0},
		{"bookmark path traversal", crawledBody("../b1"), http.StatusBadRequest, 0},
		{"bookmark query", crawledBody("b1?x=y"), http.StatusBadRequest, 0},
		{"bookmark whitespace", crawledBody("b 1"), http.StatusBadRequest, 0},
		{"missing bookmark id on other operation", `{"operation":"edited"}`, http.StatusBadRequest, 0},
		{"oversized body", `{"bookmarkId":"b1","operation":"crawled","padding":"` + strings.Repeat("a", 2<<20) + `"}`, http.StatusBadRequest, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				notifier := &spyBookmarkNotifier{}
				srv := newServer(t, notifier)

				rec := post(srv.Handler(), "Bearer "+token, tt.body)
				synctest.Wait()

				if rec.Code != tt.wantStatus {
					t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
				}
				if got := len(notifier.ids()); got != tt.wantHandled {
					t.Fatalf("handled %d bookmarks, want %d", got, tt.wantHandled)
				}
			})
		})
	}
}

func TestWebhookAcknowledgesBeforeNotifierFinishes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		notifier := &spyBookmarkNotifier{block: make(chan struct{})}
		srv := newServer(t, notifier)

		rec := post(srv.Handler(), "Bearer "+token, crawledBody("b1"))
		synctest.Wait()

		if rec.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202 while the notifier is still blocked", rec.Code)
		}
		close(notifier.block)
	})
}

func TestWebhookRejectsNonPost(t *testing.T) {
	srv := newServer(t, &spyBookmarkNotifier{})
	req := httptest.NewRequest(http.MethodGet, "/webhook/karakeep", nil)
	rec := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestProbes(t *testing.T) {
	srv := newServer(t, &spyBookmarkNotifier{})
	for _, path := range []string{"/health", "/ready"} {
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s status = %d, want 200", path, rec.Code)
		}
	}
}

func TestNewRejectsMissingSettings(t *testing.T) {
	if _, err := webhook.New(webhook.Options{Notifier: &spyBookmarkNotifier{}}); err == nil {
		t.Error("New without token succeeded, want error")
	}
	if _, err := webhook.New(webhook.Options{Token: token}); err == nil {
		t.Error("New without notifier succeeded, want error")
	}
	for _, opts := range []webhook.Options{
		{WorkerCount: -1},
		{QueueCapacity: -1},
		{ShutdownTimeout: -time.Second},
	} {
		opts.Token = token
		opts.Notifier = &spyBookmarkNotifier{}
		if srv, err := webhook.New(opts); err == nil {
			srv.Close(context.Background())
			t.Errorf("New with negative limit succeeded: %+v", opts)
		}
	}
}

func TestServeCancelsInFlightWorkAndShutsDown(t *testing.T) {
	notifier := &spyBookmarkNotifier{
		block:   make(chan struct{}),
		started: make(chan struct{}),
		ctxDone: make(chan struct{}),
	}
	srv := newServerWithOptions(t, webhook.Options{Notifier: notifier, ShutdownTimeout: 20 * time.Millisecond})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ctx, ln) }()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+ln.Addr().String()+"/webhook/karakeep", strings.NewReader(crawledBody("b1")))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()
	<-notifier.started

	cancel()

	select {
	case err := <-served:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Serve = %v, want deadline exceeded after grace period", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not return promptly after cancel")
	}
	select {
	case <-notifier.ctxDone:
	case <-time.After(time.Second):
		t.Fatal("in-flight notifier context was not canceled")
	}
	if conn, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second); err == nil {
		conn.Close()
		t.Fatal("server still accepts connections after shutdown")
	}
}

type notifierFunc func(context.Context, string) error

func (f notifierFunc) Handle(ctx context.Context, id string) error { return f(ctx, id) }

func TestWebhookBoundsWorkersAndQueue(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		block := make(chan struct{})
		var mu sync.Mutex
		var active, maxActive, calls int
		notifier := notifierFunc(func(ctx context.Context, _ string) error {
			mu.Lock()
			active++
			calls++
			maxActive = max(maxActive, active)
			mu.Unlock()
			select {
			case <-block:
			case <-ctx.Done():
			}
			mu.Lock()
			active--
			mu.Unlock()
			return nil
		})
		srv := newServerWithOptions(t, webhook.Options{Notifier: notifier, WorkerCount: 2, QueueCapacity: 1})
		for _, id := range []string{"b1", "b2", "b3"} {
			if rec := post(srv.Handler(), "Bearer "+token, crawledBody(id)); rec.Code != http.StatusAccepted {
				t.Fatalf("enqueue %s status = %d, want 202", id, rec.Code)
			}
			synctest.Wait()
		}
		before := time.Now()
		rec := post(srv.Handler(), "Bearer "+token, crawledBody("b4"))
		if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
			t.Fatalf("full queue response = %d, headers %v, want 503 with Retry-After", rec.Code, rec.Header())
		}
		if time.Since(before) != 0 {
			t.Fatal("full queue request waited for a worker")
		}
		ready := httptest.NewRecorder()
		srv.Handler().ServeHTTP(ready, httptest.NewRequest(http.MethodGet, "/ready", nil))
		if ready.Code != http.StatusServiceUnavailable {
			t.Fatalf("full queue readiness = %d, want 503", ready.Code)
		}
		mu.Lock()
		if active != 2 || calls != 2 || maxActive != 2 {
			t.Errorf("active/calls/max = %d/%d/%d, want 2/2/2", active, calls, maxActive)
		}
		mu.Unlock()
		close(block)
		synctest.Wait()
		mu.Lock()
		defer mu.Unlock()
		if calls != 3 || maxActive > 2 {
			t.Errorf("calls/max = %d/%d, want 3/2", calls, maxActive)
		}
	})
}

func TestWebhookCoalescesPendingBookmarksAndAllowsLaterDelivery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		block := make(chan struct{})
		var ids []string
		notifier := notifierFunc(func(_ context.Context, id string) error {
			ids = append(ids, id)
			<-block
			return nil
		})
		srv := newServerWithOptions(t, webhook.Options{Notifier: notifier, WorkerCount: 1, QueueCapacity: 1})
		for _, id := range []string{"b1", "b1", "b2", "b2"} {
			if rec := post(srv.Handler(), "Bearer "+token, crawledBody(id)); rec.Code != http.StatusAccepted {
				t.Fatalf("enqueue %s status = %d, want 202", id, rec.Code)
			}
			synctest.Wait()
		}
		if len(ids) != 1 {
			t.Fatalf("calls before release = %v, want [b1]", ids)
		}
		close(block)
		synctest.Wait()
		if len(ids) != 2 {
			t.Fatalf("calls after drain = %v, want [b1 b2]", ids)
		}
		post(srv.Handler(), "Bearer "+token, crawledBody("b1"))
		synctest.Wait()
		if len(ids) != 3 {
			t.Fatalf("later delivery calls = %v, want [b1 b2 b1]", ids)
		}
	})
}

func TestWebhookRetriesFailuresWithBoundedBackoff(t *testing.T) {
	for _, failures := range []int{1, 2, 3} {
		t.Run(string(rune('0'+failures))+" failures", func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var attempts []time.Time
				notifier := notifierFunc(func(context.Context, string) error {
					attempts = append(attempts, time.Now())
					if len(attempts) <= failures {
						return errors.New("temporary failure")
					}
					return nil
				})
				srv := newServer(t, notifier)
				post(srv.Handler(), "Bearer "+token, crawledBody("b1"))
				time.Sleep(4 * time.Second)
				synctest.Wait()
				if len(attempts) != min(failures+1, 3) {
					t.Fatalf("attempts = %d, want %d", len(attempts), min(failures+1, 3))
				}
				if attempts[1].Sub(attempts[0]) != time.Second {
					t.Errorf("first retry delay = %v, want 1s", attempts[1].Sub(attempts[0]))
				}
				if len(attempts) == 3 && attempts[2].Sub(attempts[1]) != 2*time.Second {
					t.Errorf("second retry delay = %v, want 2s", attempts[2].Sub(attempts[1]))
				}
				post(srv.Handler(), "Bearer "+token, crawledBody("b1"))
				synctest.Wait()
				if len(attempts) != min(failures+1, 3)+1 {
					t.Fatalf("subsequent delivery attempts = %d, want another call", len(attempts))
				}
			})
		})
	}
}

func TestCloseDrainsQueuedWorkAndRejectsNewWork(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		block := make(chan struct{})
		var calls int
		notifier := notifierFunc(func(ctx context.Context, _ string) error {
			calls++
			select {
			case <-block:
				return nil
			case <-ctx.Done():
				t.Error("worker canceled before grace expired")
				return ctx.Err()
			}
		})
		srv := newServerWithOptions(t, webhook.Options{Notifier: notifier, WorkerCount: 1, QueueCapacity: 1})
		post(srv.Handler(), "Bearer "+token, crawledBody("b1"))
		synctest.Wait()
		post(srv.Handler(), "Bearer "+token, crawledBody("b2"))
		closed := make(chan error, 1)
		go func() { closed <- srv.Close(context.Background()) }()
		synctest.Wait()
		for _, id := range []string{"b1", "b3"} {
			if rec := post(srv.Handler(), "Bearer "+token, crawledBody(id)); rec.Code != http.StatusServiceUnavailable {
				t.Errorf("stopping enqueue status = %d, want 503", rec.Code)
			}
		}
		close(block)
		if err := <-closed; err != nil {
			t.Fatalf("Close: %v", err)
		}
		if calls != 2 {
			t.Fatalf("drained calls = %d, want 2", calls)
		}
	})
}

func TestCloseDeadlineBoundsUncooperativeNotifier(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		block := make(chan struct{})
		var jobCtx context.Context
		var calls int
		notifier := notifierFunc(func(ctx context.Context, _ string) error {
			calls++
			jobCtx = ctx
			<-block
			return nil
		})
		srv := newServerWithOptions(t, webhook.Options{Notifier: notifier, WorkerCount: 1, QueueCapacity: 1})
		post(srv.Handler(), "Bearer "+token, crawledBody("b1"))
		synctest.Wait()
		post(srv.Handler(), "Bearer "+token, crawledBody("b2"))
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		before := time.Now()
		if err := srv.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Close = %v, want deadline exceeded", err)
		}
		if elapsed := time.Since(before); elapsed != time.Second {
			t.Fatalf("Close took %v, want 1s", elapsed)
		}
		if !errors.Is(jobCtx.Err(), context.Canceled) {
			t.Errorf("worker context = %v, want canceled", jobCtx.Err())
		}
		close(block)
		synctest.Wait()
		if calls != 1 {
			t.Errorf("calls after canceled drain = %d, want 1", calls)
		}
	})
}

func TestCloseCanRaceWithConcurrentEnqueues(t *testing.T) {
	srv := newServer(t, &spyBookmarkNotifier{})
	var requests sync.WaitGroup
	start := make(chan struct{})
	statuses := make(chan int, 32)
	for i := range 32 {
		requests.Go(func() {
			<-start
			id := "b" + strings.Repeat("x", i)
			statuses <- post(srv.Handler(), "Bearer "+token, crawledBody(id)).Code
		})
	}
	requests.Go(func() {
		<-start
		if err := srv.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	close(start)
	requests.Wait()
	close(statuses)
	for status := range statuses {
		if status != http.StatusAccepted && status != http.StatusServiceUnavailable {
			t.Errorf("concurrent request status = %d, want 202 or 503", status)
		}
	}
}

func TestCloseCancelsRetryBackoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls int
		srv := newServer(t, notifierFunc(func(context.Context, string) error {
			calls++
			return errors.New("temporary failure")
		}))
		post(srv.Handler(), "Bearer "+token, crawledBody("b1"))
		synctest.Wait()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := srv.Close(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("Close = %v, want canceled", err)
		}
		synctest.Wait()
		if calls != 1 {
			t.Fatalf("calls after shutdown = %d, want 1", calls)
		}
	})
}
