package vision_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/vision"
)

// maskOf is a stand-in mask payload, in the data-URL form the model sends.
// ParseSegmentation only decodes the base64; whether the bytes are a PNG is
// internal/images' concern.
func maskOf(b []byte) string {
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(b)
}

func TestParseSegmentationReadsTheModelsFormat(t *testing.T) {
	t.Parallel()

	reply, err := json.Marshal([]map[string]any{
		{"box_2d": []int{100, 200, 600, 700}, "mask": maskOf([]byte("mask bytes")), "label": "jar"},
	})
	require.NoError(t, err)

	got, err := vision.ParseSegmentation(reply)
	require.NoError(t, err)
	// [y0, x0, y1, x1] on 0–1000 becomes a normalized x/y/width/height box.
	assert.InDelta(t, 0.2, got.Box.X, 1e-9)
	assert.InDelta(t, 0.1, got.Box.Y, 1e-9)
	assert.InDelta(t, 0.5, got.Box.Width, 1e-9)
	assert.InDelta(t, 0.5, got.Box.Height, 1e-9)
	assert.Equal(t, []byte("mask bytes"), got.Mask, "the data-URL prefix is stripped and the base64 decoded")
}

// TestParseSegmentationKeepsTheLargestUsableOutline — entries that cannot be
// trusted are skipped, and of the rest the largest box is the subject.
func TestParseSegmentationKeepsTheLargestUsableOutline(t *testing.T) {
	t.Parallel()

	reply, err := json.Marshal([]map[string]any{
		{"box_2d": []int{400, 400, 500, 500}, "mask": maskOf([]byte("label on the jar"))},
		{"box_2d": []int{0, 0, 1000, 1000}, "mask": "not base64 !!"},
		{"box_2d": []int{600, 0, 100, 900}, "mask": maskOf([]byte("corners out of order"))},
		{"box_2d": []int{0, 0, 1200, 900}, "mask": maskOf([]byte("off the scale"))},
		{"box_2d": []int{0, 0, 900}, "mask": maskOf([]byte("three corners"))},
		{"box_2d": []int{100, 100, 900, 800}, "mask": maskOf([]byte("the jar"))},
	})
	require.NoError(t, err)

	got, err := vision.ParseSegmentation(reply)
	require.NoError(t, err)
	assert.Equal(t, []byte("the jar"), got.Mask)
}

func TestParseSegmentationToleratesAFencedReply(t *testing.T) {
	t.Parallel()

	reply := "```json\n[{\"box_2d\":[0,0,1000,1000],\"mask\":\"" + maskOf([]byte("m")) + "\"}]\n```"
	got, err := vision.ParseSegmentation([]byte(reply))
	require.NoError(t, err)
	assert.Equal(t, []byte("m"), got.Mask)
}

func TestUnusableSegmentationsAreMalformed(t *testing.T) {
	t.Parallel()

	for name, reply := range map[string]string{
		"prose":           `I can see a jar.`,
		"empty list":      `[]`,
		"object not list": `{"box_2d":[0,0,1000,1000],"mask":"bQ=="}`,
		"no usable entry": `[{"box_2d":[0,0,1000,1000],"mask":""}]`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := vision.ParseSegmentation([]byte(reply))
			assert.ErrorIs(t, err, vision.ErrMalformedResponse)
		})
	}
}

func TestSegmentSpeaksTheGenerateContentProtocol(t *testing.T) {
	t.Parallel()

	image := []byte{0x89, 'P', 'N', 'G'}
	var seen struct {
		path, key string
		body      map[string]any
	}
	reply, err := json.Marshal([]map[string]any{{"box_2d": []int{0, 0, 1000, 1000}, "mask": maskOf([]byte("m"))}})
	require.NoError(t, err)
	envelope, err := json.Marshal(map[string]any{
		"candidates": []any{map[string]any{"content": map[string]any{"parts": []any{map[string]any{"text": string(reply)}}}}},
	})
	require.NoError(t, err)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.path, seen.key = r.URL.Path, r.Header.Get("x-goog-api-key")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &seen.body)
		_, _ = w.Write(envelope)
	}))
	t.Cleanup(srv.Close)

	client := &vision.Client{APIKey: "secret-key", Endpoint: srv.URL, HTTP: srv.Client()}
	got, err := client.Segment(context.Background(), "gemini-2.5-flash", image, "image/png")
	require.NoError(t, err)
	assert.Equal(t, []byte("m"), got.Mask)

	assert.Equal(t, "/models/gemini-2.5-flash:generateContent", seen.path, "the model asked for, not the analysis model")
	assert.Equal(t, "secret-key", seen.key)

	config := seen.body["generationConfig"].(map[string]any)
	assert.Equal(t, "application/json", config["responseMimeType"])
	assert.NotContains(t, config, "responseSchema", "segmentation output is not constrained by a schema")

	parts := seen.body["contents"].([]any)[0].(map[string]any)["parts"].([]any)
	inline := parts[1].(map[string]any)["inline_data"].(map[string]any)
	assert.Equal(t, "image/png", inline["mime_type"])
	assert.Equal(t, base64.StdEncoding.EncodeToString(image), inline["data"])
}

func TestSegmentReportsAWithdrawnModel(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	client := &vision.Client{APIKey: "k", Endpoint: srv.URL, HTTP: srv.Client()}
	_, err := client.Segment(context.Background(), "gemini-gone", []byte("img"), "image/jpeg")
	assert.ErrorIs(t, err, vision.ErrModelNotFound)
}
