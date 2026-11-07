package webhook_test

import (
	"context"
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
	t.Helper()
	srv, err := webhook.New(webhook.Options{Token: token, Notifier: notifier})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
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
}

func TestServeCancelsInFlightWorkAndShutsDown(t *testing.T) {
	notifier := &spyBookmarkNotifier{
		block:   make(chan struct{}),
		started: make(chan struct{}),
		ctxDone: make(chan struct{}),
	}
	srv := newServer(t, notifier)
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
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not return promptly after cancel")
	}
	select {
	case <-notifier.ctxDone:
	default:
		t.Fatal("in-flight notifier context was not canceled")
	}
	if conn, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second); err == nil {
		conn.Close()
		t.Fatal("server still accepts connections after shutdown")
	}
}
