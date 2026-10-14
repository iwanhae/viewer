export type PipelineCounts = {
  blobCount: number
  photoCount: number
  albumCount: number
}

export type BlobCounts = { blobCount: number }

export type Overview = {
  measuredAt: string
  units: { counts: string; measuredAt: string }
  library: {
    albums: number
    photos: number
    blobs: number
  }
  encoding: {
    categories: Record<string, BlobCounts>
    skippedReasons: Record<string, BlobCounts>
    rawStates: Record<string, BlobCounts>
  }
  embedding: {
    categories: Record<string, BlobCounts>
    rawStates: Record<string, BlobCounts>
  }
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

export type QueueDiagnostics = {
  queuedCount: number
  oldestStatusChangedAt: string | null
  oldestAgeSeconds: number | null
  unknownAgeCount: number
}

export type BlockedQueueDiagnostics = Omit<QueueDiagnostics, 'queuedCount'> & {
  blockedCount: number
}

export type Diagnostics = {
  measuredAt: string
  encodingEnabled: boolean
  modelEnabled: boolean
  vectorStoreEnabled: boolean
  encodingExpiredLeases: number
  embeddingExpiredLeases: number
  encodingQueue: QueueDiagnostics
  embeddingQueue: QueueDiagnostics
  embeddingBlockedQueue: BlockedQueueDiagnostics
  encodingCommit: {
    committingCount: number
    reconcilingCount: number
    oldestKnownStartedAt: string | null
    unknownStartCount: number
  }
  integrity: { unreferencedBlobs: number; missingBlobPhotos: number }
  workerActivity: { lastClaimUnixMs: number; lastRenewUnixMs: number; lastResultsUnixMs: number }
  workerActivityScope: string
  vectorCount: {
    status: 'disabled' | 'available' | 'unavailable'
    measuredAt: string | null
    expectedPoints: number
    actualPoints: number | null
    countAgreement: boolean | null
    message?: string
  }
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
  blockReason: string
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
  blockReason: string
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

export type RecoveryAction = 'retry_failed_encoding' | 'retry_failed_embedding' | 'release_expired_encoding' | 'release_expired_embedding'

export type RecoveryPreview = {
  previewId: string
  actor: string
  actorNote: string
  action: RecoveryAction
  createdAt: string
  expiresAt: string
  targetCount: number
  eligibleCount: number
  skippedCount: number
  skippedReasons: Record<string, number>
  selectedPhotos: number
  affectedPhotos: number
  affectedAlbums: number
  sharedBlobWarning: boolean
}

export type RecoveryOperation = {
  id: string
  previewId: string
  actor: string
  actorNote: string
  action: RecoveryAction
  status: 'queued' | 'running' | 'completed' | 'partially_failed' | 'failed' | string
  createdAt: string
  updatedAt: string
  total: number
  eligible: number
  succeeded: number
  conflicts: number
  skipped: number
  failed: number
  selectedPhotos: number
  affectedPhotos: number
  affectedAlbums: number
  sharedBlobWarning: boolean
}

export type RecoveryResult = {
  ordinal: number
  hash: string
  status: 'queued' | 'succeeded' | 'applied' | 'conflict' | 'skipped' | 'failed' | string
  reason?: string
  updatedAt: string
}

export type RecoveryOperationsPage = { operations: RecoveryOperation[]; nextCursor: string | null }
export type RecoveryResultsPage = { operation: RecoveryOperation; results: RecoveryResult[]; nextCursor: string | null }

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

export async function adminPost<T>(path: string, body: unknown, idempotencyKey?: string): Promise<T> {
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
    body: JSON.stringify(body)
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

export function apiPath(resource: string, search = ''): string {
  return `/admin/api/${resource}${search}`
}

export function imagePath(hash: string): string {
  return `/api/image/${encodeURIComponent(hash)}?w=320`
}
