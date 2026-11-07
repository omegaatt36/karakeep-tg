package domain_test

import (
	"testing"

	"github.com/omegaatt36/karakeep-tg/domain"
)

func TestValidBookmarkID(t *testing.T) {
	for _, id := range []string{"b1", "abc_DEF-123", "550e8400-e29b-41d4-a716-446655440000"} {
		if !domain.ValidBookmarkID(id) {
			t.Errorf("rejected bookmark ID %q", id)
		}
	}
	for _, id := range []string{"", ".", "..", "../../users", "a/b", `a\b`, "%2e%2e", "a?x=1", "a#fragment", " a", "字", "a\n"} {
		if domain.ValidBookmarkID(id) {
			t.Errorf("accepted unsafe bookmark ID %q", id)
		}
	}
}
