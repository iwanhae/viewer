# Local manual run contract (Step 3)

This package is the orchestration shell, not a processor. The application registers
**no executors yet**: both stages return `503 EXECUTOR_UNAVAILABLE`, without
creating a run or claiming a blob. There is no success-reporting dummy executor.

## Admin API

All routes require existing Admin Basic auth. Mutations additionally require
`X-Admin-Action: 1`, same-origin browser metadata, and `Content-Type: application/json`.

- `POST /admin/api/runs`: `{"stage":"embedding"}` or `{"stage":"encoding"}`,
  with required `Idempotency-Key` (1–200 bytes). Returns `202` and a run object,
  with `Location: /admin/api/runs/{id}`. Replays return `200` and the same run,
  including terminal runs. The unverified Basic username scopes the key; changing
  stage for that key, or another active local run in that stage, returns `409`.
- `GET /admin/api/runs?limit=50&cursor=...`: `{ "runs": [], "nextCursor": null }`,
  newest first. Cursors are opaque; limits are 1–100. Unknown/duplicate query
  parameters and malformed cursors return `400`.
- `GET /admin/api/runs/{id}?limit=50&cursor=...`:
  `{ "run": {...}, "items": [], "nextCursor": null }`, items ordered by hash.
  Item cursors are bound to the run. Missing runs return `404`.
- `POST /admin/api/runs/{id}/cancel`: `{}` (an empty JSON body is also accepted).
  Returns `200` and a run object. Idempotently requests cancellation, stops future
  bookkeeping claims, and signals current local execution. It is **not rollback**
  and may return before in-flight work exits. Terminal runs are unchanged.

A run object always has `id`, `stage`, `actor` (an **unverified audit label**, not
identity), `executor` (local processor/version provenance), `status`,
`cancelRequested`, `createdAt`, `updatedAt`, nullable `startedAt`/`finishedAt`,
`lastError`, and `counts`. Counts always include `total`, `pending`, `running`,
`succeeded`, `failed`, `busy`, `skipped`, `cancelled`, and `interrupted`.
Items always have `hash`, `status`, nullable `startedAt`/`finishedAt`, `updatedAt`,
and `lastError`. Timestamps are UTC RFC3339Nano strings. Private ownership tokens
and idempotency keys are never serialized. Public errors are allowlisted codes,
not raw processor errors. HTTP errors use the existing `{ "error": { "code": ...,
"message": ... } }` envelope. These shapes are pinned in `admin_runs_test.go`.

Run statuses: `queued`, `running`, `completed`, `failed`, `cancelled`, `interrupted`.
Draining the snapshot completes a run even if some targets were busy/skipped;
item failures make it failed. An explicit cancellation makes it cancelled while
preserving successful results. Service shutdown/restart makes unfinished runs
interrupted, never automatically resumed. Item status is one of the count names
other than `total` (terminal outcomes cannot be overwritten).

## Executor integration boundary

`runs.New(serviceContext, catalog, map[stage]Registration)` copies immutable stage
registrations. One goroutine drains each run's finite initial snapshot; embedding
and encoding can run simultaneously, but the durable partial unique index allows
only one active local run per stage. Execution uses the service context, not the
submission request context. Shutdown cancels and waits before catalog close.

Catalog APIs: `CreateJobRun`, `FindJobRunSubmission`, `JobRun`, `JobRuns`,
`JobRunItems`, `StartJobRun`, `NextJobRunItem`, `FinishJobRunItem`,
`RequestJobRunCancel`, `FinishJobRun`. Snapshotting pending hashes takes no blob
leases. `NextJobRunItem` reserves **run bookkeeping only**, supplying a private
`ClaimToken`. Exact-hash atomic blob claim APIs must bind that token to the stage
claim and reject non-pending targets.
Do not use the existing queue-wide claim methods: they can claim outside the
snapshot. If an external worker wins after snapshot, return `busy`; never steal
its lease, retry indefinitely, or append new uploads. Check cancellation before
claims, fence every write, release only owned claims, and return `succeeded` only
after durable stage completion. Executor errors are safely reduced to machine
codes. The shell does not implement embedding, cwebp, stage lease renewal, or
cross-store commit reconciliation.

### Embedding adapter (Step 4A, not registered yet)

`recommend.NewEmbeddingExecutor(recommendService)` implements `runs.Executor` for
`stage=embedding`, reusing image loading, bounded model inference, result validation,
photo vector upload, and catalog finalization. It does not start a worker or poller.
`catalog.ClaimLocalEmbedding(ctx, hash, item.ClaimToken, leaseUntil)` claims only
that pending hash, returning `claimed`, `busy`, or `missing`; even expired processing
leases are not reclaimed by this API. Busy and missing become safe run outcomes.

Cancellation before reservation releases only the item's matching token. The
opt-in `FinalizeLocalEmbeddingResultsWithCompletion` separates the cancellable
reservation context from a bounded completion context: once reserved, vector upload
and catalog bookkeeping settle independently of run cancellation. A committed
result remains successful. Existing local/external batch finalization still uses
the original context for both phases. Failures use allowlisted run codes, never
raw provider/storage errors; transient failures leave the blob pending for retry.

Startup interrupts only unfinished run records, retaining terminal item outcomes.
Run cleanup releases only token-matching local embedding or leased encoding
claims; it does not roll back finalization or ambiguous encoding commits. Existing
local embedding crash recovery remains; external embedding/encoding leases remain
intact. External worker HTTP routes and polling/results contracts are unchanged.
Crash-stale server-side embedding result reservations are cleared at catalog
startup without changing the external worker's lease or ownership; those
reservations belonged to the dead server request, not to the worker.

### Encoding adapter (Step 4B, not registered yet)

`encoding.NewEncodingExecutor(encodingService)` implements `runs.Executor` for
`stage=encoding`. It requires the existing encoding service's spool processors
to be started with the service lifetime, not the run context. It never polls or
claims queue-wide work and does not require `WORKER_TOKEN`.

`catalog.ClaimLocalEncoding` atomically claims exactly one pending non-WebP hash
with the item's private 128-bit hex token and explicit `encoding_owner='local'`.
External claims retain their existing token/API format. Even expired external
leases are busy to this exact-target API. Local release/run restart cleanup
requires both local ownership and a matching token, and touches only leased rows.
External polling/bulk reset cannot reclaim local leases or local received output.

The executor streams a size-bounded source into a private temporary file, verifies
its SHA-256 and dimensions, then invokes `cwebp -q 85 -alpha_q 100 -metadata all
<source> -o -` with a five-minute timeout. Stdout streams into a strictly bounded
private output file; stderr is discarded. Source/output files are capped at 1 GiB,
source dimensions at 100 million pixels. Temporary files are removed on every
return; crash leftovers match the existing startup partial-spool cleanup pattern.
Already-WebP/not-smaller results use the catalog's token-fenced skip transition.
`cwebp_unavailable` means install libwebp's `cwebp` executable on the server PATH;
deployment/transient failures release the local lease for a later manual retry.

Successful output passes the existing WebP validation, `ReceiveOutput`, durable
spool, asynchronous validation, object replacement, and ambiguous-commit recovery.
Before acceptance, run cancellation kills the command and releases only that
local lease. After acceptance, cancellation does not reset received/committing
output: the executor waits for a terminal catalog outcome using an independent
completion context (bounded at 21 minutes). If that bound expires, the accepted
output remains owned by the service and recoverable on restart. No startup
registration, worker protocol changes, or UI integration are included here.

**Required later integration:** replace/remove the automatic
`recommendService.Start(ctx)` in `internal/app/app.go` when registering the manual
embedding executor. This transitional commit deliberately leaves that existing
loop unchanged while manual creation is unavailable; the final feature must not
leave automatic local embedding outside Run Manager control. External encoding
reconciliation remains the encoding service's responsibility.
