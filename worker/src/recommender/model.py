"""SigLIP2 vision tower (google/siglip2-base-patch16-224).

Adapted from scripts/vision-reference/reference_model.py, the PyTorch
reference the Go server's internal/vision port is regression-tested against
(cosine >= 0.9995). The single functional change: Attention now handles
batched input (the reference hardcoded batch 1) so the worker can embed
micro-batches.

Architecture constants are pinned to that one checkpoint; map_name raises
KeyError on anything incompatible. The head output is returned raw and
unnormalized — exactly what the server persists (it normalizes only inside
its in-memory similarity index).
"""

from __future__ import annotations

from pathlib import Path

import torch
from safetensors.torch import load_file

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
        batch = x.shape[0]
        q = self.q(x).reshape(batch, -1, HEADS, HEAD_DIM).permute(0, 2, 1, 3)
        k = self.k(x).reshape(batch, -1, HEADS, HEAD_DIM).permute(0, 2, 1, 3)
        v = self.v(x).reshape(batch, -1, HEADS, HEAD_DIM).permute(0, 2, 1, 3)
        scores = q @ k.transpose(-1, -2) / (HEAD_DIM**0.5)
        scores = torch.softmax(scores, dim=-1)
        out = (scores @ v).permute(0, 2, 1, 3).reshape(batch, -1, HIDDEN)
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
    """SigLIP's multi-head attention pooling — neither CLS nor mean pooling."""

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
        # The probe is one learned query shared by every item in the batch,
        # so it keeps a 1-sized sample dim and broadcasts against k/v.
        probe = probe.reshape(1, 1, 3, HEADS, HEAD_DIM).permute(2, 0, 3, 1, 4)
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


def load_model(path: Path, device: torch.device) -> Vision:
    """Load the safetensors checkpoint and move the model to device."""
    tensors = load_file(str(path))
    model = Vision().eval()
    state = {}
    for name, _ in model.state_dict().items():
        try:
            state[name] = tensors[map_name(name)]
        except KeyError as exc:
            raise SystemExit(
                f"checkpoint {path} is missing {exc}; "
                "expected google/siglip2-base-patch16-224"
            ) from exc
    model.load_state_dict(state)
    return model.to(device)
