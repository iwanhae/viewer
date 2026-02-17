use anyhow::{anyhow, Context, Result};
use base64::engine::general_purpose::STANDARD;
use base64::Engine;
use candle::{DType, Device, Tensor};
use candle_nn::VarBuilder;
use candle_transformers::models::siglip;
use serde::{Deserialize, Serialize};
use std::collections::HashMap;
use std::io::{self, BufRead, Read, Write};

const DEFAULT_MODEL_ID: &str = "google/siglip2-base-patch16-224";
const DEFAULT_DEVICE: &str = "cpu";
const BACKEND_NAME: &str = "siglip2-candle";

#[derive(Debug, Deserialize)]
struct EmbedRequest {
    #[serde(default)]
    request_id: String,
    #[serde(default = "default_op")]
    op: String,
    #[serde(default = "default_model_id")]
    model_id: String,
    #[serde(default = "default_device")]
    device: String,
    #[serde(default)]
    image_b64: String,
}

#[derive(Debug, Serialize)]
struct EmbedResponse {
    request_id: String,
    ok: bool,
    #[serde(skip_serializing_if = "Option::is_none")]
    error: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    error_stage: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    traceback: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    backend: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    model: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    model_id: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    device: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    image_size_bytes: Option<usize>,
    #[serde(skip_serializing_if = "Option::is_none")]
    embedding: Option<Vec<f32>>,
}

#[derive(Debug, Serialize)]
struct DebugResponse {
    ok: bool,
    #[serde(skip_serializing_if = "Option::is_none")]
    error: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    model: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    embedding: Option<Vec<f32>>,
}

#[derive(Debug)]
struct LoadedModel {
    model: siglip::Model,
    config: siglip::Config,
}

#[derive(Debug, Default)]
struct Worker {
    models: HashMap<String, LoadedModel>,
}

impl Worker {
    fn ensure_model(&mut self, model_id: &str) -> Result<&LoadedModel> {
        if !self.models.contains_key(model_id) {
            let loaded = load_model(model_id)?;
            self.models.insert(model_id.to_string(), loaded);
        }
        self.models
            .get(model_id)
            .context("model cache lookup failed after load")
    }

    fn embed(
        &mut self,
        image_bytes: &[u8],
        model_id: &str,
        device: &str,
    ) -> Result<(Vec<f32>, String)> {
        ensure_supported_device(device)?;
        let loaded = self.ensure_model(model_id)?;
        let pixel_values = preprocess_image(image_bytes, loaded.config.vision_config.image_size)?;
        let features = loaded
            .model
            .get_image_features(&pixel_values)
            .context("run image encoder")?;
        let embedding = features
            .flatten_all()
            .context("flatten image features")?
            .to_vec1::<f32>()
            .context("convert image features to vector")?;
        Ok((embedding, format!("{BACKEND_NAME}:{model_id}")))
    }
}

fn default_op() -> String {
    "embed".to_string()
}

fn default_model_id() -> String {
    DEFAULT_MODEL_ID.to_string()
}

fn default_device() -> String {
    DEFAULT_DEVICE.to_string()
}

fn ensure_supported_device(device: &str) -> Result<()> {
    let normalized = device.trim();
    if normalized.is_empty() || normalized.eq_ignore_ascii_case("cpu") {
        return Ok(());
    }
    Err(anyhow!(
        "unsupported device '{normalized}', only cpu is supported in this build"
    ))
}

fn load_model(model_id: &str) -> Result<LoadedModel> {
    let api = hf_hub::api::sync::Api::new().context("create Hugging Face API client")?;
    let repo = api.model(model_id.to_string());
    let model_file = repo
        .get("model.safetensors")
        .with_context(|| format!("download model.safetensors for {model_id}"))?;
    let config_file = repo
        .get("config.json")
        .with_context(|| format!("download config.json for {model_id}"))?;

    let config_bytes = std::fs::read(&config_file)
        .with_context(|| format!("read config file {}", config_file.display()))?;
    let config: siglip::Config =
        serde_json::from_slice(&config_bytes).context("parse SigLIP config.json")?;

    let device = Device::Cpu;
    let vb = unsafe {
        VarBuilder::from_mmaped_safetensors(std::slice::from_ref(&model_file), DType::F32, &device)
    }
    .with_context(|| format!("load model weights from {}", model_file.display()))?;
    let model = siglip::Model::new(&config, vb).context("initialize SigLIP model")?;

    Ok(LoadedModel { model, config })
}

fn preprocess_image(image_bytes: &[u8], image_size: usize) -> Result<Tensor> {
    let image = image::load_from_memory(image_bytes).context("decode image")?;
    let image = image.resize_to_fill(
        image_size as u32,
        image_size as u32,
        image::imageops::FilterType::Triangle,
    );
    let pixels = image.to_rgb8().into_raw();

    let tensor = Tensor::from_vec(pixels, (image_size, image_size, 3), &Device::Cpu)
        .context("build image tensor")?
        .permute((2, 0, 1))
        .context("permute image tensor to CHW")?
        .to_dtype(DType::F32)
        .context("convert image tensor to float32")?
        .affine(2. / 255., -1.)
        .context("normalize image tensor")?
        .unsqueeze(0)
        .context("add image batch dimension")?;
    Ok(tensor)
}

fn success_response(req: &EmbedRequest, model: String, embedding: Vec<f32>) -> EmbedResponse {
    EmbedResponse {
        request_id: req.request_id.clone(),
        ok: true,
        error: None,
        error_stage: None,
        traceback: None,
        backend: Some(BACKEND_NAME.to_string()),
        model: Some(model),
        model_id: Some(req.model_id.clone()),
        device: Some(req.device.clone()),
        image_size_bytes: None,
        embedding: Some(embedding),
    }
}

fn ping_response(req: &EmbedRequest) -> EmbedResponse {
    EmbedResponse {
        request_id: req.request_id.clone(),
        ok: true,
        error: None,
        error_stage: None,
        traceback: None,
        backend: Some(BACKEND_NAME.to_string()),
        model: Some(BACKEND_NAME.to_string()),
        model_id: Some(req.model_id.clone()),
        device: Some(req.device.clone()),
        image_size_bytes: None,
        embedding: None,
    }
}

fn error_response(
    req_id: String,
    err: anyhow::Error,
    error_stage: &str,
    model_id: String,
    device: String,
    image_size_bytes: usize,
) -> EmbedResponse {
    EmbedResponse {
        request_id: req_id,
        ok: false,
        error: Some(err.to_string()),
        error_stage: Some(error_stage.to_string()),
        traceback: None,
        backend: Some(BACKEND_NAME.to_string()),
        model: None,
        model_id: Some(model_id),
        device: Some(device),
        image_size_bytes: if image_size_bytes > 0 {
            Some(image_size_bytes)
        } else {
            None
        },
        embedding: None,
    }
}

fn write_json_line<T: Serialize>(stdout: &mut io::Stdout, value: &T) -> Result<()> {
    let body = serde_json::to_string(value).context("serialize JSON response")?;
    stdout
        .write_all(body.as_bytes())
        .context("write JSON response")?;
    stdout.write_all(b"\n").context("write newline")?;
    stdout.flush().context("flush response")?;
    Ok(())
}

fn run_worker_mode() -> i32 {
    let stdin = io::stdin();
    let mut stdout = io::stdout();
    let mut worker = Worker::default();

    for line in stdin.lock().lines() {
        let line = match line {
            Ok(line) => line,
            Err(err) => {
                eprintln!("failed to read stdin: {err}");
                return 1;
            }
        };
        let line = line.trim();
        if line.is_empty() {
            continue;
        }

        let mut req_id = String::new();
        let mut model_id = default_model_id();
        let mut device = default_device();
        let mut image_size_bytes = 0usize;
        let mut error_stage = "request_parse";

        let response = match serde_json::from_str::<EmbedRequest>(line) {
            Ok(mut req) => {
                if req.model_id.trim().is_empty() {
                    req.model_id = default_model_id();
                }
                if req.device.trim().is_empty() {
                    req.device = default_device();
                }

                req_id = req.request_id.clone();
                model_id = req.model_id.clone();
                device = req.device.clone();

                let op = req.op.trim().to_ascii_lowercase();
                if op == "ping" {
                    ping_response(&req)
                } else if op != "embed" {
                    error_response(
                        req_id.clone(),
                        anyhow!("unsupported op '{op}'"),
                        error_stage,
                        model_id.clone(),
                        device.clone(),
                        image_size_bytes,
                    )
                } else {
                    error_stage = "decode";
                    match STANDARD.decode(req.image_b64.as_bytes()) {
                        Ok(image_bytes) => {
                            image_size_bytes = image_bytes.len();
                            error_stage = "inference";
                            match worker.embed(&image_bytes, &model_id, &device) {
                                Ok((embedding, used_model)) => {
                                    success_response(&req, used_model, embedding)
                                }
                                Err(err) => error_response(
                                    req_id.clone(),
                                    err,
                                    error_stage,
                                    model_id.clone(),
                                    device.clone(),
                                    image_size_bytes,
                                ),
                            }
                        }
                        Err(err) => error_response(
                            req_id.clone(),
                            anyhow!("decode image_b64: {err}"),
                            error_stage,
                            model_id.clone(),
                            device.clone(),
                            image_size_bytes,
                        ),
                    }
                }
            }
            Err(err) => error_response(
                req_id.clone(),
                anyhow!("parse request JSON: {err}"),
                error_stage,
                model_id.clone(),
                device.clone(),
                image_size_bytes,
            ),
        };

        if let Err(err) = write_json_line(&mut stdout, &response) {
            eprintln!("failed to write stdout: {err}");
            return 1;
        }
    }

    0
}

fn run_debug_mode() -> i32 {
    let mut input = Vec::new();
    if let Err(err) = io::stdin().read_to_end(&mut input) {
        let _ = print_debug_response(DebugResponse {
            ok: false,
            error: Some(format!("read stdin: {err}")),
            model: None,
            embedding: None,
        });
        return 1;
    }
    if input.is_empty() {
        let _ = print_debug_response(DebugResponse {
            ok: false,
            error: Some("missing stdin image bytes".to_string()),
            model: None,
            embedding: None,
        });
        return 1;
    }

    let model_id =
        std::env::var("SIGLIP2_MODEL_ID").unwrap_or_else(|_| DEFAULT_MODEL_ID.to_string());
    let device = std::env::var("SIGLIP2_DEVICE").unwrap_or_else(|_| DEFAULT_DEVICE.to_string());
    let mut worker = Worker::default();
    match worker.embed(&input, &model_id, &device) {
        Ok((embedding, model)) => {
            let _ = print_debug_response(DebugResponse {
                ok: true,
                error: None,
                model: Some(model),
                embedding: Some(embedding),
            });
            0
        }
        Err(err) => {
            let _ = print_debug_response(DebugResponse {
                ok: false,
                error: Some(err.to_string()),
                model: None,
                embedding: None,
            });
            1
        }
    }
}

fn print_debug_response(resp: DebugResponse) -> Result<()> {
    let body = serde_json::to_string(&resp).context("serialize debug response")?;
    println!("{body}");
    Ok(())
}

fn main() {
    let mode = std::env::var("RECO_WORKER_MODE").unwrap_or_default();
    let exit_code = if mode.eq_ignore_ascii_case("worker") {
        run_worker_mode()
    } else {
        run_debug_mode()
    };
    std::process::exit(exit_code);
}
