package telegram_test

import (
	"encoding/xml"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/omegaatt36/karakeep-tg/domain"
	"github.com/omegaatt36/karakeep-tg/internal/repository/telegram"
)

const messageLimit = 4096

func utf16Len(s string) int { return len(utf16.Encode([]rune(s))) }

func link(href, text string) string { return `<a href="` + href + `">` + text + `</a>` }

func TestRender(t *testing.T) {
	tests := []struct {
		name string
		msg  domain.Message
		want string
	}{
		{
			name: "title url and summary",
			msg:  domain.Message{Title: "Title", URL: "https://e.com/a", Summary: "summary"},
			want: "<b>Title</b>\n" + link("https://e.com/a", "https://e.com/a") + "\n\nsummary",
		},
		{
			name: "markup characters are escaped",
			msg:  domain.Message{Title: "<b>A&B</b>", URL: "https://e.com/?a=1&b=2", Summary: "1 < 2 && 3 > 2"},
			want: "<b>&lt;b&gt;A&amp;B&lt;/b&gt;</b>\n" + link("https://e.com/?a=1&amp;b=2", "https://e.com/?a=1&amp;b=2") + "\n\n1 &lt; 2 &amp;&amp; 3 &gt; 2",
		},
		{
			name: "quote in url cannot break out of the attribute",
			msg:  domain.Message{URL: `https://e.com/"x`},
			want: link("https://e.com/&quot;x", "https://e.com/&quot;x"),
		},
		{
			name: "empty title falls back to url",
			msg:  domain.Message{URL: "https://e.com/a", Summary: "summary"},
			want: link("https://e.com/a", "https://e.com/a") + "\n\nsummary",
		},
		{
			name: "pending marker without summary",
			msg:  domain.Message{Title: "Title", URL: "https://e.com/a", SummaryPending: true},
			want: "<b>Title</b>\n" + link("https://e.com/a", "https://e.com/a") + "\n\n⏳ 摘要尚未就緒",
		},
		{
			name: "no summary and not pending",
			msg:  domain.Message{Title: "Title", URL: "https://e.com/a"},
			want: "<b>Title</b>\n" + link("https://e.com/a", "https://e.com/a"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := telegram.Render(tt.msg); !slices.Equal(got, []string{tt.want}) {
				t.Fatalf("Render() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRenderDecodesOnlyTheDisplayedURL(t *testing.T) {
	tests := []struct {
		name        string
		url         string
		wantDisplay string
	}{
		{"percent-encoded cjk", "https://e.com/mariadb-%e8%a6%81%e8%a8%98-jemalloc/", "https://e.com/mariadb-要記-jemalloc/"},
		{"plus is kept", "https://e.com/a+b", "https://e.com/a+b"},
		{"ampersand is escaped after decoding", "https://e.com/%e8%a6%81?a=1&b=2", "https://e.com/要?a=1&amp;b=2"},
		{"encoded markup stays inert", "https://e.com/%3Cb%3E", "https://e.com/&lt;b&gt;"},
		{"encoded newline keeps the raw url", "https://e.com/a%0Ab", "https://e.com/a%0Ab"},
		{"encoded space keeps the raw url", "https://e.com/a%20b", "https://e.com/a%20b"},
		{"invalid utf-8 keeps the raw url", "https://e.com/%ff", "https://e.com/%ff"},
		{"malformed escape keeps the raw url", "https://e.com/%zz", "https://e.com/%zz"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := telegram.Render(domain.Message{URL: tt.url})

			wantHref := strings.NewReplacer("&", "&amp;").Replace(tt.url)
			if want := link(wantHref, tt.wantDisplay); !slices.Equal(got, []string{want}) {
				t.Fatalf("Render() = %q, want %q", got, want)
			}
		})
	}
}

func plainText(t *testing.T, parts []string) string {
	t.Helper()
	var text strings.Builder
	for i, part := range parts {
		var parsed strings.Builder
		decoder := xml.NewDecoder(strings.NewReader("<root>" + part + "</root>"))
		for {
			token, err := decoder.Token()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatalf("part %d has invalid HTML: %v", i, err)
			}
			if data, ok := token.(xml.CharData); ok {
				parsed.Write(data)
			}
		}
		if n := utf16Len(parsed.String()); n > messageLimit {
			t.Fatalf("part %d length = %d, want <= %d", i, n, messageLimit)
		}
		text.WriteString(parsed.String())
	}
	return text.String()
}

func TestRenderSplitsWithoutLosingContent(t *testing.T) {
	tests := []struct{ name, title, url, summary string }{
		{"long summary", "Title", "https://e.com/a", strings.Repeat("a", 10000)},
		{"long title", strings.Repeat("標題", 5000), "https://e.com/a", "summary"},
		{"long url", "Title", "https://e.com/" + strings.Repeat("x", 10000), "summary"},
		{"unicode and escapes", strings.Repeat("<😀&字>\"", 900), "https://e.com/%e8%a6%81?x=1&y=2", strings.Repeat("&<😀字>\"", 2000)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := domain.Message{Title: tt.title, URL: tt.url, Summary: tt.summary}
			got := telegram.Render(msg)
			if len(got) < 2 {
				t.Fatalf("parts = %d, want multiple", len(got))
			}
			display := strings.ReplaceAll(tt.url, "%e8%a6%81", "要")
			if want := tt.title + "\n" + display + "\n\n" + tt.summary; plainText(t, got) != want {
				t.Fatal("split changed or lost content")
			}
		})
	}
}

func TestRenderKeepsSummaryThatFitsExactly(t *testing.T) {
	title, url := "T", "https://e.com/a"
	summary := strings.Repeat("a", messageLimit-utf16Len(title+"\n"+url+"\n\n"))
	got := telegram.Render(domain.Message{Title: title, URL: url, Summary: summary})
	want := "<b>T</b>\n" + link(url, url) + "\n\n" + summary
	if !slices.Equal(got, []string{want}) {
		t.Fatal("summary that fits was split or changed")
	}
}

func TestRenderReplacesInvalidUTF8WithoutPanicking(t *testing.T) {
	got := telegram.Render(domain.Message{Title: "title\xff", URL: "https://e.com/\xff", Summary: "summary\xff"})
	if want := "title�\nhttps://e.com/�\n\nsummary�"; plainText(t, got) != want {
		t.Fatalf("got %q, want %q", plainText(t, got), want)
	}
}

func TestRenderDropsWhitespaceOnlyParts(t *testing.T) {
	url := "https://e.com/a"
	summary := strings.Repeat("a", messageLimit-utf16Len(url+"\n\n")) + "\n"
	parts := telegram.Render(domain.Message{URL: url, Summary: summary})
	if len(parts) != 1 {
		t.Fatalf("trailing newline emitted %d parts, want 1", len(parts))
	}
	if got, want := plainText(t, parts), url+"\n\n"+strings.TrimSuffix(summary, "\n"); got != want {
		t.Fatal("visible part changed")
	}
	preserved := domain.Message{Title: "  Title \t", URL: url, Summary: "\n  summary  \t"}
	if got, want := plainText(t, telegram.Render(preserved)), preserved.Title+"\n"+url+"\n\n"+preserved.Summary; got != want {
		t.Fatal("whitespace in visible parts changed")
	}
	for _, msg := range []domain.Message{
		{Title: strings.Repeat(" ", 5000), URL: "https://e.com/a"},
		{URL: strings.Repeat(" ", 5000), Summary: "visible"},
		{Title: strings.Repeat(" ", 5000), URL: strings.Repeat(" ", 5000), Summary: strings.Repeat(" ", 5000)},
	} {
		for _, part := range telegram.Render(msg) {
			if text := plainText(t, []string{part}); strings.TrimSpace(text) == "" {
				t.Fatalf("whitespace-only rendered part: %q", part)
			}
		}
	}
}
