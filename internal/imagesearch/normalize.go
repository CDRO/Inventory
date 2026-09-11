// Package imagesearch turns a product name into three picture suggestions and
// keeps the bytes on our own machine
// (docs/specs/07-shopping-list-reconciliation.md).
//
// # The browser never talks to a provider
//
// Every suggestion URL this package produces points at our own origin and
// serves bytes already fetched into the cache. Handing the frontend an
// Iconify or Google URL instead would leak every viewer's IP address and
// user-agent to that host, break entirely on a LAN-only NAS with no outbound
// route from the client, and leave a third party able to change the picture
// after the fact. The SerpAPI key never leaves the server for the same
// reason, one degree more obviously.
//
// # Normalize, never reject
//
// A rejected candidate is fetched again the next time the same query runs, and
// again after that — rejection creates exactly the refetch loop it was meant to
// avoid while still paying for every download. So a candidate is normalized on
// ingest and stored in normalized form, and one that genuinely cannot be used
// is remembered as unusable so it is never fetched again.
package imagesearch

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"regexp"
	"strings"

	xdraw "golang.org/x/image/draw"

	// Registered for their side effect: image.Decode and image.DecodeConfig
	// dispatch on the formats that have been registered, and a candidate from
	// Google Images is routinely one of these.
	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/webp"

	"github.com/CDRO/Inventory/internal/images"
)

// Limits on what will be fetched and decoded.
//
// These bound work rather than refuse service: a candidate over either limit
// becomes an "unusable" cache row and the suggestion falls back to the icon,
// so the cost is one missing photo rather than a failed New Item flow.
const (
	// MaxDownloadBytes caps a single candidate download. A product photo is
	// tens to hundreds of kilobytes; 20MB is already absurd for one and is the
	// point at which a hostile or broken URL stops being worth the bandwidth.
	MaxDownloadBytes = 20 << 20 // 20MB

	// MaxDecodePixels caps the decoded dimensions. A decompression bomb is
	// small on the wire and enormous in memory, so the guard has to be on
	// pixels rather than bytes.
	MaxDecodePixels = images.MaxPixels // 50MP

	// MaxEdge is the longest edge a stored raster may have. A suggestion is
	// shown as a card thumbnail; anything larger is bandwidth and cache budget
	// spent on pixels nobody sees.
	MaxEdge = 1024

	// JPEGQuality is the re-encode quality for normalized rasters. At this
	// setting a typical suggestion lands well under 200KB, which is what makes
	// a 1GB cache hold thousands of them rather than a few hundred originals.
	JPEGQuality = 80
)

// ErrUnusable is a candidate that cannot become a stored suggestion: too many
// pixels, or bytes that will not decode as any image format we handle.
//
// It is a distinct error because the caller records it, rather than simply
// failing: remembering that something is unusable is what prevents refetching
// it forever.
var ErrUnusable = errors.New("imagesearch: candidate unusable")

// Normalized is the stored form of a candidate.
type Normalized struct {
	Data        []byte
	ContentType string
	Width       int
	Height      int
}

// Normalize converts fetched bytes into the form that gets stored.
//
// SVG is kept as SVG — rasterizing an icon throws away the one advantage it
// has — but sanitized first. Everything else is decoded, downscaled and
// re-encoded, which also discards any metadata the original carried.
func Normalize(data []byte, contentType string) (*Normalized, error) {
	if isSVG(data, contentType) {
		clean, err := SanitizeSVG(data)
		if err != nil {
			return nil, err
		}
		return &Normalized{Data: clean, ContentType: "image/svg+xml"}, nil
	}
	return normalizeRaster(data)
}

// normalizeRaster decodes, downscales and re-encodes a bitmap image.
func normalizeRaster(data []byte) (*Normalized, error) {
	// Read the header before decoding pixels: a decompression bomb announces
	// its dimensions in a few bytes, and refusing there costs nothing, while
	// decoding first is the attack.
	if err := images.CheckDimensions(data, MaxDecodePixels); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnusable, err)
	}

	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		// An animated GIF decodes through the same path: image.Decode returns
		// its first frame, which is exactly what should be stored. An animated
		// thumbnail is never useful here and costs a multiple of the size.
		// This branch is reached only when the bytes are not a format we
		// handle at all.
		if first, gifErr := gif.Decode(bytes.NewReader(data)); gifErr == nil {
			img = first
		} else {
			return nil, fmt.Errorf("%w: decode: %v", ErrUnusable, err)
		}
	}

	scaled := downscale(img)
	bounds := scaled.Bounds()

	// PNG only when the source actually has transparency worth keeping.
	// Re-encoding an opaque photo as PNG would multiply its size for nothing.
	var buf bytes.Buffer
	contentType := "image/jpeg"
	if hasTransparency(scaled) {
		contentType = "image/png"
		if err := png.Encode(&buf, scaled); err != nil {
			return nil, fmt.Errorf("imagesearch: encode png: %w", err)
		}
	} else {
		if err := jpeg.Encode(&buf, scaled, &jpeg.Options{Quality: JPEGQuality}); err != nil {
			return nil, fmt.Errorf("imagesearch: encode jpeg: %w", err)
		}
	}

	return &Normalized{
		Data:        buf.Bytes(),
		ContentType: contentType,
		Width:       bounds.Dx(),
		Height:      bounds.Dy(),
	}, nil
}

// downscale shrinks img so its longest edge is at most MaxEdge, preserving
// aspect ratio. An image already small enough is returned untouched — upscaling
// a small icon would only invent detail.
func downscale(img image.Image) image.Image {
	bounds := img.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if width <= MaxEdge && height <= MaxEdge {
		return img
	}

	scale := float64(MaxEdge) / float64(max(width, height))
	target := image.Rect(0, 0, max(1, int(float64(width)*scale)), max(1, int(float64(height)*scale)))

	dst := image.NewRGBA(target)
	// CatmullRom rather than NearestNeighbor: this runs once per candidate, at
	// fetch time, and the result is what every viewer sees from then on.
	xdraw.CatmullRom.Scale(dst, target, img, bounds, xdraw.Over, nil)
	return dst
}

// hasTransparency reports whether img has any pixel that is not fully opaque.
//
// It short-circuits on the first translucent pixel, and skips the scan
// entirely for image types whose model cannot represent alpha.
func hasTransparency(img image.Image) bool {
	// A JPEG decodes to YCbCr, which has no alpha channel at all, so the scan
	// below could only ever return false for one.
	if _, isYCbCr := img.(*image.YCbCr); isYCbCr {
		return false
	}

	bounds := img.Bounds()
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			if _, _, _, a := img.At(x, y).RGBA(); a < 0xffff {
				return true
			}
		}
	}
	return false
}

func isSVG(data []byte, contentType string) bool {
	if strings.Contains(strings.ToLower(contentType), "svg") {
		return true
	}
	// Content-Type is provider-supplied and routinely wrong or absent, so the
	// bytes get a say too.
	head := data
	if len(head) > 1024 {
		head = head[:1024]
	}
	return bytes.Contains(bytes.ToLower(head), []byte("<svg"))
}

// SVG sanitization patterns.
//
// SVG is XML that browsers execute: it can carry <script>, embed arbitrary
// HTML through <foreignObject>, run code from event-handler attributes, and
// pull in remote content through xlink:href or a CSS url(). Serving one from
// our own origin means anything it executes runs *as* our origin, so the bytes
// are rewritten before they are ever stored.
//
// This is defence in depth rather than the only defence: the serving handler
// additionally sends Content-Security-Policy: default-src 'none' and
// X-Content-Type-Options: nosniff, so an SVG opened directly still cannot
// execute or fetch anything even if a construct slipped past these patterns.
var (
	// Each dangerous element needs two passes, and the reason is a bug this
	// package's own tests caught: a single pattern ending in
	// `(?:</tag>|/>)` matches the `/>` of a *nested* self-closing element
	// first, because RE2's non-greedy body stops at the earliest alternative.
	// `<foreignObject><body onload="x"/></foreignObject>` therefore lost only
	// as far as the inner `<body/>`, leaving the closing tag — and, with a
	// different payload, the dangerous content — behind.
	//
	// So: remove properly paired blocks including their content, then sweep up
	// any residual tag of that name, which covers the self-closing form, an
	// unclosed opening tag, and an orphaned closing tag alike.
	// The optional prefix group on every element pattern is not decoration.
	// XML lets a document bind any prefix to the SVG namespace, so
	// `<s:script>` is a script element by every parser that matters while
	// matching none of a pattern anchored on a bare `<script`. Caught in
	// review with a working proof of concept; the CSP on the serving route
	// contains it there, but a copy of the file saved and reopened from disk
	// has no such protection.
	//
	// The class is "anything that is not a delimiter" rather than `[\w.-]`,
	// because Go's `\w` is ASCII-only while an XML namespace prefix is an
	// NCName and may be any Unicode letter. `<ñ:script>` is as valid as
	// `<s:script>`, and an ASCII class would have left the same hole one
	// keystroke further away.
	svgScriptBlock  = regexp.MustCompile(`(?is)<\s*(?:[^\s<>/"'=:]+:)?script\b[^>]*>.*?</\s*(?:[^\s<>/"'=:]+:)?script\s*>`)
	svgScriptTag    = regexp.MustCompile(`(?is)</?\s*(?:[^\s<>/"'=:]+:)?script\b[^>]*>`)
	svgForeignBlock = regexp.MustCompile(`(?is)<\s*(?:[^\s<>/"'=:]+:)?foreignObject\b[^>]*>.*?</\s*(?:[^\s<>/"'=:]+:)?foreignObject\s*>`)
	svgForeignTag   = regexp.MustCompile(`(?is)</?\s*(?:[^\s<>/"'=:]+:)?foreignObject\b[^>]*>`)

	// SMIL animation elements are removed outright.
	//
	// They are a way to rewrite another element's attribute *after* the
	// document has loaded, which walks straight around every check above:
	//
	//   <a><animate attributeName="href" values="javascript:alert(1)"/></a>
	//
	// The href passes the allow-list at sanitize time because it is not there
	// yet — the animation installs it later. Nothing about a static product
	// icon needs animation, so the whole family goes rather than trying to
	// decide which attributeName values are safe to animate.
	svgAnimateBlock = regexp.MustCompile(`(?is)<\s*(?:[^\s<>/"'=:]+:)?(?:animate|animateTransform|animateMotion|animateColor|set)\b[^>]*>.*?</\s*(?:[^\s<>/"'=:]+:)?(?:animate|animateTransform|animateMotion|animateColor|set)\s*>`)
	svgAnimateTag   = regexp.MustCompile(`(?is)</?\s*(?:[^\s<>/"'=:]+:)?(?:animate|animateTransform|animateMotion|animateColor|set)\b[^>]*>`)
	svgEventAttr    = regexp.MustCompile(`(?is)\son[a-z]+\s*=\s*(?:"[^"]*"|'[^']*'|[^\s>]+)`)
	svgHrefAttr     = regexp.MustCompile(`(?is)\s(?:xlink:)?href\s*=\s*(?:"[^"]*"|'[^']*'|[^\s>]+)`)

	// CSS goes entirely: <style> elements and style="" attributes alike.
	//
	// This is deliberately blunt, and it is the lesson of four review rounds
	// on this function. Each round closed one route a stylesheet uses to
	// reference something external, and each time a different one was still
	// open — first `url(https://…)`, then `url(data:image/svg+xml,…)` in a
	// style attribute, then a bare-string `@import "https://…"` which is not
	// url()-shaped at all and so matched nothing. `image-set("a.png" 1x)` is
	// the next one along, and CSS will keep adding more.
	//
	// The common factor is that CSS is a second language with its own
	// grammar, its own escaping rules and its own set of ways to name a
	// remote resource, being scanned with regexes that only understand the
	// shapes someone thought of. Rather than enumerate them forever, the
	// stored icon simply does not get a stylesheet: a product suggestion is a
	// small static picture, and presentation attributes (fill, stroke,
	// opacity) express everything one needs without a CSS parser in the loop.
	//
	// The cost is real and worth stating: an icon that set its colours in a
	// <style> block renders in the default colours instead. That is a visual
	// downgrade on a thumbnail, against closing a class of bypass that has
	// produced a finding in every round it was left open.
	svgStyleBlock = regexp.MustCompile(`(?is)<\s*(?:[^\s<>/"'=:]+:)?style\b[^>]*>.*?</\s*(?:[^\s<>/"'=:]+:)?style\s*>`)
	svgStyleTag   = regexp.MustCompile(`(?is)</?\s*(?:[^\s<>/"'=:]+:)?style\b[^>]*>`)
	svgStyleAttr  = regexp.MustCompile(`(?is)\sstyle\s*=\s*(?:"[^"]*"|'[^']*'|[^\s>]+)`)

	// url() survives as a *presentation attribute* value — `fill="url(#grad)"`
	// is how gradients and masks are actually referenced, and that is not CSS
	// parsing, just an attribute whose value happens to use the url token.
	// The allow-list below still governs it.
	// The unquoted branch consumes `\)` as part of the token rather than
	// stopping at it. CSS's "consume a url token" does the same, and the
	// difference is not cosmetic: matching only as far as the first bare `)`
	// left the tail — `…A\)https://tracker.example/x.png)` — sitting in the
	// output as trailing text after the neutralised url(). Harmless to a
	// browser, since the result is not a valid value, but the whole point of
	// this function is that a remote address does not survive it.
	svgCSSURL     = regexp.MustCompile(`(?is)url\(\s*(?:"[^"]*"|'[^']*'|(?:\\.|[^)\\])*)\s*\)`)
	svgEntityDecl = regexp.MustCompile(`(?is)<!ENTITY\b[^>]*>`)
)

// stripRemoteRefs removes every href/xlink:href that could reach the network or
// execute, keeping only the two forms that cannot.
//
// This is an allow-list rather than a pattern of known-bad schemes, and
// deliberately so: a deny-list has to enumerate every scheme a browser might
// honour now or later, and the one it forgets is the one that ships. Go's
// regexp is RE2 and has no negative lookahead, which rules out expressing
// "data: but not data:image/" as a pattern anyway — doing the decision in code
// is both possible and easier to check by eye.
func stripRemoteRefs(in []byte) []byte {
	return svgHrefAttr.ReplaceAllFunc(in, func(attr []byte) []byte {
		value := strings.ToLower(strings.TrimSpace(attrValue(string(attr))))

		switch {
		// An inline *raster* image is self-contained: it reaches no network
		// and executes nothing, so stripping it would break legitimate icons
		// for no gain.
		//
		// The media type is checked against a fixed list rather than by the
		// "data:image/" prefix, and that distinction is the whole finding:
		// `data:image/svg+xml;base64,…` is an image by that prefix and a
		// complete document with its own `onload=` once decoded. None of the
		// patterns in this file can see it, because after base64 the
		// dangerous substrings are not present in any literal form. Decoding
		// and recursively sanitizing would be the other way out; refusing a
		// nested document instead is smaller, and an icon that needs one
		// embedded inside an attribute is not an icon worth keeping.
		case isInlineRasterImage(value):
			return attr
		// A fragment points inside this same document — how <use> and gradient
		// references work, which most real icons rely on.
		case strings.HasPrefix(value, "#"):
			return attr
		default:
			return nil
		}
	})
}

// stripUnsafeCSSURLs neutralises every CSS url() that is not provably inert.
//
// It applies the same allow-list as stripRemoteRefs, for the same reason: CSS
// can load a document too. `mask`, `fill`, `clip-path`, `filter` and
// `background` all take a url(), and a browser will honour a data: URI there
// exactly as it would in an href.
//
// An internal fragment is kept because it is how gradients, masks and <use>
// actually work — stripping `url(#grad)` would break the majority of real
// icons while protecting nothing, since it names something inside this same
// already-sanitized document.
func stripUnsafeCSSURLs(in []byte) []byte {
	return svgCSSURL.ReplaceAllFunc(in, func(match []byte) []byte {
		value := strings.ToLower(cssURLValue(string(match)))

		// A backslash means CSS escaping, and this code does not implement the
		// "consume an escaped code point" algorithm. An unquoted url() may
		// carry a literal `)` as `\)`, so a spec-compliant parser keeps reading
		// past where this one stops — meaning the classification would be made
		// against a truncated prefix while the untruncated bytes are what get
		// written back. Refusing anything escaped keeps the decision and the
		// data the same thing.
		if strings.ContainsAny(value, `\`) {
			return []byte("none")
		}

		if strings.HasPrefix(value, "#") || isInlineRasterImage(value) {
			return match
		}
		// `none` rather than deletion: these appear as property values, and
		// removing one outright leaves `mask:;` — valid enough that browsers
		// shrug, but harder to read when debugging a stored icon.
		return []byte("none")
	})
}

// cssURLValue pulls the target out of a `url( "…" )` match, tolerating either
// quote style and the unquoted form.
func cssURLValue(match string) string {
	open := strings.Index(match, "(")
	if open < 0 {
		return ""
	}
	inner := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(match[open+1:]), ")"))

	if len(inner) >= 2 && (inner[0] == '"' || inner[0] == '\'') {
		if end := strings.IndexByte(inner[1:], inner[0]); end >= 0 {
			inner = inner[1 : 1+end]
		}
	}
	return strings.TrimSpace(inner)
}

// inlineRasterTypes are the data: media types that cannot carry a document.
//
// Every one of these decodes to pixels. Anything XML-shaped — svg+xml, and
// any other `+xml` type a browser might learn to render — decodes to markup
// with its own script and event handlers, which is the thing this file exists
// to remove.
var inlineRasterTypes = []string{
	"data:image/png",
	"data:image/jpeg",
	"data:image/jpg",
	"data:image/gif",
	"data:image/webp",
	"data:image/bmp",
}

// isInlineRasterImage reports whether value is a data: URI holding a raster
// image and nothing else.
func isInlineRasterImage(value string) bool {
	for _, prefix := range inlineRasterTypes {
		if !strings.HasPrefix(value, prefix) {
			continue
		}
		// The next character must end the media type, so that
		// "data:image/png+xml" or "data:image/pngx" cannot pass by sharing a
		// prefix with a type that is allowed.
		rest := value[len(prefix):]
		if rest == "" || rest[0] == ';' || rest[0] == ',' {
			return true
		}
	}
	return false
}

// attrValue pulls the value out of an `attr="value"` match, tolerating single
// quotes and the unquoted form.
func attrValue(attr string) string {
	_, rest, found := strings.Cut(attr, "=")
	if !found {
		return ""
	}
	rest = strings.TrimSpace(rest)
	if len(rest) >= 2 && (rest[0] == '"' || rest[0] == '\'') {
		if end := strings.IndexByte(rest[1:], rest[0]); end >= 0 {
			return rest[1 : 1+end]
		}
	}
	return rest
}

// SanitizeSVG strips the executable and remote-loading parts of an SVG.
//
// A document left with nothing recognisable as SVG afterwards is treated as
// unusable rather than stored empty: an icon that sanitizes down to nothing was
// never an icon.
func SanitizeSVG(data []byte) ([]byte, error) {
	if len(data) > MaxDownloadBytes {
		return nil, fmt.Errorf("%w: svg too large", ErrUnusable)
	}

	out := data
	// Entity declarations first: they are the billion-laughs vector, and they
	// can also smuggle a payload that only becomes a <script> after expansion,
	// which the later patterns would not see.
	out = svgEntityDecl.ReplaceAll(out, nil)
	out = svgScriptBlock.ReplaceAll(out, nil)
	out = svgScriptTag.ReplaceAll(out, nil)
	out = svgForeignBlock.ReplaceAll(out, nil)
	out = svgForeignTag.ReplaceAll(out, nil)
	out = svgAnimateBlock.ReplaceAll(out, nil)
	out = svgAnimateTag.ReplaceAll(out, nil)
	out = svgEventAttr.ReplaceAll(out, nil)

	// CSS first, so that anything the url() allow-list would otherwise have to
	// reason about inside a stylesheet is simply gone by the time it runs.
	out = svgStyleBlock.ReplaceAll(out, nil)
	out = svgStyleTag.ReplaceAll(out, nil)
	out = svgStyleAttr.ReplaceAll(out, nil)

	out = stripRemoteRefs(out)
	out = stripUnsafeCSSURLs(out)

	if !bytes.Contains(bytes.ToLower(out), []byte("<svg")) {
		return nil, fmt.Errorf("%w: nothing left after sanitizing", ErrUnusable)
	}
	return out, nil
}
