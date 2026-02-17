# Chunk Size Experiment Report for `/feed` Wall Flow

## 1. Goal
Find a better S3 range-cache chunk size for the wall experience driven by `/api/feed` and follow-up `/api/image/...` loads.  
Current baseline was `1 MiB` (`1048576` bytes).

## 2. What Was Changed
- Added runtime config: `RANGE_CHUNK_SIZE_BYTES` (default `131072`).
- Added range-cache counters and snapshot API at `/api/debug/range-cache-stats`:
  - `fetchRequests`
  - `fetchBytes`
  - `cacheHits`
  - `cacheMisses`
  - `readErrors`
  - `loadedBytes`
- Added benchmark tooling:
  - `scripts/gen_bench_zip.go` (deterministic benchmark ZIP generator)
  - `scripts/chunk_bench.sh` (matrix runner + CSV aggregation)

## 3. Experiment Setup
- Date: **February 14, 2026**
- Backend: local `bin/viewer`, real S3 backend from `.env.test`
- Fixture ZIP: generated random PNG set, `120` images at `256x256`, ZIP size `23,658,022` bytes
- For each chunk size:
  1. Start server with selected `RANGE_CHUNK_SIZE_BYTES`
  2. Upload + finalize fixture album
  3. Call `/api/feed?limit=80&seed=chunk-bench-fixed-seed`
  4. Fetch 80 returned `/api/image` URLs (parallelism `8`)
  5. Collect latency and range-cache counter deltas
- Chunk sizes tested (bytes):
  - `32768`, `65536`, `131072`, `262144`, `524288`, `786432`, `1048576`, `1572864`, `2097152`
- Repetitions: `3` runs with randomized chunk order

Raw artifacts:
- `.cache/bench/20260214-022300/results.csv`
- `.cache/bench/20260214-022300/summary.csv`

## 4. Results (Averages Across 3 Runs)

| Chunk (bytes) | Feed ms | Image batch ms | Image p95 ms | Range fetch bytes |
|---:|---:|---:|---:|---:|
| 32768 | 0.559 | 2970.138 | 655.692 | 13696550 |
| 65536 | 0.580 | 3675.491 | 1435.071 | 14941734 |
| 131072 | 0.564 | 839.101 | 145.324 | 17628710 |
| 262144 | 0.559 | 656.135 | 140.935 | 20774438 |
| 524288 | 0.676 | 828.444 | 210.337 | 23133734 |
| 786432 | 0.700 | 1001.447 | 306.972 | 23658022 |
| 1048576 | 0.570 | 800.695 | 249.712 | 23658022 |
| 1572864 | 0.573 | 877.548 | 329.109 | 23658022 |
| 2097152 | 0.595 | 895.811 | 519.859 | 23658022 |

Observations:
- `/api/feed` latency stayed effectively flat across chunk sizes (around `0.56-0.70 ms`).
- Very small chunks (`32-64 KiB`) reduced transfer bytes but caused many more range requests and unstable/slower wall image batches.
- Large chunks (`>= 768 KiB`) flattened request count but transferred full ZIP-sized bytes and had worse tail latencies.

## 5. Decision Rule and Winner
Rule used:
1. Minimize image `p95` latency (primary).
2. Among chunk sizes within 5% of best p95, choose lower S3 transferred bytes.
3. Reject any option with increased read errors.

Computed from `summary.csv`:
- Best p95: `140.935 ms` at `262144`.
- 5% threshold: `147.982 ms`.
- In-threshold candidates:
  - `131072`: p95 `145.324 ms`, fetch bytes `17,628,710`
  - `262144`: p95 `140.935 ms`, fetch bytes `20,774,438`

**Recommended chunk size: `131072` bytes (128 KiB).**

Reason:
- p95 is within 5% of the best observed value.
- It cuts transferred range bytes by about `3.15 MB` (~15%) vs `262144`.
- `readErrors` remained `0`.

## 6. Rollout Plan
1. Set `RANGE_CHUNK_SIZE_BYTES=131072` in runtime env.
2. Deploy and monitor:
  - `/api/debug/range-cache-stats` deltas per request window
  - wall/image request latency percentiles
3. Re-run `scripts/chunk_bench.sh` after notable workload or storage/network changes.

## 7. Limitations
- Dataset was controlled/synthetic (deterministic generated ZIP) to make runs repeatable and avoid startup-wide index warmup noise.
- Some run-to-run variance remained (network/storage jitter), so periodic revalidation on production-like traffic is still advised.
