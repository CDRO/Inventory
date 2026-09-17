package vision

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestOffersChecksAnyModelAgainstTheSameList — the optional image model is
// looked up in the list the effective model is, from the same cache, and an
// empty or unreachable answer never offers it.
func TestOffersChecksAnyModelAgainstTheSameList(t *testing.T) {
	t.Parallel()

	lister := &stubLister{models: []string{"models/gemini-3.6-flash", "models/gemini-2.5-flash"}}
	c := NewChecker(nil, lister, "gemini-3.6-flash")

	assert.True(t, c.Offers(context.Background(), "gemini-2.5-flash"))
	assert.True(t, c.Offers(context.Background(), "models/gemini-2.5-flash"), "prefixed or not, the same model")
	assert.False(t, c.Offers(context.Background(), "gemini-1.0-pro-vision"), "deprecated away")
	assert.False(t, c.Offers(context.Background(), "  "), "unset is never offered")
	assert.Equal(t, StatusOK, c.Status(context.Background()))
	assert.Equal(t, 1, lister.calls, "one model list serves both models")

	unreachable := NewChecker(nil, &stubLister{err: errors.New("dial tcp: timeout")}, "gemini-3.6-flash")
	assert.False(t, unreachable.Offers(context.Background(), "gemini-2.5-flash"))
}
