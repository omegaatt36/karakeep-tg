package domain

import "errors"

type SummaryStatus string

const (
	SummaryPending SummaryStatus = "pending"
	SummarySuccess SummaryStatus = "success"
	SummaryFailure SummaryStatus = "failure"
)

var ErrBookmarkNotFound = errors.New("bookmark not found")

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
