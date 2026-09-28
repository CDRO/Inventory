package vision_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/vision"
)

// The classification that rides along on every analysis call
// (docs/specs/41-mixed-photo-classification.md): two optional fields on the
// reply every mode already made, never a second call.

func TestClassificationRidesAlongOnEveryPhysicalMode(t *testing.T) {
	t.Parallel()

	// The empty items array is the common shape for a genuine list misread as
	// a shelf: nothing physical was seen, and the lines are what matters.
	reply := `{
	  "items": [],
	  "looks_like_shopping_list": true,
	  "shopping_list_lines": ["milk", "eggs x2", "cherry tomatoes"]
	}`

	for _, mode := range []vision.Mode{vision.ModeShelf, vision.ModeProduct, vision.ModeConsumption} {
		got, err := vision.ParseAnalysis(mode, []byte(reply))
		require.NoErrorf(t, err, "mode %s", mode)
		assert.Truef(t, got.LooksLikeShoppingList, "mode %s", mode)
		assert.Equalf(t, []string{"milk", "eggs x2", "cherry tomatoes"}, got.ShoppingListLines, "mode %s", mode)
		assert.Emptyf(t, got.Items, "mode %s", mode)
	}
}

// The overwhelming majority of photos: an ordinary shelf, unchanged by this
// spec — no flag, no lines, and nothing about the items it does carry moved.
func TestAnOrdinaryPhotoIsUnclassifiedAndUnchanged(t *testing.T) {
	t.Parallel()

	got, err := vision.ParseAnalysis(vision.ModeShelf, []byte(`{
	  "items": [{"label": "Rice 1kg", "confidence": 0.9, "quantity": 1}]
	}`))
	require.NoError(t, err)

	assert.False(t, got.LooksLikeShoppingList)
	assert.Empty(t, got.ShoppingListLines, "never nil, so a caller may range over it")
	assert.NotNil(t, got.ShoppingListLines)
	require.Len(t, got.Items, 1)
	assert.Equal(t, "Rice 1kg", got.Items[0].Label)
}

// A flag with nothing behind it is not a classification anybody can act on:
// the banner would offer a list of no lines, and from_job_id would have
// nothing to create. It reads as "not a list" rather than failing the job,
// because the items the mode actually asked for are still reviewable.
func TestAFlagWithNoLinesIsNotAClassification(t *testing.T) {
	t.Parallel()

	got, err := vision.ParseAnalysis(vision.ModeShelf, []byte(`{
	  "items": [{"label": "Rice 1kg", "confidence": 0.9, "quantity": 1}],
	  "looks_like_shopping_list": true,
	  "shopping_list_lines": ["  ", ""]
	}`))
	require.NoError(t, err)

	assert.False(t, got.LooksLikeShoppingList)
	assert.Empty(t, got.ShoppingListLines)
	assert.Len(t, got.Items, 1, "the proposal underneath survives")
}

// The ambiguous both-populated case the spec calls out: a photo showing
// shelved items and a sticky note. Neither half is dropped.
func TestItemsAndListLinesMayBothBePopulated(t *testing.T) {
	t.Parallel()

	got, err := vision.ParseAnalysis(vision.ModeShelf, []byte(`{
	  "items": [{"label": "Rice 1kg", "confidence": 0.9, "quantity": 1}],
	  "looks_like_shopping_list": true,
	  "shopping_list_lines": ["milk"]
	}`))
	require.NoError(t, err)

	assert.True(t, got.LooksLikeShoppingList)
	assert.Equal(t, []string{"milk"}, got.ShoppingListLines)
	require.Len(t, got.Items, 1)
}

func TestListLinesAreTrimmedAndBounded(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("ü", 400)
	many := make([]string, 0, 250)
	many = append(many, "  milk  ", "", "   ", long)
	for i := 0; i < 246; i++ {
		many = append(many, "filler")
	}
	body, err := json.Marshal(map[string]any{
		"items":                    []any{},
		"looks_like_shopping_list": true,
		"shopping_list_lines":      many,
	})
	require.NoError(t, err)

	got, err := vision.ParseAnalysis(vision.ModeShelf, body)
	require.NoError(t, err)

	assert.Len(t, got.ShoppingListLines, 200, "one submission's worth of lines, no more")
	assert.Equal(t, "milk", got.ShoppingListLines[0], "surrounding space is not part of a line")
	// Truncated by character, not by byte: raw_text holds 255 characters, and
	// a multi-byte line cut at 255 bytes would end mid-rune.
	assert.Equal(t, 255, len([]rune(got.ShoppingListLines[1])))
}

// ModeShoppingList is the one mode that never asked about items, so an
// omitted items array is the contract being met rather than a reply that
// could not be read — the strict refusal every other mode gets would fail a
// perfectly good transcription.
func TestShoppingListModeNeedsNoItemsArray(t *testing.T) {
	t.Parallel()

	got, err := vision.ParseAnalysis(vision.ModeShoppingList, []byte(`{
	  "shopping_list_lines": ["milk", "eggs x2"]
	}`))
	require.NoError(t, err)

	assert.True(t, got.LooksLikeShoppingList, "a photo uploaded as a list is a list")
	assert.Equal(t, []string{"milk", "eggs x2"}, got.ShoppingListLines)
	assert.Empty(t, got.Items)

	_, err = vision.ParseAnalysis(vision.ModeShelf, []byte(`{"shopping_list_lines": ["milk"]}`))
	require.ErrorIs(t, err, vision.ErrMalformedResponse, "every other mode still refuses a reply with no items array")
}

// A list photo that read as nothing is not a malformed reply — it is an empty
// transcription, and the caller decides what that means (internal/ingest
// fails the job with a message rather than writing an empty list).
func TestShoppingListModeAcceptsAnEmptyTranscription(t *testing.T) {
	t.Parallel()

	got, err := vision.ParseAnalysis(vision.ModeShoppingList, []byte(`{"items": [], "shopping_list_lines": []}`))
	require.NoError(t, err)
	assert.Empty(t, got.ShoppingListLines)
}

func TestUnknownModeIsStillRefused(t *testing.T) {
	t.Parallel()

	client := &vision.Client{APIKey: "k"}
	_, err := client.Analyze(context.Background(), "gemini-test", vision.Mode("nonsense"), []byte("x"), "image/jpeg")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown mode")
}

// The prompt and the schema are the contract with the model, so they are
// asserted on the wire rather than by reading the package's own variables:
// a mode dropped from the instruction, or a schema field that never reaches
// the provider, is silent everywhere else.
func TestEveryPhysicalModeAsksForTheClassification(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		mode    vision.Mode
		asksFor bool
	}{
		{vision.ModeShelf, true},
		{vision.ModeProduct, true},
		{vision.ModeConsumption, true},
		// A photo uploaded as a list has no mismatch to surface — the person
		// chose that endpoint on purpose ("No reverse check").
		{vision.ModeShoppingList, false},
	} {
		body := captureRequest(t, tc.mode)

		parts := body["contents"].([]any)[0].(map[string]any)["parts"].([]any)
		prompt := parts[0].(map[string]any)["text"].(string)
		if tc.asksFor {
			assert.Containsf(t, prompt, "looks_like_shopping_list", "mode %s must ask for the classification", tc.mode)
			assert.Containsf(t, prompt, "shopping_list_lines", "mode %s must ask for the transcription", tc.mode)
		} else {
			assert.NotContainsf(t, prompt, "looks_like_shopping_list", "mode %s has nothing to classify", tc.mode)
			assert.Containsf(t, prompt, "shopping_list_lines", "mode %s still transcribes", tc.mode)
		}

		schema := body["generationConfig"].(map[string]any)["responseSchema"].(map[string]any)
		properties := schema["properties"].(map[string]any)
		assert.Containsf(t, properties, "looks_like_shopping_list", "mode %s", tc.mode)
		assert.Containsf(t, properties, "shopping_list_lines", "mode %s", tc.mode)
		// Not required: the common reply carries items alone, and a model
		// that answers that way must stay valid rather than fail the job.
		assert.Equalf(t, []any{"items"}, schema["required"], "mode %s", tc.mode)
	}
}

// captureRequest runs one Analyze against a stub provider and returns the
// request body it sent.
func captureRequest(t *testing.T, mode vision.Mode) map[string]any {
	t.Helper()

	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		require.NoError(t, json.Unmarshal(raw, &body))
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"{\"items\":[]}"}]}}]}`))
	}))
	t.Cleanup(srv.Close)

	client := &vision.Client{APIKey: "k", Endpoint: srv.URL, HTTP: srv.Client()}
	_, err := client.Analyze(context.Background(), "gemini-test", mode, []byte{0xFF, 0xD8}, "image/jpeg")
	require.NoError(t, err)
	return body
}
