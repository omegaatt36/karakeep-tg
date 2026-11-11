package notify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/omegaatt36/karakeep-tg/domain"
)

type BookmarkSource interface {
	GetBookmark(ctx context.Context, id string) (domain.Bookmark, error)
}

type Notifier interface {
	Send(ctx context.Context, msg domain.Message) error
}

type Options struct {
	Source          BookmarkSource
	Notifier        Notifier
	PollInterval    time.Duration
	SummaryTimeout  time.Duration
	FollowUpTimeout time.Duration
	DedupeWindow    time.Duration
}

type Service struct {
	source          BookmarkSource
	notifier        Notifier
	pollInterval    time.Duration
	summaryTimeout  time.Duration
	followUpTimeout time.Duration
	dedupeWindow    time.Duration

	mu          sync.Mutex
	claimed     map[string]claim
	nextCleanup time.Time
}

type claim struct {
	completedAt time.Time
	inFlight    bool
	pending     bool
}

func New(opts Options) (*Service, error) {
	switch {
	case opts.Source == nil:
		return nil, errors.New("bookmark source is required")
	case opts.Notifier == nil:
		return nil, errors.New("notifier is required")
	case opts.PollInterval <= 0:
		return nil, errors.New("poll interval must be positive")
	case opts.SummaryTimeout <= 0:
		return nil, errors.New("summary timeout must be positive")
	case opts.FollowUpTimeout < 0:
		return nil, errors.New("follow-up timeout must not be negative")
	case opts.DedupeWindow <= 0:
		return nil, errors.New("dedupe window must be positive")
	}

	return &Service{
		source:          opts.Source,
		notifier:        opts.Notifier,
		pollInterval:    opts.PollInterval,
		summaryTimeout:  opts.SummaryTimeout,
		followUpTimeout: opts.FollowUpTimeout,
		dedupeWindow:    opts.DedupeWindow,
		claimed:         make(map[string]claim),
	}, nil
}

func (s *Service) Handle(ctx context.Context, bookmarkID string) error {
	claimed, prevPending, prevCompletedAt := s.claim(bookmarkID)
	if !claimed {
		return nil
	}

	pending, sentPending, err := s.notify(ctx, bookmarkID, prevPending)
	if err != nil {
		s.release(bookmarkID, prevPending || sentPending, prevCompletedAt)
		return err
	}
	s.mu.Lock()
	s.claimed[bookmarkID] = claim{completedAt: time.Now(), pending: pending}
	s.mu.Unlock()
	return nil
}

func (s *Service) notify(ctx context.Context, bookmarkID string, alreadySentPending bool) (pending, sentPending bool, err error) {
	bookmark, err := s.awaitSummary(ctx, bookmarkID, s.summaryTimeout)
	if err != nil {
		return false, alreadySentPending, fmt.Errorf("await summary of bookmark %s: %w", bookmarkID, err)
	}

	if bookmark.SummaryStatus != domain.SummaryPending {
		if err := s.send(ctx, bookmarkID, bookmark); err != nil {
			return false, alreadySentPending, err
		}
		return false, alreadySentPending, nil
	}

	if !alreadySentPending {
		if err := s.send(ctx, bookmarkID, bookmark); err != nil {
			return false, false, err
		}
		alreadySentPending = true
	}
	if s.followUpTimeout <= 0 {
		return true, alreadySentPending, nil
	}

	updated, err := s.pollSummary(ctx, bookmarkID, bookmark, s.followUpTimeout)
	if err != nil {
		return true, alreadySentPending, fmt.Errorf("await follow-up summary of bookmark %s: %w", bookmarkID, err)
	}
	if updated.SummaryStatus == domain.SummaryPending {
		return true, alreadySentPending, nil
	}
	if err := s.send(ctx, bookmarkID, updated); err != nil {
		return true, alreadySentPending, err
	}
	return false, alreadySentPending, nil
}

func (s *Service) send(ctx context.Context, bookmarkID string, bookmark domain.Bookmark) error {
	msg := domain.Message{
		BookmarkID:     bookmark.ID,
		Title:          bookmark.Title,
		URL:            bookmark.URL,
		Summary:        bookmark.Summary,
		SummaryPending: bookmark.SummaryStatus == domain.SummaryPending,
	}
	if err := s.notifier.Send(ctx, msg); err != nil {
		return fmt.Errorf("send notification of bookmark %s: %w", bookmarkID, err)
	}
	return nil
}

func (s *Service) awaitSummary(ctx context.Context, bookmarkID string, timeout time.Duration) (domain.Bookmark, error) {
	bookmark, err := s.source.GetBookmark(ctx, bookmarkID)
	if err != nil || bookmark.SummaryStatus != domain.SummaryPending {
		return bookmark, err
	}
	return s.pollSummary(ctx, bookmarkID, bookmark, timeout)
}

func (s *Service) pollSummary(ctx context.Context, bookmarkID string, bookmark domain.Bookmark, timeout time.Duration) (domain.Bookmark, error) {
	pollCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(s.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
		case <-pollCtx.Done():
			return bookmark, ctx.Err()
		}

		next, err := s.source.GetBookmark(pollCtx, bookmarkID)
		if err != nil {
			if ctx.Err() != nil {
				return bookmark, ctx.Err()
			}
			if pollCtx.Err() != nil {
				return bookmark, nil
			}
			if errors.Is(err, domain.ErrBookmarkNotFound) {
				return domain.Bookmark{}, err
			}
			slog.WarnContext(pollCtx, "poll bookmark summary failed, retrying", "bookmark_id", bookmarkID, "error", err)
			continue
		}
		bookmark = next
		if bookmark.SummaryStatus != domain.SummaryPending {
			return bookmark, nil
		}
	}
}

func (s *Service) claim(bookmarkID string) (claimed, wasPending bool, prevCompletedAt time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	if !now.Before(s.nextCleanup) {
		for id, entry := range s.claimed {
			if !entry.inFlight && now.Sub(entry.completedAt) >= s.dedupeWindow {
				delete(s.claimed, id)
			}
		}
		s.nextCleanup = now.Add(min(time.Minute, s.dedupeWindow))
	}
	entry, ok := s.claimed[bookmarkID]
	if ok {
		if entry.inFlight {
			return false, false, time.Time{}
		}
		active := now.Sub(entry.completedAt) < s.dedupeWindow
		if active && !entry.pending {
			return false, false, time.Time{}
		}
		wasPending = active && entry.pending
		prevCompletedAt = entry.completedAt
	}
	s.claimed[bookmarkID] = claim{inFlight: true, pending: wasPending, completedAt: prevCompletedAt}
	return true, wasPending, prevCompletedAt
}

func (s *Service) release(bookmarkID string, keepPending bool, prevCompletedAt time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if keepPending {
		if prevCompletedAt.IsZero() {
			prevCompletedAt = time.Now()
		}
		s.claimed[bookmarkID] = claim{completedAt: prevCompletedAt, pending: true}
		return
	}
	delete(s.claimed, bookmarkID)
}
