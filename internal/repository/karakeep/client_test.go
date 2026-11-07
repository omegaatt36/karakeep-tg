package karakeep_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/omegaatt36/karakeep-tg/domain"
	"github.com/omegaatt36/karakeep-tg/internal/repository/karakeep"
)

func newClient(t *testing.T, handler http.HandlerFunc) *karakeep.Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := karakeep.New(server.URL, "key")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return client
}

func respond(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

func TestGetBookmarkSendsAuthenticatedRequest(t *testing.T) {
	var gotPath, gotAuth, gotMethod string
	client := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth, gotMethod = r.URL.Path, r.Header.Get("Authorization"), r.Method
		respond(http.StatusOK, `{"id":"b1","summarizationStatus":"pending","content":{"type":"link","url":"https://e.com"}}`)(w, r)
	})

	if _, err := client.GetBookmark(t.Context(), "b1"); err != nil {
		t.Fatalf("GetBookmark: %v", err)
	}

	if gotMethod != http.MethodGet || gotPath != "/api/v1/bookmarks/b1" || gotAuth != "Bearer key" {
		t.Fatalf("request = %s %s auth %q", gotMethod, gotPath, gotAuth)
	}
}

func TestGetBookmarkMapsFields(t *testing.T) {
	tests := []struct {
		name string
		body string
		want domain.Bookmark
	}{
		{
			name: "success with summary",
			body: `{"id":"b1","title":"Own title","summary":"sum","summarizationStatus":"success","content":{"type":"link","url":"https://e.com/a","title":"Page title"}}`,
			want: domain.Bookmark{ID: "b1", Title: "Own title", URL: "https://e.com/a", Summary: "sum", SummaryStatus: domain.SummarySuccess},
		},
		{
			name: "falls back to page title",
			body: `{"id":"b1","title":null,"summary":null,"summarizationStatus":"failure","content":{"type":"link","url":"https://e.com/a","title":"Page title"}}`,
			want: domain.Bookmark{ID: "b1", Title: "Page title", URL: "https://e.com/a", SummaryStatus: domain.SummaryFailure},
		},
		{
			name: "pending",
			body: `{"id":"b1","summarizationStatus":"pending","content":{"type":"link","url":"https://e.com/a"}}`,
			want: domain.Bookmark{ID: "b1", URL: "https://e.com/a", SummaryStatus: domain.SummaryPending},
		},
		{
			name: "null status is pending",
			body: `{"id":"b1","summarizationStatus":null,"content":{"type":"link","url":"https://e.com/a"}}`,
			want: domain.Bookmark{ID: "b1", URL: "https://e.com/a", SummaryStatus: domain.SummaryPending},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newClient(t, respond(http.StatusOK, tt.body))

			got, err := client.GetBookmark(t.Context(), "b1")
			if err != nil {
				t.Fatalf("GetBookmark: %v", err)
			}
			if got != tt.want {
				t.Fatalf("bookmark = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestGetBookmarkErrors(t *testing.T) {
	tests := []struct {
		name         string
		handler      http.HandlerFunc
		wantNotFound bool
	}{
		{"not found", respond(http.StatusNotFound, `{"message":"not found"}`), true},
		{"unauthorized", respond(http.StatusUnauthorized, `{"message":"no"}`), false},
		{"malformed json", respond(http.StatusOK, `{`), false},
		{"unknown status", respond(http.StatusOK, `{"id":"b1","summarizationStatus":"weird","content":{"type":"link","url":"u"}}`), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newClient(t, tt.handler)

			_, err := client.GetBookmark(t.Context(), "b1")
			if err == nil {
				t.Fatal("GetBookmark succeeded, want error")
			}
			if got := errors.Is(err, domain.ErrBookmarkNotFound); got != tt.wantNotFound {
				t.Fatalf("errors.Is(ErrBookmarkNotFound) = %v, want %v (err: %v)", got, tt.wantNotFound, err)
			}
		})
	}
}

func TestNewRejectsMissingSettings(t *testing.T) {
	for _, args := range [][2]string{{"", "key"}, {"http://x", ""}, {"://bad", "key"}} {
		if _, err := karakeep.New(args[0], args[1]); err == nil {
			t.Errorf("New(%q, %q) succeeded, want error", args[0], args[1])
		}
	}
}

func TestLiveBookmarkContract(t *testing.T) {
	baseURL, apiKey, id := os.Getenv("KARAKEEP_URL"), os.Getenv("KARAKEEP_API_KEY"), os.Getenv("LIVE_BOOKMARK_ID")
	if os.Getenv("LIVE_KARAKEEP") != "1" || baseURL == "" || apiKey == "" || id == "" {
		t.Skip("set LIVE_KARAKEEP=1, KARAKEEP_URL, KARAKEEP_API_KEY and LIVE_BOOKMARK_ID to run")
	}
	client, err := karakeep.New(baseURL, apiKey)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	got, err := client.GetBookmark(t.Context(), id)
	if err != nil {
		t.Fatalf("GetBookmark: %v", err)
	}

	if got.ID != id || got.URL == "" {
		t.Fatalf("bookmark = %+v, want id %q and a url", got, id)
	}
	switch got.SummaryStatus {
	case domain.SummaryPending, domain.SummarySuccess, domain.SummaryFailure:
	default:
		t.Fatalf("summary status = %q", got.SummaryStatus)
	}
	if got.SummaryStatus == domain.SummarySuccess && got.Summary == "" {
		t.Log("summary is empty although status is success (skipped job)")
	}
}
