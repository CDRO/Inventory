package matching_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/CDRO/Inventory/internal/matching"
)

func TestParseLine(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		raw      string
		wantText string
		wantQty  int
	}{
		{"a plain line is one unit", "milk", "milk", 1},
		{"trailing x form", "eggs x2", "eggs", 2},
		{"trailing x form without space", "eggs x12", "eggs", 12},
		{"trailing count then x", "eggs 2x", "eggs", 2},
		{"leading count and x", "2x eggs", "eggs", 2},
		{"leading bare count", "3 apples", "apples", 3},
		{"multi-word name keeps its words", "san marzano tomatoes x4", "san marzano tomatoes", 4},
		{"surrounding whitespace is trimmed", "   bread   ", "bread", 1},

		// The cases the pattern must NOT treat as a quantity. Getting these
		// wrong writes a number nobody asked for into an inventory batch.
		{"a trailing bare number is part of the name", "milk 2", "milk 2", 1},
		{"a percentage is not a count", "milk 3.5%", "milk 3.5%", 1},
		{"a size is not a count", "san pellegrino 500", "san pellegrino 500", 1},

		// Clamped rather than ignored, so the digits do not stay in the text
		// handed to the matcher. The user reviews the quantity before anything
		// is written, so a clamp costs a correction, never a wrong batch.
		{"an absurd count is clamped", "eggs x99999", "eggs", 999},
		{"a four-digit count is clamped the same way", "eggs x9999", "eggs", 999},
		{"a zero count falls back to one", "eggs x0", "eggs", 1},
		{"an empty line is one unit", "", "", 1},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			text, qty := matching.ParseLine(tc.raw)

			assert.Equal(t, tc.wantText, text, "text")
			assert.Equal(t, tc.wantQty, qty, "quantity")
		})
	}
}

func TestSplitLines(t *testing.T) {
	t.Parallel()

	t.Run("blank lines are dropped, duplicates are kept", func(t *testing.T) {
		t.Parallel()

		lines := matching.SplitLines("milk\n\neggs\n   \nmilk\n")

		// "milk" twice is two milks. Collapsing duplicates would be the system
		// overruling what the user wrote.
		assert.Equal(t, []string{"milk", "eggs", "milk"}, lines)
	})

	t.Run("windows line endings split the same way", func(t *testing.T) {
		t.Parallel()

		assert.Equal(t, []string{"milk", "eggs"}, matching.SplitLines("milk\r\neggs"))
	})

	t.Run("an empty list has no lines", func(t *testing.T) {
		t.Parallel()

		assert.Empty(t, matching.SplitLines("   \n\n  "))
	})
}

func TestNormalizeQuery(t *testing.T) {
	t.Parallel()

	// The normalization must match the one catalog rows are stored under, or a
	// query would be compared against a form the table does not hold.
	assert.Equal(t, "cherry tomatoes", matching.NormalizeQuery("  Cherry   TOMATOES "))
}
