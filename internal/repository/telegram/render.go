package telegram

import (
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/go-telegram/bot/models"

	"github.com/omegaatt36/karakeep-tg/domain"
)

const (
	messageLimit        = 4096
	pendingMarker       = "⏳ 摘要尚未就緒"
	openOriginalLabel   = "🔗 開啟原文"
	openKarakeepLabel   = "🗂 Karakeep"
	karakeepPreviewPath = "dashboard/preview"
)

var escaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")

// Render splits a bookmark into independently valid HTML messages. Telegram's
// limit applies to parsed text, so tag attributes and escaped entities do not
// consume the text budget. Visible title, URL, and summary content is preserved; whitespace-only
// chunks are omitted because Telegram rejects empty parsed messages.
func Render(msg domain.Message) []string {
	var builder messageBuilder
	rawURL := strings.ToValidUTF8(msg.URL, "�")
	if msg.Title != "" {
		builder.append(msg.Title, "<b>", "</b>")
		builder.append("\n", "", "")
	}
	builder.append(displayURL(rawURL), `<a href="`+escaper.Replace(rawURL)+`">`, "</a>")
	switch {
	case msg.Summary != "":
		builder.append("\n\n"+msg.Summary, "", "")
	case msg.SummaryPending:
		builder.append("\n\n"+pendingMarker, "", "")
	}
	return builder.finish()
}

type messageBuilder struct {
	parts   []string
	current strings.Builder
	used    int
	visible bool
}

func (b *messageBuilder) append(text, open, close string) {
	text = strings.ToValidUTF8(text, "�")
	if text == "" {
		b.current.WriteString(open)
		b.current.WriteString(close)
	}
	for len(text) > 0 {
		end, units := 0, 0
		for index := 0; index < len(text); {
			r, size := utf8.DecodeRuneInString(text[index:])
			width := 1
			if r >= 0x10000 {
				width = 2
			}
			if b.used+units+width > messageLimit {
				break
			}
			index += size
			end = index
			units += width
		}
		if end > 0 {
			b.current.WriteString(open)
			b.current.WriteString(escaper.Replace(text[:end]))
			b.current.WriteString(close)
			if strings.ContainsFunc(text[:end], func(r rune) bool { return !unicode.IsSpace(r) }) {
				b.visible = true
			}
			b.used += units
			text = text[end:]
		}
		if len(text) > 0 {
			b.flush()
		}
	}
}

func (b *messageBuilder) flush() {
	if b.visible {
		b.parts = append(b.parts, b.current.String())
	}
	b.current.Reset()
	b.used = 0
	b.visible = false
}

func (b *messageBuilder) finish() []string {
	if b.current.Len() > 0 {
		b.flush()
	}
	return b.parts
}

func displayURL(raw string) string {
	decoded, err := url.PathUnescape(raw)
	if err != nil || !utf8.ValidString(decoded) || strings.ContainsFunc(decoded, isUnsafeDisplayRune) {
		return raw
	}
	return decoded
}

func isUnsafeDisplayRune(r rune) bool {
	return unicode.IsControl(r) || unicode.IsSpace(r)
}

// InlineKeyboard builds action buttons for the original article URL and, when
// configured with a Karakeep base URL and valid bookmark ID, the Karakeep preview page.
func InlineKeyboard(msg domain.Message, karakeepBaseURL *url.URL) *models.InlineKeyboardMarkup {
	var buttons []models.InlineKeyboardButton
	if validButtonURL(msg.URL) {
		buttons = append(buttons, models.InlineKeyboardButton{
			Text: openOriginalLabel,
			URL:  msg.URL,
		})
	}
	if karakeepBaseURL != nil && domain.ValidBookmarkID(msg.BookmarkID) {
		buttons = append(buttons, models.InlineKeyboardButton{
			Text: openKarakeepLabel,
			URL:  karakeepBaseURL.JoinPath(karakeepPreviewPath, msg.BookmarkID).String(),
		})
	}
	if len(buttons) == 0 {
		return nil
	}
	return &models.InlineKeyboardMarkup{
		InlineKeyboard: [][]models.InlineKeyboardButton{buttons},
	}
}

func validButtonURL(raw string) bool {
	if raw == "" || !utf8.ValidString(raw) || strings.ContainsFunc(raw, isUnsafeDisplayRune) {
		return false
	}
	parsed, err := url.Parse(raw)
	return err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != ""
}
