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
  return error instanceof Error ? error.message : '알 수 없는 오류가 발생했습니다'
}

function formatTime(value: string): string {
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? value : date.toLocaleString()
}

function AdminApp() {
  return (
    <div className="admin-root">
      <header className="admin-header">
        <Link className="brand" to="/">관리자</Link>
        <nav aria-label="관리자 메뉴">
          <Link to="/">개요</Link>
          <Link to="/photos">사진</Link>
          <Link to="/albums">앨범</Link>
          <Link to="/issues">이슈</Link>
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
      <PageTitle title="개요" description="카탈로그 상태를 확인합니다." />
      <div className="toolbar">
        <button type="button" onClick={() => void refresh()} disabled={loading}>새로고침</button>
        <span className="muted">
          업데이트 {updatedAt || '—'} · 측정 {lastGood ? formatTime(lastGood.measuredAt) : '—'} · 20초마다 갱신
        </span>
      </div>
      {error && <div className="notice warning" role="alert">갱신 실패: {error}{lastGood ? ' · 마지막 정상 데이터를 표시 중입니다.' : ''}</div>}
      {loading && !lastGood && <p role="status">불러오는 중…</p>}
      {!lastGood && !loading && <ErrorPanel message={error || '개요를 불러오지 못했습니다.'} />}
      {lastGood && <>
        <section className="panel">
          <h2>라이브러리</h2>
          <div className="summary-grid">
            <Stat label="앨범" value={lastGood.library.albums} to="/albums" />
            <Stat label="사진" value={lastGood.library.photos} to="/photos" />
            <Stat label="고유 블롭" value={lastGood.library.blobs} to="/photos" />
          </div>
          <p className="muted">앨범과 사진은 라이브러리 전체 재고 수입니다. 파이프라인 표는 상태별 고유 블롭 수를 표시하며 사진·앨범 참조 수를 상태별로 나누지 않습니다.</p>
        </section>
        <CategorySection title="WebP 인코딩" categories={lastGood.encoding.categories} filterKey="encoding" />
        <CategorySection title="임베딩" categories={lastGood.embedding.categories} filterKey="embedding" />
        <HealthPanel health={lastGood.health} />
      </>}
      <ExecutionControlPanel />
      <SyncRecoveryPanel
        title="복구 작업"
        description="전체 범위 복구 동작입니다. 각 동작은 확인 후 즉시 실행되고 대기 상태로 돌아간 행 수를 표시합니다. 유효한 클레임을 강제로 해제하는 동작은 제공하지 않으며, 유효한 리스는 만료를 기다립니다."
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
    if (!window.confirm('서버의 cwebp로 대기 중인 WebP 변환 대상 전체를 인코딩합니다. CPU를 많이 사용하며 백그라운드에서 실행됩니다. 계속하시겠습니까?')) return
    const outcome = await runLocalEncoding()
    setResult(outcome.started ? '로컬 인코딩이 백그라운드에서 시작되었습니다.' : '이미 로컬 인코딩이 실행 중입니다.')
  })

  return <section className="panel" aria-label="실행 제어">
    <h2>실행 제어</h2>
    <p className="muted">서버의 로컬 실행을 제어합니다. 외부 워커는 제어하지 않습니다.</p>
    {automatic.error && <div className="notice warning" role="alert">상태 조회 실패: {automatic.error}</div>}
    {automatic.loading && !automatic.data && <p role="status">불러오는 중…</p>}
    {status && <>
      <h3>자동 임베딩</h3>
      <dl className="metadata">
        <dt>자동 설정</dt><dd>{status.enabled ? '활성' : '일시정지'}</dd>
        <dt>현재 실행</dt><dd>{status.running ? '실행 중' : '실행 중 아님'}</dd>
      </dl>
      <button type="button" disabled={!!pending} onClick={toggleAutomatic}>
        {pending === 'automatic' ? '자동 설정 변경 중…' : status.enabled ? '자동 임베딩 일시정지' : '자동 임베딩 재개'}
      </button>
      <p className="muted">일시정지는 진행 중인 묶음이 끝난 뒤 적용되며, 즉시 되돌릴 수 있어 확인 없이 전환합니다. 설정은 서버를 다시 시작하면 활성으로 돌아갑니다.</p>
      {!status.running && <p className="muted">실행 중 아님은 워커가 아직 시작되지 않았다는 뜻입니다. 모델 로드 후 자동으로 시작됩니다.</p>}
    </>}
    <h3>로컬 인코딩 실행</h3>
    <button type="button" disabled={!!pending} onClick={runLocal}>
      {pending === 'encoding' ? '시작 중…' : '로컬 인코딩 실행'}
    </button>
    <p className="muted">시작 시점의 대기(pending) 대상만 처리하고 종료합니다. 이후 업로드는 다음 실행까지 기다립니다.</p>
    {result && <p className="recovery-result" role="status">{result}</p>}
    {error && <div className="notice error" role="alert">{error}</div>}
  </section>
}

function IssuesPage() {
  const state = useAdminData<Issues>(apiPath('issues'))
  const [params] = useSearchParams()
  const entryReasonCode = params.get('entryReasonCode') || ''
  return <>
    <PageTitle title="현재 이슈" description="현재 실패 상태와 앨범별 최신 추출 실행의 실패 항목만 집계합니다." />
    {state.error && <ErrorPanel message={state.error} />}
    {state.loading && !state.data && <p role="status">불러오는 중…</p>}
    {state.data && <>
      <p className="muted">측정 {formatTime(state.data.measuredAt)} · 그룹 {state.data.totalGroups.toLocaleString()}개</p>
      <p className="muted">{state.data.countScope}</p>
      {state.data.truncated && <div className="notice warning">상위 100개 그룹만 표시합니다. 나머지 {state.data.remainingGroups.toLocaleString()}개 그룹은 응답에서 생략되었습니다.</div>}
      {state.data.groups.length === 0
        ? <section className="panel"><p>현재 기록된 실패 이슈가 없습니다.</p></section>
        : <div className="table-wrap"><table className="data-table"><thead><tr><th>단계</th><th>안전한 오류 코드</th><th>실패 수</th><th>개수 단위</th><th>확인</th></tr></thead><tbody>
          {state.data.groups.map((group) => <IssueRow key={`${group.stage}:${group.errorCode}`} group={group} />)}
        </tbody></table></div>}
      {state.data.groups.some((group) => group.errorCode === 'legacy_unknown') && <div className="notice warning">
        legacy_unknown은 기존 실패의 세부 메시지를 안전하게 분류할 수 없다는 뜻입니다. 원문 오류나 비밀 값은 이 이슈 목록에 표시하지 않습니다.
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
    encoding: '인코딩', embedding: '임베딩', album_extraction: '아카이브 추출', extraction_entry: '추출 항목'
  }
  const unitLabel: Record<string, string> = { blobs: '고유 블롭', albums: '앨범', entries: '실패 항목' }
  return <tr>
    <th scope="row">{stageLabel[group.stage] || group.stage}</th>
    <td><code>{group.errorCode}</code></td>
    <td>{target ? <CountLink value={group.failureCount} to={target} /> : group.failureCount.toLocaleString()}</td>
    <td>{unitLabel[group.countUnit] || group.countUnit}</td>
    <td>{target ? <Link to={target}>페이지 보기</Link> : '—'}</td>
  </tr>
}

function ExtractionFailureBrowser({ reasonCode }: { reasonCode: string }) {
  const [cursor, setCursor] = useState('')
  useEffect(() => { setCursor('') }, [reasonCode])
  const query = new URLSearchParams({ reasonCode, limit: '50' })
  if (cursor) query.set('cursor', cursor)
  const state = useAdminData<ExtractionFailurePage>(apiPath('extraction-failures', `?${query.toString()}`))
  return <section className="panel">
    <h2>최신 추출 실행 실패 항목 · {reasonCode}</h2>
    <p className="muted">앨범마다 가장 최근에 기록된 실행의 실패 항목만 페이지로 표시합니다. 과거 실행에서 해결된 실패는 포함하지 않습니다.</p>
    {state.error && <ErrorPanel message={state.error} />}
    {state.loading && !state.data && <p role="status">불러오는 중…</p>}
    {state.data && <>
      <div className="table-wrap"><table className="data-table"><thead><tr><th>앨범</th><th>실행</th><th>항목</th><th>코드</th></tr></thead><tbody>
        {state.data.failures.map((failure) => <tr key={`${failure.albumId}:${failure.ordinal}`}>
          <td><Link to={`/albums/${encodeURIComponent(failure.albumId)}`}>{failure.originalFilename}</Link><small className="subline">{failure.albumId}</small></td>
          <td>#{failure.runId} · 최신 실행</td><td>{failure.name}<small className="subline">항목 #{failure.ordinal}</small></td><td><code>{failure.reasonCode}</code></td>
        </tr>)}
        {state.data.failures.length === 0 && <EmptyRow columns={4} />}
      </tbody></table></div>
      <div className="pagination">
        {cursor && <button type="button" onClick={() => setCursor('')}>첫 페이지</button>}
        {state.data.nextCursor && <button type="button" onClick={() => setCursor(state.data!.nextCursor!)}>다음 실패 항목 더 보기</button>}
      </div>
    </>}
  </section>
}

// HealthPanel renders the overview's "시스템 상태" section: the catalog's
// lease and integrity counts with drill-down links, the vector-store count
// comparison, and a one-line summary of the server's feature wiring. Every
// number is a measurement from the same snapshot; the flags report
// configuration, never worker liveness.
function HealthPanel({ health }: { health: Overview['health'] }) {
  const vector = health.vectorCount
  return <section className="panel" aria-label="시스템 상태">
    <h2>시스템 상태</h2>
    <div className="summary-grid">
      <Stat label="만료된 임베딩 리스" value={health.embeddingExpiredLeases} to="/photos?embeddingLeaseExpired=true" />
      <Stat label="만료된 인코딩 리스" value={health.encodingExpiredLeases} to="/photos?encodingLeaseExpired=true" />
      {/* 참조 없는 블롭 has no list to drill into anymore: the blobs list is
          retired, so the count stays a plain number. */}
      <Stat label="참조 없는 블롭" value={health.unreferencedBlobs} />
      <Stat label="블롭 없는 사진" value={health.missingBlobPhotos} to="/photos?missingBlob=true" />
    </div>
    <h3>벡터 스토어</h3>
    {vector.status === 'disabled' && <p>비활성 — 벡터 저장소가 설정되지 않아 개수는 측정되지 않습니다.</p>}
    {vector.status === 'unavailable' && <div className="notice warning" role="status">측정 불가 — {vector.message || '벡터 개수를 읽을 수 없습니다.'}</div>}
    {vector.status === 'available' && <>
      <p>{vector.countAgreement ? '일치' : '불일치'} <span className="muted">(기대 {vector.expectedPoints.toLocaleString()} · 실제 {vector.actualPoints?.toLocaleString()})</span></p>
      <p className="muted">개수 비교일 뿐 전체 동기화나 포인트 무결성을 검증하지 않습니다.</p>
    </>}
    <p className="muted">
      인코딩 워커 API {health.encodingEnabled ? '사용 가능' : '비활성'} · 임베딩 모델 {health.modelEnabled ? '사용 가능' : '비활성'} · 벡터 스토어 {health.vectorStoreEnabled ? '사용 가능' : '비활성'}
    </p>
    <p className="muted">활성화 여부는 서버 설정을 뜻하며 워커 프로세스의 실행 상태가 아닙니다.</p>
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
    {filterKey === 'embedding' && <p className="muted">대기(queued)는 인코딩 상태와 관계없이 임베딩 작업자가 처리할 수 있는 상태입니다.</p>}
    <p className="muted">각 블롭은 해당 단계의 정규화 상태 하나에 한 번만 집계됩니다. 표의 수는 고유 블롭 수이며 링크는 해당 상태의 사진 행을 엽니다.</p>
    <table className="data-table"><thead><tr><th>상태</th><th>고유 블롭</th></tr></thead>
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
    queued: '대기', processing: '처리 중', validating: '검증 중', committing: '저장 중',
    completed: '완료', skipped: '건너뜀', failed: '실패', unknown: '알 수 없음'
  }
  return labels[state] ? `${labels[state]} (${state})` : `${state} (새 상태)`
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
    <PageTitle title="사진" description="사진 행과 연결된 앨범·블롭·파이프라인 상태를 검색합니다." />
    <PhotoFilters />
    <PhotoURLFilterNotice />
    {state.error && <ErrorPanel message={state.error} />}
    {state.loading && !state.data && <p role="status">불러오는 중…</p>}
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
      label: '이 블롭 임베딩 재시도',
      confirm: '이 블롭의 실패한 임베딩을 대기 상태로 되돌려 다시 시도합니다. 계속하시겠습니까?',
      hashes: [entity.hash]
    })
  }
  if (entity.embeddingState === 'processing') {
    actions.push({
      id: 'release_expired_claims',
      target: 'release_expired_claims',
      label: '이 블롭 임베딩 리스 해제',
      confirm: '이 블롭의 임베딩 클레임이 실제로 만료된 경우에만 대기 상태로 되돌립니다. 계속하시겠습니까?',
      hashes: [entity.hash]
    })
  }
  if (entity.encodingState === 'failed' && (entity.contentType || '').toLowerCase() !== 'image/webp') {
    actions.push({
      id: 'retry_failed_encodings',
      target: 'retry_failed_encodings',
      label: '이 블롭 인코딩 재시도',
      confirm: '이 블롭의 실패한 WebP 인코딩을 대기 상태로 되돌려 다시 시도합니다. 계속하시겠습니까?',
      hashes: [entity.hash]
    })
  }
  if (entity.encodingRaw === 'leased' || entity.encodingRaw === 'received') {
    actions.push({
      id: 'reset_stuck_encoding',
      target: 'reset_stuck_encoding',
      label: '이 블롭 인코딩 리셋',
      confirm: '이 블롭의 만료된 인코딩 리스와 커밋되지 않은 결과를 대기 상태로 되돌립니다. 계속하시겠습니까?',
      hashes: [entity.hash]
    })
  }
  return actions
}

const globalRecoveryActions: SyncRecoveryAction[] = [
  {
    id: 'retry_failed_embeddings',
    target: 'retry_failed_embeddings',
    label: '실패한 임베딩 재시도',
    confirm: '실패 상태인 임베딩 전체를 대기 상태로 되돌려 다시 시도합니다. 계속하시겠습니까?'
  },
  {
    id: 'retry_failed_encodings',
    target: 'retry_failed_encodings',
    label: '실패한 인코딩 재시도',
    confirm: '실패 상태인 WebP 인코딩 전체를 대기 상태로 되돌려 다시 시도합니다. 계속하시겠습니까?'
  },
  {
    id: 'release_expired_claims',
    target: 'release_expired_claims',
    label: '만료된 임베딩 클레임 해제',
    confirm: '만료된 임베딩 클레임 전체를 대기 상태로 되돌립니다. 유효한 클레임은 이동하지 않습니다. 계속하시겠습니까?'
  },
  {
    id: 'reset_stuck_encoding',
    target: 'reset_stuck_encoding',
    label: '인코딩 큐 리셋',
    confirm: '만료된 인코딩 리스와 커밋되지 않은 결과 전체를 대기 상태로 되돌립니다. 커밋 중인 행은 건드리지 않습니다. 계속하시겠습니까?'
  },
  {
    id: 'reindex',
    target: 'reindex',
    label: '전체 재임베딩',
    confirm: '모든 준비(ready) 블롭을 다시 임베딩합니다. 비용이 크고 되돌릴 수 없는 전역 작업입니다. 계속하시겠습니까?',
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
    return `전체 재임베딩이 등록되었습니다 · 전체 ${counts.total.toLocaleString()} · 대기 ${counts.pending.toLocaleString()} · 처리 중 ${counts.processing.toLocaleString()} · 실패 ${counts.failed.toLocaleString()} · 완료 ${counts.ready.toLocaleString()}`
  }
  return recoverResultLine(await recover(action.target, action.hashes))
}

// The result line reports the rows the server actually moved, so a no-op (for
// example an unexpired lease) stays visible instead of ending in silence.
function recoverResultLine(result: RecoverResult): string {
  if (result.retry) return `${result.retry.reset.toLocaleString()}행 재시도됨`
  if (result.encodingRetry) return `${result.encodingRetry.reset.toLocaleString()}행 재시도됨`
  if (result.release) return result.release.released > 0
    ? `${result.release.released.toLocaleString()}행 해제됨`
    : '해제된 클레임이 없습니다. 만료된 클레임만 해제 대상입니다.'
  if (result.encoding) {
    const { releasedExpiredLeased, resetReceived } = result.encoding.counts
    return `${(releasedExpiredLeased + resetReceived).toLocaleString()}행 리셋됨 (만료 리스 ${releasedExpiredLeased.toLocaleString()} · 미커밋 결과 ${resetReceived.toLocaleString()})`
  }
  return '완료되었지만 서버가 결과 수를 반환하지 않았습니다'
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
          {state.pending === action.id ? '실행 중…' : action.label}
        </button>
      ))}</div>
      : <p className="muted">현재 상태에서 실행할 수 있는 복구 동작이 없습니다.</p>}
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
  ['encodingErrorCode', '인코딩 오류 코드'],
  ['embeddingErrorCode', '임베딩 오류 코드'],
  ['encodingLeaseExpired', '인코딩 리스 만료'],
  ['embeddingLeaseExpired', '임베딩 리스 만료'],
  ['missingBlob', '블롭 없는 사진']
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
    <Field label="이름 검색" value={values.q || ''} onChange={(value) => setValues({ ...values, q: value })} />
    <Field label="앨범 ID" value={values.albumId || ''} onChange={(value) => setValues({ ...values, albumId: value })} />
    <Field label="해시" value={values.hash || ''} onChange={(value) => setValues({ ...values, hash: value })} />
    <SelectField label="인코딩" value={values.encoding || ''} onChange={(value) => setValues({ ...values, encoding: value })} options={encodingOptions} />
    <SelectField label="임베딩" value={values.embedding || ''} onChange={(value) => setValues({ ...values, embedding: value })} options={embeddingOptions} />
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
    <p>주소로 전달된 필터가 활성 상태입니다:</p>
    <ul className="filter-chips">
      {active.map(([key, label]) => <li key={key}><span>{label}</span><code>{params.get(key)}</code></li>)}
    </ul>
    <Link to={`/photos${withoutURLFilters.size ? `?${withoutURLFilters.toString()}` : ''}`}>URL 필터 지우기</Link>
  </div>
}

function AlbumsPage() {
  const location = useLocation()
  const state = useAdminData<Page<Album>>(apiPath('albums', location.search))
  const albums = state.data && 'albums' in state.data ? state.data.albums : []
  return <>
    <PageTitle title="앨범" description="업로드 아카이브의 처리 상태와 사진 수를 확인합니다." />
    <AlbumFilters />
    {state.error && <ErrorPanel message={state.error} />}
    {state.loading && !state.data && <p role="status">불러오는 중…</p>}
    {state.data && <>
      <div className="table-wrap"><table className="data-table"><thead><tr><th>파일명</th><th>ID</th><th>상태</th><th>사진</th></tr></thead><tbody>
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
    <Field label="파일명 검색" value={q} onChange={setQ} />
    <SelectField label="앨범 상태" value={status} onChange={setStatus} options={[
      ['', '전체'], ['QUEUED', '대기'], ['PROCESSING', '처리 중'], ['SUCCEEDED', '완료'], ['FAILED', '실패']
    ]} />
  </FilterPanel>
}

function PhotoTable({ photos }: { photos: Photo[] }) {
  const location = useLocation()
  return <div className="table-wrap"><table className="data-table photo-table"><thead><tr><th>미리보기</th><th>사진</th><th>앨범</th><th>인코딩</th><th>임베딩</th><th>크기</th></tr></thead><tbody>
    {photos.map((photo) => <tr key={`${photo.albumId}:${photo.idx}`}>
      <td>{photo.missingBlob ? <span className="image-fallback">사용 불가</span> : <img className="thumbnail" src={imagePath(photo.hash)} loading="lazy" alt="" onError={(event) => showImageFallback(event.currentTarget)} />}</td>
      <td><Link to={`/photos/${encodeURIComponent(photo.albumId)}/${photo.idx}`} state={returnState(location)}>{photo.name}</Link><small className="subline">{photo.width} × {photo.height} · {photo.hash}</small></td>
      <td><Link to={`/albums/${encodeURIComponent(photo.albumId)}`} state={returnState(location)}>{photo.albumName}</Link></td>
      <td>{photo.missingBlob ? '블롭 없음' : stateLabel(photo.encoding)}<RawState raw={photo.encodingRaw} /></td>
      <td>{photo.missingBlob ? '블롭 없음' : stateLabel(photo.embedding)}<RawState raw={photo.embeddingRaw} /></td>
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
    <PageTitle title={photo?.name || '사진 상세'} description={photo ? `${photo.albumName} · ${photo.width} × ${photo.height}` : undefined} />
    {state.error && <ErrorPanel message={state.error} />}
    {state.loading && !photo && <p role="status">불러오는 중…</p>}
    {photo && <div className="detail-grid">
      <section className="panel image-panel">
        {photo.missingBlob ? <div className="large-fallback">블롭을 사용할 수 없습니다</div> : <img className="detail-image" src={imagePath(photo.hash)} alt={photo.name} onError={(event) => showImageFallback(event.currentTarget)} />}
        <dl className="metadata">
          <dt>앨범</dt><dd><Link to={`/albums/${encodeURIComponent(photo.albumId)}`} state={returnState(location)}>{photo.albumName}</Link> ({photo.albumId})</dd>
          <dt>블롭</dt><dd>{photo.missingBlob ? '블롭을 사용할 수 없습니다' : <Link to={`/blobs/${encodeURIComponent(photo.hash)}`} state={returnState(location)}>{photo.hash}</Link>}</dd>
          <dt>파일명</dt><dd>{photo.name}</dd><dt>해상도</dt><dd>{photo.width} × {photo.height}</dd>
          <dt>크기</dt><dd>{photo.sizeBytes === null ? '—' : formatBytes(photo.sizeBytes)}</dd><dt>콘텐츠 형식</dt><dd>{photo.contentType ?? '—'}</dd>
          <dt>블롭 존재</dt><dd>{photo.missingBlob ? '아니요' : '예'}</dd>
        </dl>
      </section>
      <PipelineDetails encoding={{ raw: photo.encodingRaw, state: photo.encoding, error: photo.encodingError, skippedReason: photo.skippedReason, observation: photo.encodingObservation }} embedding={{ raw: photo.embeddingRaw, state: photo.embedding, error: photo.embeddingError, observation: photo.embeddingObservation }} missing={photo.missingBlob} />
    </div>}
    {photo && !photo.missingBlob && <SyncRecoveryPanel
      title="이 블롭 복구"
      description="현재 파이프라인 상태에서 실행할 수 있는 동작만 표시합니다. 각 동작은 확인 후 즉시 실행되고 대기 상태로 돌아간 행 수를 표시하며, 리스 해제는 실제로 만료된 클레임만 옮깁니다."
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
    <PageTitle title="블롭 상세" description={hash} />
    {state.error && <ErrorPanel message={state.error} />}
    {state.loading && !blob && <p role="status">불러오는 중…</p>}
    {blob && <div className="detail-grid">
      <section className="panel">
        <h2>콘텐츠</h2>
        <dl className="metadata">
          <dt>해시</dt><dd className="hash-cell">{blob.hash}</dd><dt>형식</dt><dd>{blob.contentType}</dd><dt>크기</dt><dd>{formatBytes(blob.sizeBytes)}</dd>
          <dt>사진 참조</dt><dd><Link to={filterURL('photos', { hash: blob.hash })} state={returnState(location)}>{blob.photoCount}개 사진 보기</Link></dd>
          <dt>앨범 수</dt><dd>{blob.albumCount}</dd>
        </dl>
      </section>
      <PipelineDetails encoding={{ raw: blob.encodingRaw, state: blob.encoding, error: blob.encodingError, skippedReason: blob.skippedReason, observation: blob.encodingObservation }} embedding={{ raw: blob.embeddingRaw, state: blob.embedding, error: blob.embeddingError, observation: blob.embeddingObservation }} missing={false} />
    </div>}
    {blob && <SyncRecoveryPanel
      title="이 블롭 복구"
      description="현재 파이프라인 상태에서 실행할 수 있는 동작만 표시합니다. 각 동작은 확인 후 즉시 실행되고 대기 상태로 돌아간 행 수를 표시하며, 리스 해제는 실제로 만료된 클레임만 옮깁니다."
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
    <PageTitle title={album?.originalFilename || '앨범 상세'} description={album?.id || id} />
    {state.error && <ErrorPanel message={state.error} />}
    {state.loading && !album && <p role="status">불러오는 중…</p>}
    {album && <>
      <section className="panel">
        <div className="summary-grid compact">
          <Stat label="앨범 상태" value={albumStatus(album.status)} to={`/albums?status=${encodeURIComponent(album.status)}`} />
          <Stat label="사진 행" value={album.photoCount} to={filterURL('photos', { albumId: album.id })} />
          <Stat label="블롭 없는 사진" value={album.missingBlobPhotos} to={filterURL('photos', { albumId: album.id, missingBlob: 'true' })} />
        </div>
        <p>
          <Link to={filterURL('photos', { albumId: album.id })} state={returnState(location)}>앨범의 사진을 페이지로 보기</Link>
          {' · '}<Link to={filterURL('photos', { albumId: album.id, encoding: 'failed' })} state={returnState(location)}>인코딩 실패 사진</Link>
          {' · '}<Link to={filterURL('photos', { albumId: album.id, embedding: 'failed' })} state={returnState(location)}>임베딩 실패 사진</Link>
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
    <h2>ZIP 추출</h2>
    {!album.extractionRecorded && <p className="notice warning">이 앨범에는 추출 기록이 없습니다. 기록 기능 도입 전의 기존 앨범이며 실패 항목이 0개라는 뜻은 아닙니다.</p>}
    {run && <>
      <div className="summary-grid compact">
        <Stat label="최신 실행" value={`#${run.runId} · ${run.status}`} />
        <Stat label="등록됨" value={run.registeredCount} />
        <Stat label="건너뜀" value={run.skippedCount} />
        <Stat label="실패" value={run.failedCount} />
      </div>
      <p className="muted">
        시작 {formatTime(run.startedAt)} · 완료 {run.completedAt ? formatTime(run.completedAt) : '진행 중/미기록'}
        {' · '}기록된 항목 {run.entriesRecorded.toLocaleString()}
        {run.totalEntries === null ? ' (전체 수 미확정)' : ` / 전체 ${run.totalEntries.toLocaleString()}`}
        {run.candidateEntries === null ? '' : ` · 이미지 확장자 후보 ${run.candidateEntries.toLocaleString()}`}
        {run.entriesComplete ? ' · 항목 기록 완료' : ' · 항목 기록 불완전'}
      </p>
      {run.countsProvisional && <p className="notice warning" role="status">실행이 진행 중입니다. 등록·건너뜀·실패 수는 현재까지 기록된 값이며 계속 바뀔 수 있습니다.</p>}
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
    <h2>파이프라인 상태</h2>
    {missing ? <p className="notice warning">연결된 블롭이 없어 인코딩·임베딩 상태가 없습니다.</p> : <>
      <StateDetail title="인코딩" raw={encoding.raw} state={encoding.state} error={encoding.error} observation={encoding.observation} />
      {encoding.state === 'skipped' && <p className="muted">건너뜀 분류: 저장된 콘텐츠 형식 기준 {encoding.skippedReason || '확인 불가'} (건너뛴 이유 자체는 제공되지 않습니다).</p>}
      <StateDetail title="임베딩" raw={embedding.raw} state={embedding.state} error={embedding.error} observation={embedding.observation} />
      {embedding.state === 'queued' && <p className="muted">인코딩 상태와 관계없이 임베딩 처리 대기 중입니다.</p>}
    </>}
  </section>
}

// StateDetail shows the raw stored state alongside the normalized one so a
// future state stays inspectable. It is display-only: the raw-state browse
// filters retired with the blobs list, so there is no link to build.
function StateDetail({ title, raw, state, error, observation }: { title: string; raw: string; state: string; error: string; observation?: import('./api').StageObservation }) {
  return <div className="state-detail">
    <h3>{title}</h3><p>정규화 상태: <strong>{state || '—'}</strong></p>
    <p>원본 상태: {raw || '—'}</p>
    <ObservationDetails observation={observation} />
    <ErrorDetails error={error} />
  </div>
}

function ObservationDetails({ observation }: { observation?: import('./api').StageObservation }) {
  if (!observation) return <p className="muted">관측 기록 없음</p>
  return <dl className="metadata observation-details">
    <dt>시작</dt><dd>{observation.startedAt ? formatTime(observation.startedAt) : '—'}</dd>
    <dt>완료</dt><dd>{observation.finishedAt ? formatTime(observation.finishedAt) : '—'}</dd>
    <dt>상태 변경</dt><dd>{observation.statusChangedAt ? formatTime(observation.statusChangedAt) : '—'}</dd>
    <dt>시도 횟수</dt><dd>{observation.attempts}</dd>
    <dt>하위 단계</dt><dd>{observation.phase || '—'}</dd>
    <dt>오류 코드</dt><dd>{observation.errorCode || '—'}</dd>
    <dt>재시도 가능성</dt><dd>{observation.retryability || 'unknown'}</dd>
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
    <h2>파이프라인 기록</h2>
    {state.error && <ErrorPanel message={state.error} />}
    {state.loading && !state.data && <p role="status">기록 불러오는 중…</p>}
    {events.length > 0 && <ol className="event-list">{events.map((event) => <li key={event.id}>
      <time dateTime={event.occurredAt}>{formatTime(event.occurredAt)}</time>
      <strong>{event.stage} · {event.event}</strong>
      <span>상태 {event.status}{event.phase ? ` · 단계 ${event.phase}` : ''} · 시도 {event.attemptId}</span>
      {event.errorCode && <span>오류 코드 {event.errorCode}</span>}
    </li>)}</ol>}
    {state.data && events.length === 0 && <p className="muted">마이그레이션 이후 기록된 이벤트가 없습니다.</p>}
    {state.data?.nextCursor && <p className="muted">최근 20개 이벤트만 표시합니다.</p>}
  </section>
}

// RawState keeps the raw stored value visible next to the normalized label —
// the safety net for states this version does not recognize. It is a plain
// subline: the raw filters are gone, so there is nothing to link to.
function RawState({ raw }: { raw: string }) {
  if (!raw) return null
  return <small className="subline">원본: {raw}</small>
}

function ErrorDetails({ error }: { error: string }) {
  if (!error) return <p className="muted">현재 오류 없음</p>
  return <details className="error-details"><summary>현재 오류 전문</summary><pre>{error}</pre></details>
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
    {prior ? <Link className="button-link" to={prior} state={(location.state as { adminPreviousPrevious?: string } | null)?.adminPreviousPrevious ? { adminPrevious: (location.state as { adminPreviousPrevious: string }).adminPreviousPrevious } : null}>← 이전</Link> : <span className="muted">이전은 브라우저 기록을 사용할 수 있습니다.</span>}
    <span className="muted">커서 기반 페이지</span>
    {nextCursor ? <Link className="button-link" to={next()} state={{ adminPrevious: url, adminPreviousPrevious: prior }}>다음 →</Link> : <span className="muted">마지막 페이지</span>}
  </div>
}

function BackLink({ fallback }: { fallback: string }) {
  const location = useLocation()
  const returnTo = (location.state as { returnTo?: string } | null)?.returnTo
  return <Link className="back-link" to={returnTo || fallback} state={returnTo ? location.state : null}>← 목록으로</Link>
}

function returnState(location: ReturnType<typeof useLocation>) {
  return { ...(location.state || {}), returnTo: `${location.pathname}${location.search}` }
}

function FilterPanel({ children, onSubmit }: { children: ReactNode; onSubmit: (event: FormEvent) => void }) {
  const location = useLocation()
  return <form className="filter-panel" onSubmit={onSubmit}>
    {children}<div className="filter-actions"><button type="submit">검색</button><Link to={location.pathname} className="button-link secondary">초기화</Link></div>
  </form>
}

function Field({ label, value, onChange }: { label: string; value: string; onChange: (value: string) => void }) {
  return <label className="field">{label}<input value={value} onChange={(event) => onChange(event.target.value)} /></label>
}

function SelectField({ label, value, options, onChange }: { label: string; value: string; options: [string, string][]; onChange: (value: string) => void }) {
  return <label className="field">{label}<select value={value} onChange={(event) => onChange(event.target.value)}>{options.map(([key, text]) => <option key={key} value={key}>{text}</option>)}</select></label>
}

const encodingOptions: [string, string][] = [
  ['', '전체'], ['queued', '대기'], ['processing', '처리 중'], ['validating', '검증 중'], ['committing', '저장 중'], ['completed', '완료'], ['skipped', '건너뜀'], ['failed', '실패'], ['unknown', '알 수 없음']
]
const embeddingOptions: [string, string][] = [
  ['', '전체'], ['queued', '대기'], ['processing', '처리 중'], ['completed', '완료'], ['failed', '실패'], ['unknown', '알 수 없음']
]

function ErrorPanel({ message }: { message: string }) {
  return <div className="notice error" role="alert">요청 실패: {message}</div>
}

function EmptyRow({ columns }: { columns: number }) {
  return <tr><td className="empty-row" colSpan={columns}>조건에 맞는 항목이 없습니다.</td></tr>
}

function NotFoundPage() {
  return <section className="panel"><h1>페이지를 찾을 수 없습니다</h1><Link to="/">개요로 이동</Link></section>
}

function albumStatus(status: string): string {
  const labels: Record<string, string> = { QUEUED: '대기', PROCESSING: '처리 중', SUCCEEDED: '완료', FAILED: '실패' }
  return labels[status] || `${status} (새 상태)`
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
  fallback.textContent = '이미지 사용 불가'
  image.replaceWith(fallback)
}
