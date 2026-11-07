package telegram

import (
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/omegaatt36/karakeep-tg/domain"
)

const (
	messageLimit  = 4096
	ellipsis      = "…"
	pendingMarker = "⏳ 摘要尚未就緒"
)

var escaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")

func Render(msg domain.Message) string {
	head := `<a href="` + escaper.Replace(msg.URL) + `">` + escaper.Replace(displayURL(msg.URL)) + "</a>"
	if msg.Title != "" {
		head = "<b>" + escaper.Replace(msg.Title) + "</b>\n" + head
	}

	switch {
	case msg.Summary != "":
		return head + "\n\n" + truncate(msg.Summary, messageLimit-utf16Len(head)-len("\n\n"))
	case msg.SummaryPending:
		return head + "\n\n" + pendingMarker
	default:
		return head
	}
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

func truncate(summary string, budget int) string {
	escaped := escaper.Replace(summary)
	if utf16Len(escaped) <= budget {
		return escaped
	}

	budget -= utf16Len(ellipsis)
	var b strings.Builder
	used := 0
	for _, r := range summary {
		piece := escaper.Replace(string(r))
		used += utf16Len(piece)
		if used > budget {
			break
		}
		b.WriteString(piece)
	}
	return b.String() + ellipsis
}

func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		n++
		if r >= 0x10000 {
			n++
		}
	}
	return n
}
