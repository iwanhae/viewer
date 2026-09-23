package vision

import (
	"context"
	"strings"
	"testing"

	"viewer/internal/textenc"
)

// TestEmbedTokensRejectsWrongLengths checks the argument validation: the tower
// is built for exactly textenc.MaxLen positions, and a caller passing anything
// else must get an error rather than a graph misexecution.
func TestEmbedTokensRejectsWrongLengths(t *testing.T) {
	m := &Model{}
	ctx := context.Background()

	if _, err := m.EmbedTokens(ctx, make([]int32, textenc.MaxLen-1), make([]float32, textenc.MaxLen)); err == nil {
		t.Fatalf("EmbedTokens expected an error for short token ids")
	}
	if _, err := m.EmbedTokens(ctx, make([]int32, textenc.MaxLen), make([]float32, textenc.MaxLen+1)); err == nil {
		t.Fatalf("EmbedTokens expected an error for an oversized mask")
	}
}

// TestEmbedTokensRejectsNonBinaryMask checks that fractional mask values are
// rejected: the additive key bias is derived as (mask-1)*1e30, which only
// means "ignore this key" when the mask is exactly 0 or 1.
func TestEmbedTokensRejectsNonBinaryMask(t *testing.T) {
	m := &Model{}
	mask := make([]float32, textenc.MaxLen)
	mask[3] = 0.5
	if _, err := m.EmbedTokens(context.Background(), make([]int32, textenc.MaxLen), mask); err == nil {
		t.Fatalf("EmbedTokens expected an error for a non-binary mask")
	} else if !strings.Contains(err.Error(), "0 or 1") {
		t.Fatalf("err=%v want a mask-value error", err)
	}
}

// TestEmbedTextFailsWithoutTokenizer checks the guard for a model whose text
// half never loaded: EmbedText must report the missing tokenizer rather than
// panic on a nil encoder.
func TestEmbedTextFailsWithoutTokenizer(t *testing.T) {
	m := &Model{}
	if _, err := m.EmbedText(context.Background(), "sunset"); err == nil {
		t.Fatalf("EmbedText expected an error without a tokenizer")
	}
}
