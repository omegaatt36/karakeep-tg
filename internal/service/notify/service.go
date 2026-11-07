package notify

import (
	"context"
	"errors"
	"fmt"
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
	Source         BookmarkSource
	Notifier       Notifier
	PollInterval   time.Duration
	SummaryTimeout time.Duration
	DedupeWindow   time.Duration
}

type Service struct {
	source         BookmarkSource
	notifier       Notifier
	pollInterval   time.Duration
	summaryTimeout time.Duration
	dedupeWindow   time.Duration

	mu          sync.Mutex
	claimed     map[string]claim
	nextCleanup time.Time
}

type claim struct {
	completedAt time.Time
	inFlight    bool
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
	case opts.DedupeWindow <= 0:
		return nil, errors.New("dedupe window must be positive")
	}

	return &Service{
		source:         opts.Source,
		notifier:       opts.Notifier,
		pollInterval:   opts.PollInterval,
		summaryTimeout: opts.SummaryTimeout,
		dedupeWindow:   opts.DedupeWindow,
		claimed:        make(map[string]claim),
	}, nil
}

func (s *Service) Handle(ctx context.Context, bookmarkID string) error {
	if !s.claim(bookmarkID) {
		return nil
	}

	if err := s.notify(ctx, bookmarkID); err != nil {
		s.release(bookmarkID)
		return err
	}
	s.mu.Lock()
	s.claimed[bookmarkID] = claim{completedAt: time.Now()}
	s.mu.Unlock()
	return nil
}

func (s *Service) notify(ctx context.Context, bookmarkID string) error {
	bookmark, err := s.awaitSummary(ctx, bookmarkID)
	if err != nil {
		return fmt.Errorf("await summary of bookmark %s: %w", bookmarkID, err)
	}

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

func (s *Service) awaitSummary(ctx context.Context, bookmarkID string) (domain.Bookmark, error) {
	bookmark, err := s.source.GetBookmark(ctx, bookmarkID)
	if err != nil || bookmark.SummaryStatus != domain.SummaryPending {
		return bookmark, err
	}

	pollCtx, cancel := context.WithTimeout(ctx, s.summaryTimeout)
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
			if pollCtx.Err() != nil {
				return bookmark, ctx.Err()
			}
			return domain.Bookmark{}, err
		}
		bookmark = next
		if bookmark.SummaryStatus != domain.SummaryPending {
			return bookmark, nil
		}
	}
}

func (s *Service) claim(bookmarkID string) bool {
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
	if entry, ok := s.claimed[bookmarkID]; ok && (entry.inFlight || now.Sub(entry.completedAt) < s.dedupeWindow) {
		return false
	}
	s.claimed[bookmarkID] = claim{inFlight: true}
	return true
}

func (s *Service) release(bookmarkID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.claimed, bookmarkID)
}
