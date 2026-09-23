package vision

import (
	"context"
	"encoding/json"
	"fmt"
	"math"

	"github.com/gomlx/compute/dtypes"
	"github.com/gomlx/compute/shapes"
	"github.com/gomlx/go-huggingface/hub"
	"github.com/gomlx/gomlx/core/graph"
	. "github.com/gomlx/gomlx/core/graph"
	"github.com/gomlx/gomlx/core/tensors"
	"github.com/gomlx/gomlx/ml/layers/activation"
	"github.com/gomlx/gomlx/ml/nn"
	"viewer/internal/textenc"
)

// textConfig mirrors the subset of HuggingFace's SiglipTextConfig needed for
// inference. The published SigLIP2 config.json leaves the text settings
// implicit, so the SigLIP2 defaults are applied for absent fields.
type textConfig struct {
	HiddenSize            int     `json:"hidden_size"`
	IntermediateSize      int     `json:"intermediate_size"`
	NumHiddenLayers       int     `json:"num_hidden_layers"`
	NumAttentionHead      int     `json:"num_attention_heads"`
	MaxPositionEmbeddings int     `json:"max_position_embeddings"`
	LayerNormEps          float64 `json:"layer_norm_eps"`
}

func (c *textConfig) applyDefaults() {
	if c.HiddenSize == 0 {
		c.HiddenSize = 768
	}
	if c.IntermediateSize == 0 {
		c.IntermediateSize = 3072
	}
	if c.NumHiddenLayers == 0 {
		c.NumHiddenLayers = 12
	}
	if c.NumAttentionHead == 0 {
		c.NumAttentionHead = 12
	}
	if c.MaxPositionEmbeddings == 0 {
		c.MaxPositionEmbeddings = textenc.MaxLen
	}
	if c.LayerNormEps == 0 {
		c.LayerNormEps = 1e-6
	}
}

// validate rejects configurations the fixed-length text pipeline cannot serve:
// the tokenizer always emits textenc.MaxLen positions, so a tower built for a
// different sequence length would silently read the wrong pooling position, and
// the attention head split must divide the hidden size evenly.
func (c *textConfig) validate() error {
	if c.NumAttentionHead <= 0 || c.HiddenSize%c.NumAttentionHead != 0 {
		return fmt.Errorf("text hidden size %d is not divisible by %d attention heads",
			c.HiddenSize, c.NumAttentionHead)
	}
	if c.MaxPositionEmbeddings != textenc.MaxLen {
		return fmt.Errorf("text tower expects %d positions but the tokenizer pads to %d",
			c.MaxPositionEmbeddings, textenc.MaxLen)
	}
	return nil
}

// loadTextConfig reads config.json, tolerating an absent text_config block.
func loadTextConfig(repo *hub.Repo) (textConfig, error) {
	data, err := repo.ReadFile("config.json")
	if err != nil {
		return textConfig{}, fmt.Errorf("read config.json: %w", err)
	}
	var raw struct {
		TextConfig *textConfig `json:"text_config"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return textConfig{}, fmt.Errorf("parse config.json: %w", err)
	}
	var cfg textConfig
	if raw.TextConfig != nil {
		cfg = *raw.TextConfig
	}
	cfg.applyDefaults()
	return cfg, nil
}

// loadTokenizer builds the text tokenizer from the model directory's
// tokenizer.json. Unlike the vision tower the text side cannot run without it,
// so a missing or malformed file fails the whole Load the same way a missing
// checkpoint does.
func loadTokenizer(repo *hub.Repo) (*textenc.Tokenizer, error) {
	data, err := repo.ReadFile("tokenizer.json")
	if err != nil {
		return nil, fmt.Errorf("read tokenizer.json: %w", err)
	}
	tokenizer, err := textenc.LoadFromBytes(data)
	if err != nil {
		return nil, fmt.Errorf("load tokenizer.json: %w", err)
	}
	return tokenizer, nil
}

// textWeights holds every parameter node of the text tower, in one graph.
type textWeights struct {
	tokenEmbedding, posEmbedding *Node

	layers []layerWeights

	finalGamma, finalBias *Node

	headWeight, headBias *Node
}

// loadTextWeights binds every text parameter into g as a constant. The encoder
// blocks share the vision tower's layout (and therefore its layerWeights
// loader); only the embeddings, the final norm and the head differ.
func loadTextWeights(cp *checkpoint, g *Graph, cfg textConfig) (*textWeights, error) {
	const prefix = "text_model."
	w := &textWeights{}
	read := func(name string) (*Node, error) { return cp.tensor(g, prefix+name) }
	readWeight := func(name string) (*Node, error) { return cp.weight(g, prefix+name) }

	var err error
	if w.tokenEmbedding, err = read("embeddings.token_embedding.weight"); err != nil {
		return nil, err
	}
	if w.posEmbedding, err = read("embeddings.position_embedding.weight"); err != nil {
		return nil, err
	}

	w.layers = make([]layerWeights, cfg.NumHiddenLayers)
	for i := range w.layers {
		layer, err := loadLayer(read, readWeight, i)
		if err != nil {
			return nil, err
		}
		w.layers[i] = layer
	}

	if w.finalGamma, err = read("final_layer_norm.weight"); err != nil {
		return nil, err
	}
	if w.finalBias, err = read("final_layer_norm.bias"); err != nil {
		return nil, err
	}
	if w.headWeight, err = readWeight("head.weight"); err != nil {
		return nil, err
	}
	if w.headBias, err = read("head.bias"); err != nil {
		return nil, err
	}
	return w, nil
}

// compileText builds the text tower's forward graph and JIT-compiles it. Like
// the vision graph the weights are bound as constants while the checkpoint is
// still mapped.
func (m *Model) compileText(cp *checkpoint, cfg textConfig) (*graph.Exec, error) {
	exec, err := NewExec(m.graphBackend, func(tokenIDs, attentionMask *Node) *Node {
		return textForward(tokenIDs, attentionMask, cp, cfg)
	})
	if err != nil {
		return nil, fmt.Errorf("build SigLIP2 text graph: %w", err)
	}
	if _, err := exec.Compile(
		shapes.Make(dtypes.Int32, textenc.MaxLen),
		shapes.Make(dtypes.Float32, textenc.MaxLen),
	); err != nil {
		return nil, fmt.Errorf("compile SigLIP2 text graph: %w", err)
	}
	return exec, nil
}

// textForward builds the SigLIP2 text graph for one fixed-length token
// sequence, matching HuggingFace's get_text_features for this checkpoint.
//
// The attention is bidirectional: unlike a causal decoder every position may
// attend to every other position, subject only to the padding mask, which is
// applied as an additive bias on the key axis so pad positions contribute
// nothing to the softmax.
func textForward(tokenIDs, attentionMask *Node, cp *checkpoint, cfg textConfig) *Node {
	w, err := loadTextWeights(cp, tokenIDs.Graph(), cfg)
	if err != nil {
		panic(err)
	}
	hidden := cfg.HiddenSize
	heads := cfg.NumAttentionHead
	headDim := hidden / heads
	tokens := textenc.MaxLen
	scale := 1.0 / math.Sqrt(float64(headDim))

	// The token embedding lookup returns one hidden vector per id, which the
	// position embedding then anchors to its place in the fixed 64-slot window.
	x := Gather(w.tokenEmbedding, Reshape(tokenIDs, tokens, 1))
	x = Add(x, w.posEmbedding)

	// Pad keys get (1-mask)*-1e30 added to their attention scores, which drives
	// their softmax weight to exactly zero. Real keys (mask 1) are untouched.
	keyBias := MulScalar(AddScalar(attentionMask, -1), 1e30)

	for i := range w.layers {
		layer := &w.layers[i]
		residual := x
		normed := nn.LayerNorm(x, []int{1}, cfg.LayerNormEps, layer.ln1Gamma, layer.ln1Beta, nil)
		q := heads3(Dot(normed, layer.qWeight).Product(), layer.qBias, tokens, heads, headDim)
		k := heads3(Dot(normed, layer.kWeight).Product(), layer.kBiases, tokens, heads, headDim)
		v := heads3(Dot(normed, layer.vWeight).Product(), layer.vBias, tokens, heads, headDim)
		scores := MulScalar(Einsum("bqd,bkd->bqk", q, k), scale)
		// The mask keys on the last axis only: every query row sees the same
		// set of valid keys, so the bias broadcasts over heads and queries.
		scores = Add(scores, Reshape(keyBias, 1, 1, tokens))
		context := Einsum("bqk,bkd->bqd", Softmax(scores, -1), v)
		context = TransposeAllAxes(context, 1, 0, 2)
		context = Reshape(context, tokens, hidden)
		context = addBias(Dot(context, layer.outWeight).Product(), layer.outB, hidden)
		x = Add(residual, context)

		residual = x
		normed = nn.LayerNorm(x, []int{1}, cfg.LayerNormEps, layer.ln2Gamma, layer.ln2Beta, nil)
		mlp := addBias(Dot(normed, layer.fc1Weight).Product(), layer.fc1B, cfg.IntermediateSize)
		mlp = activation.Gelu(mlp)
		mlp = addBias(Dot(mlp, layer.fc2Weight).Product(), layer.fc2B, hidden)
		x = Add(residual, mlp)
	}
	x = nn.LayerNorm(x, []int{1}, cfg.LayerNormEps, w.finalGamma, w.finalBias, nil)

	// Pool the last position (63) of the fixed-64 sequence. For short texts the
	// tokenizer right-pads, so position 63 holds a <pad> token and its hidden
	// state is garbage — but that is exactly what HuggingFace does:
	// get_text_features always reads hidden state -1 of the Fixed(64)-padded
	// sequence. The pad rows cannot contaminate the real positions on the way
	// there, because every attention softmax keys them out with the bias above,
	// so real positions never read from pads. This is not a bug even though it
	// reads what looks like padding.
	pooled := Slice(x, AxisRange(tokens-1, tokens), AxisRange(0, hidden))
	return addBias(Dot(pooled, w.headWeight).Product(), w.headBias, hidden)
}

// EmbedText encodes query and runs the text tower; the result is NOT
// L2-normalized (Qdrant Cosine handles normalization).
func (m *Model) EmbedText(ctx context.Context, text string) ([]float32, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if m.tokenizer == nil {
		return nil, fmt.Errorf("text tokenizer is not loaded")
	}
	tokenIDs, attentionMask, err := m.tokenizer.Encode(text)
	if err != nil {
		return nil, fmt.Errorf("encode text: %w", err)
	}
	return m.embedTokens(tokenIDs, attentionMask)
}

// EmbedTokens runs the tower on pre-tokenized input — the regression-test
// entry point that bypasses the tokenizer. Both slices must have length
// textenc.MaxLen (64); mask values are 0 or 1.
func (m *Model) EmbedTokens(ctx context.Context, tokenIDs []int32, attentionMask []float32) ([]float32, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(tokenIDs) != textenc.MaxLen {
		return nil, fmt.Errorf("expected %d token ids, got %d", textenc.MaxLen, len(tokenIDs))
	}
	if len(attentionMask) != textenc.MaxLen {
		return nil, fmt.Errorf("expected %d mask values, got %d", textenc.MaxLen, len(attentionMask))
	}
	for i, value := range attentionMask {
		if value != 0 && value != 1 {
			return nil, fmt.Errorf("attentionMask[%d]=%v: mask values must be 0 or 1", i, value)
		}
	}
	return m.embedTokens(tokenIDs, attentionMask)
}

// embedTokens runs the compiled text graph on one already-validated sequence.
// Like Embed it serialises on m.mu: the pure-Go backend executes the graph in
// place and shares its scratch buffers between calls.
func (m *Model) embedTokens(tokenIDs []int32, attentionMask []float32) ([]float32, error) {
	ids := tensors.FromFlatDataAndDimensions(tokenIDs, textenc.MaxLen)
	mask := tensors.FromFlatDataAndDimensions(attentionMask, textenc.MaxLen)

	m.mu.Lock()
	output, err := m.textExec.Call1(ids, mask)
	m.mu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("SigLIP2 text forward pass: %w", err)
	}
	values, ok := output.Value().([][]float32)
	if !ok {
		return nil, fmt.Errorf("unexpected SigLIP2 text output type %T", output.Value())
	}
	if len(values) != 1 {
		return nil, fmt.Errorf("SigLIP2 text returned %d embeddings for a single sequence", len(values))
	}
	return values[0], nil
}
