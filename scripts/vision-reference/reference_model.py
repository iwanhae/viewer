"""Independent PyTorch reference for the SigLIP2 vision tower.

The module tree here is written from the HuggingFace reference architecture and
loads the same safetensors checkpoint directly, so it can be used to validate the
Go port without depending on the `transformers` implementation.
"""

import os

import numpy as np
import torch
from PIL import Image
from safetensors.torch import load_file

MODEL = os.environ.get(
    "SIGLIP2_MODEL", "/tmp/siglip2-model/model.safetensors"
)
HIDDEN = 768
HEADS = 12
HEAD_DIM = 64
LAYERS = 12
INTERMEDIATE = 3072
EPS = 1e-6


def gelu(x):
    return torch.nn.functional.gelu(x, approximate="none")


class LayerNorm(torch.nn.Module):
    def __init__(self):
        super().__init__()
        self.weight = torch.nn.Parameter(torch.ones(HIDDEN))
        self.bias = torch.nn.Parameter(torch.zeros(HIDDEN))

    def forward(self, x):
        mean = x.mean(-1, keepdim=True)
        var = x.var(-1, unbiased=False, keepdim=True)
        return (x - mean) / torch.sqrt(var + EPS) * self.weight + self.bias


class Linear(torch.nn.Module):
    def __init__(self, out_features, in_features):
        super().__init__()
        self.weight = torch.nn.Parameter(torch.zeros(out_features, in_features))
        self.bias = torch.nn.Parameter(torch.zeros(out_features))

    def forward(self, x):
        return torch.nn.functional.linear(x, self.weight, self.bias)


class Attention(torch.nn.Module):
    def __init__(self):
        super().__init__()
        self.q = Linear(HIDDEN, HIDDEN)
        self.k = Linear(HIDDEN, HIDDEN)
        self.v = Linear(HIDDEN, HIDDEN)
        self.o = Linear(HIDDEN, HIDDEN)

    def forward(self, x):
        q = self.q(x).reshape(1, -1, HEADS, HEAD_DIM).permute(0, 2, 1, 3)
        k = self.k(x).reshape(1, -1, HEADS, HEAD_DIM).permute(0, 2, 1, 3)
        v = self.v(x).reshape(1, -1, HEADS, HEAD_DIM).permute(0, 2, 1, 3)
        scores = q @ k.transpose(-1, -2) / (HEAD_DIM**0.5)
        scores = torch.softmax(scores, dim=-1)
        out = (scores @ v).permute(0, 2, 1, 3).reshape(1, -1, HIDDEN)
        return self.o(out)


class Mlp(torch.nn.Module):
    def __init__(self):
        super().__init__()
        self.fc1 = Linear(INTERMEDIATE, HIDDEN)
        self.fc2 = Linear(HIDDEN, INTERMEDIATE)

    def forward(self, x):
        return self.fc2(gelu(self.fc1(x)))


class Layer(torch.nn.Module):
    def __init__(self):
        super().__init__()
        self.ln1 = LayerNorm()
        self.attn = Attention()
        self.ln2 = LayerNorm()
        self.mlp = Mlp()

    def forward(self, x):
        x = x + self.attn(self.ln1(x))
        return x + self.mlp(self.ln2(x))


class PoolHead(torch.nn.Module):
    def __init__(self):
        super().__init__()
        self.probe = torch.nn.Parameter(torch.zeros(1, 1, HIDDEN))
        self.in_proj = torch.nn.Parameter(torch.zeros(3 * HIDDEN, HIDDEN))
        self.in_proj_bias = torch.nn.Parameter(torch.zeros(3 * HIDDEN))
        self.out_proj = Linear(HIDDEN, HIDDEN)
        self.ln = LayerNorm()
        self.mlp = Mlp()

    def forward(self, x):
        batch = x.shape[0]
        qkv = torch.nn.functional.linear(x, self.in_proj, self.in_proj_bias)
        qkv = qkv.reshape(batch, -1, 3, HEADS, HEAD_DIM).permute(2, 0, 3, 1, 4)
        k, v = qkv[1], qkv[2]

        probe = torch.nn.functional.linear(self.probe, self.in_proj, self.in_proj_bias)
        probe = probe.reshape(batch, 1, 3, HEADS, HEAD_DIM).permute(2, 0, 3, 1, 4)
        q = probe[0]

        scores = q @ k.transpose(-1, -2) / (HEAD_DIM**0.5)
        scores = torch.softmax(scores, dim=-1)
        attn = (scores @ v).permute(0, 2, 1, 3).reshape(batch, 1, HIDDEN)
        attn = self.out_proj(attn)
        return (attn + self.mlp(self.ln(attn)))[:, 0]


class Vision(torch.nn.Module):
    def __init__(self):
        super().__init__()
        self.patch_weight = torch.nn.Parameter(torch.zeros(HIDDEN, 3, 16, 16))
        self.patch_bias = torch.nn.Parameter(torch.zeros(HIDDEN))
        self.pos_embed = torch.nn.Parameter(torch.zeros(196, HIDDEN))
        self.layers = torch.nn.ModuleList([Layer() for _ in range(LAYERS)])
        self.post = LayerNorm()
        self.head = PoolHead()

    def forward(self, pixel_values):
        patches = torch.nn.functional.conv2d(
            pixel_values, self.patch_weight, self.patch_bias, stride=16
        )
        x = patches.flatten(2).transpose(1, 2)
        x = x + self.pos_embed
        for layer in self.layers:
            x = layer(x)
        x = self.post(x)
        return self.head(x)


def load_model():
    tensors = load_file(MODEL)
    model = Vision().eval()
    state = {}
    for name, param in model.state_dict().items():
        key = map_name(name)
        state[name] = tensors[key]
    model.load_state_dict(state)
    return model


def map_name(name):
    """Maps this module tree's names onto the checkpoint's names."""
    if name.startswith("patch_weight"):
        return "vision_model.embeddings.patch_embedding.weight"
    if name.startswith("patch_bias"):
        return "vision_model.embeddings.patch_embedding.bias"
    if name.startswith("pos_embed"):
        return "vision_model.embeddings.position_embedding.weight"
    if name.startswith("post."):
        return "vision_model.post_layernorm." + name.split(".", 1)[1]
    if name.startswith("head.probe"):
        return "vision_model.head.probe"
    if name.startswith("head.in_proj_bias"):
        return "vision_model.head.attention.in_proj_bias"
    if name.startswith("head.in_proj"):
        return "vision_model.head.attention.in_proj_weight"
    if name.startswith("head.out_proj."):
        return "vision_model.head.attention.out_proj." + name.split(".", 2)[2]
    if name.startswith("head.ln."):
        return "vision_model.head.layernorm." + name.split(".", 2)[2]
    if name.startswith("head.mlp."):
        return "vision_model.head.mlp." + name.split(".", 2)[2]
    if name.startswith("layers."):
        _, idx, rest = name.split(".", 2)
        rest = rest.replace("ln1.", "layer_norm1.").replace("ln2.", "layer_norm2.")
        rest = rest.replace("attn.q.", "self_attn.q_proj.").replace(
            "attn.k.", "self_attn.k_proj."
        )
        rest = rest.replace("attn.v.", "self_attn.v_proj.").replace(
            "attn.o.", "self_attn.out_proj."
        )
        return f"vision_model.encoder.layers.{idx}.{rest}"
    raise KeyError(name)


def build_image(w, h):
    ys = np.arange(h, dtype=np.float64)[:, None]
    xs = np.arange(w, dtype=np.float64)[None, :]
    r = (xs * 7 + ys * 3) % 256
    g = (xs * xs + ys * 11) % 256
    b = (((xs + ys) * 5) % 256)
    arr = np.stack([r, g, b], axis=-1).astype(np.uint8)
    return Image.fromarray(arr, "RGB")


def raw_pixels(image):
    """Resize to 224x224 and scale to [0, 1], the tower's input."""
    resized = image.resize((224, 224), resample=Image.Resampling.BILINEAR)
    arr = np.asarray(resized, dtype=np.float32) / 255.0
    return arr.transpose(2, 0, 1)[None, ...]


def normalise(arr):
    """Apply SigLIP's mean=std=0.5 rescale, i.e. [0, 1] -> [-1, 1]."""
    return (arr - 0.5) / 0.5
