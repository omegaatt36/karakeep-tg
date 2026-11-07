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

	"github.com/omegaatt36/karakeep-tg/domain"
)

const (
	crawledOperation = "crawled"
	maxBodyBytes     = 1 << 20
	shutdownTimeout  = 10 * time.Second
	defaultWorkers   = 4
	defaultQueueSize = 64
	maxAttempts      = 3
)

type BookmarkNotifier interface {
	Handle(ctx context.Context, bookmarkID string) error
}

type Options struct {
	Token    string
	Notifier BookmarkNotifier

	// Zero values use four workers, 64 queued bookmarks, and a ten-second shutdown grace.
	WorkerCount     int
	QueueCapacity   int
	ShutdownTimeout time.Duration
}

type Server struct {
	tokenDigest [sha256.Size]byte
	notifier    BookmarkNotifier

	jobsCtx         context.Context
	cancelJobs      context.CancelFunc
	queue           chan string
	workers         sync.WaitGroup
	workersDone     chan struct{}
	shutdownTimeout time.Duration

	mu       sync.Mutex
	stopping bool
	pending  map[string]struct{}
}

func New(opts Options) (*Server, error) {
	switch {
	case opts.Token == "":
		return nil, errors.New("webhook token is required")
	case opts.Notifier == nil:
		return nil, errors.New("notifier is required")
	case opts.WorkerCount < 0 || opts.QueueCapacity < 0 || opts.ShutdownTimeout < 0:
		return nil, errors.New("worker count, queue capacity, and shutdown timeout must not be negative")
	}
	if opts.WorkerCount == 0 {
		opts.WorkerCount = defaultWorkers
	}
	if opts.QueueCapacity == 0 {
		opts.QueueCapacity = defaultQueueSize
	}
	if opts.ShutdownTimeout == 0 {
		opts.ShutdownTimeout = shutdownTimeout
	}

	jobsCtx, cancelJobs := context.WithCancel(context.Background())
	s := &Server{
		tokenDigest:     sha256.Sum256([]byte(opts.Token)),
		notifier:        opts.Notifier,
		jobsCtx:         jobsCtx,
		cancelJobs:      cancelJobs,
		queue:           make(chan string, opts.QueueCapacity),
		workersDone:     make(chan struct{}),
		shutdownTimeout: opts.ShutdownTimeout,
		pending:         make(map[string]struct{}),
	}
	for range opts.WorkerCount {
		s.workers.Go(s.work)
	}
	go func() {
		s.workers.Wait()
		close(s.workersDone)
	}()
	return s, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /webhook/karakeep", s.handleWebhook)
	mux.HandleFunc("GET /health", ok)
	mux.HandleFunc("GET /ready", s.ready)
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

	var errs []error
	select {
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			errs = append(errs, fmt.Errorf("serve webhook: %w", err))
		}
	case <-ctx.Done():
	}

	s.stopAccepting()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), s.shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		errs = append(errs, fmt.Errorf("shutdown webhook server: %w", err))
		if closeErr := srv.Close(); closeErr != nil {
			errs = append(errs, fmt.Errorf("close webhook server: %w", closeErr))
		}
	}
	if err := s.Close(shutdownCtx); err != nil {
		errs = append(errs, fmt.Errorf("drain webhook notifications: %w", err))
	}
	return errors.Join(errs...)
}

// Close rejects new work and waits for accepted notifications to finish. If ctx
// expires, it cancels workers and returns immediately. Notifiers must honor their
// context to stop running; an uncooperative notifier cannot delay Close.
// Call Close when using Handler without Serve to release the worker goroutines.
func (s *Server) Close(ctx context.Context) error {
	s.stopAccepting()
	select {
	case <-s.workersDone:
		s.cancelJobs()
		return nil
	default:
	}
	select {
	case <-s.workersDone:
		s.cancelJobs()
		return nil
	case <-ctx.Done():
		s.cancelJobs()
		return ctx.Err()
	}
}

func (s *Server) stopAccepting() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.stopping {
		s.stopping = true
		close(s.queue)
	}
}

func (s *Server) work() {
	for id := range s.queue {
		if s.jobsCtx.Err() != nil {
			return
		}
		s.deliver(id)
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
	}
}

func (s *Server) deliver(id string) {
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if s.jobsCtx.Err() != nil {
			return
		}
		err := s.notifier.Handle(s.jobsCtx, id)
		if err == nil {
			return
		}
		if attempt == maxAttempts || s.jobsCtx.Err() != nil {
			slog.ErrorContext(s.jobsCtx, "handle crawled bookmark", "bookmark_id", id, "attempts", attempt, "error", err)
			return
		}
		timer := time.NewTimer(time.Duration(attempt) * time.Second)
		select {
		case <-timer.C:
		case <-s.jobsCtx.Done():
			timer.Stop()
			return
		}
	}
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
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&event); err != nil || !domain.ValidBookmarkID(event.BookmarkID) {
		http.Error(w, "invalid webhook body", http.StatusBadRequest)
		return
	}
	if event.Operation != crawledOperation {
		w.WriteHeader(http.StatusOK)
		return
	}

	s.mu.Lock()
	accepted := false
	if !s.stopping {
		if _, exists := s.pending[event.BookmarkID]; exists {
			accepted = true
		} else {
			select {
			case s.queue <- event.BookmarkID:
				s.pending[event.BookmarkID] = struct{}{}
				accepted = true
			default:
			}
		}
	}
	s.mu.Unlock()
	if !accepted {
		w.Header().Set("Retry-After", "1")
		http.Error(w, "webhook queue unavailable", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) ready(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	available := !s.stopping && len(s.queue) < cap(s.queue)
	s.mu.Unlock()
	if !available {
		http.Error(w, "webhook queue unavailable", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
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
