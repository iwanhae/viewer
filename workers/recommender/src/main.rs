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
    headers: HashMap<String, String>,
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
    Err(anyhow::anyhow!(
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

    let mut headers = HashMap::new();
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
                content_length = value.parse().unwrap_or(0);
            }
            headers.insert(key, value);
        }
    }

    let mut body = vec![0u8; content_length];
    if content_length > 0 {
        reader.read_exact(&mut body)?;
    }

    Ok(HttpRequest {
        method,
        path,
        headers,
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

fn handle_embed(worker: &Arc<Mutex<Worker>>, body: &[u8]) -> Result<HttpResponse> {
    let mut req = parse_embed_request(body)?;
    req.model_id = req.model_id.trim().to_string();
    if req.model_id.is_empty() {
        req.model_id = default_model_id();
    }
    req.device = req.device.trim().to_string();
    if req.device.is_empty() {
        req.device = default_device();
    }
    let op = req.op.to_ascii_lowercase();

    if op == "ping" {
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

    let decoded = STANDARD.decode(req.image_b64.as_bytes()).map_err(|err| {
        anyhow::anyhow!("decode image_b64 for request {}: {err}", req.request_id)
    })?;
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

fn handle_healthz(worker: &Arc<Mutex<Worker>>) -> Result<HttpResponse> {
    let model_id = default_model_id();
    let device = default_device();
    let req = EmbedRequest {
        request_id: "healthz".to_string(),
        op: "ping".to_string(),
        model_id: model_id.clone(),
        device: device.clone(),
        image_b64: String::new(),
    };
    let mut guard = worker.lock().map_err(|_| anyhow::anyhow!("worker lock poisoned"))?;
    if let Err(err) = guard.ensure_model(&model_id) {
        return Ok(write_json_response(
            req.request_id.clone(),
            error_response(
                req.request_id,
                err,
                "init",
                model_id.clone(),
                device.clone(),
                0,
            ),
        )?);
    }
    Ok(write_json_response(
        "healthz".to_string(),
        ping_response(&req),
    )?)
}

fn handle_client(mut stream: TcpStream, worker: Arc<Mutex<Worker>>) {
    let response = match parse_request(&mut stream) {
        Ok(req) => match (req.method.as_str(), req.path.as_str()) {
            ("GET", "/ping") | ("GET", "/healthz") => {
                handle_healthz(&worker).unwrap_or_else(|err| {
                    let resp = error_response(
                        "system".to_string(),
                        err,
                        "healthz",
                        default_model_id(),
                        default_device(),
                        0,
                    );
                    write_json_response("system".to_string(), resp).unwrap_or_else(|write_err| {
                        HttpResponse {
                            status: 500,
                            reason: "Internal Server Error",
                            body: write_err.to_string().into_bytes(),
                        }
                    })
                })
            }
            ("POST", "/embed") => {
                match handle_embed(&worker, &req.body) {
                    Ok(response) => response,
                    Err(err) => {
                        let resp = error_response(
                            "request".to_string(),
                            err,
                            "request",
                            default_model_id(),
                            default_device(),
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
    let listen_addr = env::var("RECOMMENDER_LISTEN_ADDR").unwrap_or_else(|_| DEFAULT_LISTEN_ADDR.to_string());
    let startup_model_id = env::var("SIGLIP2_MODEL_ID").unwrap_or_else(|_| DEFAULT_MODEL_ID.to_string());
    let startup_device = env::var("SIGLIP2_DEVICE").unwrap_or_else(|_| DEFAULT_DEVICE.to_string());

    let mut worker = Worker::default();
    if let Err(err) = worker.ensure_model(&startup_model_id) {
        eprintln!("failed to initialize startup model '{startup_model_id}': {err}");
        std::process::exit(1);
    }
    if let Err(err) = ensure_supported_device(&startup_device) {
        eprintln!("unsupported startup device '{startup_device}': {err}");
        std::process::exit(1);
    }

    let listener = match TcpListener::bind(&listen_addr) {
        Ok(listener) => listener,
        Err(err) => {
            eprintln!("failed to bind {listen_addr}: {err}");
            std::process::exit(1);
        }
    };
    let worker = Arc::new(Mutex::new(worker));
    for stream in listener.incoming() {
        match stream {
            Ok(stream) => {
                let worker = Arc::clone(&worker);
                thread::spawn(move || handle_client(stream, worker));
            }
            Err(err) => {
                eprintln!("failed to accept connection: {err}");
            }
        }
    }
}
