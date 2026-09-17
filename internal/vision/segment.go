package vision

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"strings"
)

// Segmentation is the main subject of a picture, as the model outlined it: the
// first half of background removal (docs/specs/09-consumption-logging.md). The
// second half — cutting the subject out along the mask — happens in our own
// code (internal/images), so the pixels that end up in a product picture are
// the photo's own, never ones a model drew.
type Segmentation struct {
	// Box is where the subject is, normalized to [0, 1] of the picture's width
	// and height like a detection's bounding_box.
	Box Box
	// Mask is a PNG probability map covering Box: bright where the subject is,
	// dark where the background is. Its size is the model's choice, not Box's;
	// it is scaled to the box when applied.
	Mask []byte
}

// segmentPrompt asks for Gemini's segmentation output. The key names and the
// 0–1000 box scale are the ones the model is trained to produce for this task,
// so they are asked for as they are rather than renamed to match the analysis
// contract. Like the analysis prompts, nothing from any storage is ever
// interpolated into it (docs/specs/02-data-model.md).
const segmentPrompt = `Give the segmentation mask for the main product in this picture: the one ` +
	`item a household inventory would list it as, including its packaging, and nothing it stands ` +
	`on or next to. Output a JSON list of segmentation masks where each entry contains the 2D ` +
	`bounding box in the key "box_2d", the segmentation mask in key "mask", and the text label in ` +
	`the key "label". Return exactly one entry.`

// Segment asks the model to outline the main subject of a picture.
//
// Unlike Analyze there is no response schema: segmentation masks are a trained
// output format of their own, and constraining it to a schema is not
// something the model is documented to support. The reply is validated in
// ParseSegmentation instead, and anything unusable is ErrMalformedResponse.
func (c *Client) Segment(ctx context.Context, model string, image []byte, mimeType string) (*Segmentation, error) {
	body, err := json.Marshal(generateRequest{
		Contents: []content{{
			Role: "user",
			Parts: []part{
				{Text: segmentPrompt},
				{InlineData: &inlineData{MimeType: mimeType, Data: base64.StdEncoding.EncodeToString(image)}},
			},
		}},
		GenerationConfig: generationConfig{
			ResponseMimeType: "application/json",
			Temperature:      0.2,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("vision: encode request: %w", err)
	}

	text, err := c.generate(ctx, model, body)
	if err != nil {
		return nil, err
	}
	return ParseSegmentation([]byte(text))
}

// ParseSegmentation reads the model's segmentation reply.
//
// The reply is a list of {box_2d: [y0, x0, y1, x1] on a 0–1000 scale, mask:
// "data:image/png;base64,…", label}. An entry whose box is not a rectangle
// inside the picture, or whose mask is not base64, is skipped rather than
// trusted. Of those left, the largest box is the subject: the prompt asks for
// exactly one, and when a model returns more the biggest outline is the one
// most likely to be the product rather than a label on it.
//
// No usable entry at all is ErrMalformedResponse. Background removal is an
// optional nicety, and "the model outlined nothing" and "we could not read the
// outline" both mean the same thing to the person reviewing: keep the original.
func ParseSegmentation(text []byte) (*Segmentation, error) {
	text = bytes.TrimSpace(text)
	// JSON mode should never fence its output, but a model that does anyway
	// has still answered.
	if bytes.HasPrefix(text, []byte("```")) {
		text = bytes.TrimPrefix(text, []byte("```json"))
		text = bytes.TrimPrefix(text, []byte("```"))
		text = bytes.TrimSuffix(bytes.TrimSpace(text), []byte("```"))
	}

	var entries []struct {
		Box2D []float64 `json:"box_2d"`
		Mask  string    `json:"mask"`
	}
	if err := json.Unmarshal(text, &entries); err != nil {
		return nil, fmt.Errorf("%w: segmentation: %v", ErrMalformedResponse, err)
	}

	var best *Segmentation
	bestArea := 0.0
	for _, e := range entries {
		box, ok := segmentationBox(e.Box2D)
		if !ok {
			continue
		}
		mask, ok := decodeMask(e.Mask)
		if !ok {
			continue
		}
		if area := box.Width * box.Height; area > bestArea {
			best, bestArea = &Segmentation{Box: box, Mask: mask}, area
		}
	}
	if best == nil {
		return nil, fmt.Errorf("%w: segmentation: no usable mask", ErrMalformedResponse)
	}
	return best, nil
}

// segmentationBox converts a [y0, x0, y1, x1] box on the 0–1000 scale into a
// normalized Box. Corners out of order, outside the scale, or not numbers at
// all describe no rectangle in the picture.
func segmentationBox(v []float64) (Box, bool) {
	if len(v) != 4 {
		return Box{}, false
	}
	for _, n := range v {
		if math.IsNaN(n) || math.IsInf(n, 0) || n < 0 || n > 1000 {
			return Box{}, false
		}
	}
	y0, x0, y1, x1 := v[0], v[1], v[2], v[3]
	if x1 <= x0 || y1 <= y0 {
		return Box{}, false
	}
	return Box{X: x0 / 1000, Y: y0 / 1000, Width: (x1 - x0) / 1000, Height: (y1 - y0) / 1000}, true
}

// decodeMask strips the data-URL prefix the model puts on a mask and decodes
// the base64 after it. Whether the bytes are a PNG is for the image code that
// applies the mask to decide.
func decodeMask(s string) ([]byte, bool) {
	if i := strings.Index(s, "base64,"); i >= 0 {
		s = s[i+len("base64,"):]
	}
	data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || len(data) == 0 {
		return nil, false
	}
	return data, true
}
