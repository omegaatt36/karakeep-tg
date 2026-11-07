package telegram

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/omegaatt36/karakeep-tg/domain"
)

const (
	deliveryTTL     = time.Hour
	cleanupInterval = time.Minute
)

type Sender struct {
	bot         *bot.Bot
	chatIDs     []string
	mu          sync.Mutex
	deliveries  map[string]*bookmarkDelivery
	nextCleanup time.Time
}

// A bookmark gate serializes replays without blocking unrelated bookmarks.
// refs includes queued callers, preventing cleanup from evicting an active gate.
type bookmarkDelivery struct {
	gate  chan struct{}
	refs  int
	chats map[string]*chatDelivery
}

type chatDelivery struct {
	// Keep the original snapshot on partial retry: a changed render cannot safely
	// reuse an offset into the old content. Completed chats remain deduplicated.
	parts       []string
	next        int
	lastSuccess time.Time
}

func New(token string, chatIDs []string, opts ...bot.Option) (*Sender, error) {
	if len(chatIDs) == 0 {
		return nil, errors.New("at least one chat id is required")
	}
	b, err := bot.New(token, opts...)
	if err != nil {
		return nil, fmt.Errorf("create telegram bot: %w", err)
	}
	return &Sender{bot: b, chatIDs: append([]string(nil), chatIDs...), deliveries: make(map[string]*bookmarkDelivery)}, nil
}

func (s *Sender) Send(ctx context.Context, msg domain.Message) error {
	delivery, release, err := s.acquire(ctx, msg.BookmarkID)
	if err != nil {
		return err
	}
	defer release()
	parts := Render(msg)
	if len(parts) == 0 {
		return errors.New("telegram message has no deliverable text")
	}

	var errs []error
	for _, chatID := range s.chatIDs {
		progress := delivery.chats[chatID]
		if progress == nil || time.Since(progress.lastSuccess) >= deliveryTTL {
			progress = &chatDelivery{parts: parts}
			delivery.chats[chatID] = progress
		}
		if err := s.sendChat(ctx, chatID, progress); err != nil {
			slog.ErrorContext(ctx, "send telegram message", "chat_id", chatID, "bookmark_id", msg.BookmarkID, "part", progress.next+1, "error", err)
			errs = append(errs, fmt.Errorf("chat %s part %d: %w", chatID, progress.next+1, err))
		}
		if progress.lastSuccess.IsZero() {
			delete(delivery.chats, chatID)
		}
	}
	return errors.Join(errs...)
}

func (s *Sender) sendChat(ctx context.Context, chatID string, progress *chatDelivery) error {
	retriedRateLimit := false
	for progress.next < len(progress.parts) {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, err := s.bot.SendMessage(ctx, &bot.SendMessageParams{
			ChatID:    parseChatID(chatID),
			Text:      progress.parts[progress.next],
			ParseMode: models.ParseModeHTML,
		})
		if err != nil {
			var rateLimit *bot.TooManyRequestsError
			if !retriedRateLimit && errors.As(err, &rateLimit) && rateLimit.RetryAfter > 0 {
				retriedRateLimit = true
				timer := time.NewTimer(time.Duration(rateLimit.RetryAfter) * time.Second)
				select {
				case <-timer.C:
					continue
				case <-ctx.Done():
					timer.Stop()
					return ctx.Err()
				}
			}
			return err
		}
		progress.next++
		// Only confirmed successful parts extend the TTL. A failed attempt never
		// extends retention; partial deliveries expire one hour after their last
		// successful part, completed deliveries after their final successful part.
		progress.lastSuccess = time.Now()
	}
	return nil
}

func (s *Sender) acquire(ctx context.Context, bookmarkID string) (*bookmarkDelivery, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	s.mu.Lock()
	s.cleanup(time.Now())
	delivery := s.deliveries[bookmarkID]
	if delivery == nil {
		delivery = &bookmarkDelivery{gate: make(chan struct{}, 1), chats: make(map[string]*chatDelivery)}
		s.deliveries[bookmarkID] = delivery
	}
	delivery.refs++
	s.mu.Unlock()

	releaseRef := func() {
		s.mu.Lock()
		delivery.refs--
		if delivery.refs == 0 && len(delivery.chats) == 0 {
			delete(s.deliveries, bookmarkID)
		}
		s.mu.Unlock()
	}
	select {
	case delivery.gate <- struct{}{}:
		return delivery, func() { <-delivery.gate; releaseRef() }, nil
	case <-ctx.Done():
		releaseRef()
		return nil, nil, ctx.Err()
	}
}

// cleanup runs under mu at most once per minute, with no background goroutine.
// Every chat is also checked on access, so cleanup cadence does not extend TTL.
func (s *Sender) cleanup(now time.Time) {
	if now.Before(s.nextCleanup) {
		return
	}
	s.nextCleanup = now.Add(cleanupInterval)
	for bookmarkID, delivery := range s.deliveries {
		if delivery.refs != 0 {
			continue
		}
		for chatID, progress := range delivery.chats {
			if now.Sub(progress.lastSuccess) >= deliveryTTL {
				delete(delivery.chats, chatID)
			}
		}
		if len(delivery.chats) == 0 {
			delete(s.deliveries, bookmarkID)
		}
	}
}

func parseChatID(raw string) any {
	if id, err := strconv.ParseInt(raw, 10, 64); err == nil {
		return id
	}
	return raw
}
