export type PipelineCounts = {
  blobCount: number
  photoCount: number
  albumCount: number
}

export type BlobCounts = { blobCount: number }

// OverviewVectorCount is the health section's vector-store half. status is
// 'disabled' (no store wired), 'available' (counted), or 'unavailable' (the
// count failed; the overview itself still succeeds).
export type OverviewVectorCount = {
  status: 'disabled' | 'available' | 'unavailable'
  measuredAt: string | null
  expectedPoints: number
  actualPoints: number | null
  countAgreement: boolean | null
  message?: string
}

// OverviewHealth carries the catalog's integrity and lease counts plus the
// feature wiring (encoding worker API, embedding model, vector store). The
// flags report configuration, not worker liveness.
export type OverviewHealth = {
  encodingExpiredLeases: number
  embeddingExpiredLeases: number
  unreferencedBlobs: number
  missingBlobPhotos: number
  encodingEnabled: boolean
  modelEnabled: boolean
  vectorStoreEnabled: boolean
  vectorCount: OverviewVectorCount
}

export type Overview = {
  measuredAt: string
  library: {
    albums: number
    photos: number
    blobs: number
  }
  encoding: {
    categories: Record<string, BlobCounts>
  }
  embedding: {
    categories: Record<string, BlobCounts>
  }
  health: OverviewHealth
}

export type IssueGroup = {
  stage: 'encoding' | 'embedding' | 'album_extraction' | 'extraction_entry' | string
  errorCode: string
  failureCount: number
  countUnit: 'blobs' | 'albums' | 'entries'
}

export type Issues = {
  measuredAt: string
  groups: IssueGroup[]
  totalGroups: number
  remainingGroups: number
  truncated: boolean
  countScope: string
}

export type ExtractionFailure = {
  albumId: string
  originalFilename: string
  runId: number
  ordinal: number
  name: string
  candidate: boolean
  reasonCode: string
}

export type ExtractionFailurePage = {
  reasonCode: string
  failures: ExtractionFailure[]
  nextCursor: string | null
}

export type Photo = {
  albumId: string
  albumName: string
  idx: number
  name: string
  hash: string
  missingBlob: boolean
  width: number
  height: number
  sizeBytes: number | null
  contentType: string | null
  encodingRaw: string
  encoding: string
  encodingError: string
  encodingErrorCode: string
  embeddingRaw: string
  embedding: string
  embeddingError: string
  embeddingErrorCode: string
  skippedReason: string
  encodingObservation?: StageObservation
  embeddingObservation?: StageObservation
}

export type StageObservation = {
  startedAt: string | null
  finishedAt: string | null
  statusChangedAt: string | null
  status: string
  phase: string | null
  attempts: number
  errorCode: string
  retryability: 'retryable' | 'not_retryable' | 'unknown' | string
}

export type Blob = {
  hash: string
  sizeBytes: number
  contentType: string
  encodingRaw: string
  encoding: string
  encodingError: string
  encodingErrorCode: string
  embeddingRaw: string
  embedding: string
  embeddingError: string
  embeddingErrorCode: string
  skippedReason: string
  photoCount: number
  albumCount: number
  encodingObservation?: StageObservation
  embeddingObservation?: StageObservation
}

export type PipelineEvent = {
  id: number
  stage: 'encoding' | 'embedding'
  event: string
  status: string
  phase: string | null
  attemptId: number
  errorCode: string
  retryability: string
  occurredAt: string
}

export type HistoryPage = { events: PipelineEvent[]; nextCursor: string | null }

// EmbeddingCounts mirrors catalog.EmbeddingCounts, the snapshot POST
// /admin/api/reindex answers with.
export type EmbeddingCounts = {
  total: number
  ready: number
  failed: number
  processing: number
  pending: number
}

export type RecoverTarget =
  | 'release_expired_claims'
  | 'release_all_claims'
  | 'retry_failed_embeddings'
  | 'retry_failed_encodings'
  | 'reset_stuck_encoding'

// One synchronous recovery answer from POST /admin/api/recover: exactly one
// action payload is set. Only the row counts the dashboard shows are typed;
// the fresh stats snapshots the server also returns are not rendered.
export type RecoverResult = {
  target: string
  release?: { released: number }
  retry?: { reset: number }
  encoding?: { counts: { releasedExpiredLeased: number; resetReceived: number } }
  encodingRetry?: { reset: number }
}

// AutomaticEmbedding mirrors the recommend service's status for the admin
// execution-control panel: enabled is the process-local pause switch, running
// means the worker loop exists (model loaded and started) — not that a batch
// is in flight this instant.
export type AutomaticEmbedding = { enabled: boolean; running: boolean }

export type Album = {
  id: string
  originalFilename: string
  status: string
  error: string
  photoCount: number
}

export type AlbumDetail = Album & {
  encoding: {
    categories: Record<string, PipelineCounts>
    skippedReasons: Record<string, PipelineCounts>
    rawStates: Record<string, PipelineCounts>
  }
  embedding: {
    categories: Record<string, PipelineCounts>
    rawStates: Record<string, PipelineCounts>
  }
  missingBlobPhotos: number
  extractionRecorded: boolean
  latestExtraction: ExtractionRun | null
}

export type ExtractionRun = {
  runId: number
  status: string
  startedAt: string
  completedAt: string | null
  failureReasonCode: string
  errorDetail: string
  entriesComplete: boolean
  countsProvisional: boolean
  entriesRecorded: number
  candidateEntriesRecorded: number
  totalEntries: number | null
  candidateEntries: number | null
  registeredCount: number
  skippedCount: number
  failedCount: number
}

export type ExtractionRunsPage = { runs: ExtractionRun[]; nextCursor: string | null }

export type ExtractionEntry = {
  runId: number
  ordinal: number
  name: string
  candidate: boolean
  outcome: 'registered' | 'skipped' | 'failed'
  reasonCode: string
  errorDetail: string
  photoIndex: number | null
  hash: string
}

export type ExtractionEntriesPage = {
  run: ExtractionRun
  runSelection: 'latest' | 'explicit'
  entries: ExtractionEntry[]
  nextCursor: string | null
}

export type Page<T> = { nextCursor: string | null } & (
  | { photos: T[] }
  | { blobs: T[] }
  | { albums: T[] }
)

export async function adminGet<T>(path: string, signal?: AbortSignal): Promise<T> {
  const response = await fetch(path, {
    method: 'GET',
    credentials: 'same-origin',
    cache: 'no-store',
    headers: { Accept: 'application/json' },
    signal
  })
  let body: unknown
  try {
    body = await response.json()
  } catch {
    throw new Error(`서버 응답을 읽을 수 없습니다 (HTTP ${response.status})`)
  }
  if (!response.ok) {
    const error = body as { error?: { message?: string } }
    throw new AdminAPIError(error.error?.message || `요청 실패 (HTTP ${response.status})`, response.status)
  }
  return body as T
}

export class AdminAPIError extends Error {
  constructor(message: string, readonly status: number) {
    super(message)
    this.name = 'AdminAPIError'
  }
}

export async function adminPost<T>(path: string, body: unknown, idempotencyKey?: string, signal?: AbortSignal): Promise<T> {
  const headers: Record<string, string> = {
    Accept: 'application/json',
    'Content-Type': 'application/json',
    'X-Admin-Action': '1'
  }
  if (idempotencyKey) headers['Idempotency-Key'] = idempotencyKey
  const response = await fetch(path, {
    method: 'POST',
    credentials: 'same-origin',
    cache: 'no-store',
    headers,
    body: JSON.stringify(body),
    signal
  })
  let result: unknown
  try {
    result = await response.json()
  } catch {
    throw new AdminAPIError(`서버 응답을 읽을 수 없습니다 (HTTP ${response.status})`, response.status)
  }
  if (!response.ok) {
    const error = result as { error?: { message?: string } }
    throw new AdminAPIError(error.error?.message || `요청 실패 (HTTP ${response.status})`, response.status)
  }
  return result as T
}

// recover runs one recovery target synchronously. An optional hashes list
// narrows the target to those blobs; without it the target applies library
// wide, which is the historical behavior.
export async function recover(target: RecoverTarget, hashes?: string[]): Promise<RecoverResult> {
  const body = hashes && hashes.length > 0 ? { target, hashes } : { target }
  return adminPost<RecoverResult>(apiPath('recover'), body)
}

// reindex flips every ready embedding back to pending and answers with the
// fresh embedding counts, so the pending surge is visible immediately.
export async function reindex(): Promise<EmbeddingCounts> {
  return adminPost<EmbeddingCounts>(apiPath('reindex'), {})
}

// runLocalEncoding starts one background drain over every pending non-WebP
// blob, encoded locally with the server's cwebp. started=false means a drain
// is already in flight — the panel reports that instead of erroring.
export async function runLocalEncoding(): Promise<{ started: boolean }> {
  return adminPost<{ started: boolean }>(apiPath('encoding/run-local'), {})
}

export function apiPath(resource: string, search = ''): string {
  return `/admin/api/${resource}${search}`
}

export function imagePath(hash: string): string {
  return `/api/image/${encodeURIComponent(hash)}?w=320`
}
