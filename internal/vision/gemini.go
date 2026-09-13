package vision

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// defaultGenerateEndpoint is the base Gemini generateContent URLs are built
// from.
const defaultGenerateEndpoint = "https://generativelanguage.googleapis.com/v1beta"

// maxLabelRunes matches products.name, so a label is always storable as a
// product name without a second truncation somewhere downstream.
const maxLabelRunes = 255

// maxResponseBytes bounds what is read back. A shelf's worth of items is a few
// kilobytes of JSON; this is room for a very full shelf, not for a runaway.
const maxResponseBytes = 2 << 20

// Mode is which of the two ingestion prompts to use
// (docs/specs/06-vision-shelf-ingestion.md).
type Mode string

const (
	// ModeShelf finds every product on a shelf, where it is, and where the
	// shelf sits in the house.
	ModeShelf Mode = "shelf"
	// ModeProduct identifies one product and does no spatial inference.
	ModeProduct Mode = "product"
	// ModeConsumption finds every item being used up or discarded in one
	// photo (docs/specs/09-consumption-logging.md). Like ModeShelf it may
	// return several items with a bounding box each, for the review screen's
	// crop; unlike ModeShelf it infers no location path, since consumption
	// never places anything.
	ModeConsumption Mode = "consumption"
)

var (
	// ErrModelNotFound is the provider saying the model id does not exist. The
	// caller should invalidate the availability cache, so the deployment
	// reports model_unavailable instead of failing photo after photo.
	ErrModelNotFound = errors.New("vision: model not found")

	// ErrMalformedResponse is a reply that does not satisfy the contract. It
	// must fail the job rather than become an empty proposal: "the model saw
	// nothing" and "we could not read what the model said" are different
	// statements, and only the first is true of an empty list.
	ErrMalformedResponse = errors.New("vision: malformed response")
)

// Analysis is GeminiShelfAnalysis from the spec, validated.
type Analysis struct {
	Items []Item
}

// Item is one detected product.
type Item struct {
	Label      string
	Confidence float64
	Quantity   int
	// BoundingBox is normalized to [0, 1] of the image's width and height, so
	// it stays valid however the frontend scales the photo. Nil for a product
	// photo, and for a box the model got wrong — the item is kept either way.
	BoundingBox *Box
	// ProposedLocationPath is root to leaf, possibly shorter than the full
	// path, possibly empty. Never a reason to drop the item.
	ProposedLocationPath []string
}

// Box is a normalized rectangle.
type Box struct {
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

// Client calls Gemini's generateContent with structured output.
type Client struct {
	APIKey   string
	Endpoint string
	HTTP     *http.Client
}

// NewClient returns a client for the public Gemini endpoint.
//
// The timeout is generous because a 50MP shelf photo is a large request and
// the model takes seconds to answer. The job runner's own timeout still bounds
// the whole job.
func NewClient(apiKey string) *Client {
	return &Client{
		APIKey:   apiKey,
		Endpoint: defaultGenerateEndpoint,
		HTTP:     &http.Client{Timeout: 90 * time.Second},
	}
}

// Analyze sends one photo to the model and returns the validated analysis.
//
// Structured output (responseMimeType plus responseSchema) is used rather than
// asking for JSON in prose, so the reply is constrained to the contract at the
// source instead of being coaxed out of free text
// (docs/specs/06-vision-shelf-ingestion.md).
func (c *Client) Analyze(ctx context.Context, model string, mode Mode, image []byte, mimeType string) (*Analysis, error) {
	prompt, ok := prompts[mode]
	if !ok {
		return nil, fmt.Errorf("vision: unknown mode %q", mode)
	}

	body, err := json.Marshal(generateRequest{
		Contents: []content{{
			Role: "user",
			Parts: []part{
				{Text: prompt},
				{InlineData: &inlineData{MimeType: mimeType, Data: base64.StdEncoding.EncodeToString(image)}},
			},
		}},
		GenerationConfig: generationConfig{
			ResponseMimeType: "application/json",
			ResponseSchema:   analysisSchema,
			Temperature:      0.2,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("vision: encode request: %w", err)
	}

	endpoint := c.Endpoint
	if endpoint == "" {
		endpoint = defaultGenerateEndpoint
	}
	name := strings.TrimPrefix(model, "models/")
	url := strings.TrimSuffix(endpoint, "/") + "/models/" + name + ":generateContent"

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("vision: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// A header rather than ?key=: a URL ends up in proxy logs and error
	// strings, and this error string is logged.
	req.Header.Set("x-goog-api-key", c.APIKey)

	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: 90 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("vision: call generateContent: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("vision: read response: %w", err)
	}

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("%w: %s", ErrModelNotFound, model)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("vision: generateContent returned %s", resp.Status)
	}

	var decoded generateResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, fmt.Errorf("%w: envelope: %v", ErrMalformedResponse, err)
	}
	if len(decoded.Candidates) == 0 {
		reason := "no candidates"
		if decoded.PromptFeedback != nil && decoded.PromptFeedback.BlockReason != "" {
			reason = "blocked: " + decoded.PromptFeedback.BlockReason
		}
		return nil, fmt.Errorf("%w: %s", ErrMalformedResponse, reason)
	}

	var text strings.Builder
	for _, p := range decoded.Candidates[0].Content.Parts {
		text.WriteString(p.Text)
	}
	return ParseAnalysis(mode, []byte(text.String()))
}

// ParseAnalysis validates the model's JSON against the contract.
//
// What fails the whole analysis: text that is not JSON, a missing items array,
// an item with no label or a quantity below one. Those mean the reply cannot
// be trusted as a description of the photo.
//
// What is repaired instead: a bounding box outside [0, 1] is dropped (the item
// stays, it just has no crop), a confidence outside [0, 1] is clamped, and
// blank path segments are removed. Dropping an item because one of its
// secondary fields was off would be the silent loss the spec rules out.
//
// A product photo keeps exactly one item — the most confident — and has no
// box or path, whatever the model sent.
func ParseAnalysis(mode Mode, text []byte) (*Analysis, error) {
	var wire struct {
		Items *[]struct {
			Label                string   `json:"label"`
			Confidence           *float64 `json:"confidence"`
			Quantity             *int     `json:"quantity"`
			BoundingBox          *Box     `json:"bounding_box"`
			ProposedLocationPath []string `json:"proposed_location_path"`
		} `json:"items"`
	}
	if err := json.Unmarshal(text, &wire); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformedResponse, err)
	}
	if wire.Items == nil {
		return nil, fmt.Errorf("%w: no items array", ErrMalformedResponse)
	}

	out := &Analysis{Items: make([]Item, 0, len(*wire.Items))}
	for i, w := range *wire.Items {
		label := strings.TrimSpace(w.Label)
		if label == "" {
			return nil, fmt.Errorf("%w: item %d has no label", ErrMalformedResponse, i)
		}
		if utf8.RuneCountInString(label) > maxLabelRunes {
			label = string([]rune(label)[:maxLabelRunes])
		}
		if w.Quantity == nil || *w.Quantity < 1 {
			return nil, fmt.Errorf("%w: item %d has no positive quantity", ErrMalformedResponse, i)
		}

		item := Item{Label: label, Quantity: *w.Quantity, ProposedLocationPath: []string{}}
		if w.Confidence != nil && !math.IsNaN(*w.Confidence) {
			item.Confidence = math.Max(0, math.Min(1, *w.Confidence))
		}
		if mode == ModeShelf || mode == ModeConsumption {
			if validBox(w.BoundingBox) {
				item.BoundingBox = w.BoundingBox
			}
		}
		if mode == ModeShelf {
			for _, segment := range w.ProposedLocationPath {
				if s := strings.TrimSpace(segment); s != "" {
					item.ProposedLocationPath = append(item.ProposedLocationPath, s)
				}
			}
		}
		out.Items = append(out.Items, item)
	}

	if mode == ModeProduct && len(out.Items) > 1 {
		sort.SliceStable(out.Items, func(a, b int) bool { return out.Items[a].Confidence > out.Items[b].Confidence })
		out.Items = out.Items[:1]
	}
	return out, nil
}

// validBox reports whether a box lies within the image. A small overshoot from
// rounding is tolerated; anything past that is a box the model invented.
func validBox(b *Box) bool {
	if b == nil {
		return false
	}
	const slack = 0.01
	for _, v := range []float64{b.X, b.Y, b.Width, b.Height} {
		if math.IsNaN(v) || v < 0 || v > 1 {
			return false
		}
	}
	return b.Width > 0 && b.Height > 0 && b.X+b.Width <= 1+slack && b.Y+b.Height <= 1+slack
}

// prompts are the two instructions. They describe the task and nothing else:
// no catalog text, no product names from any storage, nothing a stranger wrote
// is ever interpolated into them (docs/specs/02-data-model.md).
var prompts = map[Mode]string{
	ModeShelf: `You are the vision system of a household inventory app. The photo shows a shelf, ` +
		`cupboard, fridge or similar storage place. List every distinct product you can see. ` +
		`For each: a short label naming the product as printed on it (brand, product, size if legible); ` +
		`how many units of it are visible; your confidence from 0 to 1; a bounding box around all ` +
		`units, as fractions of the image width and height; and, if the photo lets you infer it, the ` +
		`location path from the room down to this spot, for example ["Basement", "Right Shelf", "Layer 2"]. ` +
		`Return a shorter path, or an empty one, rather than guessing. Do not list things that are not ` +
		`products, such as the shelf itself.`,
	ModeProduct: `You are the vision system of a household inventory app. The photo shows one product ` +
		`someone just bought. Identify it: a short label naming the product as printed on it (brand, ` +
		`product, size if legible), quantity 1 unless several identical units are clearly shown, and ` +
		`your confidence from 0 to 1. Return exactly one item. Do not return a bounding box or a ` +
		`location path.`,
	ModeConsumption: `You are the vision system of a household inventory app. The photo shows one or more ` +
		`items someone has just used up, consumed or is discarding — for example an empty carton, an ` +
		`emptied jar, or packaging being thrown away. For each distinct product: a short label naming it ` +
		`as printed on it (brand, product, size if legible); how many units are being used up or ` +
		`discarded; your confidence from 0 to 1; and a bounding box around all units, as fractions of the ` +
		`image width and height. Do not return a location path. Do not list things that are not products, ` +
		`such as a bin or the shelf itself.`,
}

// analysisSchema is GeminiShelfAnalysis in Gemini's schema dialect.
var analysisSchema = map[string]any{
	"type": "OBJECT",
	"properties": map[string]any{
		"items": map[string]any{
			"type": "ARRAY",
			"items": map[string]any{
				"type": "OBJECT",
				"properties": map[string]any{
					"label":      map[string]any{"type": "STRING"},
					"confidence": map[string]any{"type": "NUMBER"},
					"quantity":   map[string]any{"type": "INTEGER"},
					"bounding_box": map[string]any{
						"type": "OBJECT",
						"properties": map[string]any{
							"x":      map[string]any{"type": "NUMBER"},
							"y":      map[string]any{"type": "NUMBER"},
							"width":  map[string]any{"type": "NUMBER"},
							"height": map[string]any{"type": "NUMBER"},
						},
						"required": []string{"x", "y", "width", "height"},
					},
					"proposed_location_path": map[string]any{
						"type":  "ARRAY",
						"items": map[string]any{"type": "STRING"},
					},
				},
				"required": []string{"label", "confidence", "quantity"},
			},
		},
	},
	"required": []string{"items"},
}

type generateRequest struct {
	Contents         []content        `json:"contents"`
	GenerationConfig generationConfig `json:"generationConfig"`
}

type content struct {
	Role  string `json:"role,omitempty"`
	Parts []part `json:"parts"`
}

type part struct {
	Text       string      `json:"text,omitempty"`
	InlineData *inlineData `json:"inline_data,omitempty"`
}

type inlineData struct {
	MimeType string `json:"mime_type"`
	Data     string `json:"data"`
}

type generationConfig struct {
	ResponseMimeType string         `json:"responseMimeType"`
	ResponseSchema   map[string]any `json:"responseSchema"`
	Temperature      float64        `json:"temperature"`
}

type generateResponse struct {
	Candidates []struct {
		Content struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"content"`
	} `json:"candidates"`
	PromptFeedback *struct {
		BlockReason string `json:"blockReason"`
	} `json:"promptFeedback"`
}
