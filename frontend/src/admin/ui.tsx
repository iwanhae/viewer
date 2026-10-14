import { useCallback, useEffect, useLayoutEffect, useRef, useState, type FormEvent, type ReactNode } from 'react'
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
  apiPath,
  imagePath,
  type Album,
  type AlbumDetail,
  type Blob,
  type Overview,
  type Page,
  type HistoryPage,
  type Photo,
  type PipelineEvent,
  type PipelineCounts
} from './api'

type LoadState<T> = { path: string; data: T | null; error: string; loading: boolean }

function useAdminData<T>(path: string): LoadState<T> {
  const [state, setState] = useState<LoadState<T>>(() => ({ path, data: null, error: '', loading: true }))
  useEffect(() => {
    const controller = new AbortController()
    setState({ path, data: null, error: '', loading: true })
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
  }, [path])
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
          <Link to="/blobs">블롭</Link>
          <Link to="/albums">앨범</Link>
          <a href="/admin/legacy">기존 통계·복구</a>
        </nav>
      </header>
      <main className="admin-main">
        <Routes>
          <Route path="/" element={<OverviewPage />} />
          <Route path="/photos" element={<PhotosPage />} />
          <Route path="/photos/:albumId/:idx" element={<PhotoDetailPage />} />
          <Route path="/blobs" element={<BlobsPage />} />
          <Route path="/blobs/:hash" element={<BlobDetailPage />} />
          <Route path="/albums" element={<AlbumsPage />} />
          <Route path="/albums/:id" element={<AlbumDetailPage />} />
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
            <Stat label="고유 블롭" value={lastGood.library.blobs} to="/blobs" />
            <Stat label="미참조 블롭" value={lastGood.library.unreferencedBlobs} to="/blobs?unreferenced=true" />
          </div>
          {lastGood.library.missingBlobPhotos > 0 && <div className="notice warning">
            블롭이 없는 사진 {lastGood.library.missingBlobPhotos}개가 있습니다. 현재 API는 이 항목만 거르는 필터를 제공하지 않습니다.
          </div>}
          <p className="muted">파이프라인의 사진 수는 블롭이 존재하는 사진 행 기준이며, 앨범 수는 범주 간 중복될 수 있습니다.</p>
        </section>
        <CategorySection title="WebP 인코딩" categories={lastGood.encoding.categories} rawStates={lastGood.encoding.rawStates} filterKey="encoding" />
        <section className="panel">
          <h2>건너뜀: 저장된 콘텐츠 형식 기준</h2>
          <p className="muted">건너뛴 이유를 추측하지 않습니다. 저장된 콘텐츠 형식이 WebP인지 여부만 표시합니다.</p>
          <CountTable counts={lastGood.encoding.skippedReasons} filterKey="skippedReason" />
        </section>
        <CategorySection title="임베딩" categories={lastGood.embedding.categories} rawStates={lastGood.embedding.rawStates} filterKey="embedding" />
      </>}
    </>
  )
}

function CategorySection({ title, categories, rawStates, filterKey, albumId }: {
  title: string
  categories: Record<string, PipelineCounts>
  rawStates: Record<string, PipelineCounts>
  filterKey: 'encoding' | 'embedding'
  albumId?: string
}) {
  const order = filterKey === 'encoding'
    ? ['queued', 'processing', 'validating', 'committing', 'completed', 'skipped', 'failed', 'unknown']
    : ['queued', 'blocked', 'processing', 'completed', 'failed', 'unknown']
  const states = [...new Set([...order, ...Object.keys(categories)])]
  return <section className="panel">
    <h2>{title}</h2>
    {filterKey === 'embedding' && <p className="muted">대기(queued)는 처리 가능 상태입니다. 차단(blocked)은 인코딩 게이트가 끝날 때까지 임베딩 작업자가 가져가지 않는 대기 항목입니다.</p>}
    <table className="data-table"><thead><tr><th>상태</th><th>사진 행</th><th>고유 블롭</th><th>앨범 (필터 없음)</th></tr></thead>
      <tbody>{states.map((state) => <tr key={state}>
        <th scope="row"><Link to={filterURL('photos', { [filterKey]: state, ...(albumId ? { albumId } : {}) })}>{stateLabel(state)}</Link></th>
        <td><CountLink value={categories[state]?.photoCount || 0} to={filterURL('photos', { [filterKey]: state, ...(albumId ? { albumId } : {}) })} /></td>
        <td><CountLink value={categories[state]?.blobCount || 0} to={filterURL('blobs', { [filterKey]: state, ...(albumId ? { albumId } : {}) })} /></td>
        <td>{categories[state]?.albumCount || 0}</td>
      </tr>)}</tbody>
    </table>
    <RawStateTable states={rawStates} filterKey={filterKey} albumId={albumId} />
  </section>
}

function RawStateTable({ states, filterKey, albumId }: { states: Record<string, PipelineCounts>; filterKey: 'encoding' | 'embedding'; albumId?: string }) {
  const entries = Object.entries(states).sort(([left], [right]) => left.localeCompare(right))
  if (entries.length === 0) return null
  return <details className="raw-states">
    <summary>원본 상태 값 ({entries.length})</summary>
    <table className="data-table"><thead><tr><th>저장된 상태</th><th>사진 행</th><th>고유 블롭</th><th>앨범</th></tr></thead>
      <tbody>{entries.map(([raw, counts]) => <tr key={raw}>
        <th scope="row">{raw || '(빈 값)'}</th>
        <td>{raw ? <CountLink value={counts.photoCount} to={filterURL('photos', { [`${filterKey}Raw`]: raw, ...(albumId ? { albumId } : {}) })} /> : counts.photoCount}</td>
        <td>{raw ? <CountLink value={counts.blobCount} to={filterURL('blobs', { [`${filterKey}Raw`]: raw, ...(albumId ? { albumId } : {}) })} /> : counts.blobCount}</td>
        <td>{counts.albumCount}</td>
      </tr>)}</tbody>
    </table>
  </details>
}

function CountTable({ counts, filterKey, albumId }: { counts: Record<string, PipelineCounts>; filterKey: 'skippedReason'; albumId?: string }) {
  return <table className="data-table"><thead><tr><th>콘텐츠 형식</th><th>사진 행</th><th>고유 블롭</th><th>앨범 (필터 없음)</th></tr></thead>
    <tbody>{Object.entries(counts).map(([reason, count]) => <tr key={reason}>
      <th scope="row">{reason}</th>
      <td><CountLink value={count.photoCount} to={filterURL('photos', { encoding: 'skipped', [filterKey]: reason, ...(albumId ? { albumId } : {}) })} /></td>
      <td><CountLink value={count.blobCount} to={filterURL('blobs', { encoding: 'skipped', [filterKey]: reason, ...(albumId ? { albumId } : {}) })} /></td>
      <td>{count.albumCount}</td>
    </tr>)}</tbody>
  </table>
}

function filterURL(resource: 'photos' | 'blobs' | 'albums', values: Record<string, string>): string {
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
    completed: '완료', skipped: '건너뜀', failed: '실패', unknown: '알 수 없음', blocked: '차단됨'
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
    {state.error && <ErrorPanel message={state.error} />}
    {state.loading && !state.data && <p role="status">불러오는 중…</p>}
    {state.data && <>
      <PhotoTable photos={photos} />
      <Pagination nextCursor={nextCursor} />
    </>}
  </>
}

function PhotoFilters() {
  const [params] = useSearchParams()
  const navigate = useNavigate()
  const [values, setValues] = useState<Record<string, string>>({})
  useEffect(() => {
    const initial: Record<string, string> = {}
    for (const key of ['q', 'albumId', 'hash', 'encoding', 'embedding', 'encodingRaw', 'embeddingRaw', 'skippedReason']) initial[key] = params.get(key) || ''
    setValues(initial)
  }, [params])
  const submit = (event: FormEvent) => {
    event.preventDefault()
    const query = new URLSearchParams()
    for (const [key, value] of Object.entries(values)) if (value.trim()) query.set(key, value.trim())
    navigate(`/photos${query.size ? `?${query}` : ''}`)
  }
  return <FilterPanel onSubmit={submit}>
    <Field label="이름 검색" value={values.q || ''} onChange={(value) => setValues({ ...values, q: value })} />
    <Field label="앨범 ID" value={values.albumId || ''} onChange={(value) => setValues({ ...values, albumId: value })} />
    <Field label="해시" value={values.hash || ''} onChange={(value) => setValues({ ...values, hash: value })} />
    <SelectField label="인코딩" value={values.encoding || ''} onChange={(value) => setValues({ ...values, encoding: value })} options={encodingOptions} />
    <SelectField label="임베딩" value={values.embedding || ''} onChange={(value) => setValues({ ...values, embedding: value })} options={embeddingOptions} />
    <Field label="원본 인코딩 상태" value={values.encodingRaw || ''} onChange={(value) => setValues({ ...values, encodingRaw: value })} />
    <Field label="원본 임베딩 상태" value={values.embeddingRaw || ''} onChange={(value) => setValues({ ...values, embeddingRaw: value })} />
    <SelectField label="건너뜀 형식" value={values.skippedReason || ''} onChange={(value) => setValues({ ...values, skippedReason: value })} options={skippedOptions} />
  </FilterPanel>
}

function BlobsPage() {
  const location = useLocation()
  const path = apiPath('blobs', location.search)
  const state = useAdminData<Page<Blob>>(path)
  const blobs = state.data && 'blobs' in state.data ? state.data.blobs : []
  return <>
    <PageTitle title="블롭" description="고유 콘텐츠 해시를 기준으로 인코딩·임베딩과 참조 수를 확인합니다." />
    <BlobFilters />
    {state.error && <ErrorPanel message={state.error} />}
    {state.loading && !state.data && <p role="status">불러오는 중…</p>}
    {state.data && <>
      <div className="table-wrap"><table className="data-table"><thead><tr><th>해시</th><th>형식</th><th>크기</th><th>인코딩</th><th>임베딩</th><th>사진</th><th>앨범</th></tr></thead><tbody>
        {blobs.map((blob) => <tr key={blob.hash}>
          <td className="hash-cell"><Link to={`/blobs/${encodeURIComponent(blob.hash)}`} state={returnState(location)}>{blob.hash}</Link></td>
          <td>{blob.contentType || '—'}</td><td>{formatBytes(blob.sizeBytes)}</td>
          <td>{stateLabel(blob.encoding)} <RawState raw={blob.encodingRaw} filterKey="encodingRaw" /></td>
          <td>{stateLabel(blob.embedding)} <RawState raw={blob.embeddingRaw} filterKey="embeddingRaw" /></td>
          <td><CountLink value={blob.photoCount} to={filterURL('photos', { hash: blob.hash })} /></td><td>{blob.albumCount}</td>
        </tr>)}
        {blobs.length === 0 && <EmptyRow columns={7} />}
      </tbody></table></div>
      <Pagination nextCursor={state.data.nextCursor} />
    </>}
  </>
}

function BlobFilters() {
  const [params] = useSearchParams()
  const navigate = useNavigate()
  const [values, setValues] = useState<Record<string, string>>({})
  useEffect(() => {
    const initial: Record<string, string> = {}
    for (const key of ['albumId', 'hash', 'encoding', 'embedding', 'encodingRaw', 'embeddingRaw', 'skippedReason', 'unreferenced']) initial[key] = params.get(key) || ''
    setValues(initial)
  }, [params])
  const submit = (event: FormEvent) => {
    event.preventDefault()
    const query = new URLSearchParams()
    for (const [key, value] of Object.entries(values)) if (value.trim()) query.set(key, value.trim())
    navigate(`/blobs${query.size ? `?${query}` : ''}`)
  }
  return <FilterPanel onSubmit={submit}>
    <Field label="앨범 ID" value={values.albumId || ''} onChange={(value) => setValues({ ...values, albumId: value })} />
    <Field label="해시" value={values.hash || ''} onChange={(value) => setValues({ ...values, hash: value })} />
    <SelectField label="인코딩" value={values.encoding || ''} onChange={(value) => setValues({ ...values, encoding: value })} options={encodingOptions} />
    <SelectField label="임베딩" value={values.embedding || ''} onChange={(value) => setValues({ ...values, embedding: value })} options={embeddingOptions} />
    <Field label="원본 인코딩 상태" value={values.encodingRaw || ''} onChange={(value) => setValues({ ...values, encodingRaw: value })} />
    <Field label="원본 임베딩 상태" value={values.embeddingRaw || ''} onChange={(value) => setValues({ ...values, embeddingRaw: value })} />
    <SelectField label="건너뜀 형식" value={values.skippedReason || ''} onChange={(value) => setValues({ ...values, skippedReason: value })} options={skippedOptions} />
    <SelectField label="참조 여부" value={values.unreferenced || ''} onChange={(value) => setValues({ ...values, unreferenced: value })} options={[
      ['', '전체'], ['true', '미참조'], ['false', '참조됨']
    ]} />
  </FilterPanel>
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
      <td>{photo.missingBlob ? '블롭 없음' : stateLabel(photo.encoding)}<RawState raw={photo.encodingRaw} filterKey="encodingRaw" /></td>
      <td>{photo.missingBlob ? '블롭 없음' : stateLabel(photo.embedding)}{photo.blockReason === 'encoding_gate' && <small className="subline warning-text">인코딩 게이트 대기</small>}<RawState raw={photo.embeddingRaw} filterKey="embeddingRaw" /></td>
      <td>{photo.sizeBytes === null ? '—' : formatBytes(photo.sizeBytes)}</td>
    </tr>)}
    {photos.length === 0 && <EmptyRow columns={6} />}
  </tbody></table></div>
}

function PhotoDetailPage() {
  const { albumId = '', idx = '' } = useParams()
  const location = useLocation()
  const path = apiPath(`photos/${encodeURIComponent(albumId)}/${encodeURIComponent(idx)}`)
  const state = useAdminData<Photo>(path)
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
      <PipelineDetails encoding={{ raw: photo.encodingRaw, state: photo.encoding, error: photo.encodingError, skippedReason: photo.skippedReason, observation: photo.encodingObservation }} embedding={{ raw: photo.embeddingRaw, state: photo.embedding, error: photo.embeddingError, blockReason: photo.blockReason, observation: photo.embeddingObservation }} missing={photo.missingBlob} />
    </div>}
  </>
}

function BlobDetailPage() {
  const { hash = '' } = useParams()
  const location = useLocation()
  const state = useAdminData<Blob>(apiPath(`blobs/${encodeURIComponent(hash)}`))
  const blob = state.data
  return <>
    <BackLink fallback="/blobs" />
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
      <PipelineDetails encoding={{ raw: blob.encodingRaw, state: blob.encoding, error: blob.encodingError, skippedReason: blob.skippedReason, observation: blob.encodingObservation }} embedding={{ raw: blob.embeddingRaw, state: blob.embedding, error: blob.embeddingError, blockReason: blob.blockReason, observation: blob.embeddingObservation }} missing={false} />
    </div>}
    {blob && <PipelineHistory hash={blob.hash} />}
  </>
}

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
          <Stat label="블롭 없는 사진" value={album.missingBlobPhotos} />
        </div>
        <p><Link to={filterURL('photos', { albumId: album.id })} state={returnState(location)}>앨범의 사진을 페이지로 보기</Link></p>
        <ErrorDetails error={album.error} />
      </section>
      <CategorySection title="앨범 인코딩 상태" categories={album.encoding.categories} rawStates={album.encoding.rawStates} filterKey="encoding" albumId={album.id} />
      <section className="panel"><h2>건너뜀 형식</h2><CountTable counts={album.encoding.skippedReasons} filterKey="skippedReason" albumId={album.id} /></section>
      <CategorySection title="앨범 임베딩 상태" categories={album.embedding.categories} rawStates={album.embedding.rawStates} filterKey="embedding" albumId={album.id} />
    </>}
  </>
}

function PipelineDetails({ encoding, embedding, missing }: {
  encoding: { raw: string; state: string; error: string; skippedReason?: string; observation?: import('./api').StageObservation }
  embedding: { raw: string; state: string; error: string; blockReason?: string; observation?: import('./api').StageObservation }
  missing: boolean
}) {
  return <section className="panel pipeline-detail">
    <h2>파이프라인 상태</h2>
    {missing ? <p className="notice warning">연결된 블롭이 없어 인코딩·임베딩 상태가 없습니다.</p> : <>
      <StateDetail title="인코딩" raw={encoding.raw} state={encoding.state} error={encoding.error} filterKey="encodingRaw" observation={encoding.observation} />
      {encoding.state === 'skipped' && <p className="muted">건너뜀 분류: 저장된 콘텐츠 형식 기준 {encoding.skippedReason || '확인 불가'} (건너뛴 이유 자체는 제공되지 않습니다).</p>}
      <StateDetail title="임베딩" raw={embedding.raw} state={embedding.state} error={embedding.error} filterKey="embeddingRaw" observation={embedding.observation} />
      {embedding.blockReason === 'encoding_gate'
        ? <p className="notice warning">임베딩은 대기 중이지만 인코딩 게이트 때문에 작업자가 아직 가져가지 않습니다. 이는 일반 대기 상태와 구분됩니다.</p>
        : embedding.state === 'queued' && <p className="muted">대기 중이며 현재 인코딩 게이트로 차단된 상태는 아닙니다.</p>}
    </>}
  </section>
}

function StateDetail({ title, raw, state, error, filterKey, observation }: { title: string; raw: string; state: string; error: string; filterKey: 'encodingRaw' | 'embeddingRaw'; observation?: import('./api').StageObservation }) {
  const unknown = state === 'unknown' && raw !== ''
  return <div className="state-detail">
    <h3>{title}</h3><p>정규화 상태: <strong>{state || '—'}</strong></p>
    <p>원본 상태: {raw ? <>{raw}{unknown && <> · <Link to={filterURL(title === '인코딩' ? 'blobs' : 'blobs', { [filterKey]: raw })}>이 원본 상태 보기</Link></>}</> : '—'}</p>
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

function PipelineHistory({ hash }: { hash: string }) {
  const [stage, setStage] = useState('')
  const [moreEvents, setMoreEvents] = useState<PipelineEvent[]>([])
  const [moreCursor, setMoreCursor] = useState<string | null>(null)
  const [hasLoadedMore, setHasLoadedMore] = useState(false)
  const [moreLoading, setMoreLoading] = useState(false)
  const [moreError, setMoreError] = useState('')
  const requestKey = `${hash}\0${stage}`
  const requestIdentity = useRef({ key: requestKey, generation: 0 })
  useLayoutEffect(() => {
    if (requestIdentity.current.key !== requestKey) {
      requestIdentity.current = {
        key: requestKey,
        generation: requestIdentity.current.generation + 1
      }
    }
  }, [requestKey])
  const requestGeneration = requestIdentity.current.generation +
    (requestIdentity.current.key === requestKey ? 0 : 1)
  const query = new URLSearchParams()
  if (stage) query.set('stage', stage)
  const path = apiPath(`blobs/${encodeURIComponent(hash)}/history`, query.size ? `?${query}` : '')
  const state = useAdminData<HistoryPage>(path)
  useEffect(() => {
    if (requestIdentity.current.key !== requestKey || requestIdentity.current.generation !== requestGeneration) return
    setMoreEvents([])
    setMoreCursor(null)
    setHasLoadedMore(false)
    setMoreLoading(false)
    setMoreError('')
  }, [path, requestKey, requestGeneration])

  const loadMore = async () => {
    const cursor = hasLoadedMore ? moreCursor : state.data?.nextCursor
    if (!cursor || moreLoading) return
    const isCurrentRequest = () => requestIdentity.current.key === requestKey &&
      requestIdentity.current.generation === requestGeneration
    if (isCurrentRequest()) {
      setMoreLoading((current) => isCurrentRequest() ? true : current)
      setMoreError((current) => isCurrentRequest() ? '' : current)
    }
    try {
      const params = new URLSearchParams(query)
      params.set('limit', '50')
      params.set('cursor', cursor)
      const page = await adminGet<HistoryPage>(apiPath(`blobs/${encodeURIComponent(hash)}/history`, `?${params}`))
      if (!isCurrentRequest()) return
      setMoreEvents((current) => isCurrentRequest() ? [...current, ...page.events] : current)
      setMoreCursor((current) => isCurrentRequest() ? page.nextCursor : current)
      setHasLoadedMore((current) => isCurrentRequest() ? true : current)
    } catch (cause) {
      setMoreError((current) => isCurrentRequest() ? errorMessage(cause) : current)
    } finally {
      setMoreLoading((current) => isCurrentRequest() ? false : current)
    }
  }
  const events = [...(state.data?.events || []), ...moreEvents]
  const nextCursor = hasLoadedMore ? moreCursor : state.data?.nextCursor ?? null
  return <section className="panel pipeline-history">
    <h2>파이프라인 기록</h2>
    <label className="field">단계<select value={stage} onChange={(event) => setStage(event.target.value)}>
      <option value="">전체</option><option value="encoding">인코딩</option><option value="embedding">임베딩</option>
    </select></label>
    {state.error && <ErrorPanel message={state.error} />}
    {state.loading && !state.data && <p role="status">기록 불러오는 중…</p>}
    {moreError && <ErrorPanel message={moreError} />}
    {events.length > 0 && <ol className="event-list">{events.map((event) => <li key={event.id}>
      <time dateTime={event.occurredAt}>{formatTime(event.occurredAt)}</time>
      <strong>{event.stage} · {event.event}</strong>
      <span>상태 {event.status}{event.phase ? ` · 단계 ${event.phase}` : ''} · 시도 {event.attemptId}</span>
      {event.errorCode && <span>오류 코드 {event.errorCode}</span>}
    </li>)}</ol>}
    {state.data && events.length === 0 && <p className="muted">마이그레이션 이후 기록된 이벤트가 없습니다.</p>}
    {nextCursor && <button type="button" onClick={() => void loadMore()} disabled={moreLoading}>{moreLoading ? '불러오는 중…' : '다음 기록 더 보기'}</button>}
  </section>
}

function RawState({ raw, filterKey }: { raw: string; filterKey: 'encodingRaw' | 'embeddingRaw' }) {
  if (!raw) return null
  return <small className="subline">원본: {raw}{(filterKey === 'encodingRaw' ? !['pending', 'leased', 'received', 'committing', 'done', 'skipped', 'failed'].includes(raw) : !['pending', 'leased', 'ready', 'failed'].includes(raw)) && <> · <Link to={filterURL('blobs', { [filterKey]: raw })}>조회</Link></>}</small>
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
  ['', '전체'], ['queued', '대기'], ['blocked', '차단됨'], ['processing', '처리 중'], ['completed', '완료'], ['failed', '실패'], ['unknown', '알 수 없음']
]
const skippedOptions: [string, string][] = [['', '전체'], ['webp', 'WebP'], ['nonWebp', 'WebP 아님']]

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
