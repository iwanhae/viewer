package vision

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"math"

	// Image formats accepted by the ingest pipeline.
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"

	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"
)

// Preprocess decodes an image and prepares it for the vision tower: it is
// resized to a square (preserving the aspect ratio, so the image fills the
// target and the excess is cropped) and rescaled to [-1, 1].
//
// This mirrors the preprocessing the previous worker applied: a triangle
// (bilinear) resample followed by rescale with mean and std of 0.5.
func Preprocess(data []byte, imageSize int) (Image, error) {
	if imageSize <= 0 {
		imageSize = InputImageSize
	}
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return Image{}, fmt.Errorf("decode image: %w", err)
	}

	// "Resize to fill": scale so the shorter side covers the target, then
	// centre-crop the overflow.
	bounds := src.Bounds()
	srcW, srcH := bounds.Dx(), bounds.Dy()
	if srcW <= 0 || srcH <= 0 {
		return Image{}, fmt.Errorf("decode image: empty image %dx%d", srcW, srcH)
	}
	scale := math.Max(float64(imageSize)/float64(srcW), float64(imageSize)/float64(srcH))
	scaledW := max(int(math.Round(float64(srcW)*scale)), imageSize)
	scaledH := max(int(math.Round(float64(srcH)*scale)), imageSize)

	rgba := toRGBA(src)
	resized := resampleBilinear(rgba, scaledW, scaledH)

	offsetX := (scaledW - imageSize) / 2
	offsetY := (scaledH - imageSize) / 2

	pixels := make([]float32, imageChannels*imageSize*imageSize)
	plane := imageSize * imageSize
	for y := 0; y < imageSize; y++ {
		srcRow := (y + offsetY) * resized.Stride
		dstRow := y * imageSize
		for x := 0; x < imageSize; x++ {
			srcOffset := srcRow + (x+offsetX)*4
			dstOffset := dstRow + x
			// Rescale to [0, 1] and then normalise with mean=std=0.5, which
			// maps [0, 1] onto [-1, 1].
			pixels[dstOffset] = float32(resized.Pix[srcOffset])/127.5 - 1
			pixels[plane+dstOffset] = float32(resized.Pix[srcOffset+1])/127.5 - 1
			pixels[2*plane+dstOffset] = float32(resized.Pix[srcOffset+2])/127.5 - 1
		}
	}
	return Image{Pixels: pixels}, nil
}

// toRGBA converts any decoded image into an 8-bit RGBA canvas.
func toRGBA(src image.Image) *image.RGBA {
	bounds := src.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	switch typed := src.(type) {
	case *image.RGBA:
		if typed.Rect == dst.Rect && typed.Stride == dst.Stride {
			copy(dst.Pix, typed.Pix)
			return dst
		}
	case *image.NRGBA:
		if typed.Rect == dst.Rect && typed.Stride == dst.Stride {
			// NRGBA is not premultiplied, so convert explicitly.
			for i := 0; i < len(typed.Pix); i += 4 {
				a := uint32(typed.Pix[i+3])
				r := uint32(typed.Pix[i]) * a / 0xff
				g := uint32(typed.Pix[i+1]) * a / 0xff
				b := uint32(typed.Pix[i+2]) * a / 0xff
				dst.Pix[i] = uint8(r)
				dst.Pix[i+1] = uint8(g)
				dst.Pix[i+2] = uint8(b)
				dst.Pix[i+3] = uint8(a)
			}
			return dst
		}
	}
	for y := 0; y < bounds.Dy(); y++ {
		for x := 0; x < bounds.Dx(); x++ {
			dst.Set(x, y, color.RGBAModel.Convert(src.At(bounds.Min.X+x, bounds.Min.Y+y)))
		}
	}
	return dst
}

// resampleBilinear resizes src to width x height using a separable triangle
// filter with an anti-aliasing support that widens as the image shrinks. This
// matches the resampling the previous Python/Pillow based preprocessing did.
func resampleBilinear(src *image.RGBA, width, height int) *image.RGBA {
	srcW, srcH := src.Rect.Dx(), src.Rect.Dy()
	if srcW == width && srcH == height {
		return src
	}

	// Horizontal pass: srcW x srcH -> width x srcH.
	horizontal := make([]float32, width*srcH*4)
	xContrib := buildContributions(srcW, width)
	for y := 0; y < srcH; y++ {
		srcRow := y * src.Stride
		dstRow := y * width * 4
		for x := 0; x < width; x++ {
			contrib := xContrib[x]
			var r, g, b, a float32
			for _, item := range contrib {
				offset := srcRow + item.index*4
				weight := item.weight
				r += float32(src.Pix[offset]) * weight
				g += float32(src.Pix[offset+1]) * weight
				b += float32(src.Pix[offset+2]) * weight
				a += float32(src.Pix[offset+3]) * weight
			}
			offset := dstRow + x*4
			horizontal[offset] = r
			horizontal[offset+1] = g
			horizontal[offset+2] = b
			horizontal[offset+3] = a
		}
	}

	// Vertical pass: width x srcH -> width x height.
	dst := image.NewRGBA(image.Rect(0, 0, width, height))
	yContrib := buildContributions(srcH, height)
	for y := 0; y < height; y++ {
		contrib := yContrib[y]
		dstRow := y * dst.Stride
		for x := 0; x < width; x++ {
			var r, g, b, a float32
			for _, item := range contrib {
				offset := (item.index*width + x) * 4
				weight := item.weight
				r += horizontal[offset] * weight
				g += horizontal[offset+1] * weight
				b += horizontal[offset+2] * weight
				a += horizontal[offset+3] * weight
			}
			offset := dstRow + x*4
			dst.Pix[offset] = clampByte(r)
			dst.Pix[offset+1] = clampByte(g)
			dst.Pix[offset+2] = clampByte(b)
			dst.Pix[offset+3] = clampByte(a)
		}
	}
	return dst
}

// contribution is one source index and its normalised weight.
type contribution struct {
	index  int
	weight float32
}

// buildContributions precomputes the filter weights mapping srcSize samples to
// dstSize output positions.
//
// The filter is a triangle (tent) whose support widens when the image is
// reduced, so every source sample contributes: this is the standard
// anti-aliased bilinear resample.
func buildContributions(srcSize, dstSize int) [][]contribution {
	if dstSize <= 0 || srcSize <= 0 {
		return nil
	}
	scale := float64(srcSize) / float64(dstSize)
	filterScale := math.Max(scale, 1)
	support := filterScale

	result := make([][]contribution, dstSize)
	for i := 0; i < dstSize; i++ {
		center := (float64(i)+0.5)*scale - 0.5
		left := int(math.Floor(center - support))
		right := int(math.Ceil(center + support))
		items := make([]contribution, 0, right-left+1)
		var total float64
		for j := left; j <= right; j++ {
			if j < 0 || j >= srcSize {
				continue
			}
			weight := triangle((float64(j) - center) / filterScale)
			if weight <= 0 {
				continue
			}
			items = append(items, contribution{index: j, weight: float32(weight)})
			total += weight
		}
		if total == 0 {
			// Fall back to nearest neighbour when the filter misses every
			// sample (can only happen for degenerate sizes).
			nearest := min(max(int(math.Round(center)), 0), srcSize-1)
			items = []contribution{{index: nearest, weight: 1}}
		} else if total != 1 {
			for k := range items {
				items[k].weight = float32(float64(items[k].weight) / total)
			}
		}
		result[i] = items
	}
	return result
}

// triangle is the linear (tent) filter, zero outside [-1, 1].
func triangle(x float64) float64 {
	if x < 0 {
		x = -x
	}
	if x >= 1 {
		return 0
	}
	return 1 - x
}

// clampByte rounds and clamps a float sample into an 8-bit channel value,
// matching Pillow's rounding behaviour.
func clampByte(value float32) uint8 {
	rounded := math.Round(float64(value))
	switch {
	case rounded <= 0:
		return 0
	case rounded >= 255:
		return 255
	default:
		return uint8(rounded)
	}
}
