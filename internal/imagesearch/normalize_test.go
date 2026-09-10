package imagesearch_test

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CDRO/Inventory/internal/imagesearch"
)

// An SVG is served from our own origin, so anything it manages to execute runs
// as our origin. These are the constructs that would do it.
func TestSanitizeSVGStripsExecutableConstructs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		svg     string
		absent  []string
		present []string
	}{
		{
			name:   "script element",
			svg:    `<svg xmlns="http://www.w3.org/2000/svg"><script>fetch('/api/auth/me')</script><circle r="5"/></svg>`,
			absent: []string{"script", "fetch"},
			// The drawing survives: sanitizing must leave a usable icon, not
			// an empty file.
			present: []string{"<circle"},
		},
		{
			name:   "self-closing script element",
			svg:    `<svg xmlns="http://www.w3.org/2000/svg"><script src="https://evil.example/x.js"/><rect/></svg>`,
			absent: []string{"script", "evil.example"},
		},
		{
			// Regression: a single pattern ending in `(?:</tag>|/>)` stops at
			// the *inner* self-closing element, leaving the rest of the block
			// — including the payload — in the stored file.
			name:    "script containing a nested self-closing element",
			svg:     `<svg xmlns="http://www.w3.org/2000/svg"><script><img src="x"/>steal()</script><rect/></svg>`,
			absent:  []string{"script", "steal"},
			present: []string{"<rect"},
		},
		{
			name:   "foreignObject smuggling HTML",
			svg:    `<svg xmlns="http://www.w3.org/2000/svg"><foreignObject><body onload="alert(1)"/></foreignObject><rect/></svg>`,
			absent: []string{"foreignObject", "onload", "alert"},
		},
		{
			name:   "event handler attribute",
			svg:    `<svg xmlns="http://www.w3.org/2000/svg"><rect onclick="alert(1)" onmouseover='steal()' width="10"/></svg>`,
			absent: []string{"onclick", "onmouseover", "alert", "steal"},
			// The element and its legitimate attributes stay.
			present: []string{"<rect", `width="10"`},
		},
		{
			name:   "external xlink reference",
			svg:    `<svg xmlns="http://www.w3.org/2000/svg"><image xlink:href="https://tracker.example/pixel.png"/><rect/></svg>`,
			absent: []string{"tracker.example"},
		},
		{
			name:   "protocol-relative reference",
			svg:    `<svg xmlns="http://www.w3.org/2000/svg"><image href="//tracker.example/pixel.png"/><rect/></svg>`,
			absent: []string{"tracker.example"},
		},
		{
			name:   "javascript: href",
			svg:    `<svg xmlns="http://www.w3.org/2000/svg"><a href="javascript:alert(1)"><rect/></a></svg>`,
			absent: []string{"javascript:", "alert"},
		},
		{
			name:   "css url() pulling a remote resource",
			svg:    `<svg xmlns="http://www.w3.org/2000/svg"><rect style="fill:url('https://tracker.example/p.svg')"/></svg>`,
			absent: []string{"tracker.example"},
		},
		{
			name:   "entity declaration",
			svg:    `<!DOCTYPE svg [<!ENTITY xxe SYSTEM "file:///etc/passwd">]><svg xmlns="http://www.w3.org/2000/svg"><rect/></svg>`,
			absent: []string{"ENTITY", "etc/passwd"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			clean, err := imagesearch.SanitizeSVG([]byte(tc.svg))
			require.NoError(t, err)

			lowered := strings.ToLower(string(clean))
			for _, needle := range tc.absent {
				assert.NotContains(t, lowered, strings.ToLower(needle),
					"a sanitized SVG is served from our own origin; %q must not survive", needle)
			}
			for _, needle := range tc.present {
				assert.Contains(t, string(clean), needle, "the icon itself must survive sanitizing")
			}
			assert.Contains(t, lowered, "<svg", "the result must still be an SVG")
		})
	}
}

// TestSanitizeSVGKeepsInlineDataImages — a data: image URI is self-contained
// and reaches no network, so stripping it would break legitimate icons for no
// security gain.
func TestSanitizeSVGKeepsInlineDataImages(t *testing.T) {
	t.Parallel()

	svg := `<svg xmlns="http://www.w3.org/2000/svg"><image href="data:image/png;base64,iVBORw0KGgo="/></svg>`

	clean, err := imagesearch.SanitizeSVG([]byte(svg))
	require.NoError(t, err)

	assert.Contains(t, string(clean), "data:image/png")
}

// TestSanitizeSVGRejectsWhatIsLeftWithNothing — an "icon" whose entire content
// was executable was never an icon.
func TestSanitizeSVGRejectsWhatIsLeftWithNothing(t *testing.T) {
	t.Parallel()

	_, err := imagesearch.SanitizeSVG([]byte(`<script>alert(1)</script>`))

	require.ErrorIs(t, err, imagesearch.ErrUnusable)
}

func TestNormalizeDownscalesLargeRasters(t *testing.T) {
	t.Parallel()

	// 2000x1000: over the long-edge cap, well under the pixel guard.
	src := image.NewRGBA(image.Rect(0, 0, 2000, 1000))
	for x := 0; x < 2000; x++ {
		for y := 0; y < 1000; y++ {
			src.Set(x, y, color.RGBA{R: 200, G: 100, B: 50, A: 255})
		}
	}
	var buf bytes.Buffer
	require.NoError(t, jpeg.Encode(&buf, src, nil))

	out, err := imagesearch.Normalize(buf.Bytes(), "image/jpeg")
	require.NoError(t, err)

	assert.Equal(t, imagesearch.MaxEdge, out.Width, "the longest edge is capped")
	assert.Equal(t, imagesearch.MaxEdge/2, out.Height, "and the aspect ratio is preserved")
	assert.Equal(t, "image/jpeg", out.ContentType)
	assert.Less(t, len(out.Data), buf.Len(), "a normalized suggestion must be smaller than the original")
}

// TestNormalizeKeepsTransparencyAsPNG — re-encoding a transparent icon as JPEG
// would put a black box behind it.
func TestNormalizeKeepsTransparencyAsPNG(t *testing.T) {
	t.Parallel()

	src := image.NewRGBA(image.Rect(0, 0, 40, 40))
	src.Set(5, 5, color.RGBA{R: 255, A: 128}) // translucent
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, src))

	out, err := imagesearch.Normalize(buf.Bytes(), "image/png")
	require.NoError(t, err)

	assert.Equal(t, "image/png", out.ContentType)
}

// TestNormalizeLeavesSmallImagesAlone — upscaling a 32px icon to 1024 would
// only invent detail and multiply the bytes stored.
func TestNormalizeDoesNotUpscale(t *testing.T) {
	t.Parallel()

	src := image.NewRGBA(image.Rect(0, 0, 32, 32))
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, src))

	out, err := imagesearch.Normalize(buf.Bytes(), "image/png")
	require.NoError(t, err)

	assert.Equal(t, 32, out.Width)
	assert.Equal(t, 32, out.Height)
}

// TestNormalizeRejectsUndecodableBytesAsUnusable is what makes negative caching
// possible: the caller can only remember "never fetch this again" if the error
// distinguishes a broken candidate from a transient failure.
func TestNormalizeRejectsUndecodableBytesAsUnusable(t *testing.T) {
	t.Parallel()

	_, err := imagesearch.Normalize([]byte("this is not an image at all"), "image/jpeg")

	require.ErrorIs(t, err, imagesearch.ErrUnusable)
}

// TestNormalizeDetectsSVGFromBytesNotJustContentType — providers routinely
// serve an SVG as text/plain or with no type at all, and an unsanitized SVG
// that slipped through as "some raster we could not decode" would be discarded
// rather than served, but the reverse (a raster mistaken for SVG) would store
// bytes unsanitized.
func TestNormalizeDetectsSVGFromBytesNotJustContentType(t *testing.T) {
	t.Parallel()

	svg := `<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script><rect/></svg>`

	out, err := imagesearch.Normalize([]byte(svg), "text/plain")
	require.NoError(t, err)

	assert.Equal(t, "image/svg+xml", out.ContentType)
	assert.NotContains(t, string(out.Data), "script")
}
