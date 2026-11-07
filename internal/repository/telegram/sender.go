package telegram

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/omegaatt36/karakeep-tg/domain"
)

type Sender struct {
	bot     *bot.Bot
	chatIDs []string
}

func New(token string, chatIDs []string, opts ...bot.Option) (*Sender, error) {
	if len(chatIDs) == 0 {
		return nil, errors.New("at least one chat id is required")
	}
	b, err := bot.New(token, opts...)
	if err != nil {
		return nil, fmt.Errorf("create telegram bot: %w", err)
	}
	return &Sender{bot: b, chatIDs: chatIDs}, nil
}

func (s *Sender) Send(ctx context.Context, msg domain.Message) error {
	text := Render(msg)

	var errs []error
	for _, chatID := range s.chatIDs {
		_, err := s.bot.SendMessage(ctx, &bot.SendMessageParams{
			ChatID:    parseChatID(chatID),
			Text:      text,
			ParseMode: models.ParseModeHTML,
		})
		if err != nil {
			slog.ErrorContext(ctx, "send telegram message", "chat_id", chatID, "bookmark_id", msg.BookmarkID, "error", err)
			errs = append(errs, fmt.Errorf("chat %s: %w", chatID, err))
		}
	}
	return errors.Join(errs...)
}

func parseChatID(raw string) any {
	if id, err := strconv.ParseInt(raw, 10, 64); err == nil {
		return id
	}
	return raw
}
