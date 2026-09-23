// Package vision implements the SigLIP2 vision encoder used to turn images into
// embedding vectors.
//
// Everything runs in-process through GoMLX. The computation graph is built once
// with the checkpoint's weights bound in as constants, and then reused for every
// image.
package vision

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/gomlx/compute"
	"github.com/gomlx/compute/dtypes"
	_ "github.com/gomlx/compute/gobackend"
	"github.com/gomlx/compute/shapes"
	"github.com/gomlx/go-huggingface/hub"
	"github.com/gomlx/go-huggingface/models/safetensors"
	"github.com/gomlx/gomlx/core/graph"
	. "github.com/gomlx/gomlx/core/graph"
	"github.com/gomlx/gomlx/core/tensors"
	"github.com/gomlx/gomlx/ml/layers/activation"
	"github.com/gomlx/gomlx/ml/nn"
	"viewer/internal/textenc"
)

const (
	// DefaultModelID is the SigLIP2 checkpoint used when none is configured.
	DefaultModelID = "google/siglip2-base-patch16-224"

	// DefaultBackend is the GoMLX backend used when none is configured. The
	// pure-Go backend keeps the binary self-contained (no CGO, no runtime
	// download); the XLA backend is much faster but needs the XLA runtime.
	DefaultBackend = "go"

	// InputImageSize is the side of the square input the vision tower expects.
	InputImageSize = 224

	imageChannels = 3
)

// Config describes how to build a Model.
type Config struct {
	// ModelID is a HuggingFace repository id, or a local directory holding
	// config.json and a .safetensors checkpoint.
	ModelID string
	// Backend is the GoMLX backend configuration, e.g. "go" or "xla:cpu".
	Backend string
	// CacheDir overrides the HuggingFace cache root.
	CacheDir string
	// AuthToken is the HuggingFace token, needed only for gated repositories.
	AuthToken string
}

// Image is a preprocessed, channels-first image ready for the vision tower.
type Image struct {
	// Pixels holds float32 RGB values in [-1, 1], laid out channel-major:
	// all red pixels, then green, then blue.
	Pixels []float32
}

// Model is a loaded SigLIP2 vision tower. It is safe for concurrent use.
type Model struct {
	config Config

	imageSize int
	embedDim  int
	layers    int
	heads     int
	headDim   int

	graphBackend compute.Backend
	exec         *graph.Exec

	// textExec is the compiled text tower and tokenizer encodes queries for
	// it; together they power EmbedText.
	textExec  *graph.Exec
	tokenizer *textenc.Tokenizer

	// mu serialises calls into exec: the pure-Go backend executes the graph
	// in place and shares its scratch buffers between calls.
	mu sync.Mutex
}

// visionConfig mirrors the subset of HuggingFace's SiglipVisionConfig needed for
// inference. The published SigLIP2 config.json leaves the vision settings
// implicit, so the SigLIP2 defaults are applied for absent fields.
type visionConfig struct {
	HiddenSize       int     `json:"hidden_size"`
	IntermediateSize int     `json:"intermediate_size"`
	NumHiddenLayers  int     `json:"num_hidden_layers"`
	NumAttentionHead int     `json:"num_attention_heads"`
	NumChannels      int     `json:"num_channels"`
	PatchSize        int     `json:"patch_size"`
	LayerNormEps     float64 `json:"layer_norm_eps"`
	ImageSize        int     `json:"image_size"`
}

func (c *visionConfig) applyDefaults() {
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
	if c.NumChannels == 0 {
		c.NumChannels = imageChannels
	}
	if c.PatchSize == 0 {
		c.PatchSize = 16
	}
	if c.LayerNormEps == 0 {
		c.LayerNormEps = 1e-6
	}
	if c.ImageSize == 0 {
		c.ImageSize = InputImageSize
	}
}

// Load resolves the checkpoint and compiles the inference graph.
func Load(ctx context.Context, cfg Config) (*Model, error) {
	if strings.TrimSpace(cfg.ModelID) == "" {
		cfg.ModelID = DefaultModelID
	}
	if strings.TrimSpace(cfg.Backend) == "" {
		cfg.Backend = DefaultBackend
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	repo, err := newRepo(cfg)
	if err != nil {
		return nil, err
	}
	visCfg, err := loadVisionConfig(repo)
	if err != nil {
		return nil, err
	}
	visCfg.applyDefaults()
	if visCfg.NumAttentionHead <= 0 || visCfg.HiddenSize%visCfg.NumAttentionHead != 0 {
		return nil, fmt.Errorf("hidden size %d is not divisible by %d attention heads",
			visCfg.HiddenSize, visCfg.NumAttentionHead)
	}
	if visCfg.ImageSize%visCfg.PatchSize != 0 {
		return nil, fmt.Errorf("image size %d is not divisible by patch size %d",
			visCfg.ImageSize, visCfg.PatchSize)
	}
	if visCfg.NumChannels != imageChannels {
		return nil, fmt.Errorf("unsupported channel count %d", visCfg.NumChannels)
	}

	// The text tower is part of the same checkpoint and always loads with the
	// vision tower: EmbedText has no fallback, so a missing or malformed
	// tokenizer.json fails Load exactly like a missing checkpoint would.
	textCfg, err := loadTextConfig(repo)
	if err != nil {
		return nil, err
	}
	if err := textCfg.validate(); err != nil {
		return nil, err
	}
	tokenizer, err := loadTokenizer(repo)
	if err != nil {
		return nil, err
	}

	weights, err := openWeights(repo, visCfg)
	if err != nil {
		return nil, err
	}
	backend, err := backendFor(cfg.Backend)
	if err != nil {
		weights.Close()
		return nil, err
	}

	model := &Model{
		config:       cfg,
		imageSize:    visCfg.ImageSize,
		embedDim:     visCfg.HiddenSize,
		layers:       visCfg.NumHiddenLayers,
		heads:        visCfg.NumAttentionHead,
		headDim:      visCfg.HiddenSize / visCfg.NumAttentionHead,
		graphBackend: backend,
		tokenizer:    tokenizer,
	}
	exec, err := model.compile(weights, visCfg)
	if err != nil {
		weights.Close()
		return nil, err
	}
	textExec, err := model.compileText(weights, textCfg)
	weights.Close()
	if err != nil {
		exec.Finalize()
		return nil, err
	}
	model.exec = exec
	model.textExec = textExec
	return model, nil
}

// newRepo resolves the model location. An existing directory is read directly;
// a path-like value that does not exist is an error rather than a repository id,
// so a broken local deployment fails fast instead of hitting the Hub (which
// matters for air-gapped installs, where that request would hang or 404).
func newRepo(cfg Config) (*hub.Repo, error) {
	if info, err := os.Stat(cfg.ModelID); err == nil {
		if !info.IsDir() {
			return nil, fmt.Errorf("model path %q is not a directory", cfg.ModelID)
		}
		return hub.NewLocal(cfg.ModelID), nil
	} else if looksLikePath(cfg.ModelID) {
		return nil, fmt.Errorf("model directory %q: %w", cfg.ModelID, err)
	}

	repo := hub.New(cfg.ModelID).WithAuth(cfg.AuthToken)
	if cfg.CacheDir != "" {
		repo = repo.WithCacheDir(cfg.CacheDir)
	}
	return repo, nil
}

// looksLikePath reports whether a model id is a filesystem path rather than a
// HuggingFace repository id such as "google/siglip2-base-patch16-224".
func looksLikePath(modelID string) bool {
	return filepath.IsAbs(modelID) ||
		strings.HasPrefix(modelID, "./") ||
		strings.HasPrefix(modelID, "../") ||
		strings.HasPrefix(modelID, "~/")
}

// loadVisionConfig reads config.json, tolerating an absent vision_config block.
func loadVisionConfig(repo *hub.Repo) (visionConfig, error) {
	data, err := repo.ReadFile("config.json")
	if err != nil {
		return visionConfig{}, fmt.Errorf("read config.json: %w", err)
	}
	var raw struct {
		VisionConfig *visionConfig `json:"vision_config"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return visionConfig{}, fmt.Errorf("parse config.json: %w", err)
	}
	var cfg visionConfig
	if raw.VisionConfig != nil {
		cfg = *raw.VisionConfig
	}
	cfg.applyDefaults()
	return cfg, nil
}

// checkpoint provides read access to the model's safetensors weights.
type checkpoint struct {
	reader *safetensors.TensorReader
}

// openWeights memory-maps the checkpoint file.
func openWeights(repo *hub.Repo, _ visionConfig) (*checkpoint, error) {
	model, err := safetensors.New(repo)
	if err != nil {
		return nil, fmt.Errorf("read safetensors index: %w", err)
	}
	file := safetensorsFile(model)
	if file == "" {
		return nil, fmt.Errorf("no .safetensors checkpoint found in %s", repo.ID)
	}
	reader, err := model.NewTensorReader(file)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", file, err)
	}
	return &checkpoint{reader: reader}, nil
}

// safetensorsFile picks the checkpoint filename, preferring model.safetensors.
func safetensorsFile(model *safetensors.Model) string {
	if _, ok := model.Headers["model.safetensors"]; ok {
		return "model.safetensors"
	}
	for name := range model.Headers {
		return name
	}
	return ""
}

// Close releases the memory map. It must be called after the graph is compiled,
// because constants read from it are copied into the graph during construction.
func (c *checkpoint) Close() {
	if c == nil || c.reader == nil {
		return
	}
	_ = c.reader.Close()
	c.reader = nil
}

// weight reads a weight matrix from the checkpoint and orients it as
// [in_features, out_features] so it can be used directly with Dot. Checkpoints
// store linear weights transposed.
func (c *checkpoint) weight(g *Graph, name string) (*Node, error) {
	tensor, err := c.reader.ReadTensor(nil, name)
	if err != nil {
		return nil, fmt.Errorf("read weight %s: %w", name, err)
	}
	defer func() { _ = tensor.FinalizeAll() }()
	node := Const(g, tensor)
	if node.Rank() != 2 {
		return nil, fmt.Errorf("weight %s: expected rank 2, got rank %d", name, node.Rank())
	}
	return TransposeAllAxes(node, 1, 0), nil
}

// tensor reads a single value (bias, embedding table or probe) as a constant.
func (c *checkpoint) tensor(g *Graph, name string) (*Node, error) {
	tensor, err := c.reader.ReadTensor(nil, name)
	if err != nil {
		return nil, fmt.Errorf("read weight %s: %w", name, err)
	}
	defer func() { _ = tensor.FinalizeAll() }()
	return Const(g, tensor), nil
}

// weights holds every parameter node of the vision tower, in one graph.
type weights struct {
	patchWeight, patchBias *Node
	posEmbedding           *Node

	layers []layerWeights

	postGamma, postBias *Node

	probe             *Node
	headInProjWeight  *Node
	headInProjBias    *Node
	headOutProjWeight *Node
	headOutProjBias   *Node
	headLnGamma       *Node
	headLnBias        *Node
	headFC1Weight     *Node
	headFC1Bias       *Node
	headFC2Weight     *Node
	headFC2Bias       *Node
}

// layerWeights holds the parameter nodes of one encoder block.
type layerWeights struct {
	ln1Gamma, ln1Beta *Node
	qWeight, qBias    *Node
	kWeight, kBiases  *Node
	vWeight, vBias    *Node
	outWeight, outB   *Node
	ln2Gamma, ln2Beta *Node
	fc1Weight, fc1B   *Node
	fc2Weight, fc2B   *Node
}

// loadWeights binds every vision parameter into g as a constant.
func loadWeights(checkpoint *checkpoint, g *Graph, cfg visionConfig) (*weights, error) {
	const prefix = "vision_model."
	w := &weights{}
	read := func(name string) (*Node, error) { return checkpoint.tensor(g, prefix+name) }
	readWeight := func(name string) (*Node, error) { return checkpoint.weight(g, prefix+name) }

	var err error
	if w.patchWeight, err = read("embeddings.patch_embedding.weight"); err != nil {
		return nil, err
	}
	if w.patchBias, err = read("embeddings.patch_embedding.bias"); err != nil {
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

	simple := []struct {
		dst    **Node
		name   string
		weight bool
	}{
		{&w.postGamma, "post_layernorm.weight", false},
		{&w.postBias, "post_layernorm.bias", false},
		{&w.probe, "head.probe", false},
		{&w.headInProjWeight, "head.attention.in_proj_weight", true},
		{&w.headInProjBias, "head.attention.in_proj_bias", false},
		{&w.headOutProjWeight, "head.attention.out_proj.weight", true},
		{&w.headOutProjBias, "head.attention.out_proj.bias", false},
		{&w.headLnGamma, "head.layernorm.weight", false},
		{&w.headLnBias, "head.layernorm.bias", false},
		{&w.headFC1Weight, "head.mlp.fc1.weight", true},
		{&w.headFC1Bias, "head.mlp.fc1.bias", false},
		{&w.headFC2Weight, "head.mlp.fc2.weight", true},
		{&w.headFC2Bias, "head.mlp.fc2.bias", false},
	}
	for _, item := range simple {
		readFn := read
		if item.weight {
			readFn = readWeight
		}
		node, err := readFn(item.name)
		if err != nil {
			return nil, err
		}
		*item.dst = node
	}
	return w, nil
}

func loadLayer(read, readWeight func(string) (*Node, error), index int) (layerWeights, error) {
	prefix := fmt.Sprintf("encoder.layers.%d.", index)
	var layer layerWeights
	simple := []struct {
		dst    **Node
		name   string
		weight bool
	}{
		{&layer.ln1Gamma, "layer_norm1.weight", false},
		{&layer.ln1Beta, "layer_norm1.bias", false},
		{&layer.qWeight, "self_attn.q_proj.weight", true},
		{&layer.qBias, "self_attn.q_proj.bias", false},
		{&layer.kWeight, "self_attn.k_proj.weight", true},
		{&layer.kBiases, "self_attn.k_proj.bias", false},
		{&layer.vWeight, "self_attn.v_proj.weight", true},
		{&layer.vBias, "self_attn.v_proj.bias", false},
		{&layer.outWeight, "self_attn.out_proj.weight", true},
		{&layer.outB, "self_attn.out_proj.bias", false},
		{&layer.ln2Gamma, "layer_norm2.weight", false},
		{&layer.ln2Beta, "layer_norm2.bias", false},
		{&layer.fc1Weight, "mlp.fc1.weight", true},
		{&layer.fc1B, "mlp.fc1.bias", false},
		{&layer.fc2Weight, "mlp.fc2.weight", true},
		{&layer.fc2B, "mlp.fc2.bias", false},
	}
	for _, item := range simple {
		readFn := read
		if item.weight {
			readFn = readWeight
		}
		node, err := readFn(prefix + item.name)
		if err != nil {
			return layerWeights{}, err
		}
		*item.dst = node
	}
	return layer, nil
}

// backendFor resolves the GoMLX backend for the given configuration.
func backendFor(name string) (compute.Backend, error) {
	backend, err := compute.NewWithConfig(name)
	if err != nil {
		return nil, fmt.Errorf("create GoMLX backend %q: %w", name, err)
	}
	return backend, nil
}

// compile builds the forward graph and JIT-compiles it.
//
// The checkpoint is kept open only for the duration of the build: constants are
// copied into the graph as it is constructed.
func (m *Model) compile(checkpoint *checkpoint, cfg visionConfig) (*graph.Exec, error) {
	exec, err := NewExec(m.graphBackend, func(pixelValues *Node) *Node {
		return m.forward(pixelValues, checkpoint, cfg)
	})
	if err != nil {
		return nil, fmt.Errorf("build SigLIP2 graph: %w", err)
	}
	// Build and compile the graph now, while the checkpoint is still mapped: the
	// weights are copied into the graph during construction.
	if _, err := exec.Compile(inputShape(cfg.ImageSize, 1)); err != nil {
		return nil, fmt.Errorf("compile SigLIP2 graph: %w", err)
	}
	return exec, nil
}

// forward builds the SigLIP2 vision graph for one batch of pixel values.
//
// The checkpoint's weights are bound into the graph as constants, which is why
// it must stay open until the graph has been compiled.
func (m *Model) forward(pixelValues *Node, checkpoint *checkpoint, cfg visionConfig) *Node {
	enc := m.encoder(pixelValues, checkpoint, cfg)
	return poolHead(pixelValues.Graph(), enc.w, enc.x, enc.tokens, cfg.HiddenSize,
		m.heads, m.headDim, cfg.IntermediateSize, cfg.LayerNormEps, enc.scale)
}

// poolHead applies SigLIP's multi-head attention pooling head: a learned probe
// attends over the patch sequence, then a residual MLP refines the result.
//
// The probe and the patch sequence share one fused QKV projection, laid out as
// query | key | value along the last axis of hidden.
func poolHead(g *Graph, w *weights, x *Node, tokens, hidden, heads, headDim, intermediate int,
	epsilon, scale float64) *Node {
	seqQKV := Reshape(
		addBias(Dot(x, w.headInProjWeight).Product(), w.headInProjBias, 3*hidden),
		tokens, 3, hidden)
	// Slicing the flat hidden axis keeps the result contiguous, so it can be
	// reshaped straight into attention heads.
	seqK := splitHeads(Slice(seqQKV, AxisRange(0, tokens), AxisRange(1, 2), AxisRange(0, hidden)), tokens, heads, headDim)
	seqV := splitHeads(Slice(seqQKV, AxisRange(0, tokens), AxisRange(2, 3), AxisRange(0, hidden)), tokens, heads, headDim)

	probeQKV := Reshape(
		addBias(Dot(Reshape(w.probe, 1, hidden), w.headInProjWeight).Product(), w.headInProjBias, 3*hidden),
		1, 3, hidden)
	probeQ := splitHeads(Slice(probeQKV, AxisRange(0, 1), AxisRange(0, 1), AxisRange(0, hidden)), 1, heads, headDim)

	scores := MulScalar(Einsum("bqd,bkd->bqk", probeQ, seqK), scale)
	pooled := Einsum("bqk,bkd->bqd", Softmax(scores, -1), seqV)
	// [heads, 1, headDim] -> [1, hidden]
	pooled = Reshape(pooled, 1, hidden)
	pooled = addBias(Dot(pooled, w.headOutProjWeight).Product(), w.headOutProjBias, hidden)

	refined := nn.LayerNorm(pooled, []int{1}, epsilon, w.headLnGamma, w.headLnBias, nil)
	refined = addBias(Dot(refined, w.headFC1Weight).Product(), w.headFC1Bias, intermediate)
	refined = activation.Gelu(refined)
	refined = addBias(Dot(refined, w.headFC2Weight).Product(), w.headFC2Bias, hidden)
	// The head returns only the probe position: a [batch, hidden] matrix.
	return Add(pooled, refined)
}

// splitHeads reshapes a [rows, hidden] projection into [heads, rows, headDim].
func splitHeads(projected *Node, rows, heads, headDim int) *Node {
	return TransposeAllAxes(Reshape(projected, rows, heads, headDim), 1, 0, 2)
}

// heads3 splits a [tokens, heads*headDim] projection into [heads, tokens, headDim].
func heads3(projected, bias *Node, tokens, heads, headDim int) *Node {
	width := heads * headDim
	projected = addBias(projected, bias, width)
	return reshapeHeads(projected, tokens, heads, headDim)
}

// reshapeHeads turns [tokens, hidden] into [heads, tokens, headDim].
func reshapeHeads(x *Node, tokens, heads, headDim int) *Node {
	return TransposeAllAxes(Reshape(x, tokens, heads, headDim), 1, 0, 2)
}

// addBias adds a [width] bias to a [..., width] tensor.
func addBias(x, bias *Node, width int) *Node {
	return Add(x, Reshape(bias, 1, width))
}

// inputShape is the NCHW shape of a batch of preprocessed images.
func inputShape(imageSize, batch int) shapes.Shape {
	return shapes.Make(dtypes.Float32, batch, imageChannels, imageSize, imageSize)
}

// Embed computes the embedding of a single preprocessed image.
func (m *Model) Embed(ctx context.Context, image Image) ([]float32, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	expected := imageChannels * m.imageSize * m.imageSize
	if len(image.Pixels) != expected {
		return nil, fmt.Errorf("expected %d pixels, got %d", expected, len(image.Pixels))
	}

	input := tensors.FromShape(inputShape(m.imageSize, 1))
	input.MustMutableFlatData(func(data any) {
		copy(data.([]float32), image.Pixels)
	})

	m.mu.Lock()
	output, err := m.exec.Call1(input)
	m.mu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("SigLIP2 forward pass: %w", err)
	}
	values, ok := output.Value().([][]float32)
	if !ok {
		return nil, fmt.Errorf("unexpected SigLIP2 output type %T", output.Value())
	}
	if len(values) != 1 {
		return nil, fmt.Errorf("SigLIP2 returned %d embeddings for a single image", len(values))
	}
	return values[0], nil
}

// ImageSize reports the square input side the tower expects.
func (m *Model) ImageSize() int { return m.imageSize }

// Describe returns a human readable summary for logs.
func (m *Model) Describe() string {
	return fmt.Sprintf("siglip2 model=%s backend=%s image_size=%d layers=%d heads=%d embed_dim=%d",
		m.config.ModelID, m.config.Backend, m.imageSize, m.layers, m.heads, m.embedDim)
}

// Close releases the compiled graphs. The model must not be used afterwards.
func (m *Model) Close() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.exec != nil {
		m.exec.Finalize()
		m.exec = nil
	}
	if m.textExec != nil {
		m.textExec.Finalize()
		m.textExec = nil
	}
	return nil
}

// encoderResult carries the encoder output plus what the pooling head needs.
type encoderResult struct {
	w      *weights
	x      *Node
	tokens int
	scale  float64
}

// encoder runs the patch embedding and transformer blocks.
func (m *Model) encoder(pixelValues *Node, checkpoint *checkpoint, cfg visionConfig) encoderResult {
	g := pixelValues.Graph()
	w, err := loadWeights(checkpoint, g, cfg)
	if err != nil {
		panic(err)
	}
	imageSize := cfg.ImageSize
	patchSize := cfg.PatchSize
	hidden := cfg.HiddenSize
	heads := m.heads
	headDim := m.headDim
	tokens := (imageSize / patchSize) * (imageSize / patchSize)
	scale := 1.0 / math.Sqrt(float64(headDim))
	epsilon := cfg.LayerNormEps
	intermediate := cfg.IntermediateSize

	patches := ConvGeneral(pixelValues, w.patchWeight,
		ConvolveAxesConfig{
			InputBatch: 0, InputChannels: 1, InputSpatial: []int{2, 3},
			KernelInputChannels: 1, KernelOutputChannels: 0, KernelSpatial: []int{2, 3},
			OutputBatch: 0, OutputChannels: 1, OutputSpatial: []int{2, 3},
		},
		[]int{patchSize, patchSize},
		[][2]int{{0, 0}, {0, 0}},
		[]int{1, 1},
		[]int{1, 1},
		1, 1)
	patches = Add(patches, Reshape(w.patchBias, 1, hidden, 1, 1))
	patches = TransposeAllAxes(patches, 0, 2, 3, 1)
	x := Reshape(patches, tokens, hidden)
	x = Add(x, w.posEmbedding)

	for i := range w.layers {
		layer := &w.layers[i]
		residual := x
		normed := nn.LayerNorm(x, []int{1}, epsilon, layer.ln1Gamma, layer.ln1Beta, nil)
		q := heads3(Dot(normed, layer.qWeight).Product(), layer.qBias, tokens, heads, headDim)
		k := heads3(Dot(normed, layer.kWeight).Product(), layer.kBiases, tokens, heads, headDim)
		v := heads3(Dot(normed, layer.vWeight).Product(), layer.vBias, tokens, heads, headDim)
		scores := MulScalar(Einsum("bqd,bkd->bqk", q, k), scale)
		context := Einsum("bqk,bkd->bqd", Softmax(scores, -1), v)
		context = TransposeAllAxes(context, 1, 0, 2)
		context = Reshape(context, tokens, hidden)
		context = addBias(Dot(context, layer.outWeight).Product(), layer.outB, hidden)
		x = Add(residual, context)

		residual = x
		normed = nn.LayerNorm(x, []int{1}, epsilon, layer.ln2Gamma, layer.ln2Beta, nil)
		mlp := addBias(Dot(normed, layer.fc1Weight).Product(), layer.fc1B, intermediate)
		mlp = activation.Gelu(mlp)
		mlp = addBias(Dot(mlp, layer.fc2Weight).Product(), layer.fc2B, hidden)
		x = Add(residual, mlp)
	}
	x = nn.LayerNorm(x, []int{1}, epsilon, w.postGamma, w.postBias, nil)
	return encoderResult{w: w, x: x, tokens: tokens, scale: scale}
}
