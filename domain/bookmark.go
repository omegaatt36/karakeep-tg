package domain

import "errors"

type SummaryStatus string

const (
	SummaryPending SummaryStatus = "pending"
	SummarySuccess SummaryStatus = "success"
	SummaryFailure SummaryStatus = "failure"
)

var ErrBookmarkNotFound = errors.New("bookmark not found")

// ValidBookmarkID accepts an opaque identifier that cannot change a URL path.
func ValidBookmarkID(id string) bool {
	if id == "" {
		return false
	}
	for _, r := range id {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			continue
		}
		return false
	}
	return true
}

type Bookmark struct {
	ID            string
	Title         string
	URL           string
	Summary       string
	SummaryStatus SummaryStatus
}

type Message struct {
	BookmarkID     string
	Title          string
	URL            string
	Summary        string
	SummaryPending bool
}
