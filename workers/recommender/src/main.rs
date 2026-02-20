use anyhow::{Context, Result};
use base64::engine::general_purpose::STANDARD;
use base64::Engine;
use candle::{DType, Device, Tensor};
use candle_nn::VarBuilder;
use candle_transformers::models::siglip;
use serde::{Deserialize, Serialize};
use std::collections::{hash_map::Entry, HashMap};
use std::env;
use std::io::{BufRead, BufReader, Read, Write};
use std::net::{TcpListener, TcpStream};
use std::sync::{Arc, RwLock};
use std::thread;
use std::time::Instant;
use tracing::{debug, error, info, warn};
use tracing_subscriber::EnvFilter;

#[cfg(feature = "mkl")]
#[allow(clippy::too_many_arguments)]
#[no_mangle]
// Candle's MKL backend references `hgemm_`; provide a local fallback for MKL toolchains
// that do not export this symbol.
pub unsafe extern "C" fn hgemm_(
    transa: *const std::ffi::c_char,
    transb: *const std::ffi::c_char,
    m: *const std::ffi::c_int,
    n: *const std::ffi::c_int,
    k: *const std::ffi::c_int,
    alpha: *const half::f16,
    a: *const half::f16,
    lda: *const std::ffi::c_int,
    b: *const half::f16,
    ldb: *const std::ffi::c_int,
    beta: *const half::f16,
    c: *mut half::f16,
    ldc: *const std::ffi::c_int,
) {
    if transa.is_null()
        || transb.is_null()
        || m.is_null()
        || n.is_null()
        || k.is_null()
        || alpha.is_null()
        || a.is_null()
        || lda.is_null()
        || b.is_null()
        || ldb.is_null()
        || beta.is_null()
        || c.is_null()
        || ldc.is_null()
    {
        return;
    }

    let transa = ((*transa as u8) as char).to_ascii_uppercase();
    let transb = ((*transb as u8) as char).to_ascii_uppercase();
    let transa_n = match transa {
        'N' => true,
        'T' | 'C' => false,
        _ => return,
    };
    let transb_n = match transb {
        'N' => true,
        'T' | 'C' => false,
        _ => return,
    };

    let (m, n, k) = (*m, *n, *k);
    let (lda, ldb, ldc) = (*lda, *ldb, *ldc);
    if m < 0 || n < 0 || k < 0 || lda <= 0 || ldb <= 0 || ldc <= 0 {
        return;
    }
    let (m, n, k, lda, ldb, ldc) = (
        m as usize,
        n as usize,
        k as usize,
        lda as usize,
        ldb as usize,
        ldc as usize,
    );
    if m == 0 || n == 0 {
        return;
    }
    if (transa_n && lda < m) || (!transa_n && lda < k) {
        return;
    }
    if (transb_n && ldb < k) || (!transb_n && ldb < n) {
        return;
    }
    if ldc < m {
        return;
    }

    let a_cols = if transa_n { k } else { m };
    let b_cols = if transb_n { n } else { k };
    let Some(a_len) = lda.checked_mul(a_cols) else {
        return;
    };
    let Some(b_len) = ldb.checked_mul(b_cols) else {
        return;
    };
    let Some(c_len) = ldc.checked_mul(n) else {
        return;
    };

    let a = std::slice::from_raw_parts(a, a_len);
    let b = std::slice::from_raw_parts(b, b_len);
    let c = std::slice::from_raw_parts_mut(c, c_len);
    let alpha = (*alpha).to_f32();
    let beta = (*beta).to_f32();

    for col in 0..n {
        for row in 0..m {
            let mut acc = 0f32;
            for depth in 0..k {
                let a_idx = if transa_n {
                    row + depth * lda
                } else {
                    depth + row * lda
                };
                let b_idx = if transb_n {
                    depth + col * ldb
                } else {
                    col + depth * ldb
                };
                acc += a[a_idx].to_f32() * b[b_idx].to_f32();
            }

            let c_idx = row + col * ldc;
            let cur = c[c_idx].to_f32();
            c[c_idx] = half::f16::from_f32(alpha * acc + beta * cur);
        }
    }
}

const DEFAULT_LISTEN_ADDR: &str = "0.0.0.0:18081";
const DEFAULT_MODEL_ID: &str = "google/siglip2-base-patch16-224";
const DEFAULT_LOG_FILTER: &str = "info,recommender=info";

#[derive(Debug, Clone)]
struct RuntimeConfig {
    listen_addr: String,
    model_id: String,
}

impl RuntimeConfig {
    fn from_env() -> Self {
        Self {
            listen_addr: env_or_default("RECOMMENDER_LISTEN_ADDR", DEFAULT_LISTEN_ADDR),
            model_id: env_or_default("SIGLIP2_MODEL_ID", DEFAULT_MODEL_ID),
        }
    }
}

#[derive(Debug, Deserialize)]
struct EmbedRequest {
    #[serde(default)]
    request_id: String,
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
    embedding: Option<Vec<f32>>,
}

#[derive(Debug)]
struct LoadedModel {
    model: siglip::Model,
    config: siglip::Config,
}

#[derive(Debug, Default)]
struct Worker {
    models: RwLock<HashMap<String, Arc<LoadedModel>>>,
}

impl Worker {
    fn ensure_model(&self, model_id: &str) -> Result<Arc<LoadedModel>> {
        if let Some(model) = self
            .models
            .read()
            .map_err(|_| anyhow::anyhow!("worker model cache lock poisoned"))?
            .get(model_id)
            .cloned()
        {
            debug!(model_id = %model_id, "model cache hit");
            return Ok(model);
        }
        info!(model_id = %model_id, "model cache miss; loading model");
        let loaded = Arc::new(load_model(model_id)?);
        let mut guard = self
            .models
            .write()
            .map_err(|_| anyhow::anyhow!("worker model cache lock poisoned"))?;
        match guard.entry(model_id.to_string()) {
            Entry::Occupied(entry) => {
                debug!(
                    model_id = %model_id,
                    "model cache filled by another thread while loading"
                );
                Ok(Arc::clone(entry.get()))
            }
            Entry::Vacant(entry) => {
                info!(model_id = %model_id, "model loaded");
                Ok(Arc::clone(entry.insert(loaded)))
            }
        }
    }

    fn embed(&self, image_bytes: &[u8], model_id: &str) -> Result<Vec<f32>> {
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
        Ok(embedding)
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

fn init_logging() -> Result<()> {
    let env_filter = EnvFilter::try_from_default_env()
        .or_else(|_| EnvFilter::try_new(DEFAULT_LOG_FILTER))
        .context("build logging filter")?;
    tracing_subscriber::fmt()
        .with_env_filter(env_filter)
        .compact()
        .try_init()
        .map_err(|err| anyhow::anyhow!("initialize tracing subscriber: {err}"))?;
    Ok(())
}

fn log_error_chain(prefix: &str, err: &anyhow::Error) {
    error!(error = %err, "{prefix}");
    for (idx, cause) in err.chain().skip(1).enumerate() {
        error!(cause_index = idx + 1, cause = %cause, "error cause");
    }
}

fn load_model(model_id: &str) -> Result<LoadedModel> {
    info!(model_id = %model_id, "resolving model artifacts");
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

    info!(model_id = %model_id, "model artifacts loaded");
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

fn success_response(req: &EmbedRequest, embedding: Vec<f32>) -> EmbedResponse {
    EmbedResponse {
        request_id: req.request_id.clone(),
        ok: true,
        error: None,
        embedding: Some(embedding),
    }
}

fn ping_response(request_id: &str) -> EmbedResponse {
    EmbedResponse {
        request_id: request_id.to_string(),
        ok: true,
        error: None,
        embedding: None,
    }
}

fn error_response(req_id: String, err: anyhow::Error) -> EmbedResponse {
    EmbedResponse {
        request_id: req_id,
        ok: false,
        error: Some(err.to_string()),
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

    Ok(HttpRequest { method, path, body })
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

fn decode_image_b64(raw: &str) -> Result<Vec<u8>> {
    let trimmed = raw.trim();
    if trimmed.is_empty() {
        return Err(anyhow::anyhow!("image_b64 is required"));
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
        reason: if response.ok {
            "OK"
        } else {
            "Internal Server Error"
        },
        body,
    })
}

fn handle_embed(worker: &Arc<Worker>, config: &RuntimeConfig, body: &[u8]) -> Result<HttpResponse> {
    let request_started = Instant::now();
    let req = parse_embed_request(body)?;
    info!(
        request_id = %req.request_id,
        model_id = %config.model_id,
        "handling embed request"
    );

    let decoded = match decode_image_b64(&req.image_b64) {
        Ok(decoded) => decoded,
        Err(err) => {
            warn!(
                request_id = %req.request_id,
                model_id = %config.model_id,
                stage = "decode",
                error = %err,
                "failed to decode image payload"
            );
            return Ok(write_json_response(
                req.request_id.clone(),
                error_response(req.request_id, err),
            )?);
        }
    };
    let image_size_bytes = decoded.len();
    debug!(
        request_id = %req.request_id,
        model_id = %config.model_id,
        image_size_bytes,
        "decoded image payload"
    );

    if let Err(err) = worker.ensure_model(&config.model_id) {
        error!(
            request_id = %req.request_id,
            model_id = %config.model_id,
            image_size_bytes,
            stage = "init",
            error = %err,
            "failed to initialize model for embed request"
        );
        return Ok(write_json_response(
            req.request_id.clone(),
            error_response(req.request_id, err),
        )?);
    }
    let request_id = req.request_id.clone();
    let inference_started = Instant::now();
    let result = worker.embed(&decoded, &config.model_id);
    match result {
        Ok(embedding) => {
            info!(
                request_id = %request_id,
                model_id = %config.model_id,
                image_size_bytes,
                embedding_len = embedding.len(),
                inference_elapsed_ms = inference_started.elapsed().as_millis() as u64,
                elapsed_ms = request_started.elapsed().as_millis() as u64,
                "embed inference succeeded"
            );
            Ok(write_json_response(
                request_id,
                success_response(&req, embedding),
            )?)
        }
        Err(err) => {
            error!(
                request_id = %request_id,
                model_id = %config.model_id,
                image_size_bytes,
                stage = "inference",
                error = %err,
                inference_elapsed_ms = inference_started.elapsed().as_millis() as u64,
                elapsed_ms = request_started.elapsed().as_millis() as u64,
                "embed inference failed"
            );
            Ok(write_json_response(
                request_id.clone(),
                error_response(request_id, err),
            )?)
        }
    }
}

fn handle_ping_or_healthz(
    worker: &Arc<Worker>,
    config: &RuntimeConfig,
    request_id: &str,
) -> Result<HttpResponse> {
    let started = Instant::now();
    debug!(
        request_id = %request_id,
        model_id = %config.model_id,
        "handling health check request"
    );
    worker
        .ensure_model(&config.model_id)
        .with_context(|| format!("initialize model '{}'", config.model_id))?;
    info!(
        request_id = %request_id,
        model_id = %config.model_id,
        elapsed_ms = started.elapsed().as_millis() as u64,
        "health check request succeeded"
    );
    Ok(write_json_response(
        request_id.to_string(),
        ping_response(request_id),
    )?)
}

fn handle_client(mut stream: TcpStream, worker: Arc<Worker>, config: Arc<RuntimeConfig>) {
    let request_started = Instant::now();
    let mut method = "UNKNOWN".to_string();
    let mut path = "UNKNOWN".to_string();
    let response = match parse_request(&mut stream) {
        Ok(req) => {
            method = req.method.clone();
            path = req.path.clone();
            info!(
                method = %method,
                path = %path,
                body_len = req.body.len(),
                "received HTTP request"
            );
            match (req.method.as_str(), req.path.as_str()) {
                ("GET", "/ping") => handle_ping_or_healthz(&worker, &config, "ping")
                    .unwrap_or_else(|err| {
                        error!(
                            method = "GET",
                            path = "/ping",
                            stage = "ping",
                            error = %err,
                            "failed to serve /ping"
                        );
                        let resp = error_response("ping".to_string(), err);
                        write_json_response("ping".to_string(), resp).unwrap_or_else(|write_err| {
                            error!(
                                method = "GET",
                                path = "/ping",
                                stage = "serialize_response",
                                error = %write_err,
                                "failed to serialize /ping error response"
                            );
                            HttpResponse {
                                status: 500,
                                reason: "Internal Server Error",
                                body: write_err.to_string().into_bytes(),
                            }
                        })
                    }),
                ("GET", "/healthz") => handle_ping_or_healthz(&worker, &config, "healthz")
                    .unwrap_or_else(|err| {
                        error!(
                            method = "GET",
                            path = "/healthz",
                            stage = "healthz",
                            error = %err,
                            "failed to serve /healthz"
                        );
                        let resp = error_response("healthz".to_string(), err);
                        write_json_response("healthz".to_string(), resp).unwrap_or_else(
                            |write_err| {
                                error!(
                                    method = "GET",
                                    path = "/healthz",
                                    stage = "serialize_response",
                                    error = %write_err,
                                    "failed to serialize /healthz error response"
                                );
                                HttpResponse {
                                    status: 500,
                                    reason: "Internal Server Error",
                                    body: write_err.to_string().into_bytes(),
                                }
                            },
                        )
                    }),
                ("POST", "/embed") => match handle_embed(&worker, &config, &req.body) {
                    Ok(response) => response,
                    Err(err) => {
                        error!(
                            method = "POST",
                            path = "/embed",
                            stage = "request",
                            body_len = req.body.len(),
                            error = %err,
                            "failed to process /embed request"
                        );
                        let resp = error_response("request".to_string(), err);
                        write_json_response("request".to_string(), resp).unwrap_or_else(
                            |write_err| {
                                error!(
                                    method = "POST",
                                    path = "/embed",
                                    stage = "serialize_response",
                                    error = %write_err,
                                    "failed to serialize /embed error response"
                                );
                                HttpResponse {
                                    status: 500,
                                    reason: "Internal Server Error",
                                    body: write_err.to_string().into_bytes(),
                                }
                            },
                        )
                    }
                },
                _ => {
                    warn!(method = %req.method, path = %req.path, status = 404, "route not found");
                    HttpResponse {
                        status: 404,
                        reason: "Not Found",
                        body: b"{\"ok\":false,\"error\":\"not found\"}".to_vec(),
                    }
                }
            }
        }
        Err(err) => {
            warn!(status = 400, error = %err, "failed to parse HTTP request");
            let body = format!("{{\"ok\":false,\"error\":\"{err}\"}}");
            HttpResponse {
                status: 400,
                reason: "Bad Request",
                body: body.into_bytes(),
            }
        }
    };

    let status = response.status;
    let elapsed_ms = request_started.elapsed().as_millis() as u64;
    if let Err(err) = send_response(&mut stream, response) {
        warn!(
            method = %method,
            path = %path,
            status,
            elapsed_ms,
            error = %err,
            "failed to write HTTP response"
        );
        return;
    }
    info!(method = %method, path = %path, status, elapsed_ms, "request completed");
}

fn main() {
    if let Err(err) = init_logging() {
        panic!("failed to initialize logging: {err}");
    }

    let config = RuntimeConfig::from_env();

    let worker = Worker::default();
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
            error!(
                listen_addr = %config.listen_addr,
                error = %err,
                "failed to bind listener"
            );
            std::process::exit(1);
        }
    };
    info!(
        listen_addr = %config.listen_addr,
        model_id = %config.model_id,
        "recommender listening"
    );

    let worker = Arc::new(worker);
    let config = Arc::new(config);
    for stream in listener.incoming() {
        match stream {
            Ok(stream) => {
                let worker = Arc::clone(&worker);
                let config = Arc::clone(&config);
                thread::spawn(move || handle_client(stream, worker, config));
            }
            Err(err) => {
                warn!(error = %err, "failed to accept connection");
            }
        }
    }
}
