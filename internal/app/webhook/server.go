package webhook

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	crawledOperation = "crawled"
	maxBodyBytes     = 1 << 20
	shutdownTimeout  = 10 * time.Second
)

type BookmarkNotifier interface {
	Handle(ctx context.Context, bookmarkID string) error
}

type Options struct {
	Token    string
	Notifier BookmarkNotifier
}

type Server struct {
	tokenDigest [sha256.Size]byte
	notifier    BookmarkNotifier

	jobsCtx    context.Context
	cancelJobs context.CancelFunc
	jobs       sync.WaitGroup
}

func New(opts Options) (*Server, error) {
	switch {
	case opts.Token == "":
		return nil, errors.New("webhook token is required")
	case opts.Notifier == nil:
		return nil, errors.New("notifier is required")
	}

	jobsCtx, cancelJobs := context.WithCancel(context.Background())
	return &Server{
		tokenDigest: sha256.Sum256([]byte(opts.Token)),
		notifier:    opts.Notifier,
		jobsCtx:     jobsCtx,
		cancelJobs:  cancelJobs,
	}, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /webhook/karakeep", s.handleWebhook)
	mux.HandleFunc("GET /health", ok)
	mux.HandleFunc("GET /ready", ok)
	return mux
}

func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	select {
	case err := <-serveErr:
		s.cancelJobs()
		s.jobs.Wait()
		return fmt.Errorf("serve webhook: %w", err)
	case <-ctx.Done():
	}

	s.cancelJobs()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown webhook server: %w", err)
	}
	s.jobs.Wait()
	return nil
}

type webhookEvent struct {
	BookmarkID string `json:"bookmarkId"`
	Operation  string `json:"operation"`
}

func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		slog.WarnContext(r.Context(), "reject unauthenticated webhook", "remote_addr", r.RemoteAddr)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var event webhookEvent
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&event); err != nil || event.BookmarkID == "" {
		http.Error(w, "invalid webhook body", http.StatusBadRequest)
		return
	}
	if event.Operation != crawledOperation {
		w.WriteHeader(http.StatusOK)
		return
	}

	s.jobs.Go(func() {
		if err := s.notifier.Handle(s.jobsCtx, event.BookmarkID); err != nil {
			slog.ErrorContext(s.jobsCtx, "handle crawled bookmark", "bookmark_id", event.BookmarkID, "error", err)
		}
	})
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) authorized(r *http.Request) bool {
	got, found := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !found {
		return false
	}
	gotDigest := sha256.Sum256([]byte(got))
	return subtle.ConstantTimeCompare(gotDigest[:], s.tokenDigest[:]) == 1
}

func ok(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
}
