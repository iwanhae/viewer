#!/usr/bin/env python3
import base64
import io
import json
import os
import sys
from typing import List, Tuple

MEAN = [0.48145466, 0.4578275, 0.40821073]
STD = [0.26862954, 0.26130258, 0.27577711]


class Backend:
    name = "unknown"

    def embed(self, image_bytes: bytes, model_id: str, device: str) -> Tuple[List[float], str]:
        raise RuntimeError("backend is not initialized")


class ONNXBackend(Backend):
    def __init__(self):
        import numpy as np
        import onnxruntime as ort
        from PIL import Image

        self.np = np
        self.Image = Image

        model_path = os.environ.get("SIGLIP2_ONNX_PATH", "")
        if not model_path:
            raise RuntimeError("SIGLIP2_ONNX_PATH is not set")

        providers = ["CPUExecutionProvider"]
        self.session = ort.InferenceSession(model_path, providers=providers)
        self.input_name = self.session.get_inputs()[0].name
        self.name = f"siglip2-onnx:{os.path.basename(model_path)}"

    def preprocess(self, image_bytes: bytes):
        img = self.Image.open(io.BytesIO(image_bytes)).convert("RGB").resize((224, 224))
        arr = self.np.asarray(img).astype("float32") / 255.0
        arr = (arr - self.np.array(MEAN, dtype="float32")) / self.np.array(STD, dtype="float32")
        arr = self.np.transpose(arr, (2, 0, 1))
        arr = self.np.expand_dims(arr, axis=0)
        return arr

    def embed(self, image_bytes: bytes, model_id: str, device: str) -> Tuple[List[float], str]:
        pixel_values = self.preprocess(image_bytes)
        output = self.session.run(None, {self.input_name: pixel_values})[0]
        vec = output.reshape(-1).astype("float32").tolist()
        return vec, self.name


class TransformersBackend(Backend):
    def __init__(self):
        from PIL import Image
        import torch
        from transformers import AutoModel, AutoProcessor

        self.Image = Image
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

    def embed(self, image_bytes: bytes, model_id: str, device: str) -> Tuple[List[float], str]:
        model, processor, use_device = self._load(model_id, device)
        image = self.Image.open(io.BytesIO(image_bytes)).convert("RGB")
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


def main() -> int:
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
        try:
            req = json.loads(line)
            req_id = str(req.get("request_id", ""))
            image_b64 = req.get("image_b64", "")
            model_id = str(req.get("model_id", "google/siglip2-base-patch16-224"))
            device = str(req.get("device", "cpu"))
            if not image_b64:
                raise ValueError("missing image_b64")
            if backend_error:
                raise RuntimeError(backend_error)
            if backend is None:
                raise RuntimeError("backend not initialized")

            image_bytes = base64.b64decode(image_b64)
            vector, used_model = backend.embed(image_bytes, model_id, device)
            resp = {
                "request_id": req_id,
                "ok": True,
                "model": used_model,
                "embedding": vector,
            }
        except Exception as exc:
            resp = {
                "request_id": req.get("request_id", "") if 'req' in locals() else "",
                "ok": False,
                "error": str(exc),
            }
        sys.stdout.write(json.dumps(resp, separators=(",", ":")) + "\n")
        sys.stdout.flush()

    return 0


if __name__ == "__main__":
    raise SystemExit(main())
