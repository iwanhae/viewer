use anyhow::{Context, Result};
use base64::engine::general_purpose::STANDARD;
use base64::Engine;
use candle::{DType, Device, Tensor};
use candle_nn::VarBuilder;
use candle_transformers::models::siglip;
use serde::{Deserialize, Serialize};
use std::collections::HashMap;
use std::env;
use std::io::{BufRead, BufReader, Read, Write};
use std::net::{TcpListener, TcpStream};
use std::sync::{Arc, Mutex};
use std::thread;

const DEFAULT_LISTEN_ADDR: &str = "0.0.0.0:18081";
const DEFAULT_MODEL_ID: &str = "google/siglip2-base-patch16-224";
const DEFAULT_DEVICE: &str = "cpu";
const BACKEND_NAME: &str = "siglip2-candle";

#[derive(Debug, Clone)]
struct RuntimeConfig {
    listen_addr: String,
    model_id: String,
    device: String,
}

impl RuntimeConfig {
    fn from_env() -> Self {
        Self {
            listen_addr: env_or_default("RECOMMENDER_LISTEN_ADDR", DEFAULT_LISTEN_ADDR),
            model_id: env_or_default("SIGLIP2_MODEL_ID", DEFAULT_MODEL_ID),
            device: env_or_default("SIGLIP2_DEVICE", DEFAULT_DEVICE),
        }
    }
}

#[derive(Debug, Deserialize)]
struct EmbedRequest {
    #[serde(default)]
    request_id: String,
    #[serde(default = "default_op")]
    op: String,
    #[serde(default)]
    model_id: String,
    #[serde(default)]
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

    fn model(&self, model_id: &str) -> Result<&LoadedModel> {
        self.models
            .get(model_id)
            .with_context(|| format!("model '{model_id}' is not initialized"))
    }

    fn embed(
        &self,
        image_bytes: &[u8],
        model_id: &str,
        device: &str,
    ) -> Result<(Vec<f32>, String)> {
        ensure_supported_device(device)?;
        let loaded = self.model(model_id)?;
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

#[derive(Debug)]
struct HttpRequest {
    method: String,
    path: String,
    body: Vec<u8>,
}

#[derive(Debug)]
struct HttpResponse {
    status: u16,
    reason: &'static str,
    body: Vec<u8>,
}

fn default_op() -> String {
    "embed".to_string()
}

fn env_or_default(key: &str, fallback: &str) -> String {
    match env::var(key) {
        Ok(value) => {
            let trimmed = value.trim();
            if trimmed.is_empty() {
                fallback.to_string()
            } else {
                trimmed.to_string()
            }
        }
        Err(_) => fallback.to_string(),
    }
}

fn ensure_supported_device(device: &str) -> Result<()> {
    let normalized = device.trim();
    if normalized.is_empty() || normalized.eq_ignore_ascii_case("cpu") {
        return Ok(());
    }
    Err(anyhow::anyhow!(
        "unsupported device '{normalized}', only cpu is supported in this build"
    ))
}

fn log_error_chain(prefix: &str, err: &anyhow::Error) {
    eprintln!("{prefix}: {err}");
    for (idx, cause) in err.chain().skip(1).enumerate() {
        eprintln!("  caused_by[{}]: {}", idx + 1, cause);
    }
}

fn load_model(model_id: &str) -> Result<LoadedModel> {
    let api = hf_hub::api::sync::ApiBuilder::from_env()
        .with_progress(false)
        .build()
        .context("create Hugging Face API client")?;
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

fn parse_request(stream: &mut TcpStream) -> Result<HttpRequest> {
    let mut reader = BufReader::new(stream);
    let mut request_line = String::new();
    reader.read_line(&mut request_line)?;
    if request_line.trim().is_empty() {
        return Err(anyhow::anyhow!("empty request line"));
    }
    let mut parts = request_line.split_whitespace();
    let method = parts.next().context("request method missing")?.to_string();
    let target = parts.next().context("request path missing")?;
    let path = target.split('?').next().unwrap_or("/").trim().to_string();
    let _version = parts.next();

    let mut content_length = 0usize;
    loop {
        let mut line = String::new();
        reader.read_line(&mut line)?;
        let trimmed = line.trim_end();
        if trimmed.is_empty() {
            break;
        }
        if let Some((name, value)) = trimmed.split_once(':') {
            let key = name.trim().to_lowercase();
            let value = value.trim().to_string();
            if key.eq_ignore_ascii_case("content-length") {
                content_length = value
                    .parse::<usize>()
                    .with_context(|| format!("invalid content-length header '{value}'"))?;
            }
        }
    }

    let mut body = vec![0u8; content_length];
    if content_length > 0 {
        reader.read_exact(&mut body)?;
    }

    Ok(HttpRequest {
        method,
        path,
        body,
    })
}

fn response_to_string(response: &HttpResponse) -> String {
    format!(
        "HTTP/1.1 {} {}\r\n\
         Content-Type: application/json\r\n\
         Content-Length: {}\r\n\
         Connection: close\r\n\
         \r\n{}",
        response.status,
        response.reason,
        response.body.len(),
        String::from_utf8_lossy(&response.body),
    )
}

fn send_response(stream: &mut TcpStream, response: HttpResponse) -> Result<()> {
    let body = response_to_string(&response);
    stream.write_all(body.as_bytes())?;
    stream.flush()?;
    Ok(())
}

fn parse_embed_request(body: &[u8]) -> Result<EmbedRequest> {
    serde_json::from_slice::<EmbedRequest>(body).context("parse /embed request body")
}

fn apply_request_defaults(req: &mut EmbedRequest, config: &RuntimeConfig) {
    req.model_id = req.model_id.trim().to_string();
    if req.model_id.is_empty() {
        req.model_id = config.model_id.clone();
    }

    req.device = req.device.trim().to_string();
    if req.device.is_empty() {
        req.device = config.device.clone();
    }
}

fn decode_image_b64(raw: &str) -> Result<Vec<u8>> {
    let trimmed = raw.trim();
    if trimmed.is_empty() {
        return Err(anyhow::anyhow!("image_b64 is required for op=embed"));
    }

    let encoded = match trimmed.split_once(',') {
        Some((prefix, data)) if prefix.to_ascii_lowercase().contains(";base64") => data.trim(),
        _ => trimmed,
    };
    STANDARD
        .decode(encoded.as_bytes())
        .context("decode image_b64 payload")
}

fn write_json_response(req_id: String, response: EmbedResponse) -> Result<HttpResponse> {
    let mut response = response;
    response.request_id = req_id;
    let body = serde_json::to_vec(&response).context("serialize JSON response")?;
    Ok(HttpResponse {
        status: if response.ok { 200 } else { 500 },
        reason: if response.ok { "OK" } else { "Internal Server Error" },
        body,
    })
}

fn handle_embed(worker: &Arc<Mutex<Worker>>, config: &RuntimeConfig, body: &[u8]) -> Result<HttpResponse> {
    let mut req = parse_embed_request(body)?;
    apply_request_defaults(&mut req, config);
    let op = req.op.to_ascii_lowercase();

    if op == "ping" {
        if let Err(err) = ensure_supported_device(&req.device) {
            return Ok(write_json_response(
                req.request_id.clone(),
                error_response(
                    req.request_id,
                    err,
                    "request",
                    req.model_id.clone(),
                    req.device.clone(),
                    0,
                ),
            )?);
        }
        let mut guard = worker.lock().map_err(|_| anyhow::anyhow!("worker lock poisoned"))?;
        if let Err(err) = guard.ensure_model(&req.model_id) {
            return Ok(write_json_response(
                req.request_id.clone(),
                error_response(
                    req.request_id,
                    err,
                    "init",
                    req.model_id.clone(),
                    req.device.clone(),
                    0,
                ),
            )?);
        }
        return Ok(write_json_response(
            req.request_id.clone(),
            ping_response(&req),
        )?);
    }
    if op != "embed" {
        return Ok(write_json_response(
            req.request_id.clone(),
            error_response(
                req.request_id,
                anyhow::anyhow!("unsupported op '{op}'"),
                "request",
                req.model_id.clone(),
                req.device.clone(),
                0,
            ),
        )?);
    }

    let decoded = match decode_image_b64(&req.image_b64) {
        Ok(decoded) => decoded,
        Err(err) => {
            return Ok(write_json_response(
                req.request_id.clone(),
                error_response(
                    req.request_id,
                    err,
                    "decode",
                    req.model_id.clone(),
                    req.device.clone(),
                    0,
                ),
            )?);
        }
    };
    let image_size_bytes = decoded.len();

    let mut guard = worker.lock().map_err(|_| anyhow::anyhow!("worker lock poisoned"))?;
    if let Err(err) = guard.ensure_model(&req.model_id) {
        return Ok(write_json_response(
            req.request_id.clone(),
            error_response(
                req.request_id,
                err,
                "init",
                req.model_id.clone(),
                req.device.clone(),
                image_size_bytes,
            ),
        )?);
    }
    let request_id = req.request_id.clone();
    let result = guard.embed(&decoded, &req.model_id, &req.device);
    match result {
        Ok((embedding, model_id)) => Ok(write_json_response(
            request_id,
            success_response(&req, model_id, embedding),
        )?),
        Err(err) => Ok(write_json_response(
            request_id.clone(),
            error_response(
                request_id,
                err,
                "inference",
                req.model_id.clone(),
                req.device.clone(),
                image_size_bytes,
            ),
        )?),
    }
}

fn handle_ping_or_healthz(
    worker: &Arc<Mutex<Worker>>,
    config: &RuntimeConfig,
    request_id: &str,
) -> Result<HttpResponse> {
    ensure_supported_device(&config.device).context("validate startup device")?;

    let req = EmbedRequest {
        request_id: request_id.to_string(),
        op: "ping".to_string(),
        model_id: config.model_id.clone(),
        device: config.device.clone(),
        image_b64: String::new(),
    };
    let mut guard = worker.lock().map_err(|_| anyhow::anyhow!("worker lock poisoned"))?;
    guard
        .ensure_model(&config.model_id)
        .with_context(|| format!("initialize model '{}'", config.model_id))?;
    Ok(write_json_response(
        request_id.to_string(),
        ping_response(&req),
    )?)
}

fn handle_client(mut stream: TcpStream, worker: Arc<Mutex<Worker>>, config: Arc<RuntimeConfig>) {
    let response = match parse_request(&mut stream) {
        Ok(req) => match (req.method.as_str(), req.path.as_str()) {
            ("GET", "/ping") => {
                handle_ping_or_healthz(&worker, &config, "ping").unwrap_or_else(|err| {
                    let resp = error_response(
                        "ping".to_string(),
                        err,
                        "ping",
                        config.model_id.clone(),
                        config.device.clone(),
                        0,
                    );
                    write_json_response("ping".to_string(), resp).unwrap_or_else(|write_err| {
                        HttpResponse {
                            status: 500,
                            reason: "Internal Server Error",
                            body: write_err.to_string().into_bytes(),
                        }
                    })
                })
            }
            ("GET", "/healthz") => {
                handle_ping_or_healthz(&worker, &config, "healthz").unwrap_or_else(|err| {
                    let resp = error_response(
                        "healthz".to_string(),
                        err,
                        "healthz",
                        config.model_id.clone(),
                        config.device.clone(),
                        0,
                    );
                    write_json_response("healthz".to_string(), resp).unwrap_or_else(|write_err| {
                        HttpResponse {
                            status: 500,
                            reason: "Internal Server Error",
                            body: write_err.to_string().into_bytes(),
                        }
                    })
                })
            }
            ("POST", "/embed") => {
                match handle_embed(&worker, &config, &req.body) {
                    Ok(response) => response,
                    Err(err) => {
                        let resp = error_response(
                            "request".to_string(),
                            err,
                            "request",
                            config.model_id.clone(),
                            config.device.clone(),
                            req.body.len(),
                        );
                        write_json_response("request".to_string(), resp).unwrap_or_else(|write_err| {
                            HttpResponse {
                                status: 500,
                                reason: "Internal Server Error",
                                body: write_err.to_string().into_bytes(),
                            }
                        })
                    }
                }
            }
            _ => HttpResponse {
                status: 404,
                reason: "Not Found",
                body: b"{\"ok\":false,\"error\":\"not found\"}".to_vec(),
            },
        },
        Err(err) => {
            let body = format!("{{\"ok\":false,\"error\":\"{err}\"}}");
            HttpResponse {
                status: 400,
                reason: "Bad Request",
                body: body.into_bytes(),
            }
        }
    };

    if let Err(err) = send_response(&mut stream, response) {
        eprintln!("failed to write response: {err}");
    }
}

fn main() {
    let config = RuntimeConfig::from_env();

    if let Err(err) = ensure_supported_device(&config.device) {
        eprintln!("unsupported startup device '{}': {err}", config.device);
        std::process::exit(1);
    }

    let mut worker = Worker::default();
    if let Err(err) = worker.ensure_model(&config.model_id) {
        let hf_home = env::var("HF_HOME").unwrap_or_else(|_| "<default>".to_string());
        log_error_chain(
            &format!(
                "failed to initialize model '{}' during startup (HF_HOME={hf_home})",
                config.model_id
            ),
            &err,
        );
        std::process::exit(1);
    }

    let listener = match TcpListener::bind(&config.listen_addr) {
        Ok(listener) => listener,
        Err(err) => {
            eprintln!("failed to bind {}: {err}", config.listen_addr);
            std::process::exit(1);
        }
    };
    eprintln!(
        "recommender listening on {} with model '{}' (device '{}')",
        config.listen_addr, config.model_id, config.device
    );

    let worker = Arc::new(Mutex::new(worker));
    let config = Arc::new(config);
    for stream in listener.incoming() {
        match stream {
            Ok(stream) => {
                let worker = Arc::clone(&worker);
                let config = Arc::clone(&config);
                thread::spawn(move || handle_client(stream, worker, config));
            }
            Err(err) => {
                eprintln!("failed to accept connection: {err}");
            }
        }
    }
}
