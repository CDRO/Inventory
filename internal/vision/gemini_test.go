package vision_test

import (
	"context"
	"encoding/base64"
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

func TestParseAnalysisAcceptsTheSpecExample(t *testing.T) {
	t.Parallel()

	got, err := vision.ParseAnalysis(vision.ModeShelf, []byte(`{
	  "items": [{
	    "label": "Barilla Penne 500g", "confidence": 0.91, "quantity": 3,
	    "bounding_box": {"x": 0.12, "y": 0.30, "width": 0.10, "height": 0.22},
	    "proposed_location_path": ["Basement", "Right Shelf", "Layer 2", "Front-Right"]
	  }]
	}`))
	require.NoError(t, err)
	require.Len(t, got.Items, 1)

	item := got.Items[0]
	assert.Equal(t, "Barilla Penne 500g", item.Label)
	assert.Equal(t, 3, item.Quantity)
	assert.InDelta(t, 0.91, item.Confidence, 1e-9)
	require.NotNil(t, item.BoundingBox)
	assert.InDelta(t, 0.22, item.BoundingBox.Height, 1e-9)
	assert.Equal(t, []string{"Basement", "Right Shelf", "Layer 2", "Front-Right"}, item.ProposedLocationPath)
}

// TestMalformedAnalysesFail — each of these must fail the job, never become a
// proposal with nothing in it.
func TestMalformedAnalysesFail(t *testing.T) {
	t.Parallel()

	for name, text := range map[string]string{
		"not json":          `Sure! Here are the items: pasta, rice`,
		"truncated":         `{"items": [{"label": "Rice", "quantity": 1`,
		"no items key":      `{"products": []}`,
		"items is null":     `{"items": null}`,
		"blank label":       `{"items": [{"label": "  ", "confidence": 0.5, "quantity": 1}]}`,
		"zero quantity":     `{"items": [{"label": "Rice", "confidence": 0.5, "quantity": 0}]}`,
		"missing quantity":  `{"items": [{"label": "Rice", "confidence": 0.5}]}`,
		"quantity a string": `{"items": [{"label": "Rice", "confidence": 0.5, "quantity": "three"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := vision.ParseAnalysis(vision.ModeShelf, []byte(text))
			assert.ErrorIs(t, err, vision.ErrMalformedResponse)
		})
	}
}

// TestAnEmptyShelfIsAValidAnalysis — zero items is a real answer, distinct
// from an unreadable one.
func TestAnEmptyShelfIsAValidAnalysis(t *testing.T) {
	t.Parallel()

	got, err := vision.ParseAnalysis(vision.ModeShelf, []byte(`{"items": []}`))
	require.NoError(t, err)
	assert.Empty(t, got.Items)
}

// TestSecondaryFieldsAreRepairedNotFatal — a bad box or a stray path segment
// must not cost the user the item.
func TestSecondaryFieldsAreRepairedNotFatal(t *testing.T) {
	t.Parallel()

	got, err := vision.ParseAnalysis(vision.ModeShelf, []byte(`{"items": [
	  {"label": "Rice", "confidence": 1.7, "quantity": 2,
	   "bounding_box": {"x": 0.8, "y": 0.1, "width": 0.9, "height": 0.2},
	   "proposed_location_path": ["Kitchen", " ", "Top"]},
	  {"label": "Beans", "confidence": -2, "quantity": 1,
	   "bounding_box": {"x": 0.1, "y": 0.1, "width": 0, "height": 0.2}}
	]}`))
	require.NoError(t, err)
	require.Len(t, got.Items, 2, "no item is dropped")

	assert.Nil(t, got.Items[0].BoundingBox, "a box running off the image is dropped")
	assert.Equal(t, 1.0, got.Items[0].Confidence)
	assert.Equal(t, []string{"Kitchen", "Top"}, got.Items[0].ProposedLocationPath)

	assert.Nil(t, got.Items[1].BoundingBox, "a zero-width box is dropped")
	assert.Equal(t, 0.0, got.Items[1].Confidence)
	assert.NotNil(t, got.Items[1].ProposedLocationPath, "an absent path is an empty one")
}

func TestProductModeKeepsOneItemWithoutSpatialFields(t *testing.T) {
	t.Parallel()

	got, err := vision.ParseAnalysis(vision.ModeProduct, []byte(`{"items": [
	  {"label": "Maybe Oats", "confidence": 0.4, "quantity": 1},
	  {"label": "Rolled Oats 1kg", "confidence": 0.9, "quantity": 1,
	   "bounding_box": {"x": 0.1, "y": 0.1, "width": 0.5, "height": 0.5},
	   "proposed_location_path": ["Pantry"]}
	]}`))
	require.NoError(t, err)
	require.Len(t, got.Items, 1)
	assert.Equal(t, "Rolled Oats 1kg", got.Items[0].Label, "the most confident identification wins")
	assert.Nil(t, got.Items[0].BoundingBox)
	assert.Empty(t, got.Items[0].ProposedLocationPath)
}

func TestLongLabelsAreTruncatedByCharacter(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("é", 300)
	got, err := vision.ParseAnalysis(vision.ModeShelf, []byte(`{"items": [{"label": "`+long+`", "confidence": 1, "quantity": 1}]}`))
	require.NoError(t, err)
	assert.Equal(t, strings.Repeat("é", 255), got.Items[0].Label)
}

// TestAnalyzeSpeaksTheGenerateContentProtocol drives the client against a fake
// endpoint: the request carries the image, the structured-output settings and
// the key in a header, and the reply's text part is parsed.
func TestAnalyzeSpeaksTheGenerateContentProtocol(t *testing.T) {
	t.Parallel()

	image := []byte{0xFF, 0xD8, 0xFF, 0xE0, 'j', 'p', 'g'}
	var seen struct {
		path, key, query string
		body             map[string]any
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.path, seen.key, seen.query = r.URL.Path, r.Header.Get("x-goog-api-key"), r.URL.RawQuery
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &seen.body)
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"{\"items\":[{\"label\":\"Rice\",\"confidence\":0.8,\"quantity\":2}]}"}]}}]}`))
	}))
	t.Cleanup(srv.Close)

	client := &vision.Client{APIKey: "secret-key", Endpoint: srv.URL, HTTP: srv.Client()}
	got, err := client.Analyze(context.Background(), "models/gemini-2.0-flash", vision.ModeShelf, image, "image/jpeg")
	require.NoError(t, err)
	require.Len(t, got.Items, 1)
	assert.Equal(t, "Rice", got.Items[0].Label)

	assert.Equal(t, "/models/gemini-2.0-flash:generateContent", seen.path)
	assert.Equal(t, "secret-key", seen.key)
	assert.NotContains(t, seen.query, "secret-key", "the key never goes in the URL")

	config := seen.body["generationConfig"].(map[string]any)
	assert.Equal(t, "application/json", config["responseMimeType"])
	assert.NotNil(t, config["responseSchema"], "structured output, not JSON-in-prose")

	parts := seen.body["contents"].([]any)[0].(map[string]any)["parts"].([]any)
	inline := parts[1].(map[string]any)["inline_data"].(map[string]any)
	assert.Equal(t, "image/jpeg", inline["mime_type"])
	assert.Equal(t, base64.StdEncoding.EncodeToString(image), inline["data"])
}

func TestAnalyzeReportsProviderFailures(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		status int
		body   string
		want   error
	}{
		"model gone":     {http.StatusNotFound, `{"error":{"code":404}}`, vision.ErrModelNotFound},
		"blocked":        {http.StatusOK, `{"candidates":[],"promptFeedback":{"blockReason":"SAFETY"}}`, vision.ErrMalformedResponse},
		"garbage body":   {http.StatusOK, `<html>proxy error</html>`, vision.ErrMalformedResponse},
		"prose not json": {http.StatusOK, `{"candidates":[{"content":{"parts":[{"text":"I see rice."}]}}]}`, vision.ErrMalformedResponse},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(srv.Close)

			client := &vision.Client{APIKey: "k", Endpoint: srv.URL, HTTP: srv.Client()}
			_, err := client.Analyze(context.Background(), "gemini-x", vision.ModeShelf, []byte("img"), "image/png")
			assert.ErrorIs(t, err, tc.want)
		})
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	client := &vision.Client{APIKey: "k", Endpoint: srv.URL, HTTP: srv.Client()}
	_, err := client.Analyze(context.Background(), "gemini-x", vision.ModeShelf, []byte("img"), "image/png")
	require.Error(t, err)
	assert.NotErrorIs(t, err, vision.ErrMalformedResponse, "an outage is not a malformed reply")
}
