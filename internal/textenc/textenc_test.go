package textenc

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The synthetic vocabulary in testdata/tokenizer-mini.json spells "cat" and
// "bat" from the letters c/a/t/b, marks word starts with "▁" and merges
// ["▁","cat"] into "▁cat", so a handful of short sentences exercise every
// pipeline stage with ids that can be worked out on paper.
const (
	miniCat        = 12 // "cat"
	miniSpaceMark  = 4  // "▁", the normalized form of a space
	miniTon        = 16 // "ton", built through the t→to→ton chain
	miniBat        = 14 // "bat"
	miniSpaceCat   = 17 // "▁cat", from the ["▁","cat"] merge
	miniUnknownRun = 3  // "<unk>"
)

// TestSyntheticEncode checks the invariants Encode guarantees — exact length,
// trailing <eos>, padding and mask agreement — against a committed
// mini-tokenizer, so `make test` needs no checkpoint download. The expected
// ids were derived by hand and cross-checked against Hugging Face's
// `tokenizers` library reading the same file.
func TestSyntheticEncode(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "tokenizer-mini.json"))
	if err != nil {
		t.Fatalf("read mini tokenizer: %v", err)
	}
	tok, err := LoadFromBytes(raw)
	if err != nil {
		t.Fatalf("load mini tokenizer: %v", err)
	}

	// "cat bat" normalizes to the single word "cat▁bat"; the merges c→ca,
	// ca→cat, b→ba, ba→bat leave the "▁" unmerged because nothing merges
	// with it, giving cat ▁ bat <eos>.
	cases := []struct {
		label string
		text  string
		want  []int32
	}{
		{"two words", "cat bat", []int32{miniCat, miniSpaceMark, miniBat, eosID}},
		// A leading space survives normalization as "▁" and then merges
		// with cat, which is why " cat" is one token, not three.
		{"leading space", " cat", []int32{miniSpaceCat, eosID}},
		{"chained merges", "ton", []int32{miniTon, eosID}},
		// The mini tokenizer has no byte-fallback, so unknown runes each
		// become one <unk> — the same behaviour the real tokenizer shows
		// for out-of-vocabulary characters.
		{"unknown runes", "xyz", []int32{miniUnknownRun, miniUnknownRun, miniUnknownRun, eosID}},
		// Empty text still ends up with <eos> as its only real token, at
		// position 0, which is what the tower sees for an empty caption.
		{"empty", "", []int32{eosID}},
	}

	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			ids, mask, err := tok.Encode(tc.text)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			wantIDs := padWith(t, tc.want, PadID)
			wantMask := padWith(t, ones(t, len(tc.want)), 0)
			if diff := diffSlices(wantIDs, ids); diff != "" {
				t.Errorf("ids wrong: %s", diff)
			}
			if diff := diffSlices(wantMask, mask); diff != "" {
				t.Errorf("mask wrong: %s", diff)
			}
		})
	}

	t.Run("truncation keeps eos last", func(t *testing.T) {
		// 25 repetitions tokenize to more than 63 content tokens (each one
		// contributes cat ▁ bat ▁cat), so the input must be cut and still
		// end with <eos> at position MaxLen-1 with a full mask.
		text := strings.Repeat("cat bat ", 25)
		ids, mask, err := tok.Encode(text)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		wantIDs := make([]int32, 0, MaxLen)
		for i := 0; len(wantIDs) < MaxLen-1; i++ {
			switch i % 3 {
			case 0:
				// The first "cat" has no "▁" in front; every later one
				// merged into "▁cat".
				if i == 0 {
					wantIDs = append(wantIDs, miniCat)
				} else {
					wantIDs = append(wantIDs, miniSpaceCat)
				}
			case 1:
				wantIDs = append(wantIDs, miniSpaceMark)
			default:
				wantIDs = append(wantIDs, miniBat)
			}
		}
		wantIDs = append(wantIDs, eosID)

		if got := len(ids); got != MaxLen {
			t.Fatalf("len(ids) = %d, want %d", got, MaxLen)
		}
		if diff := diffSlices(wantIDs, ids); diff != "" {
			t.Errorf("ids wrong: %s", diff)
		}
		for i, m := range mask {
			if m != 1 {
				t.Errorf("mask[%d] = %v, want 1 (no padding after truncation)", i, m)
			}
		}
	})
}

// fixtureCase mirrors one entry of testdata/cases.json.
type fixtureCase struct {
	Label string    `json:"label"`
	Text  string    `json:"text"`
	IDs   []int32   `json:"ids"`
	Mask  []float32 `json:"mask"`
}

// loadFixtureCases reads the committed fixture outputs, which were generated
// once by an Hugging Face `tokenizers` script from the real SigLIP2
// tokenizer.json.
func loadFixtureCases(t *testing.T) []fixtureCase {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "cases.json"))
	if os.IsNotExist(err) {
		t.Skip("testdata/cases.json is not present")
	}
	if err != nil {
		t.Fatalf("read cases.json: %v", err)
	}
	var cases []fixtureCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatalf("parse cases.json: %v", err)
	}
	if len(cases) == 0 {
		t.Fatalf("cases.json holds no cases")
	}
	return cases
}

// TestFixtureCasesShape validates the committed fixture file itself: every
// case must be a well-formed Encode result — 64 ids, 64 mask values, <eos> as
// the last real token and nothing but padding after it. The check needs no
// tokenizer, so it runs in `make test` and catches a corrupt or truncated
// fixture long before any model is available.
func TestFixtureCasesShape(t *testing.T) {
	for _, tc := range loadFixtureCases(t) {
		t.Run(tc.Label, func(t *testing.T) {
			if len(tc.IDs) != MaxLen {
				t.Fatalf("len(ids) = %d, want %d", len(tc.IDs), MaxLen)
			}
			if len(tc.Mask) != MaxLen {
				t.Fatalf("len(mask) = %d, want %d", len(tc.Mask), MaxLen)
			}
			real := 0
			for i, m := range tc.Mask {
				if m != 0 && m != 1 {
					t.Fatalf("mask[%d] = %v, want only 0 or 1", i, m)
				}
				real += int(m)
			}
			if tc.IDs[real-1] != eosID {
				t.Errorf("ids[%d] = %d, want %d (<eos> as last real token)", real-1, tc.IDs[real-1], eosID)
			}
			for i := real; i < MaxLen; i++ {
				if tc.IDs[i] != PadID {
					t.Errorf("ids[%d] = %d, want %d (<pad>)", i, tc.IDs[i], PadID)
				}
			}
		})
	}
}

// TestAgainstRealTokenizer validates this package against fixture ids
// produced by Hugging Face's own `tokenizers` library. It is deliberately
// opt-in because it needs the 34 MB tokenizer.json of
// google/siglip2-base-patch16-224: run it with TEXTENC_TOKENIZER_JSON
// pointing at that file (and testdata/cases.json present, which it is in the
// repository). The same pattern as the vision package's VISION_REFERENCE_DIR
// check.
func TestAgainstRealTokenizer(t *testing.T) {
	path := os.Getenv("TEXTENC_TOKENIZER_JSON")
	if path == "" {
		t.Skip("TEXTENC_TOKENIZER_JSON is not set")
	}
	cases := loadFixtureCases(t)

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read tokenizer.json: %v", err)
	}
	tok, err := LoadFromBytes(raw)
	if err != nil {
		t.Fatalf("load tokenizer.json: %v", err)
	}

	for _, tc := range cases {
		t.Run(tc.Label, func(t *testing.T) {
			ids, mask, err := tok.Encode(tc.Text)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			if diff := diffSlices(tc.IDs, ids); diff != "" {
				t.Errorf("ids wrong: %s", diff)
			}
			if diff := diffSlices(tc.Mask, mask); diff != "" {
				t.Errorf("mask wrong: %s", diff)
			}
			real := 0
			for _, m := range mask {
				real += int(m)
			}
			t.Logf("real=%d last_real_id=%d", real, ids[real-1])
		})
	}
}

// padWith extends want to MaxLen by filling with fill, producing the full
// expected output of an Encode from its leading real tokens.
func padWith[T int32 | float32](t *testing.T, want []T, fill T) []T {
	t.Helper()
	if len(want) > MaxLen {
		t.Fatalf("expected output has %d entries, more than MaxLen", len(want))
	}
	out := make([]T, MaxLen)
	copy(out, want)
	for i := len(want); i < MaxLen; i++ {
		out[i] = fill
	}
	return out
}

// ones builds n mask entries of 1.
func ones(t *testing.T, n int) []float32 {
	t.Helper()
	out := make([]float32, n)
	for i := range out {
		out[i] = 1
	}
	return out
}

// diffSlices describes the first places two equal-length slices differ, so a
// failure shows the divergence instead of two 64-wide blobs.
func diffSlices[T comparable](want, got []T) string {
	if len(want) != len(got) {
		return fmt.Sprintf("len = %d, want %d", len(got), len(want))
	}
	var b strings.Builder
	for i := range want {
		if want[i] != got[i] {
			fmt.Fprintf(&b, "\n  [%d] got %v, want %v", i, got[i], want[i])
			if b.Len() > 400 {
				break
			}
		}
	}
	if b.Len() == 0 {
		return ""
	}
	return "differences (got vs want):" + b.String()
}
