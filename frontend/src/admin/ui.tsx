import { useCallback, useEffect, useRef, useState, type FormEvent, type ReactNode } from 'react'
import {
  Link,
  Route,
  Routes,
  useLocation,
  useNavigate,
  useParams,
  useSearchParams
} from 'react-router-dom'
import {
  adminGet,
  adminPost,
  apiPath,
  imagePath,
  recover,
  reindex,
  runLocalEncoding,
  type Album,
  type AlbumDetail,
  type Blob,
  type BlobCounts,
  type Overview,
  type Issues,
  type IssueGroup,
  type ExtractionFailurePage,
  type Page,
  type HistoryPage,
  type Photo,
  type RecoverResult,
  type RecoverTarget,
  type AutomaticEmbedding
} from './api'

type LoadState<T> = { path: string; data: T | null; error: string; loading: boolean }

function useAdminData<T>(path: string, refreshKey = 0): LoadState<T> {
  const [state, setState] = useState<LoadState<T>>(() => ({ path, data: null, error: '', loading: true }))
  useEffect(() => {
    if (!path) {
      setState({ path, data: null, error: '', loading: false })
      return
    }
    const controller = new AbortController()
    // A refresh of the same path keeps the last data on screen (so an action's
    // result line and the page itself do not blink away); a path change still
    // resets immediately.
    setState((old) => old.path === path
      ? { path, data: old.data, error: '', loading: true }
      : { path, data: null, error: '', loading: true })
    adminGet<T>(path, controller.signal)
      .then((data) => {
        if (!controller.signal.aborted) setState({ path, data, error: '', loading: false })
      })
      .catch((error: unknown) => {
        if (!controller.signal.aborted) {
          setState({ path, data: null, error: errorMessage(error), loading: false })
        }
      })
    return () => controller.abort()
  }, [path, refreshKey])
  // Effects run after render. Keying the returned state prevents one paint of
  // the prior filter's rows while the next request is starting.
  return state.path === path ? state : { path, data: null, error: '', loading: true }
}

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : 'An unknown error occurred'
}

function formatTime(value: string): string {
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? value : date.toLocaleString()
}

function AdminApp() {
  return (
    <div className="admin-root">
      <header className="admin-header">
        <Link className="brand" to="/">Admin</Link>
        <nav aria-label="Admin navigation">
          <Link to="/">Overview</Link>
          <Link to="/photos">Photos</Link>
          <Link to="/albums">Albums</Link>
          <Link to="/issues">Issues</Link>
        </nav>
      </header>
      <main className="admin-main">
        <Routes>
          <Route path="/" element={<OverviewPage />} />
          <Route path="/photos" element={<PhotosPage />} />
          <Route path="/photos/:albumId/:idx" element={<PhotoDetailPage />} />
          <Route path="/blobs/:hash" element={<BlobDetailPage />} />
          <Route path="/albums" element={<AlbumsPage />} />
          <Route path="/albums/:id" element={<AlbumDetailPage />} />
          <Route path="/issues" element={<IssuesPage />} />
          <Route path="*" element={<NotFoundPage />} />
        </Routes>
      </main>
    </div>
  )
}

export default AdminApp

function OverviewPage() {
  const [lastGood, setLastGood] = useState<Overview | null>(null)
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(true)
  const [updatedAt, setUpdatedAt] = useState('')
  const inFlight = useRef(false)
  const refresh = useCallback(async (signal?: AbortSignal) => {
    if (inFlight.current) return
    inFlight.current = true
    try {
      const data = await adminGet<Overview>(apiPath('overview'), signal)
      if (signal?.aborted) return
      setLastGood(data)
      setError('')
      setUpdatedAt(new Date().toLocaleTimeString())
    } catch (cause) {
      if (!signal?.aborted) setError(errorMessage(cause))
    } finally {
      inFlight.current = false
      if (!signal?.aborted) setLoading(false)
    }
  }, [])

  useEffect(() => {
    const controller = new AbortController()
    let inFlight = false
    const run = async () => {
      if (inFlight || document.visibilityState !== 'visible') return
      inFlight = true
      await refresh(controller.signal)
      inFlight = false
    }
    void run()
    const timer = window.setInterval(() => void run(), 20_000)
    const onVisibility = () => {
      if (document.visibilityState === 'visible') void run()
    }
    document.addEventListener('visibilitychange', onVisibility)
    return () => {
      controller.abort()
      window.clearInterval(timer)
      document.removeEventListener('visibilitychange', onVisibility)
    }
  }, [refresh])

  return (
    <>
      <PageTitle title="Overview" description="Review the catalog's current status." />
      <div className="toolbar">
        <button type="button" onClick={() => void refresh()} disabled={loading}>Refresh</button>
        <span className="muted">
          Updated {updatedAt || '—'} · Measured {lastGood ? formatTime(lastGood.measuredAt) : '—'} · Refreshes every 20 seconds
        </span>
      </div>
      {error && <div className="notice warning" role="alert">Refresh failed: {error}{lastGood ? ' · Showing the last good data.' : ''}</div>}
      {loading && !lastGood && <p role="status">Loading…</p>}
      {!lastGood && !loading && <ErrorPanel message={error || 'Could not load the overview.'} />}
      {lastGood && <>
        <section className="panel">
          <h2>Library</h2>
          <div className="summary-grid">
            <Stat label="Albums" value={lastGood.library.albums} to="/albums" />
            <Stat label="Photos" value={lastGood.library.photos} to="/photos" />
            <Stat label="Unique blobs" value={lastGood.library.blobs} to="/photos" />
          </div>
          <p className="muted">Albums and photos are whole-library inventory counts. The pipeline tables show unique blob counts per state and do not split photo or album reference counts by state.</p>
        </section>
        <CategorySection title="WebP encoding" categories={lastGood.encoding.categories} filterKey="encoding" />
        <CategorySection title="Embedding" categories={lastGood.embedding.categories} filterKey="embedding" />
        <HealthPanel health={lastGood.health} />
      </>}
      <ExecutionControlPanel />
      <SyncRecoveryPanel
        title="Recovery actions"
        description="Library-wide recovery actions. Each action runs immediately after confirmation and reports how many rows it returned to the queued state. No action force-releases valid claims; valid leases wait for expiry."
        actions={globalRecoveryActions}
        onDone={() => void refresh()}
      />
    </>
  )
}

// ExecutionControlPanel is the successor of the retired local-runs page: the
// automatic-embedding pause switch and the local encoding drain trigger, both
// process-local controls over work that continues on its own.
function ExecutionControlPanel() {
  const automatic = useAdminData<AutomaticEmbedding>(apiPath('automatic/embedding'))
  // The most recent toggle's answer wins over the initial read until the
  // panel remounts: the POST already returns the status it produced, so a
  // follow-up GET would only add a race window.
  const [override, setOverride] = useState<AutomaticEmbedding | null>(null)
  const status = override ?? automatic.data
  const [pending, setPending] = useState('')
  const [result, setResult] = useState('')
  const [error, setError] = useState('')
  const running = useRef(false)

  const act = async (name: string, action: () => Promise<void>) => {
    if (running.current) return
    running.current = true
    setPending(name)
    setError('')
    try {
      await action()
    } catch (cause) {
      setError(errorMessage(cause))
    } finally {
      running.current = false
      setPending('')
    }
  }

  const toggleAutomatic = () => void act('automatic', async () => {
    const next = await adminPost<AutomaticEmbedding>(apiPath('automatic/embedding'), { enabled: !(status?.enabled ?? true) })
    setOverride(next)
  })

  const runLocal = () => void act('encoding', async () => {
    if (!window.confirm("Encodes every queued WebP conversion target with the server's cwebp. It is CPU-intensive and runs in the background. Continue?")) return
    const outcome = await runLocalEncoding()
    setResult(outcome.started ? 'Local encoding started in the background.' : 'Local encoding is already running.')
  })

  return <section className="panel" aria-label="Run control">
    <h2>Run control</h2>
    <p className="muted">Controls the server's local execution. External workers are not controlled.</p>
    {automatic.error && <div className="notice warning" role="alert">Status read failed: {automatic.error}</div>}
    {automatic.loading && !automatic.data && <p role="status">Loading…</p>}
    {status && <>
      <h3>Automatic embedding</h3>
      <dl className="metadata">
        <dt>Automatic</dt><dd>{status.enabled ? 'enabled' : 'paused'}</dd>
        <dt>Currently</dt><dd>{status.running ? 'running' : 'not running'}</dd>
      </dl>
      <button type="button" disabled={!!pending} onClick={toggleAutomatic}>
        {pending === 'automatic' ? 'Updating automatic setting…' : status.enabled ? 'Pause automatic embedding' : 'Resume automatic embedding'}
      </button>
      <p className="muted">Pausing takes effect after the in-flight batch finishes and is instantly reversible, so the switch happens without confirmation. The setting returns to enabled when the server restarts.</p>
      {!status.running && <p className="muted">"Not running" means the worker has not started yet. It starts automatically once the model has loaded.</p>}
    </>}
    <h3>Run local encoding</h3>
    <button type="button" disabled={!!pending} onClick={runLocal}>
      {pending === 'encoding' ? 'Starting…' : 'Run local encoding'}
    </button>
    <p className="muted">Processes only the targets that are pending at start time and then exits. Later uploads wait for the next run.</p>
    {result && <p className="recovery-result" role="status">{result}</p>}
    {error && <div className="notice error" role="alert">{error}</div>}
  </section>
}

function IssuesPage() {
  const state = useAdminData<Issues>(apiPath('issues'))
  const [params] = useSearchParams()
  const entryReasonCode = params.get('entryReasonCode') || ''
  return <>
    <PageTitle title="Current issues" description="Aggregates only the current failure states and the failed entries of each album's latest extraction run." />
    {state.error && <ErrorPanel message={state.error} />}
    {state.loading && !state.data && <p role="status">Loading…</p>}
    {state.data && <>
      <p className="muted">Measured {formatTime(state.data.measuredAt)} · {state.data.totalGroups.toLocaleString()} group(s)</p>
      <p className="muted">{state.data.countScope}</p>
      {state.data.truncated && <div className="notice warning">Showing the top 100 groups only. The remaining {state.data.remainingGroups.toLocaleString()} group(s) were omitted from the response.</div>}
      {state.data.groups.length === 0
        ? <section className="panel"><p>No failure issues are currently recorded.</p></section>
        : <div className="table-wrap"><table className="data-table"><thead><tr><th>Stage</th><th>Safe error code</th><th>Failures</th><th>Count unit</th><th>View</th></tr></thead><tbody>
          {state.data.groups.map((group) => <IssueRow key={`${group.stage}:${group.errorCode}`} group={group} />)}
        </tbody></table></div>}
      {state.data.groups.some((group) => group.errorCode === 'legacy_unknown') && <div className="notice warning">
        legacy_unknown means the detail message of a legacy failure cannot be safely classified. Raw errors and secret values are not shown in this issue list.
      </div>}
    </>}
    {entryReasonCode && <ExtractionFailureBrowser reasonCode={entryReasonCode} />}
  </>
}

function IssueRow({ group }: { group: IssueGroup }) {
  let target = ''
  if (group.stage === 'encoding') {
    target = filterURL('photos', { encoding: 'failed', encodingErrorCode: group.errorCode })
  } else if (group.stage === 'embedding') {
    target = filterURL('photos', { embedding: 'failed', embeddingErrorCode: group.errorCode })
  } else if (group.stage === 'album_extraction') {
    target = '/albums?status=FAILED'
  } else if (group.stage === 'extraction_entry') {
    target = `/issues?entryReasonCode=${encodeURIComponent(group.errorCode)}`
  }
  const stageLabel: Record<string, string> = {
    encoding: 'Encoding', embedding: 'Embedding', album_extraction: 'Archive extraction', extraction_entry: 'Extraction entries'
  }
  const unitLabel: Record<string, string> = { blobs: 'Unique blobs', albums: 'Albums', entries: 'Failed entries' }
  return <tr>
    <th scope="row">{stageLabel[group.stage] || group.stage}</th>
    <td><code>{group.errorCode}</code></td>
    <td>{target ? <CountLink value={group.failureCount} to={target} /> : group.failureCount.toLocaleString()}</td>
    <td>{unitLabel[group.countUnit] || group.countUnit}</td>
    <td>{target ? <Link to={target}>View page</Link> : '—'}</td>
  </tr>
}

function ExtractionFailureBrowser({ reasonCode }: { reasonCode: string }) {
  const [cursor, setCursor] = useState('')
  useEffect(() => { setCursor('') }, [reasonCode])
  const query = new URLSearchParams({ reasonCode, limit: '50' })
  if (cursor) query.set('cursor', cursor)
  const state = useAdminData<ExtractionFailurePage>(apiPath('extraction-failures', `?${query.toString()}`))
  return <section className="panel">
    <h2>Latest extraction run failures · {reasonCode}</h2>
    <p className="muted">Shows only the failed entries of each album's most recently recorded run, paged. Failures already resolved in earlier runs are not included.</p>
    {state.error && <ErrorPanel message={state.error} />}
    {state.loading && !state.data && <p role="status">Loading…</p>}
    {state.data && <>
      <div className="table-wrap"><table className="data-table"><thead><tr><th>Album</th><th>Run</th><th>Entry</th><th>Code</th></tr></thead><tbody>
        {state.data.failures.map((failure) => <tr key={`${failure.albumId}:${failure.ordinal}`}>
          <td><Link to={`/albums/${encodeURIComponent(failure.albumId)}`}>{failure.originalFilename}</Link><small className="subline">{failure.albumId}</small></td>
          <td>#{failure.runId} · Latest run</td><td>{failure.name}<small className="subline">Entry #{failure.ordinal}</small></td><td><code>{failure.reasonCode}</code></td>
        </tr>)}
        {state.data.failures.length === 0 && <EmptyRow columns={4} />}
      </tbody></table></div>
      <div className="pagination">
        {cursor && <button type="button" onClick={() => setCursor('')}>First page</button>}
        {state.data.nextCursor && <button type="button" onClick={() => setCursor(state.data!.nextCursor!)}>More failures</button>}
      </div>
    </>}
  </section>
}

// HealthPanel renders the overview's "System health" section: the catalog's
// lease and integrity counts with drill-down links, the vector-store count
// comparison, and a one-line summary of the server's feature wiring. Every
// number is a measurement from the same snapshot; the flags report
// configuration, never worker liveness.
function HealthPanel({ health }: { health: Overview['health'] }) {
  const vector = health.vectorCount
  return <section className="panel" aria-label="System health">
    <h2>System health</h2>
    <div className="summary-grid">
      <Stat label="Expired embedding leases" value={health.embeddingExpiredLeases} to="/photos?embeddingLeaseExpired=true" />
      <Stat label="Expired encoding leases" value={health.encodingExpiredLeases} to="/photos?encodingLeaseExpired=true" />
      {/* Unreferenced blobs have no list to drill into anymore: the blobs list is
          retired, so the count stays a plain number. */}
      <Stat label="Unreferenced blobs" value={health.unreferencedBlobs} />
      <Stat label="Photos without blobs" value={health.missingBlobPhotos} to="/photos?missingBlob=true" />
    </div>
    <h3>Vector store</h3>
    {vector.status === 'disabled' && <p>Inactive — the vector store is not configured, so counts are not measured.</p>}
    {vector.status === 'unavailable' && <div className="notice warning" role="status">Unavailable — {vector.message || 'Could not read the vector counts.'}</div>}
    {vector.status === 'available' && <>
      <p>{vector.countAgreement ? 'Match' : 'Mismatch'} <span className="muted">(Expected {vector.expectedPoints.toLocaleString()} · Actual {vector.actualPoints?.toLocaleString()})</span></p>
      <p className="muted">This is only a count comparison; it does not verify full synchronization or point integrity.</p>
    </>}
    <p className="muted">
      Encoding worker API {health.encodingEnabled ? 'enabled' : 'disabled'} · Embedding model {health.modelEnabled ? 'enabled' : 'disabled'} · Vector store {health.vectorStoreEnabled ? 'enabled' : 'disabled'}
    </p>
    <p className="muted">Enabled or disabled refers to server configuration, not whether the worker process is running.</p>
  </section>
}

// CategorySection renders one pipeline's unique-blob counts per category.
// The counts stay unique-blob counts (that is what the overview measures),
// but the drilldown opens photo rows filtered by the category — the blobs
// list is gone and photos are the only browsable rows left.
function CategorySection({ title, categories, filterKey }: {
  title: string
  categories: Record<string, BlobCounts>
  filterKey: 'encoding' | 'embedding'
}) {
  const order = filterKey === 'encoding'
    ? ['queued', 'processing', 'validating', 'committing', 'completed', 'skipped', 'failed', 'unknown']
    : ['queued', 'processing', 'completed', 'failed', 'unknown']
  const states = [...new Set([...order, ...Object.keys(categories)])]
  return <section className="panel">
    <h2>{title}</h2>
    {filterKey === 'embedding' && <p className="muted">Queued (queued) is a state the embedding worker can process regardless of the encoding state.</p>}
    <p className="muted">Each blob is counted once per normalized state of the stage. The table's numbers are unique blob counts, and the links open that state's photo rows.</p>
    <table className="data-table"><thead><tr><th>Status</th><th>Unique blobs</th></tr></thead>
      <tbody>{states.map((state) => <tr key={state}>
        <th scope="row"><Link to={filterURL('photos', { [filterKey]: state })}>{stateLabel(state)}</Link></th>
        <td><CountLink value={categories[state]?.blobCount || 0} to={filterURL('photos', { [filterKey]: state })} /></td>
      </tr>)}</tbody>
    </table>
  </section>
}

function filterURL(resource: 'photos' | 'albums', values: Record<string, string>): string {
  const query = new URLSearchParams(values)
  return `/${resource}?${query.toString()}`
}

function Stat({ label, value, to }: { label: string; value: number | string; to?: string }) {
  const content = <><span>{label}</span><strong>{typeof value === 'number' ? value.toLocaleString() : value}</strong></>
  return to ? <Link className="stat" to={to}>{content}</Link> : <div className="stat">{content}</div>
}

function CountLink({ value, to }: { value: number; to: string }) {
  return <Link className="count-link" to={to}>{value.toLocaleString()}</Link>
}

function stateLabel(state: string): string {
  const labels: Record<string, string> = {
    queued: 'Queued', processing: 'Processing', validating: 'Validating', committing: 'Committing',
    completed: 'Completed', skipped: 'Skipped', failed: 'Failed', unknown: 'Unknown'
  }
  return labels[state] ? `${labels[state]} (${state})` : `${state} (new state)`
}

function PageTitle({ title, description }: { title: string; description?: string }) {
  return <div className="page-title"><h1>{title}</h1>{description && <p>{description}</p>}</div>
}

function PhotosPage() {
  const location = useLocation()
  const path = apiPath('photos', location.search)
  const state = useAdminData<Page<Photo>>(path)
  const photos = state.data && 'photos' in state.data ? state.data.photos : []
  const nextCursor = state.data?.nextCursor || null
  return <>
    <PageTitle title="Photos" description="Search photo rows together with their linked albums, blobs, and pipeline states." />
    <PhotoFilters />
    <PhotoURLFilterNotice />
    {state.error && <ErrorPanel message={state.error} />}
    {state.loading && !state.data && <p role="status">Loading…</p>}
    {state.data && <>
      <PhotoTable photos={photos} />
      <Pagination nextCursor={nextCursor} />
    </>}
  </>
}

type SyncRecoveryAction = {
  id: string
  target: RecoverTarget | 'reindex'
  label: string
  confirm: string
  hashes?: string[]
  danger?: boolean
}

type RecoveryEntity = {
  hash: string
  contentType: string | null
  encodingState: string
  encodingRaw: string
  embeddingState: string
}

// Buttons follow the entity's current normalized states, so the panel only
// offers actions the server would act on for this blob right now. The server
// still re-checks eligibility and answers with the rows it actually moved.
function entityRecoveryActions(entity: RecoveryEntity): SyncRecoveryAction[] {
  const actions: SyncRecoveryAction[] = []
  if (entity.embeddingState === 'failed') {
    actions.push({
      id: 'retry_failed_embeddings',
      target: 'retry_failed_embeddings',
      label: "Retry this blob's embedding",
      confirm: "Returns this blob's failed embedding to the queued state and retries it. Continue?",
      hashes: [entity.hash]
    })
  }
  if (entity.embeddingState === 'processing') {
    actions.push({
      id: 'release_expired_claims',
      target: 'release_expired_claims',
      label: "Release this blob's embedding lease",
      confirm: "Returns this blob's embedding claim to the queued state only if it has actually expired. Continue?",
      hashes: [entity.hash]
    })
  }
  if (entity.encodingState === 'failed' && (entity.contentType || '').toLowerCase() !== 'image/webp') {
    actions.push({
      id: 'retry_failed_encodings',
      target: 'retry_failed_encodings',
      label: "Retry this blob's encoding",
      confirm: "Returns this blob's failed WebP encoding to the queued state and retries it. Continue?",
      hashes: [entity.hash]
    })
  }
  if (entity.encodingRaw === 'leased' || entity.encodingRaw === 'received') {
    actions.push({
      id: 'reset_stuck_encoding',
      target: 'reset_stuck_encoding',
      label: "Reset this blob's encoding",
      confirm: "Returns this blob's expired encoding leases and uncommitted results to the queued state. Continue?",
      hashes: [entity.hash]
    })
  }
  return actions
}

const globalRecoveryActions: SyncRecoveryAction[] = [
  {
    id: 'retry_failed_embeddings',
    target: 'retry_failed_embeddings',
    label: 'Retry failed embeddings',
    confirm: 'Returns every failed embedding to the queued state and retries them. Continue?'
  },
  {
    id: 'retry_failed_encodings',
    target: 'retry_failed_encodings',
    label: 'Retry failed encodings',
    confirm: 'Returns every failed WebP encoding to the queued state and retries them. Continue?'
  },
  {
    id: 'release_expired_claims',
    target: 'release_expired_claims',
    label: 'Release expired embedding claims',
    confirm: 'Returns every expired embedding claim to the queued state. Valid claims are not moved. Continue?'
  },
  {
    id: 'reset_stuck_encoding',
    target: 'reset_stuck_encoding',
    label: 'Reset encoding queue',
    confirm: 'Returns every expired encoding lease and uncommitted result to the queued state. Rows being committed are untouched. Continue?'
  },
  {
    id: 'reindex',
    target: 'reindex',
    label: 'Re-embed everything',
    confirm: 'Re-embeds every ready blob. This is a costly, irreversible global operation. Continue?',
    danger: true
  }
]

function useSyncRecovery(onDone: () => void) {
  const [pending, setPending] = useState('')
  const [result, setResult] = useState('')
  const [error, setError] = useState('')
  const running = useRef(false)
  const run = async (action: SyncRecoveryAction) => {
    if (running.current) return
    if (!window.confirm(action.confirm)) return
    running.current = true
    setPending(action.id)
    setResult('')
    setError('')
    try {
      setResult(await invokeSyncRecovery(action))
      onDone()
    } catch (cause) {
      setError(errorMessage(cause))
    } finally {
      running.current = false
      setPending('')
    }
  }
  return { busy: !!pending, pending, result, error, run }
}

async function invokeSyncRecovery(action: SyncRecoveryAction): Promise<string> {
  if (action.target === 'reindex') {
    const counts = await reindex()
    return `Full re-embedding scheduled · total ${counts.total.toLocaleString()} · queued ${counts.pending.toLocaleString()} · processing ${counts.processing.toLocaleString()} · failed ${counts.failed.toLocaleString()} · ready ${counts.ready.toLocaleString()}`
  }
  return recoverResultLine(await recover(action.target, action.hashes))
}

// The result line reports the rows the server actually moved, so a no-op (for
// example an unexpired lease) stays visible instead of ending in silence.
function recoverResultLine(result: RecoverResult): string {
  if (result.retry) return `Retried ${result.retry.reset.toLocaleString()} row(s)`
  if (result.encodingRetry) return `Retried ${result.encodingRetry.reset.toLocaleString()} row(s)`
  if (result.release) return result.release.released > 0
    ? `Released ${result.release.released.toLocaleString()} row(s)`
    : 'No claims were released. Only expired claims are released.'
  if (result.encoding) {
    const { releasedExpiredLeased, resetReceived } = result.encoding.counts
    return `Reset ${(releasedExpiredLeased + resetReceived).toLocaleString()} row(s) (expired leases ${releasedExpiredLeased.toLocaleString()} · uncommitted results ${resetReceived.toLocaleString()})`
  }
  return 'Completed, but the server returned no counts'
}

function SyncRecoveryPanel({ title, description, actions, onDone }: {
  title: string
  description: string
  actions: SyncRecoveryAction[]
  onDone: () => void
}) {
  const state = useSyncRecovery(onDone)
  return <section className="panel" aria-label={title}>
    <h2>{title}</h2>
    <p className="muted">{description}</p>
    {actions.length > 0
      ? <div className="recovery-actions">{actions.map((action) => (
        <button key={action.id} type="button" className={action.danger ? 'danger-button' : undefined} disabled={state.busy} onClick={() => void state.run(action)}>
          {state.pending === action.id ? 'Running…' : action.label}
        </button>
      ))}</div>
      : <p className="muted">No recovery actions are available in the current state.</p>}
    {state.result && <p className="recovery-result" role="status">{state.result}</p>}
    {state.error && <div className="notice error" role="alert">{state.error}</div>}
  </section>
}

// The five form fields the photos page exposes. Everything else the endpoint
// supports is URL-only: deep links from the overview, issues, and album pages
// keep working, and PhotoURLFilterNotice explains them, but the form never
// grows hidden inputs for them.
const photoFormKeys = ['q', 'albumId', 'hash', 'encoding', 'embedding'] as const

// URL-only photo filters, with the label the notice renders for each.
const photoURLFilters: [key: string, label: string][] = [
  ['encodingErrorCode', 'Encoding error code'],
  ['embeddingErrorCode', 'Embedding error code'],
  ['encodingLeaseExpired', 'Encoding lease expired'],
  ['embeddingLeaseExpired', 'Embedding lease expired'],
  ['missingBlob', 'Photos without blobs']
]

function PhotoFilters() {
  const [params] = useSearchParams()
  const navigate = useNavigate()
  const [values, setValues] = useState<Record<string, string>>({})
  useEffect(() => {
    const initial: Record<string, string> = {}
    for (const key of photoFormKeys) initial[key] = params.get(key) || ''
    setValues(initial)
  }, [params])
  const submit = (event: FormEvent) => {
    event.preventDefault()
    const query = new URLSearchParams()
    // URL-only filters stay honored until cleared from the notice, so a deep
    // link keeps narrowing whatever the operator searches for next.
    for (const [key] of photoURLFilters) {
      const value = params.get(key)
      if (value) query.set(key, value)
    }
    for (const key of photoFormKeys) {
      const value = values[key]
      if (value.trim()) query.set(key, value.trim())
    }
    navigate(`/photos${query.size ? `?${query}` : ''}`)
  }
  return <FilterPanel onSubmit={submit}>
    <Field label="Name search" value={values.q || ''} onChange={(value) => setValues({ ...values, q: value })} />
    <Field label="Album ID" value={values.albumId || ''} onChange={(value) => setValues({ ...values, albumId: value })} />
    <Field label="Hash" value={values.hash || ''} onChange={(value) => setValues({ ...values, hash: value })} />
    <SelectField label="Encoding" value={values.encoding || ''} onChange={(value) => setValues({ ...values, encoding: value })} options={encodingOptions} />
    <SelectField label="Embedding" value={values.embedding || ''} onChange={(value) => setValues({ ...values, embedding: value })} options={embeddingOptions} />
  </FilterPanel>
}

// PhotoURLFilterNotice surfaces the URL-only filters a deep link activated,
// so the page explains itself instead of silently returning a subset. The
// clear link drops exactly those filters and keeps the form's five.
function PhotoURLFilterNotice() {
  const [params] = useSearchParams()
  const active = photoURLFilters.filter(([key]) => params.get(key))
  if (active.length === 0) return null
  const withoutURLFilters = new URLSearchParams()
  for (const key of photoFormKeys) {
    const value = params.get(key)
    if (value) withoutURLFilters.set(key, value)
  }
  return <div className="notice url-filters" role="status">
    <p>Filters from the URL are active:</p>
    <ul className="filter-chips">
      {active.map(([key, label]) => <li key={key}><span>{label}</span><code>{params.get(key)}</code></li>)}
    </ul>
    <Link to={`/photos${withoutURLFilters.size ? `?${withoutURLFilters.toString()}` : ''}`}>Clear URL filters</Link>
  </div>
}

function AlbumsPage() {
  const location = useLocation()
  const state = useAdminData<Page<Album>>(apiPath('albums', location.search))
  const albums = state.data && 'albums' in state.data ? state.data.albums : []
  return <>
    <PageTitle title="Albums" description="Review the processing status and photo counts of upload archives." />
    <AlbumFilters />
    {state.error && <ErrorPanel message={state.error} />}
    {state.loading && !state.data && <p role="status">Loading…</p>}
    {state.data && <>
      <div className="table-wrap"><table className="data-table"><thead><tr><th>Filename</th><th>ID</th><th>Status</th><th>Photos</th></tr></thead><tbody>
        {albums.map((album) => <tr key={album.id}>
          <td><Link to={`/albums/${encodeURIComponent(album.id)}`} state={returnState(location)}>{album.originalFilename}</Link></td>
          <td className="hash-cell">{album.id}</td><td>{albumStatus(album.status)}</td>
          <td><CountLink value={album.photoCount} to={filterURL('photos', { albumId: album.id })} /></td>
        </tr>)}
        {albums.length === 0 && <EmptyRow columns={4} />}
      </tbody></table></div>
      <Pagination nextCursor={state.data.nextCursor} />
    </>}
  </>
}

function AlbumFilters() {
  const [params] = useSearchParams()
  const navigate = useNavigate()
  const [q, setQ] = useState(params.get('q') || '')
  const [status, setStatus] = useState(params.get('status') || '')
  useEffect(() => { setQ(params.get('q') || ''); setStatus(params.get('status') || '') }, [params])
  const submit = (event: FormEvent) => {
    event.preventDefault()
    const query = new URLSearchParams()
    if (q.trim()) query.set('q', q.trim())
    if (status) query.set('status', status)
    navigate(`/albums${query.size ? `?${query}` : ''}`)
  }
  return <FilterPanel onSubmit={submit}>
    <Field label="Filename search" value={q} onChange={setQ} />
    <SelectField label="Album status" value={status} onChange={setStatus} options={[
      ['', 'All'], ['QUEUED', 'Queued'], ['PROCESSING', 'Processing'], ['SUCCEEDED', 'Succeeded'], ['FAILED', 'Failed']
    ]} />
  </FilterPanel>
}

function PhotoTable({ photos }: { photos: Photo[] }) {
  const location = useLocation()
  return <div className="table-wrap"><table className="data-table photo-table"><thead><tr><th>Preview</th><th>Photo</th><th>Album</th><th>Encoding</th><th>Embedding</th><th>Size</th></tr></thead><tbody>
    {photos.map((photo) => <tr key={`${photo.albumId}:${photo.idx}`}>
      <td>{photo.missingBlob ? <span className="image-fallback">Unavailable</span> : <img className="thumbnail" src={imagePath(photo.hash)} loading="lazy" alt="" onError={(event) => showImageFallback(event.currentTarget)} />}</td>
      <td><Link to={`/photos/${encodeURIComponent(photo.albumId)}/${photo.idx}`} state={returnState(location)}>{photo.name}</Link><small className="subline">{photo.width} × {photo.height} · {photo.hash}</small></td>
      <td><Link to={`/albums/${encodeURIComponent(photo.albumId)}`} state={returnState(location)}>{photo.albumName}</Link></td>
      <td>{photo.missingBlob ? 'No blob' : stateLabel(photo.encoding)}<RawState raw={photo.encodingRaw} /></td>
      <td>{photo.missingBlob ? 'No blob' : stateLabel(photo.embedding)}<RawState raw={photo.embeddingRaw} /></td>
      <td>{photo.sizeBytes === null ? '—' : formatBytes(photo.sizeBytes)}</td>
    </tr>)}
    {photos.length === 0 && <EmptyRow columns={6} />}
  </tbody></table></div>
}

function PhotoDetailPage() {
  const { albumId = '', idx = '' } = useParams()
  const location = useLocation()
  const [refreshKey, setRefreshKey] = useState(0)
  const path = apiPath(`photos/${encodeURIComponent(albumId)}/${encodeURIComponent(idx)}`)
  const state = useAdminData<Photo>(path, refreshKey)
  const photo = state.data
  return <>
    <BackLink fallback="/photos" />
    <PageTitle title={photo?.name || 'Photo detail'} description={photo ? `${photo.albumName} · ${photo.width} × ${photo.height}` : undefined} />
    {state.error && <ErrorPanel message={state.error} />}
    {state.loading && !photo && <p role="status">Loading…</p>}
    {photo && <div className="detail-grid">
      <section className="panel image-panel">
        {photo.missingBlob ? <div className="large-fallback">Blob unavailable</div> : <img className="detail-image" src={imagePath(photo.hash)} alt={photo.name} onError={(event) => showImageFallback(event.currentTarget)} />}
        <dl className="metadata">
          <dt>Album</dt><dd><Link to={`/albums/${encodeURIComponent(photo.albumId)}`} state={returnState(location)}>{photo.albumName}</Link> ({photo.albumId})</dd>
          <dt>Blob</dt><dd>{photo.missingBlob ? 'Blob unavailable' : <Link to={`/blobs/${encodeURIComponent(photo.hash)}`} state={returnState(location)}>{photo.hash}</Link>}</dd>
          <dt>Filename</dt><dd>{photo.name}</dd><dt>Resolution</dt><dd>{photo.width} × {photo.height}</dd>
          <dt>Size</dt><dd>{photo.sizeBytes === null ? '—' : formatBytes(photo.sizeBytes)}</dd><dt>Content type</dt><dd>{photo.contentType ?? '—'}</dd>
          <dt>Blob present</dt><dd>{photo.missingBlob ? 'No' : 'Yes'}</dd>
        </dl>
      </section>
      <PipelineDetails encoding={{ raw: photo.encodingRaw, state: photo.encoding, error: photo.encodingError, skippedReason: photo.skippedReason, observation: photo.encodingObservation }} embedding={{ raw: photo.embeddingRaw, state: photo.embedding, error: photo.embeddingError, observation: photo.embeddingObservation }} missing={photo.missingBlob} />
    </div>}
    {photo && !photo.missingBlob && <SyncRecoveryPanel
      title="Recover this blob"
      description="Shows only the actions that can run in the current pipeline state. Each action runs immediately after confirmation and reports how many rows it returned to the queued state; lease release moves only claims that have actually expired."
      actions={entityRecoveryActions({
        hash: photo.hash,
        contentType: photo.contentType,
        encodingState: photo.encoding,
        encodingRaw: photo.encodingRaw,
        embeddingState: photo.embedding
      })}
      onDone={() => setRefreshKey((key) => key + 1)}
    />}
  </>
}

function BlobDetailPage() {
  const { hash = '' } = useParams()
  const location = useLocation()
  const [refreshKey, setRefreshKey] = useState(0)
  const state = useAdminData<Blob>(apiPath(`blobs/${encodeURIComponent(hash)}`), refreshKey)
  const blob = state.data
  return <>
    <BackLink fallback="/photos" />
    <PageTitle title="Blob detail" description={hash} />
    {state.error && <ErrorPanel message={state.error} />}
    {state.loading && !blob && <p role="status">Loading…</p>}
    {blob && <div className="detail-grid">
      <section className="panel">
        <h2>Content</h2>
        <dl className="metadata">
          <dt>Hash</dt><dd className="hash-cell">{blob.hash}</dd><dt>Type</dt><dd>{blob.contentType}</dd><dt>Size</dt><dd>{formatBytes(blob.sizeBytes)}</dd>
          <dt>Photo references</dt><dd><Link to={filterURL('photos', { hash: blob.hash })} state={returnState(location)}>View {blob.photoCount.toLocaleString()} photo(s)</Link></dd>
          <dt>Album count</dt><dd>{blob.albumCount}</dd>
        </dl>
      </section>
      <PipelineDetails encoding={{ raw: blob.encodingRaw, state: blob.encoding, error: blob.encodingError, skippedReason: blob.skippedReason, observation: blob.encodingObservation }} embedding={{ raw: blob.embeddingRaw, state: blob.embedding, error: blob.embeddingError, observation: blob.embeddingObservation }} missing={false} />
    </div>}
    {blob && <SyncRecoveryPanel
      title="Recover this blob"
      description="Shows only the actions that can run in the current pipeline state. Each action runs immediately after confirmation and reports how many rows it returned to the queued state; lease release moves only claims that have actually expired."
      actions={entityRecoveryActions({
        hash: blob.hash,
        contentType: blob.contentType,
        encodingState: blob.encoding,
        encodingRaw: blob.encodingRaw,
        embeddingState: blob.embedding
      })}
      onDone={() => setRefreshKey((key) => key + 1)}
    />}
    {blob && <PipelineHistory hash={blob.hash} />}
  </>
}

// AlbumDetailPage is the slim album drilldown: processing state, scoped
// counts, drilldown links into the photos list, and the latest extraction
// run. The per-album pipeline tables moved to photos?albumId=&encoding=...
// deep links; only the album's own facts stay on this page.
function AlbumDetailPage() {
  const { id = '' } = useParams()
  const location = useLocation()
  const state = useAdminData<AlbumDetail>(apiPath(`albums/${encodeURIComponent(id)}`))
  const album = state.data
  return <>
    <BackLink fallback="/albums" />
    <PageTitle title={album?.originalFilename || 'Album detail'} description={album?.id || id} />
    {state.error && <ErrorPanel message={state.error} />}
    {state.loading && !album && <p role="status">Loading…</p>}
    {album && <>
      <section className="panel">
        <div className="summary-grid compact">
          <Stat label="Album status" value={albumStatus(album.status)} to={`/albums?status=${encodeURIComponent(album.status)}`} />
          <Stat label="Photo rows" value={album.photoCount} to={filterURL('photos', { albumId: album.id })} />
          <Stat label="Photos without blobs" value={album.missingBlobPhotos} to={filterURL('photos', { albumId: album.id, missingBlob: 'true' })} />
        </div>
        <p>
          <Link to={filterURL('photos', { albumId: album.id })} state={returnState(location)}>Browse this album's photos</Link>
          {' · '}<Link to={filterURL('photos', { albumId: album.id, encoding: 'failed' })} state={returnState(location)}>Encoding-failed photos</Link>
          {' · '}<Link to={filterURL('photos', { albumId: album.id, embedding: 'failed' })} state={returnState(location)}>Embedding-failed photos</Link>
        </p>
        <ErrorDetails error={album.error} />
      </section>
      <LatestExtractionPanel album={album} />
    </>}
  </>
}

// LatestExtractionPanel renders the newest recorded extraction run only.
// Older runs stay in the append-only history for the issues page's
// latest-run failure browser; this page no longer pages through them.
function LatestExtractionPanel({ album }: { album: AlbumDetail }) {
  const run = album.latestExtraction
  return <section className="panel">
    <h2>ZIP extraction</h2>
    {!album.extractionRecorded && <p className="notice warning">This album has no extraction record. It predates the extraction-recording feature, which does not mean it has zero failed entries.</p>}
    {run && <>
      <div className="summary-grid compact">
        <Stat label="Latest run" value={`#${run.runId} · ${run.status}`} />
        <Stat label="Registered" value={run.registeredCount} />
        <Stat label="Skipped" value={run.skippedCount} />
        <Stat label="Failed" value={run.failedCount} />
      </div>
      <p className="muted">
        Started {formatTime(run.startedAt)} · Finished {run.completedAt ? formatTime(run.completedAt) : 'running/not recorded'}
        {' · '}entries recorded {run.entriesRecorded.toLocaleString()}
        {run.totalEntries === null ? ' (total unknown)' : ` / total ${run.totalEntries.toLocaleString()}`}
        {run.candidateEntries === null ? '' : ` · image-extension candidates ${run.candidateEntries.toLocaleString()}`}
        {run.entriesComplete ? ' · entry recording complete' : ' · entry recording incomplete'}
      </p>
      {run.countsProvisional && <p className="notice warning" role="status">The run is still in progress. The registered, skipped, and failed counts are the values recorded so far and may still change.</p>}
      {run.failureReasonCode && <p className="notice error">{run.failureReasonCode}{run.errorDetail ? ` · ${run.errorDetail}` : ''}</p>}
    </>}
  </section>
}

function PipelineDetails({ encoding, embedding, missing }: {
  encoding: { raw: string; state: string; error: string; skippedReason?: string; observation?: import('./api').StageObservation }
  embedding: { raw: string; state: string; error: string; observation?: import('./api').StageObservation }
  missing: boolean
}) {
  return <section className="panel pipeline-detail">
    <h2>Pipeline status</h2>
    {missing ? <p className="notice warning">There is no linked blob, so encoding and embedding states do not exist.</p> : <>
      <StateDetail title="Encoding" raw={encoding.raw} state={encoding.state} error={encoding.error} observation={encoding.observation} />
      {encoding.state === 'skipped' && <p className="muted">Skipped classification: {encoding.skippedReason || 'unavailable'} by the stored content type (the reason for skipping itself is not provided).</p>}
      <StateDetail title="Embedding" raw={embedding.raw} state={embedding.state} error={embedding.error} observation={embedding.observation} />
      {embedding.state === 'queued' && <p className="muted">Waiting for embedding processing regardless of the encoding state.</p>}
    </>}
  </section>
}

// StateDetail shows the raw stored state alongside the normalized one so a
// future state stays inspectable. It is display-only: the raw-state browse
// filters retired with the blobs list, so there is no link to build.
function StateDetail({ title, raw, state, error, observation }: { title: string; raw: string; state: string; error: string; observation?: import('./api').StageObservation }) {
  return <div className="state-detail">
    <h3>{title}</h3><p>Normalized state: <strong>{state || '—'}</strong></p>
    <p>Raw state: {raw || '—'}</p>
    <ObservationDetails observation={observation} />
    <ErrorDetails error={error} />
  </div>
}

function ObservationDetails({ observation }: { observation?: import('./api').StageObservation }) {
  if (!observation) return <p className="muted">No observation recorded</p>
  return <dl className="metadata observation-details">
    <dt>Started</dt><dd>{observation.startedAt ? formatTime(observation.startedAt) : '—'}</dd>
    <dt>Finished</dt><dd>{observation.finishedAt ? formatTime(observation.finishedAt) : '—'}</dd>
    <dt>Last status change</dt><dd>{observation.statusChangedAt ? formatTime(observation.statusChangedAt) : '—'}</dd>
    <dt>Attempts</dt><dd>{observation.attempts}</dd>
    <dt>Phase</dt><dd>{observation.phase || '—'}</dd>
    <dt>Error code</dt><dd>{observation.errorCode || '—'}</dd>
    <dt>Retryability</dt><dd>{observation.retryability || 'unknown'}</dd>
  </dl>
}

// PipelineHistory renders the blob's recent pipeline events in one bounded
// fetch. The stage filter and load-more paging retired with the raw-state
// browse surface: the newest twenty events answer "what happened to this
// blob", and deeper forensics belong in the server logs.
function PipelineHistory({ hash }: { hash: string }) {
  const state = useAdminData<HistoryPage>(apiPath(`blobs/${encodeURIComponent(hash)}/history`, '?limit=20'))
  const events = state.data?.events || []
  return <section className="panel pipeline-history">
    <h2>Pipeline history</h2>
    {state.error && <ErrorPanel message={state.error} />}
    {state.loading && !state.data && <p role="status">Loading history…</p>}
    {events.length > 0 && <ol className="event-list">{events.map((event) => <li key={event.id}>
      <time dateTime={event.occurredAt}>{formatTime(event.occurredAt)}</time>
      <strong>{event.stage} · {event.event}</strong>
      <span>Status {event.status}{event.phase ? ` · Phase ${event.phase}` : ''} · Attempt {event.attemptId}</span>
      {event.errorCode && <span>Error code {event.errorCode}</span>}
    </li>)}</ol>}
    {state.data && events.length === 0 && <p className="muted">No events recorded since the migration.</p>}
    {state.data?.nextCursor && <p className="muted">Only the latest 20 events are shown.</p>}
  </section>
}

// RawState keeps the raw stored value visible next to the normalized label —
// the safety net for states this version does not recognize. It is a plain
// subline: the raw filters are gone, so there is nothing to link to.
function RawState({ raw }: { raw: string }) {
  if (!raw) return null
  return <small className="subline">Raw: {raw}</small>
}

function ErrorDetails({ error }: { error: string }) {
  if (!error) return <p className="muted">No current error</p>
  return <details className="error-details"><summary>Current error detail</summary><pre>{error}</pre></details>
}

function Pagination({ nextCursor }: { nextCursor: string | null }) {
  const location = useLocation()
  const prior = (location.state as { adminPrevious?: string } | null)?.adminPrevious
  const params = new URLSearchParams(location.search)
  const url = `${location.pathname}${location.search}`
  const next = () => {
    if (!nextCursor) return url
    params.set('cursor', nextCursor)
    return `${location.pathname}?${params.toString()}`
  }
  return <div className="pagination">
    {prior ? <Link className="button-link" to={prior} state={(location.state as { adminPreviousPrevious?: string } | null)?.adminPreviousPrevious ? { adminPrevious: (location.state as { adminPreviousPrevious: string }).adminPreviousPrevious } : null}>← Previous</Link> : <span className="muted">For previous pages, use the browser history.</span>}
    <span className="muted">Cursor-based pages</span>
    {nextCursor ? <Link className="button-link" to={next()} state={{ adminPrevious: url, adminPreviousPrevious: prior }}>Next →</Link> : <span className="muted">Last page</span>}
  </div>
}

function BackLink({ fallback }: { fallback: string }) {
  const location = useLocation()
  const returnTo = (location.state as { returnTo?: string } | null)?.returnTo
  return <Link className="back-link" to={returnTo || fallback} state={returnTo ? location.state : null}>← Back to list</Link>
}

function returnState(location: ReturnType<typeof useLocation>) {
  return { ...(location.state || {}), returnTo: `${location.pathname}${location.search}` }
}

function FilterPanel({ children, onSubmit }: { children: ReactNode; onSubmit: (event: FormEvent) => void }) {
  const location = useLocation()
  return <form className="filter-panel" onSubmit={onSubmit}>
    {children}<div className="filter-actions"><button type="submit">Search</button><Link to={location.pathname} className="button-link secondary">Reset</Link></div>
  </form>
}

function Field({ label, value, onChange }: { label: string; value: string; onChange: (value: string) => void }) {
  return <label className="field">{label}<input value={value} onChange={(event) => onChange(event.target.value)} /></label>
}

function SelectField({ label, value, options, onChange }: { label: string; value: string; options: [string, string][]; onChange: (value: string) => void }) {
  return <label className="field">{label}<select value={value} onChange={(event) => onChange(event.target.value)}>{options.map(([key, text]) => <option key={key} value={key}>{text}</option>)}</select></label>
}

const encodingOptions: [string, string][] = [
  ['', 'All'], ['queued', 'Queued'], ['processing', 'Processing'], ['validating', 'Validating'], ['committing', 'Committing'], ['completed', 'Completed'], ['skipped', 'Skipped'], ['failed', 'Failed'], ['unknown', 'Unknown']
]
const embeddingOptions: [string, string][] = [
  ['', 'All'], ['queued', 'Queued'], ['processing', 'Processing'], ['completed', 'Completed'], ['failed', 'Failed'], ['unknown', 'Unknown']
]

function ErrorPanel({ message }: { message: string }) {
  return <div className="notice error" role="alert">Request failed: {message}</div>
}

function EmptyRow({ columns }: { columns: number }) {
  return <tr><td className="empty-row" colSpan={columns}>No items match the filters.</td></tr>
}

function NotFoundPage() {
  return <section className="panel"><h1>Page not found</h1><Link to="/">Go to overview</Link></section>
}

function albumStatus(status: string): string {
  const labels: Record<string, string> = { QUEUED: 'Queued', PROCESSING: 'Processing', SUCCEEDED: 'Succeeded', FAILED: 'Failed' }
  return labels[status] || `${status} (new state)`
}

function formatBytes(value: number): string {
  if (!Number.isFinite(value)) return '—'
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB']
  let size = value
  let unit = 0
  while (Math.abs(size) >= 1024 && unit < units.length - 1) { size /= 1024; unit++ }
  return `${size.toLocaleString(undefined, { maximumFractionDigits: unit ? 1 : 0 })} ${units[unit]}`
}

function showImageFallback(image: HTMLImageElement) {
  const fallback = document.createElement('span')
  fallback.className = image.classList.contains('detail-image') ? 'large-fallback' : 'image-fallback'
  fallback.textContent = 'Image unavailable'
  image.replaceWith(fallback)
}
