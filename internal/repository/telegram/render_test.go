package telegram_test

import (
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/omegaatt36/karakeep-tg/domain"
	"github.com/omegaatt36/karakeep-tg/internal/repository/telegram"
)

const messageLimit = 4096

func utf16Len(s string) int { return len(utf16.Encode([]rune(s))) }

func TestRender(t *testing.T) {
	tests := []struct {
		name string
		msg  domain.Message
		want string
	}{
		{
			name: "title url and summary",
			msg:  domain.Message{Title: "Title", URL: "https://e.com/a", Summary: "summary"},
			want: "<b>Title</b>\nhttps://e.com/a\n\nsummary",
		},
		{
			name: "markup characters are escaped",
			msg:  domain.Message{Title: "<b>A&B</b>", URL: "https://e.com/?a=1&b=2", Summary: "1 < 2 && 3 > 2"},
			want: "<b>&lt;b&gt;A&amp;B&lt;/b&gt;</b>\nhttps://e.com/?a=1&amp;b=2\n\n1 &lt; 2 &amp;&amp; 3 &gt; 2",
		},
		{
			name: "empty title falls back to url",
			msg:  domain.Message{URL: "https://e.com/a", Summary: "summary"},
			want: "https://e.com/a\n\nsummary",
		},
		{
			name: "pending marker without summary",
			msg:  domain.Message{Title: "Title", URL: "https://e.com/a", SummaryPending: true},
			want: "<b>Title</b>\nhttps://e.com/a\n\n⏳ 摘要尚未就緒",
		},
		{
			name: "no summary and not pending",
			msg:  domain.Message{Title: "Title", URL: "https://e.com/a"},
			want: "<b>Title</b>\nhttps://e.com/a",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := telegram.Render(tt.msg); got != tt.want {
				t.Fatalf("Render() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRenderTruncatesOnlyTheSummary(t *testing.T) {
	tests := []struct {
		name    string
		summary string
	}{
		{"ascii", strings.Repeat("a", 10000)},
		{"cjk", strings.Repeat("字", 5000)},
		{"surrogate pairs", strings.Repeat("😀", 3000)},
		{"escape heavy", strings.Repeat("<", 5000)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := domain.Message{Title: "Title", URL: "https://e.com/a", Summary: tt.summary}
			head := "<b>Title</b>\nhttps://e.com/a\n\n"

			got := telegram.Render(msg)

			if n := utf16Len(got); n > messageLimit {
				t.Fatalf("length = %d UTF-16 units, want <= %d", n, messageLimit)
			}
			if !strings.HasPrefix(got, head) {
				t.Fatalf("title and url were altered: %.60q", got)
			}
			if !strings.HasSuffix(got, "…") {
				t.Fatalf("truncated message does not end with an ellipsis")
			}
			if utf16Len(got) < messageLimit-8 {
				t.Fatalf("length = %d, truncated more than needed", utf16Len(got))
			}
		})
	}
}

func TestRenderKeepsSummaryThatFitsExactly(t *testing.T) {
	head := "<b>T</b>\nhttps://e.com/a\n\n"
	summary := strings.Repeat("a", messageLimit-utf16Len(head))

	got := telegram.Render(domain.Message{Title: "T", URL: "https://e.com/a", Summary: summary})

	if got != head+summary {
		t.Fatalf("a summary that fits was truncated (len %d)", utf16Len(got))
	}
}
