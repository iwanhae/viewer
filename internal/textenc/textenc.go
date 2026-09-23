// Package textenc turns natural-language text (English and Korean alike)
// into the token ids the SigLIP2 text tower expects.
//
// The checkpoint's tokenizer.json describes a Gemma-style byte-pair encoding:
// a Replace normalizer turns every space into "▁", a Split pre-tokenizer with
// MergedWithPrevious then hands the whole normalized string to the BPE model
// as a single word (every regular space is gone by the time it runs), and a
// TemplateProcessing post-processor appends <eos>. There is no <bos>. The
// tower consumes exactly MaxLen positions, so Encode keeps at most MaxLen-1
// content tokens, leaves <eos> as the last real token, and right-pads
// everything after it with <pad> (id 0).
//
// Parsing and BPE are delegated to github.com/gomlx/go-huggingface's
// hftokenizer, which understands every construct this checkpoint's
// tokenizer.json uses: the object-map vocabulary, merges given as ["a","b"]
// pairs, the Replace normalizer, the Split pre-tokenizer and the
// TemplateProcessing post-processor. Two of its limitations are accepted
// rather than worked around. Byte-fallback is declared in tokenizer.json but
// not implemented at encode time, so a character with no vocabulary entry
// encodes to <unk> (id 3) instead of <0xNN> byte pieces; output therefore
// matches the reference only for text whose characters all have vocabulary
// entries, which covers English, Korean syllables and the emoji in the
// fixtures. The merge loop also rescans the whole word for the best-ranked
// pair after every merge instead of keeping a priority queue, which is slower
// but selects the same merges, so the ids come out identical.
package textenc

import (
	"fmt"

	"github.com/gomlx/go-huggingface/tokenizers/hftokenizer"
)

const (
	// MaxLen is the model's fixed token budget: the text tower is trained
	// with 64 position embeddings, so every Encode returns exactly 64 ids
	// and a 64-wide attention mask.
	MaxLen = 64

	// PadID is the id of <pad>, used for every position after the content;
	// the attention mask is 0 there so the tower ignores the padding.
	PadID = 0

	// eosID is the id of <eos>, which the post-processor appends after the
	// content. The checkpoint fixes the special-token ids (<pad>=0,
	// <eos>=1, <bos>=2, <unk>=3), so they are constants here rather than
	// something read from the file at load time.
	eosID = 1
)

// Tokenizer encodes text for the SigLIP2 text tower. It is safe for
// concurrent use: encoding only reads the immutable vocabulary and merge
// table built at load time.
type Tokenizer struct {
	hf *hftokenizer.Tokenizer
}

// LoadFromBytes parses a tokenizer.json payload — the same bytes
// checkpoint.Ensure fetches into the model directory.
func LoadFromBytes(data []byte) (*Tokenizer, error) {
	// A nil config keeps hftokenizer from layering tokenizer_config.json
	// behaviour (add_bos_token and friends) on top of tokenizer.json: the
	// template post-processor alone defines this checkpoint's special-token
	// layout, and everything it needs lives in the file being parsed.
	hf, err := hftokenizer.NewFromContent(nil, data)
	if err != nil {
		return nil, fmt.Errorf("parse tokenizer.json: %w", err)
	}
	return &Tokenizer{hf: hf}, nil
}

// Encode lowercases nothing, replaces spaces with ▁, runs BPE, appends <eos>
// (via the post-processor), truncates the content to MaxLen-1 tokens, and
// right-pads with PadID to exactly MaxLen. attentionMask is 1.0 where a
// position is real (content or <eos>) and 0.0 where it is padding.
//
// The returned slices always have length MaxLen and always end with exactly
// one <eos> as the last real token, regardless of what the underlying library
// produced: the guarantees are enforced here because every consumer (the
// tower's position embeddings, the Qdrant vector layout) depends on them.
// Encoding itself cannot fail — a malformed tokenizer.json is rejected by
// LoadFromBytes — so err is always nil and exists to keep the signature
// stable for callers that pipeline load and encode.
func (t *Tokenizer) Encode(text string) (tokenIDs []int32, attentionMask []float32, err error) {
	// hftokenizer applies the post-processor because AddSpecialTokens
	// defaults to true, so a successful encode ends with <eos>. Only that
	// trailing marker is stripped: an <eos> in the middle of the content can
	// only come from the same literal in the input text, which the reference
	// tokenizer keeps as-is.
	raw := t.hf.Encode(text)

	content := raw
	if n := len(content); n > 0 && content[n-1] == eosID {
		content = content[:n-1]
	}
	if len(content) > MaxLen-1 {
		content = content[:MaxLen-1]
	}

	tokenIDs = make([]int32, MaxLen)
	for i, id := range content {
		tokenIDs[i] = int32(id)
	}
	tokenIDs[len(content)] = eosID

	attentionMask = make([]float32, MaxLen)
	for i := 0; i <= len(content); i++ {
		attentionMask[i] = 1
	}
	return tokenIDs, attentionMask, nil
}
