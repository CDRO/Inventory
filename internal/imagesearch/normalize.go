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
	svgScriptBlock    = regexp.MustCompile(`(?is)<\s*script\b[^>]*>.*?</\s*script\s*>`)
	svgScriptTag      = regexp.MustCompile(`(?is)</?\s*script\b[^>]*>`)
	svgForeignBlock   = regexp.MustCompile(`(?is)<\s*foreignObject\b[^>]*>.*?</\s*foreignObject\s*>`)
	svgForeignTag     = regexp.MustCompile(`(?is)</?\s*foreignObject\b[^>]*>`)

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
	svgAnimateBlock = regexp.MustCompile(`(?is)<\s*(?:animate|animateTransform|animateMotion|animateColor|set)\b[^>]*>.*?</\s*(?:animate|animateTransform|animateMotion|animateColor|set)\s*>`)
	svgAnimateTag   = regexp.MustCompile(`(?is)</?\s*(?:animate|animateTransform|animateMotion|animateColor|set)\b[^>]*>`)
	svgEventAttr      = regexp.MustCompile(`(?is)\son[a-z]+\s*=\s*(?:"[^"]*"|'[^']*'|[^\s>]+)`)
	svgHrefAttr       = regexp.MustCompile(`(?is)\s(?:xlink:)?href\s*=\s*(?:"[^"]*"|'[^']*'|[^\s>]+)`)
	svgCSSExternalURL = regexp.MustCompile(`(?is)url\(\s*['"]?\s*(?:https?:|//|javascript:)[^)]*\)`)
	svgEntityDecl     = regexp.MustCompile(`(?is)<!ENTITY\b[^>]*>`)
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
		// An inline image is self-contained: it reaches no network and
		// executes nothing, so stripping it would break legitimate icons for
		// no gain.
		case strings.HasPrefix(value, "data:image/"):
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
	out = stripRemoteRefs(out)
	out = svgCSSExternalURL.ReplaceAll(out, []byte("none"))

	if !bytes.Contains(bytes.ToLower(out), []byte("<svg")) {
		return nil, fmt.Errorf("%w: nothing left after sanitizing", ErrUnusable)
	}
	return out, nil
}
