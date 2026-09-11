package matching

import (
	"regexp"
	"strconv"
	"strings"
)

// NormalizeQuery lowercases, trims and collapses whitespace.
//
// This is the canonical normalization for product text, and it lives here
// rather than in the store because it defines what "the same product name"
// means — a matching rule, not a storage detail. store.NormalizeCatalogName
// delegates to it, so the form a catalog row is *written* under and the form a
// query is *compared* under cannot drift apart. They must agree: a query
// normalized differently would be searching for a shape the table does not
// hold, and would silently miss every row.
//
// The dependency runs one way on purpose — the store imports this package, and
// this package imports nothing of the store — so that a normalization change
// cannot introduce an import cycle between the data layer and the rule that
// interprets it.
func NormalizeQuery(text string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(text))), " ")
}

// quantityPattern matches an explicit multiplier at either end of a line:
// "2x eggs", "2 eggs", "eggs x2", "eggs 2x".
//
// A bare trailing number is deliberately NOT a multiplier. "milk 2" is far
// more likely to be a shorthand for 2% milk than a request for two milks, and
// "san pellegrino 500" is a size. Guessing wrong here writes the wrong
// quantity into someone's inventory, and the cost of not guessing is that the
// user types a number they can already see on screen.
// The digit runs are generous so that clamping happens in exactly one place.
// A narrower pattern would make "eggs x9999" clamp to the ceiling while "eggs
// x99999" was not recognised as a multiplier at all, leaving the digits stuck
// in the text handed to the matcher — an arbitrary cliff between two typos of
// the same kind.
var quantityPattern = regexp.MustCompile(`^(?:(\d{1,6})\s*[xX*]?\s+|)(.*?)(?:\s+[xX*]\s*(\d{1,6})|\s+(\d{1,6})\s*[xX])?$`)

// maxParsedQuantity bounds what a parsed multiplier may claim.
//
// A line reading "eggs x9999" is a typo or a joke, not a pantry. Clamping is
// safe because nothing is written from a parsed quantity: the spec has the
// user review and edit it at the confirm step, so the worst case is a number
// they correct on screen rather than a batch nobody meant.
const maxParsedQuantity = 999

// ParseLine splits one raw shopping-list line into the text to match and the
// quantity the user asked for.
//
// The quantity defaults to 1, which is what "milk" on a shopping list means.
// The returned text is the line with any multiplier removed, so matching sees
// "eggs" rather than "eggs x2" — the trigram score against a product named
// "Eggs" would otherwise be dragged down by characters that were never part of
// the name.
func ParseLine(raw string) (text string, quantity int) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", 1
	}

	groups := quantityPattern.FindStringSubmatch(trimmed)
	if groups == nil {
		return trimmed, 1
	}

	// groups[1] leading count, groups[2] the remaining text,
	// groups[3] and groups[4] the two trailing forms.
	text = strings.TrimSpace(groups[2])
	for _, candidate := range []string{groups[1], groups[3], groups[4]} {
		if candidate == "" {
			continue
		}
		parsed, err := strconv.Atoi(candidate)
		if err != nil || parsed < 1 {
			continue
		}
		quantity = min(parsed, maxParsedQuantity)
		break
	}

	if quantity == 0 {
		quantity = 1
	}
	if text == "" {
		// The line was nothing but a number. Keep the original so the user can
		// see what they wrote and fix it, rather than showing them a blank row.
		return trimmed, quantity
	}
	return text, quantity
}

// SplitLines turns a pasted shopping list into its lines.
//
// Blank lines are dropped rather than becoming empty items: people separate
// sections of a list with them, and an empty row is not something to resolve.
// Duplicate lines are kept — writing "milk" twice usually means two milks, and
// silently collapsing them would be the system deciding it knows better.
func SplitLines(raw string) []string {
	var out []string
	for _, line := range strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}
