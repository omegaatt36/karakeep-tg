package karakeep

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/omegaatt36/karakeep-tg/domain"
)

const requestTimeout = 10 * time.Second

type Client struct {
	baseURL    *url.URL
	apiKey     string
	httpClient *http.Client
}

func New(baseURL, apiKey string) (*Client, error) {
	if apiKey == "" {
		return nil, errors.New("api key is required")
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return nil, errors.New("base url must be an HTTP(S) URL without credentials, query, or fragment")
	}

	return &Client{
		baseURL:    parsed,
		apiKey:     apiKey,
		httpClient: &http.Client{Timeout: requestTimeout},
	}, nil
}

type bookmarkResponse struct {
	ID                  string  `json:"id"`
	Title               *string `json:"title"`
	Summary             *string `json:"summary"`
	SummarizationStatus *string `json:"summarizationStatus"`
	Content             struct {
		URL   string  `json:"url"`
		Title *string `json:"title"`
	} `json:"content"`
}

func (c *Client) GetBookmark(ctx context.Context, id string) (domain.Bookmark, error) {
	if !domain.ValidBookmarkID(id) {
		return domain.Bookmark{}, errors.New("invalid bookmark id")
	}
	endpoint := c.baseURL.JoinPath("api/v1/bookmarks", id)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return domain.Bookmark{}, fmt.Errorf("create bookmark request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return domain.Bookmark{}, fmt.Errorf("get bookmark %s: %w", id, err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return domain.Bookmark{}, fmt.Errorf("get bookmark %s: %w", id, domain.ErrBookmarkNotFound)
	case resp.StatusCode != http.StatusOK:
		return domain.Bookmark{}, fmt.Errorf("get bookmark %s: unexpected status %s", id, resp.Status)
	}

	var body bookmarkResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return domain.Bookmark{}, fmt.Errorf("decode bookmark %s: %w", id, err)
	}
	return toBookmark(body)
}

func toBookmark(body bookmarkResponse) (domain.Bookmark, error) {
	status, err := toSummaryStatus(body.SummarizationStatus)
	if err != nil {
		return domain.Bookmark{}, fmt.Errorf("map bookmark %s: %w", body.ID, err)
	}

	title := deref(body.Title)
	if title == "" {
		title = deref(body.Content.Title)
	}
	return domain.Bookmark{
		ID:            body.ID,
		Title:         title,
		URL:           body.Content.URL,
		Summary:       deref(body.Summary),
		SummaryStatus: status,
	}, nil
}

func toSummaryStatus(raw *string) (domain.SummaryStatus, error) {
	switch status := domain.SummaryStatus(deref(raw)); status {
	case "":
		return domain.SummaryPending, nil
	case domain.SummaryPending, domain.SummarySuccess, domain.SummaryFailure:
		return status, nil
	default:
		return "", fmt.Errorf("unknown summarization status %q", status)
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
