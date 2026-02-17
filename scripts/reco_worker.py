#!/usr/bin/env python3
import base64
import io
import json
import os
import sys
import traceback
from typing import List, Tuple

MEAN = [0.48145466, 0.4578275, 0.40821073]
STD = [0.26862954, 0.26130258, 0.27577711]


class Backend:
    name = "unknown"

    def embed(self, image, model_id: str, device: str) -> Tuple[List[float], str]:
        raise RuntimeError("backend is not initialized")


class ONNXBackend(Backend):
    def __init__(self):
        import numpy as np
        import onnxruntime as ort

        self.np = np

        model_path = os.environ.get("SIGLIP2_ONNX_PATH", "")
        if not model_path:
            raise RuntimeError("SIGLIP2_ONNX_PATH is not set")

        providers = ["CPUExecutionProvider"]
        self.session = ort.InferenceSession(model_path, providers=providers)
        self.input_name = self.session.get_inputs()[0].name
        self.name = f"siglip2-onnx:{os.path.basename(model_path)}"

    def preprocess(self, image):
        img = image.resize((224, 224))
        arr = self.np.asarray(img).astype("float32") / 255.0
        arr = (arr - self.np.array(MEAN, dtype="float32")) / self.np.array(STD, dtype="float32")
        arr = self.np.transpose(arr, (2, 0, 1))
        arr = self.np.expand_dims(arr, axis=0)
        return arr

    def embed(self, image, model_id: str, device: str) -> Tuple[List[float], str]:
        pixel_values = self.preprocess(image)
        output = self.session.run(None, {self.input_name: pixel_values})[0]
        vec = output.reshape(-1).astype("float32").tolist()
        return vec, self.name


class TransformersBackend(Backend):
    def __init__(self):
        import torch
        from transformers import AutoModel, AutoProcessor

        self.torch = torch
        self.AutoModel = AutoModel
        self.AutoProcessor = AutoProcessor
        self.model_cache = {}
        self.processor_cache = {}
        self.name = "siglip2-transformers"

    def _load(self, model_id: str, device: str):
        if model_id not in self.model_cache:
            processor = self.AutoProcessor.from_pretrained(model_id)
            model = self.AutoModel.from_pretrained(model_id)
            model.eval()
            self.processor_cache[model_id] = processor
            self.model_cache[model_id] = model

        model = self.model_cache[model_id]
        processor = self.processor_cache[model_id]

        if device.startswith("cuda") and self.torch.cuda.is_available():
            model = model.to(device)
        else:
            device = "cpu"
            model = model.to("cpu")

        self.model_cache[model_id] = model
        return model, processor, device

    def embed(self, image, model_id: str, device: str) -> Tuple[List[float], str]:
        model, processor, use_device = self._load(model_id, device)
        inputs = processor(images=image, return_tensors="pt")
        if use_device != "cpu":
            inputs = {k: v.to(use_device) for k, v in inputs.items()}

        with self.torch.no_grad():
            if hasattr(model, "get_image_features"):
                out = model.get_image_features(**inputs)
            else:
                out = model(**inputs).last_hidden_state.mean(dim=1)
        vec = out[0].float().cpu().tolist()
        return vec, f"{self.name}:{model_id}"


def pick_backend() -> Backend:
    errors = []
    try:
        return ONNXBackend()
    except Exception as exc:
        errors.append(f"onnx:{exc}")
    try:
        return TransformersBackend()
    except Exception as exc:
        errors.append(f"transformers:{exc}")
    raise RuntimeError("no SigLIP2 backend available (" + "; ".join(errors) + ")")


def decode_image_rgb(image_bytes: bytes):
    # Normalize potentially malformed/truncated images into a consistent RGB PIL image.
    from PIL import Image, ImageFile, ImageOps

    if not image_bytes:
        raise ValueError("empty image bytes")

    ImageFile.LOAD_TRUNCATED_IMAGES = True
    with Image.open(io.BytesIO(image_bytes)) as opened:
        opened.load()
        image = ImageOps.exif_transpose(opened)
        if image.mode != "RGB":
            image = image.convert("RGB")
    return image


def run_worker_mode() -> int:
    backend = None
    backend_error = ""
    try:
        backend = pick_backend()
    except Exception as exc:
        backend_error = str(exc)

    for raw in sys.stdin:
        line = raw.strip()
        if not line:
            continue
        req = {}
        req_id = ""
        model_id = "google/siglip2-base-patch16-224"
        device = "cpu"
        image_size_bytes = 0
        error_stage = "request_parse"
        try:
            req = json.loads(line)
            req_id = str(req.get("request_id", ""))
            op = str(req.get("op", "embed"))
            model_id = str(req.get("model_id", "google/siglip2-base-patch16-224"))
            device = str(req.get("device", "cpu"))

            if backend_error:
                raise RuntimeError(backend_error)
            if backend is None:
                raise RuntimeError("backend not initialized")

            if op == "ping":
                resp = {
                    "request_id": req_id,
                    "ok": True,
                    "model": backend.name,
                }
            else:
                error_stage = "request_parse"
                image_b64 = req.get("image_b64", "")
                if not image_b64:
                    raise ValueError("missing image_b64")
                image_bytes = base64.b64decode(image_b64, validate=True)
                image_size_bytes = len(image_bytes)
                error_stage = "decode"
                image = decode_image_rgb(image_bytes)
                error_stage = "inference"
                vector, used_model = backend.embed(image, model_id, device)
                error_stage = ""
                resp = {
                    "request_id": req_id,
                    "ok": True,
                    "model": used_model,
                    "embedding": vector,
                }
        except Exception as exc:
            tb = traceback.format_exc()
            sys.stderr.write(tb + "\n")
            sys.stderr.flush()
            resp = {
                "request_id": req_id,
                "ok": False,
                "error": f"{type(exc).__name__}: {exc}",
                "error_stage": error_stage,
                "backend": backend.name if backend is not None else "",
                "model_id": model_id,
                "device": device,
                "image_size_bytes": image_size_bytes,
                "traceback": tb,
            }
        sys.stdout.write(json.dumps(resp, separators=(",", ":")) + "\n")
        sys.stdout.flush()

    return 0


def run_debug_mode() -> int:
    image_bytes = sys.stdin.buffer.read()
    if not image_bytes:
        sys.stdout.write(json.dumps({"ok": False, "error": "missing stdin image bytes"}) + "\n")
        return 1

    try:
        backend = pick_backend()
        model_id = os.environ.get("SIGLIP2_MODEL_ID", "google/siglip2-base-patch16-224")
        device = os.environ.get("SIGLIP2_DEVICE", "cpu")
        image = decode_image_rgb(image_bytes)
        vector, used_model = backend.embed(image, model_id, device)
        sys.stdout.write(json.dumps({"ok": True, "model": used_model, "embedding": vector}, separators=(",", ":")) + "\n")
        return 0
    except Exception as exc:
        sys.stdout.write(json.dumps({"ok": False, "error": str(exc)}) + "\n")
        return 1


def main() -> int:
    mode = os.environ.get("RECO_WORKER_MODE", "").strip().lower()
    if mode == "worker":
        return run_worker_mode()
    return run_debug_mode()


if __name__ == "__main__":
    raise SystemExit(main())
