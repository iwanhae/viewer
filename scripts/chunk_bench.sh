#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT_DIR}"

if [[ -f ".env.test" ]]; then
  set -a
  # shellcheck disable=SC1091
  . ./.env.test
  set +a
elif [[ -f ".env.example" ]]; then
  set -a
  # shellcheck disable=SC1091
  . ./.env.example
  set +a
fi

: "${S3_ENDPOINT:?S3_ENDPOINT is required}"
: "${S3_BUCKET:?S3_BUCKET is required}"
: "${S3_ACCESS_KEY:?S3_ACCESS_KEY is required}"
: "${S3_SECRET_KEY:?S3_SECRET_KEY is required}"

command -v curl >/dev/null 2>&1 || { echo "curl is required"; exit 1; }
command -v go >/dev/null 2>&1 || { echo "go is required"; exit 1; }
command -v node >/dev/null 2>&1 || { echo "node is required"; exit 1; }
command -v shuf >/dev/null 2>&1 || { echo "shuf is required"; exit 1; }

if [[ ! -x "./bin/viewer" ]]; then
  echo "missing ./bin/viewer (run make build first)"
  exit 1
fi

BENCH_PORT="${BENCH_PORT:-${TEST_PORT:-18080}}"
BASE_URL="${BENCH_BASE_URL:-http://127.0.0.1:${BENCH_PORT}}"
CHUNK_SIZES="${CHUNK_SIZES:-32768 65536 131072 262144 524288 786432 1048576 1572864 2097152}"
RUNS="${RUNS:-3}"
FEED_LIMIT="${FEED_LIMIT:-80}"
IMAGE_COUNT="${IMAGE_COUNT:-80}"
PARALLEL="${PARALLEL:-8}"
FEED_SEED="${FEED_SEED:-chunk-bench-fixed-seed}"
SERVER_STARTUP_TIMEOUT_SEC="${SERVER_STARTUP_TIMEOUT_SEC:-30}"
OUT_DIR="${BENCH_OUT_DIR:-.cache/bench/$(date +%Y%m%d-%H%M%S)}"

FIXTURE_IMAGES="${FIXTURE_IMAGES:-120}"
FIXTURE_WIDTH="${FIXTURE_WIDTH:-256}"
FIXTURE_HEIGHT="${FIXTURE_HEIGHT:-256}"
FIXTURE_ZIP="${FIXTURE_ZIP:-${OUT_DIR}/bench-album.zip}"

mkdir -p "${OUT_DIR}"

RESULTS_CSV="${OUT_DIR}/results.csv"
SUMMARY_CSV="${OUT_DIR}/summary.csv"

echo "run,chunk_size,feed_ms,image_batch_ms,image_p50_ms,image_p95_ms,image_count,range_fetch_requests,range_fetch_bytes,range_cache_hits,range_cache_misses,range_read_errors,range_loaded_bytes" > "${RESULTS_CSV}"

SERVER_PID=""

stop_server() {
  if [[ -n "${SERVER_PID}" ]]; then
    kill "${SERVER_PID}" >/dev/null 2>&1 || true
    wait "${SERVER_PID}" >/dev/null 2>&1 || true
    SERVER_PID=""
  fi
}

cleanup() {
  stop_server
}
trap cleanup EXIT

ensure_fixture_zip() {
  if [[ -s "${FIXTURE_ZIP}" ]]; then
    return 0
  fi
  mkdir -p "$(dirname "${FIXTURE_ZIP}")"
  go run ./scripts/gen_bench_zip.go \
    -out "${FIXTURE_ZIP}" \
    -images "${FIXTURE_IMAGES}" \
    -width "${FIXTURE_WIDTH}" \
    -height "${FIXTURE_HEIGHT}"
}

start_server() {
  local chunk_size="$1"
  local server_log="${OUT_DIR}/server-chunk-${chunk_size}-run-${CURRENT_RUN}.log"
  PORT="${BENCH_PORT}" RANGE_CHUNK_SIZE_BYTES="${chunk_size}" SKIP_WARMUP=true ./bin/viewer > "${server_log}" 2>&1 &
  SERVER_PID=$!

  for _ in $(seq 1 "${SERVER_STARTUP_TIMEOUT_SEC}"); do
    if ! kill -0 "${SERVER_PID}" >/dev/null 2>&1; then
      echo "viewer server exited before healthcheck for chunk=${chunk_size} run=${CURRENT_RUN}"
      sed -n '1,200p' "${server_log}" || true
      return 1
    fi
    if curl -fsS "${BASE_URL}/healthz" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done

  echo "viewer server did not become healthy within timeout for chunk=${chunk_size} run=${CURRENT_RUN}"
  sed -n '1,200p' "${server_log}" || true
  return 1
}

seed_album() {
  local fixture_size
  local create_resp
  local album_id

  fixture_size="$(wc -c < "${FIXTURE_ZIP}" | tr -d ' ')"
  create_resp="$(curl -fsS -H 'Content-Type: application/json' -d "{\"filename\":\"bench-album.zip\",\"sizeBytes\":${fixture_size}}" "${BASE_URL}/api/albums")"
  album_id="$(printf '%s' "${create_resp}" | node -e 'let d="";process.stdin.on("data",c=>d+=c);process.stdin.on("end",()=>{const o=JSON.parse(d);if(!o.albumId){process.exit(1)}process.stdout.write(String(o.albumId));});')"

  curl -fsS -F "file=@${FIXTURE_ZIP};type=application/zip" "${BASE_URL}/api/albums/${album_id}/upload" >/dev/null
  curl -fsS -X POST "${BASE_URL}/api/albums/${album_id}/finalize" >/dev/null
  printf '%s' "${album_id}"
}

compute_image_quantiles_ms() {
  local times_file="$1"
  node - "${times_file}" <<'NODE'
const fs = require("fs");
const path = process.argv[2];
const raw = fs.readFileSync(path, "utf8").trim();
if (!raw) {
  console.log("0.000 0.000 0");
  process.exit(0);
}
const samples = raw.split(/\s+/).map(Number).filter(Number.isFinite).sort((a, b) => a - b);
if (samples.length === 0) {
  console.log("0.000 0.000 0");
  process.exit(0);
}
const pick = (p) => {
  const idx = Math.min(samples.length - 1, Math.max(0, Math.ceil(samples.length * p) - 1));
  return samples[idx] * 1000;
};
console.log(`${pick(0.5).toFixed(3)} ${pick(0.95).toFixed(3)} ${samples.length}`);
NODE
}

compute_stats_delta() {
  local before_json="$1"
  local after_json="$2"
  node - "${before_json}" "${after_json}" <<'NODE'
const before = JSON.parse(process.argv[2]);
const after = JSON.parse(process.argv[3]);
const fields = [
  "fetchRequests",
  "fetchBytes",
  "cacheHits",
  "cacheMisses",
  "readErrors",
  "loadedBytes",
];
const out = fields.map((f) => Number(after[f] || 0) - Number(before[f] || 0));
console.log(out.join(" "));
NODE
}

feed_sources() {
  local feed_json_file="$1"
  local limit="$2"
  node - "${feed_json_file}" "${limit}" <<'NODE'
const fs = require("fs");
const path = process.argv[2];
const limit = Math.max(0, Number(process.argv[3] || "0"));
const payload = JSON.parse(fs.readFileSync(path, "utf8"));
const items = Array.isArray(payload.items) ? payload.items : [];
for (const item of items.slice(0, limit)) {
  if (item && typeof item.src === "string" && item.src.length > 0) {
    process.stdout.write(item.src + "\n");
  }
}
NODE
}

ensure_fixture_zip

for CURRENT_RUN in $(seq 1 "${RUNS}"); do
  mapfile -t ORDERED_SIZES < <(printf '%s\n' ${CHUNK_SIZES} | shuf)
  for chunk_size in "${ORDERED_SIZES[@]}"; do
    echo "bench run=${CURRENT_RUN} chunk=${chunk_size}"
    stop_server
    start_server "${chunk_size}"

    run_dir="${OUT_DIR}/run-${CURRENT_RUN}-chunk-${chunk_size}"
    mkdir -p "${run_dir}"
    seed_album > "${run_dir}/album_id.txt"

    before_stats="$(curl -fsS "${BASE_URL}/api/debug/range-cache-stats")"
    feed_url="${BASE_URL}/api/feed?limit=${FEED_LIMIT}&seed=${FEED_SEED}"
    feed_body="${run_dir}/feed.json"
    feed_seconds="$(curl -fsS -o "${feed_body}" -w '%{time_total}' "${feed_url}")"
    feed_ms="$(node -e 'const v=Number(process.argv[1]||0); console.log((v*1000).toFixed(3));' "${feed_seconds}")"

    urls_file="${run_dir}/image-urls.txt"
    feed_sources "${feed_body}" "${IMAGE_COUNT}" > "${urls_file}"
    image_count="$(wc -l < "${urls_file}" | tr -d ' ')"

    image_times_file="${run_dir}/image-times.txt"
    : > "${image_times_file}"
    image_batch_ms="0.000"
    image_p50_ms="0.000"
    image_p95_ms="0.000"
    sample_count="0"

    if [[ "${image_count}" -gt 0 ]]; then
      start_ns="$(date +%s%N)"
      sed "s#^#${BASE_URL}#" "${urls_file}" | xargs -P "${PARALLEL}" -I{} sh -c 'curl -fsS -o /dev/null -w "%{time_total}\n" "$1"' _ "{}" >> "${image_times_file}"
      end_ns="$(date +%s%N)"
      image_batch_ms="$(node -e 'const start=BigInt(process.argv[1]); const end=BigInt(process.argv[2]); console.log((Number(end-start)/1e6).toFixed(3));' "${start_ns}" "${end_ns}")"
      read -r image_p50_ms image_p95_ms sample_count < <(compute_image_quantiles_ms "${image_times_file}")
    fi

    after_stats="$(curl -fsS "${BASE_URL}/api/debug/range-cache-stats")"
    read -r range_fetch_requests range_fetch_bytes range_cache_hits range_cache_misses range_read_errors range_loaded_bytes < <(compute_stats_delta "${before_stats}" "${after_stats}")

    printf '%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s\n' \
      "${CURRENT_RUN}" \
      "${chunk_size}" \
      "${feed_ms}" \
      "${image_batch_ms}" \
      "${image_p50_ms}" \
      "${image_p95_ms}" \
      "${sample_count}" \
      "${range_fetch_requests}" \
      "${range_fetch_bytes}" \
      "${range_cache_hits}" \
      "${range_cache_misses}" \
      "${range_read_errors}" \
      "${range_loaded_bytes}" \
      >> "${RESULTS_CSV}"

    stop_server
  done
done

node - "${RESULTS_CSV}" "${SUMMARY_CSV}" <<'NODE'
const fs = require("fs");
const [resultsPath, summaryPath] = process.argv.slice(2);
const lines = fs.readFileSync(resultsPath, "utf8").trim().split(/\r?\n/);
const header = lines.shift();
if (!header || lines.length === 0) {
  fs.writeFileSync(summaryPath, "");
  process.exit(0);
}

const cols = header.split(",");
const numCols = new Set([
  "feed_ms",
  "image_batch_ms",
  "image_p50_ms",
  "image_p95_ms",
  "image_count",
  "range_fetch_requests",
  "range_fetch_bytes",
  "range_cache_hits",
  "range_cache_misses",
  "range_read_errors",
  "range_loaded_bytes",
]);

const rows = lines.map((line) => {
  const vals = line.split(",");
  const row = {};
  cols.forEach((c, i) => {
    row[c] = numCols.has(c) ? Number(vals[i] || 0) : vals[i];
  });
  return row;
});

const byChunk = new Map();
for (const r of rows) {
  const key = Number(r.chunk_size);
  if (!byChunk.has(key)) byChunk.set(key, []);
  byChunk.get(key).push(r);
}

const out = [];
out.push([
  "chunk_size",
  "runs",
  "avg_feed_ms",
  "avg_image_batch_ms",
  "avg_image_p50_ms",
  "avg_image_p95_ms",
  "avg_range_fetch_requests",
  "avg_range_fetch_bytes",
  "avg_range_cache_hits",
  "avg_range_cache_misses",
  "avg_range_read_errors",
].join(","));

const keys = [...byChunk.keys()].sort((a, b) => a - b);
for (const key of keys) {
  const group = byChunk.get(key);
  const mean = (field) => group.reduce((acc, r) => acc + Number(r[field] || 0), 0) / group.length;
  out.push([
    key,
    group.length,
    mean("feed_ms").toFixed(3),
    mean("image_batch_ms").toFixed(3),
    mean("image_p50_ms").toFixed(3),
    mean("image_p95_ms").toFixed(3),
    mean("range_fetch_requests").toFixed(3),
    mean("range_fetch_bytes").toFixed(3),
    mean("range_cache_hits").toFixed(3),
    mean("range_cache_misses").toFixed(3),
    mean("range_read_errors").toFixed(3),
  ].join(","));
}

fs.writeFileSync(summaryPath, out.join("\n") + "\n");
NODE

echo "benchmark raw results: ${RESULTS_CSV}"
echo "benchmark summary: ${SUMMARY_CSV}"
