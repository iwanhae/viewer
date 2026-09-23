package vision

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"testing"

	"viewer/internal/textenc"
)

// Tolerances measured against the PyTorch reference bundle. The tower tracks
// float32 PyTorch closely; the preprocessor differs only in resampler rounding,
// which is worth at most one 8-bit level per channel.
const (
	// embeddingTolerance is the largest per-component deviation accepted from
	// the reference embedding.
	embeddingTolerance = 1e-4
	// pixelTolerance accepts one 8-bit level of resampling rounding, expressed
	// on the [-1, 1] scale the tower consumes (2/255).
	pixelTolerance = 2.0 / 127.5
	// minCosine guards against a systematically wrong embedding even if every
	// individual component stayed within tolerance.
	minCosine = 0.9995
)

// referenceCases are the images the reference bundle was generated from.
var referenceCases = []string{"1000x300", "640x480", "13x7"}

// TestAgainstReference validates this package against an independent PyTorch
// implementation of the same checkpoint. It is skipped unless
// VISION_REFERENCE_DIR points at a bundle holding:
//
//   - model/                   config.json plus model.safetensors
//   - vision_reference.json    embeddings produced by the reference
//   - refpix_rtf_<case>.bin    reference pixels in [-1, 1], channel-major
//   - refpixels_<case>.bin     pixels the reference fed its own tower
//   - refimg_<case>.png        the source images
//
// The bundle is produced by scripts/vision-reference; the test is deliberately
// opt-in because it needs a 1.5 GB checkpoint.
func TestAgainstReference(t *testing.T) {
	dir := os.Getenv("VISION_REFERENCE_DIR")
	if dir == "" {
		t.Skip("VISION_REFERENCE_DIR is not set")
	}

	// Pixel comparison: the resampler is validated against Pillow, which the
	// reference uses to apply the same resize-to-fill geometry.
	t.Run("preprocess", func(t *testing.T) {
		for _, label := range referenceCases {
			source, err := os.ReadFile(fmt.Sprintf("%s/refimg_%s.png", dir, label))
			if err != nil {
				t.Fatalf("read source image %s: %v", label, err)
			}
			got, err := Preprocess(source, InputImageSize)
			if err != nil {
				t.Fatalf("preprocess %s: %v", label, err)
			}
			want := readReferencePixels(t, fmt.Sprintf("%s/refpix_rtf_%s.bin", dir, label))
			if len(got.Pixels) != len(want) {
				t.Fatalf("preprocess %s: got %d pixels, want %d", label, len(got.Pixels), len(want))
			}

			var maxAbs, sum float64
			for i := range want {
				diff := math.Abs(float64(got.Pixels[i] - want[i]))
				maxAbs = math.Max(maxAbs, diff)
				sum += diff
			}
			meanAbs := sum / float64(len(want))
			t.Logf("preprocess %s: max_abs=%g mean_abs=%g", label, maxAbs, meanAbs)
			if maxAbs > pixelTolerance {
				t.Errorf("preprocess %s: max_abs=%g exceeds %g", label, maxAbs, pixelTolerance)
			}
		}
	})

	// Embedding comparison: the tower is fed the reference's own pixels so its
	// numerics are checked independently of the resampler.
	t.Run("embedding", func(t *testing.T) {
		reference := loadReferenceEmbeddings(t, dir)
		model, err := Load(context.Background(), Config{ModelID: dir + "/model", Backend: "go"})
		if err != nil {
			t.Fatalf("load model: %v", err)
		}
		t.Log(model.Describe())

		for _, label := range referenceCases {
			want, ok := reference[label]
			if !ok {
				t.Fatalf("reference has no embedding for %s", label)
			}
			image := Image{Pixels: readReferencePixels(t, fmt.Sprintf("%s/refpixels_%s.bin", dir, label))}
			got, err := model.Embed(context.Background(), image)
			if err != nil {
				t.Fatalf("embed %s: %v", label, err)
			}
			if len(got) != len(want) {
				t.Fatalf("embed %s: got %d values, want %d", label, len(got), len(want))
			}

			cosine, maxAbs := compareVectors(got, want)
			t.Logf("embed %s: max_abs_diff=%g cosine=%.8f", label, maxAbs, cosine)
			if maxAbs > embeddingTolerance {
				t.Errorf("embed %s: max_abs_diff=%g exceeds %g\n  got  =%v\n  want =%v",
					label, maxAbs, embeddingTolerance, got[:4], want[:4])
			}
			if cosine < minCosine {
				t.Errorf("embed %s: cosine=%.8f below %g", label, cosine, minCosine)
			}
		}
	})
}

// textCases are the texts the reference bundle's text half was generated from;
// they mirror TEXT_CASES in scripts/vision-reference/generate.py.
var textCases = []string{"english", "korean", "mixed", "emoji", "empty", "long"}

// referenceTextCase is one entry of text_reference.json: the case text, the
// ids and mask the reference tokenizer produced, and the text tower's
// embedding for that exact token sequence.
type referenceTextCase struct {
	Text      string    `json:"text"`
	IDs       []int32   `json:"ids"`
	Mask      []float32 `json:"mask"`
	Embedding []float32 `json:"embedding"`
}

// loadReferenceText reads the reference's expected text embeddings.
func loadReferenceText(t *testing.T, dir string) map[string]referenceTextCase {
	t.Helper()
	raw, err := os.ReadFile(dir + "/text_reference.json")
	if err != nil {
		t.Fatalf("read text reference: %v", err)
	}
	var decoded struct {
		Text map[string]referenceTextCase `json:"text"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("parse text reference: %v", err)
	}
	if len(decoded.Text) == 0 {
		t.Fatalf("text reference has no cases")
	}
	return decoded.Text
}

// TestTextAgainstReference validates the text tower against an independent
// PyTorch implementation of the same checkpoint. It shares TestAgainstReference's
// bundle and opt-in behaviour, and its two subtests per case separate the two
// things that could be wrong: "tower" feeds the reference's own ids and mask
// through EmbedTokens so only the GoMLX graph is measured, while "endtoend"
// feeds the raw text through EmbedText, adding the tokenizer to the measured
// path.
func TestTextAgainstReference(t *testing.T) {
	dir := os.Getenv("VISION_REFERENCE_DIR")
	if dir == "" {
		t.Skip("VISION_REFERENCE_DIR is not set")
	}
	reference := loadReferenceText(t, dir)
	model, err := Load(context.Background(), Config{ModelID: dir + "/model", Backend: "go"})
	if err != nil {
		t.Fatalf("load model: %v", err)
	}
	t.Log(model.Describe())

	for _, label := range textCases {
		want, ok := reference[label]
		if !ok {
			t.Fatalf("reference has no text embedding for %s", label)
		}
		if len(want.IDs) != textenc.MaxLen || len(want.Mask) != textenc.MaxLen {
			t.Fatalf("text case %s: reference has %d ids and %d mask values, want %d",
				label, len(want.IDs), len(want.Mask), textenc.MaxLen)
		}
		if len(want.Embedding) == 0 {
			t.Fatalf("text case %s: reference has an empty embedding", label)
		}

		t.Run("tower/"+label, func(t *testing.T) {
			got, err := model.EmbedTokens(context.Background(), want.IDs, want.Mask)
			if err != nil {
				t.Fatalf("embed tokens %s: %v", label, err)
			}
			checkTextEmbedding(t, label, got, want.Embedding)
		})

		t.Run("endtoend/"+label, func(t *testing.T) {
			got, err := model.EmbedText(context.Background(), want.Text)
			if err != nil {
				t.Fatalf("embed text %s: %v", label, err)
			}
			checkTextEmbedding(t, label, got, want.Embedding)
		})
	}
}

// checkTextEmbedding applies the shared tolerances to one text embedding.
func checkTextEmbedding(t *testing.T, label string, got, want []float32) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("embed text %s: got %d values, want %d", label, len(got), len(want))
	}
	cosine, maxAbs := compareVectors(got, want)
	t.Logf("embed text %s: max_abs_diff=%g cosine=%.8f", label, maxAbs, cosine)
	if maxAbs > embeddingTolerance {
		t.Errorf("embed text %s: max_abs_diff=%g exceeds %g\n  got  =%v\n  want =%v",
			label, maxAbs, embeddingTolerance, got[:4], want[:4])
	}
	if cosine < minCosine {
		t.Errorf("embed text %s: cosine=%.8f below %g", label, cosine, minCosine)
	}
}

func compareVectors(got, want []float32) (cosine, maxAbs float64) {
	var dot, normGot, normWant float64
	for i := range want {
		g, w := float64(got[i]), float64(want[i])
		maxAbs = math.Max(maxAbs, math.Abs(g-w))
		dot += g * w
		normGot += g * g
		normWant += w * w
	}
	if normGot == 0 || normWant == 0 {
		return 0, maxAbs
	}
	return dot / (math.Sqrt(normGot) * math.Sqrt(normWant)), maxAbs
}

// readReferencePixels reads one little-endian float32 blob.
func readReferencePixels(t *testing.T, path string) []float32 {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if len(raw)%4 != 0 {
		t.Fatalf("read %s: %d bytes is not a whole number of float32 values", path, len(raw))
	}
	pixels := make([]float32, len(raw)/4)
	for i := range pixels {
		pixels[i] = math.Float32frombits(binary.LittleEndian.Uint32(raw[i*4:]))
	}
	return pixels
}

// loadReferenceEmbeddings reads the reference's expected embeddings.
func loadReferenceEmbeddings(t *testing.T, dir string) map[string][]float32 {
	t.Helper()
	raw, err := os.ReadFile(dir + "/vision_reference.json")
	if err != nil {
		t.Fatalf("read reference: %v", err)
	}
	var decoded struct {
		Embedding map[string][]float32 `json:"embedding"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("parse reference: %v", err)
	}
	if len(decoded.Embedding) == 0 {
		t.Fatalf("reference has no embeddings")
	}
	return decoded.Embedding
}
