package vision

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"testing"
	"time"

	"github.com/CDRO/Inventory/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"
)

// requireLiveGemini skips the test unless a real key is available, so the
// suite stays hermetic by default and only reaches the network when someone
// deliberately asks it to.
func requireLiveGemini(t *testing.T) (key, model string) {
	t.Helper()

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

	// NoError and Len(1) are the two assertions carrying live signal: they
	// depend on what the model actually saw. Everything else about a
	// ModeProduct item — a non-empty label, quantity >= 1, confidence in
	// [0, 1], no box, no path — is guaranteed by ParseAnalysis's own
	// construction once those two hold, real response or not, so asserting
	// them again here would test the parser a second time, not Gemini.
	require.NoError(t, err)
	require.Len(t, analysis.Items, 1, "ModeProduct must return exactly one item")
}
