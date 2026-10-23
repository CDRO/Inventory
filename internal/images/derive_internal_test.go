package images

import (
	"image"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestBoxShrinkAveragesEachBlock — a 4×4 4:2:0 picture shrunk by 2 is the
// per-block mean of the luma plane, and the chroma plane — one sample per
// 2×2 block in 4:2:0 — comes through unchanged into the 4:4:4 result.
func TestBoxShrinkAveragesEachBlock(t *testing.T) {
	t.Parallel()

	src := image.NewYCbCr(image.Rect(0, 0, 4, 4), image.YCbCrSubsampleRatio420)
	// Luma: the top-left block is 0,10,20,30 (mean 15), the top-right block is
	// all 100, the bottom-left all 200, the bottom-right 250,250,250,254
	// (mean 251).
	for y := 0; y < 4; y++ {
		for x := 0; x < 4; x++ {
			var v uint8
			switch {
			case x < 2 && y < 2:
				v = uint8(10 * (y*2 + x))
			case x >= 2 && y < 2:
				v = 100
			case x < 2:
				v = 200
			default:
				v = 250
			}
			src.Y[src.YOffset(x, y)] = v
		}
	}
	src.Y[src.YOffset(3, 3)] = 254
	// Chroma, one sample per block.
	for i, cb := range []uint8{40, 80, 120, 160} {
		src.Cb[i] = cb
		src.Cr[i] = 255 - cb
	}

	dst := boxShrink(src, 2)

	assert.Equal(t, image.Rect(0, 0, 2, 2), dst.Bounds())
	assert.Equal(t, image.YCbCrSubsampleRatio444, dst.SubsampleRatio)
	assert.Equal(t, uint8(15), dst.Y[dst.YOffset(0, 0)])
	assert.Equal(t, uint8(100), dst.Y[dst.YOffset(1, 0)])
	assert.Equal(t, uint8(200), dst.Y[dst.YOffset(0, 1)])
	assert.Equal(t, uint8(251), dst.Y[dst.YOffset(1, 1)])
	assert.Equal(t, uint8(40), dst.Cb[dst.COffset(0, 0)])
	assert.Equal(t, uint8(160), dst.Cb[dst.COffset(1, 1)])
	assert.Equal(t, uint8(255-80), dst.Cr[dst.COffset(1, 0)])
}

// TestPreShrinkFactorLeavesHeadroomForEveryVariant — the factor is bounded by
// whichever variant needs the most source: the preview along the long edge,
// the largest thumbnail along the short one, each with its margin.
func TestPreShrinkFactorLeavesHeadroomForEveryVariant(t *testing.T) {
	t.Parallel()

	assert.Equal(t, 3, preShrinkFactor(8660, 5773), "50 MP: 8660/2400 = 3.6, 5773/1152 = 5.0")
	assert.Equal(t, 3, preShrinkFactor(5773, 8660), "orientation does not matter")
	assert.Equal(t, 2, preShrinkFactor(4800, 2400))
	assert.Equal(t, 1, preShrinkFactor(4000, 3000), "12 MP: 4000/2400 is below 2, so no pre-shrink")
	assert.Equal(t, 1, preShrinkFactor(1024, 768))
	assert.Equal(t, 2, preShrinkFactor(9000, 2304), "a panorama is bounded by its short side: 2304/1152 = 2")
}
