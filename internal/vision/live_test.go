package vision

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/CDRO/Inventory/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"
)

// requireLiveGemini skips the test unless it has been explicitly asked to run
// the real Gemini API. That ask is GEMINI_LIVE_TEST=1, deliberately separate
// from GEMINI_API_KEY: the key is ordinary app config, present in any
// environment where the vision feature actually works, and `go test ./...`
// must not spend a paid API call just because that key happens to be set —
// only when someone deliberately opts in to exercising this specific test.
func requireLiveGemini(t *testing.T) (key, model string) {
	t.Helper()

	if os.Getenv("GEMINI_LIVE_TEST") != "1" {
		t.Skip("GEMINI_LIVE_TEST not set to 1; go test ./... does not reach the live Gemini API by default")
	}
	key = os.Getenv("GEMINI_API_KEY")
	if key == "" {
		t.Skip("GEMINI_API_KEY not set; export it to exercise the real Gemini API")
	}
	model = os.Getenv("GEMINI_MODEL")
	if model == "" {
		model = config.DefaultGeminiModel
	}
	return key, model
}

// labeledProductPhoto renders a plain package label as a PNG, so the live
// test has something a vision model can actually read without shipping a
// binary fixture into the repo.
func labeledProductPhoto(t *testing.T, text string) []byte {
	t.Helper()

	img := image.NewRGBA(image.Rect(0, 0, 320, 120))
	draw.Draw(img, img.Bounds(), &image.Uniform{C: color.White}, image.Point{}, draw.Src)

	d := &font.Drawer{
		Dst:  img,
		Src:  &image.Uniform{C: color.Black},
		Face: basicfont.Face7x13,
		Dot:  fixed.P(16, 60),
	}
	d.DrawString(text)

	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, img))
	return buf.Bytes()
}

// TestLiveListModelsSeesConfiguredModel proves the key is valid and the
// configured GEMINI_MODEL is actually one Gemini currently offers — the same
// check Status runs on every /healthz hit, against the real provider instead
// of a stub.
func TestLiveListModelsSeesConfiguredModel(t *testing.T) {
	key, model := requireLiveGemini(t)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	c := NewChecker(nil, NewAPILister(key), model)

	assert.Equal(t, StatusOK, c.Status(ctx))
}

// TestLiveAnalyzeProductHonoursContract sends a real, readable label through
// Analyze in ModeProduct and checks that the model actually found something:
// the call succeeds and returns exactly one item. It does not assert what the
// label says, or re-check fields ParseAnalysis already guarantees regardless
// of the response — pinning the model's exact wording would make the test
// flaky across model revisions for no safety gained.
func TestLiveAnalyzeProductHonoursContract(t *testing.T) {
	key, model := requireLiveGemini(t)
	photo := labeledProductPhoto(t, "ORGANIC WHOLE MILK 1L")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client := NewClient(key)
	analysis, err := client.Analyze(ctx, model, ModeProduct, photo, "image/png")
	if isProviderOutage(err) {
		t.Skipf("Gemini answered with an outage status, not a contract violation: %v", err)
	}

	// NoError and Len(1) are the two assertions carrying live signal: they
	// depend on what the model actually saw. Everything else about a
	// ModeProduct item — a non-empty label, quantity >= 1, confidence in
	// [0, 1], no box, no path — is guaranteed by ParseAnalysis's own
	// construction once those two hold, real response or not, so asserting
	// them again here would test the parser a second time, not Gemini.
	require.NoError(t, err)
	require.Len(t, analysis.Items, 1, "ModeProduct must return exactly one item")
}

// isProviderOutage reports whether err is the client-side symptom of a Gemini
// outage (429 or 5xx) rather than a contract violation in a response the
// provider actually returned successfully. generate (gemini.go) wraps any
// non-2xx, non-404 status as "vision: generateContent returned <resp.Status>",
// so the status line is still there to read back out of the error string
// without any change to production code.
func isProviderOutage(err error) bool {
	if err == nil {
		return false
	}
	const prefix = "generateContent returned "
	idx := strings.Index(err.Error(), prefix)
	if idx < 0 {
		return false
	}
	status := err.Error()[idx+len(prefix):]
	return strings.HasPrefix(status, "5") || strings.HasPrefix(status, "429")
}

// TestProviderOutageIsRecognizedFromRealErrors exercises both the skip branch
// and the hard-fail branch against a local fake Gemini endpoint, so the
// distinction is checked on every run instead of only during an actual
// outage. A 503 or 429 status is the client-side symptom of an outage and
// must be recognized as one; a 200 with a body that violates the contract is
// not an outage at all and must not be, however the two errors are wrapped by
// the same generate call.
func TestProviderOutageIsRecognizedFromRealErrors(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       string
		wantOutage bool
	}{
		{"503 service unavailable is an outage", http.StatusServiceUnavailable, `{}`, true},
		{"429 too many requests is an outage", http.StatusTooManyRequests, `{}`, true},
		{"500 internal server error is an outage", http.StatusInternalServerError, `{}`, true},
		{"200 with a contract-violating body is not an outage", http.StatusOK, `not json`, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.statusCode)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()

			client := &Client{APIKey: "test", Endpoint: srv.URL, HTTP: srv.Client()}
			_, err := client.Analyze(context.Background(), "gemini-test", ModeProduct, []byte("fake-image-bytes"), "image/png")

			require.Error(t, err)
			assert.Equal(t, tt.wantOutage, isProviderOutage(err), "err: %v", err)
		})
	}
}
