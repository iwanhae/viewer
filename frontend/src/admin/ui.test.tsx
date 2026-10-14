// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from 'vitest'
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'
import AdminApp from './ui'
import type { AlbumDetail, Blob, ExtractionEntry, ExtractionRun, Overview, Photo } from './api'

const pipeline = { blobCount: 1, photoCount: 2, albumCount: 1 }
const overview: Overview = {
  measuredAt: '2026-10-14T12:00:00Z',
  units: { counts: 'counts', measuredAt: 'RFC3339 UTC' },
  library: { albums: 1, photos: 3, blobs: 1, unreferencedBlobs: 1, missingBlobPhotos: 1 },
  encoding: {
    categories: { queued: pipeline, processing: { blobCount: 0, photoCount: 0, albumCount: 0 }, validating: { blobCount: 0, photoCount: 0, albumCount: 0 }, committing: { blobCount: 0, photoCount: 0, albumCount: 0 }, completed: { blobCount: 0, photoCount: 0, albumCount: 0 }, skipped: { blobCount: 0, photoCount: 0, albumCount: 0 }, failed: { blobCount: 0, photoCount: 0, albumCount: 0 }, unknown: { blobCount: 0, photoCount: 0, albumCount: 0 } },
    skippedReasons: { webp: { blobCount: 0, photoCount: 0, albumCount: 0 }, nonWebp: { blobCount: 0, photoCount: 0, albumCount: 0 } },
    rawStates: { future_encode_state: pipeline }
  },
  embedding: {
    categories: { queued: pipeline, blocked: { blobCount: 0, photoCount: 0, albumCount: 0 }, processing: { blobCount: 0, photoCount: 0, albumCount: 0 }, completed: { blobCount: 0, photoCount: 0, albumCount: 0 }, failed: { blobCount: 0, photoCount: 0, albumCount: 0 }, unknown: { blobCount: 0, photoCount: 0, albumCount: 0 } },
    rawStates: { future_embed_state: pipeline }
  }
}

const missingPhoto: Photo = {
  albumId: 'album-1', albumName: 'archive.zip', idx: 0, name: 'lost.jpg', hash: 'gone', missingBlob: true,
  width: 20, height: 30, sizeBytes: null, contentType: null, encodingRaw: '', encoding: '', encodingError: '', encodingErrorCode: '',
  embeddingRaw: '', embedding: '', embeddingError: '', embeddingErrorCode: '', skippedReason: '', blockReason: ''
}

const detailBlob: Blob = {
  hash: 'history-hash', sizeBytes: 10, contentType: 'image/jpeg', encodingRaw: 'pending', encoding: 'queued',
  encodingError: '', encodingErrorCode: '', embeddingRaw: 'pending', embedding: 'queued', embeddingError: '', embeddingErrorCode: '', skippedReason: '',
  blockReason: '', photoCount: 0, albumCount: 0,
  encodingObservation: { startedAt: null, finishedAt: null, statusChangedAt: null, status: 'pending', phase: null, attempts: 0, errorCode: '', retryability: 'unknown' },
  embeddingObservation: { startedAt: '2026-10-14T12:00:00Z', finishedAt: null, statusChangedAt: '2026-10-14T12:00:00Z', status: 'processing', phase: null, attempts: 2, errorCode: 'legacy_unknown', retryability: 'unknown' }
}

const extractionRun = (runId: number, status = 'succeeded'): ExtractionRun => ({
  runId, status, startedAt: `2026-10-14T12:0${runId}:00Z`, completedAt: `2026-10-14T12:0${runId}:02Z`,
  failureReasonCode: '', errorDetail: '', entriesComplete: true, countsProvisional: status === 'running', entriesRecorded: 2, candidateEntriesRecorded: 1,
  totalEntries: 2, candidateEntries: 1, registeredCount: 1, skippedCount: 1, failedCount: 0
})

const albumDetail: AlbumDetail = {
  id: 'album-1', originalFilename: 'archive.zip', status: 'SUCCEEDED', error: '', photoCount: 1,
  encoding: { categories: {}, skippedReasons: {}, rawStates: {} },
  embedding: { categories: {}, rawStates: {} }, missingBlobPhotos: 0,
  extractionRecorded: true, latestExtraction: extractionRun(2)
}

const extractionEntry = (runId: number, ordinal: number, name: string, outcome: ExtractionEntry['outcome'], reasonCode = ''): ExtractionEntry => ({
  runId, ordinal, name, candidate: outcome !== 'skipped' || reasonCode !== 'unsupported_extension', outcome,
  reasonCode, errorDetail: reasonCode === 'invalid_image' ? 'image decode failed' : '',
  photoIndex: outcome === 'registered' ? 0 : null, hash: outcome === 'registered' ? 'sha256-hash' : ''
})

function jsonResponse(body: unknown, status = 200) {
  return { ok: status >= 200 && status < 300, status, json: async () => body } as Response
}

function mockFetch(responder: (url: string) => Response | Promise<Response>) {
  const fetchMock = vi.fn((input: RequestInfo | URL) => Promise.resolve(responder(String(input))))
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

function mockFetchWithInit(responder: (url: string, init?: RequestInit) => Response | Promise<Response>) {
  const fetchMock = vi.fn((input: RequestInfo | URL, init?: RequestInit) => Promise.resolve(responder(String(input), init)))
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

function deferred<T>() {
  let resolve!: (value: T) => void
  let reject!: (reason?: unknown) => void
  const promise = new Promise<T>((resolvePromise, rejectPromise) => {
    resolve = resolvePromise
    reject = rejectPromise
  })
  return { promise, resolve, reject }
}

function renderAt(path: string) {
  return render(<MemoryRouter initialEntries={[path]} future={{ v7_startTransition: true, v7_relativeSplatPath: true }}><AdminApp /></MemoryRouter>)
}

const recoveryPreview = (overrides: Record<string, unknown> = {}) => ({
  previewId: 'preview-1', actor: 'operator', actorNote: 'Basic-auth username label; token holder is not individually verified',
  action: 'retry_failed_embedding', createdAt: '2026-10-14T12:00:00Z', expiresAt: '2026-10-14T12:10:00Z',
  targetCount: 4, eligibleCount: 2, skippedCount: 2, skippedReasons: { not_failed: 2 },
  selectedPhotos: 1, affectedPhotos: 3, affectedAlbums: 2, sharedBlobWarning: true, ...overrides
})

const recoveryOperation = (overrides: Record<string, unknown> = {}) => ({
  id: 'operation-1', previewId: 'preview-1', actor: 'operator', actorNote: 'Basic-auth username label; token holder is not individually verified',
  action: 'retry_failed_embedding', status: 'queued', createdAt: '2026-10-14T12:00:00Z', updatedAt: '2026-10-14T12:00:00Z',
  total: 4, eligible: 2, succeeded: 0, conflicts: 0, skipped: 2, failed: 0,
  selectedPhotos: 1, affectedPhotos: 3, affectedAlbums: 2, sharedBlobWarning: true, ...overrides
})

describe('admin React UI', () => {
  afterEach(() => { cleanup(); vi.unstubAllGlobals(); vi.restoreAllMocks() })

  it('drills overview category counts into matching URL filters and preserves unknown raw states', async () => {
    const fetchMock = mockFetch((url) => url === '/admin/api/overview'
      ? jsonResponse(overview)
      : jsonResponse({ photos: [], nextCursor: null }))
    renderAt('/')

    const section = (await screen.findByRole('heading', { name: 'WebP 인코딩' })).parentElement!
    const row = await within(section).findByRole('row', { name: /대기 \(queued\)/ })
    const photoCount = within(row).getByRole('link', { name: '2' })
    expect(photoCount.getAttribute('href')).toBe('/photos?encoding=queued')
    const rawLink = screen.getAllByRole('link', { name: '2' }).find((link) => link.getAttribute('href')?.includes('encodingRaw=future_encode_state'))
    expect(rawLink?.getAttribute('href')).toContain('encodingRaw=future_encode_state')
    expect(screen.getByText(/블롭이 없는 사진 1개가 있습니다/)).toBeTruthy()
    expect(screen.getByText(/이 항목만 거르는 필터를 제공하지 않습니다/)).toBeTruthy()
    fireEvent.click(photoCount)
    await waitFor(() => expect(fetchMock).toHaveBeenCalledWith('/admin/api/photos?encoding=queued', expect.anything()))
  })

  it('keeps supported list filters in the URL and renders missing-blob nullable fields', async () => {
    const fetchMock = mockFetch((url) => url === '/admin/api/photos?hash=gone'
      ? jsonResponse({ photos: [missingPhoto], nextCursor: null })
      : jsonResponse(overview))
    renderAt('/photos?hash=gone')

    expect((await screen.findAllByText('블롭 없음')).length).toBe(2)
    expect(screen.getByText('사용 불가')).toBeTruthy()
    expect(screen.queryByRole('img')).toBeNull()
    expect(fetchMock).toHaveBeenCalledWith('/admin/api/photos?hash=gone', expect.anything())
    expect(screen.getByLabelText('해시')).toHaveProperty('value', 'gone')
  })

  it('shows request errors without dropping the current route', async () => {
    mockFetch(() => jsonResponse({ error: { message: 'catalog unavailable' } }, 500))
    renderAt('/albums?status=FAILED')
    expect((await screen.findByRole('alert')).textContent).toContain('catalog unavailable')
    expect(screen.getByRole('combobox', { name: '앨범 상태' })).toHaveProperty('value', 'FAILED')
  })

  it('renders nullable observations as unavailable and pages safe blob history', async () => {
    const fetchMock = mockFetch((url) => {
      if (url === '/admin/api/blobs/history-hash') return jsonResponse(detailBlob)
      if (url === '/admin/api/blobs/history-hash/history') return jsonResponse({
        events: [{ id: 1, stage: 'encoding', event: 'claimed', status: 'leased', phase: null, attemptId: 1, errorCode: '', retryability: 'unknown', occurredAt: '2026-10-14T12:00:00Z' }],
        nextCursor: '1'
      })
      if (url === '/admin/api/blobs/history-hash/history?limit=50&cursor=1') return jsonResponse({
        events: [{ id: 2, stage: 'encoding', event: 'commit_started', status: 'committing', phase: 'committing', attemptId: 1, errorCode: '', retryability: 'unknown', occurredAt: '2026-10-14T12:01:00Z' }],
        nextCursor: null
      })
      return jsonResponse({ events: [], nextCursor: null })
    })
    renderAt('/blobs/history-hash')

    expect(await screen.findByText(/encoding · claimed/)).toBeTruthy()
    expect(screen.getAllByText('—').length).toBeGreaterThan(0)
    expect(screen.getAllByText('unknown').length).toBeGreaterThan(0)
    expect(screen.getByText('legacy_unknown')).toBeTruthy()
    fireEvent.click(screen.getByRole('button', { name: '다음 기록 더 보기' }))
    expect(await screen.findByText(/encoding · commit_started/)).toBeTruthy()
    await waitFor(() => expect(fetchMock).toHaveBeenCalledWith('/admin/api/blobs/history-hash/history?limit=50&cursor=1', expect.anything()))
  })

  it('ignores load-more results and errors after the selected history stage changes', async () => {
    const oldStageMore = deferred<Response>()
    const encodingMore = deferred<Response>()
    const event = (id: number, stage: string, name: string) => ({
      id, stage, event: name, status: 'done', phase: null, attemptId: 1,
      errorCode: '', retryability: 'unknown', occurredAt: '2026-10-14T12:00:00Z'
    })
    const fetchMock = mockFetch((url) => {
      if (url === '/admin/api/blobs/history-hash') return jsonResponse(detailBlob)
      if (url === '/admin/api/blobs/history-hash/history') return jsonResponse({ events: [event(1, 'encoding', 'all_initial')], nextCursor: '1' })
      if (url === '/admin/api/blobs/history-hash/history?limit=50&cursor=1') return oldStageMore.promise
      if (url === '/admin/api/blobs/history-hash/history?stage=encoding') return jsonResponse({ events: [event(2, 'encoding', 'encoding_filtered')], nextCursor: '10' })
      if (url === '/admin/api/blobs/history-hash/history?stage=encoding&limit=50&cursor=10') return encodingMore.promise
      if (url === '/admin/api/blobs/history-hash/history?stage=embedding') return jsonResponse({ events: [event(3, 'embedding', 'embedding_filtered')], nextCursor: null })
      return jsonResponse({ events: [], nextCursor: null })
    })
    renderAt('/blobs/history-hash')

    expect(await screen.findByText(/encoding · all_initial/)).toBeTruthy()
    fireEvent.click(screen.getByRole('button', { name: '다음 기록 더 보기' }))
    await waitFor(() => expect(fetchMock).toHaveBeenCalledWith('/admin/api/blobs/history-hash/history?limit=50&cursor=1', expect.anything()))
    fireEvent.change(screen.getByRole('combobox', { name: '단계' }), { target: { value: 'encoding' } })
    expect(await screen.findByText(/encoding · encoding_filtered/)).toBeTruthy()

    await act(async () => {
      oldStageMore.resolve(jsonResponse({ events: [event(99, 'embedding', 'stale_all_stage_page')], nextCursor: null }))
      await Promise.resolve()
    })
    expect(screen.queryByText(/stale_all_stage_page/)).toBeNull()

    await waitFor(() => expect(screen.getByRole('button', { name: '다음 기록 더 보기' }).hasAttribute('disabled')).toBe(false))
    fireEvent.click(screen.getByRole('button', { name: '다음 기록 더 보기' }))
    await waitFor(() => expect(fetchMock).toHaveBeenCalledWith('/admin/api/blobs/history-hash/history?stage=encoding&limit=50&cursor=10', expect.anything()))
    fireEvent.change(screen.getByRole('combobox', { name: '단계' }), { target: { value: 'embedding' } })
    expect(await screen.findByText(/embedding · embedding_filtered/)).toBeTruthy()

    await act(async () => {
      encodingMore.reject(new Error('stale encoding page failed'))
      await Promise.resolve()
    })
    await waitFor(() => expect(screen.queryByRole('alert')).toBeNull())
    expect(screen.queryByText(/stale encoding page failed/)).toBeNull()
    expect(screen.queryByRole('button', { name: '다음 기록 더 보기' })).toBeNull()
  })

  it('does not show stale rows when a changed filter fails and the aborted request resolves late', async () => {
    const firstRequest = deferred<Response>()
    const secondRequest = deferred<Response>()
    const fetchMock = mockFetch((url) => url === '/admin/api/photos?hash=first'
      ? firstRequest.promise
      : secondRequest.promise)
    renderAt('/photos?hash=first')
    await waitFor(() => expect(fetchMock).toHaveBeenCalledWith('/admin/api/photos?hash=first', expect.anything()))

    fireEvent.change(await screen.findByLabelText('해시'), { target: { value: 'second' } })
    fireEvent.click(screen.getByRole('button', { name: '검색' }))
    await waitFor(() => expect(fetchMock).toHaveBeenCalledWith('/admin/api/photos?hash=second', expect.anything()))
    expect(screen.getByRole('status').textContent).toContain('불러오는 중')

    await act(async () => {
      secondRequest.reject(new Error('second filter failed'))
      await Promise.resolve()
    })
    expect((await screen.findByRole('alert')).textContent).toContain('second filter failed')

    await act(async () => {
      firstRequest.resolve(jsonResponse({ photos: [{ ...missingPhoto, name: 'stale.jpg' }], nextCursor: null }))
      await Promise.resolve()
    })
    expect(screen.queryByText('stale.jpg')).toBeNull()
    expect(screen.getByRole('alert').textContent).toContain('second filter failed')
  })

  it('shows append-only extraction runs, outcome filters, and paged per-entry reasons', async () => {
    const fetchMock = mockFetch((url) => {
      if (url === '/admin/api/albums/album-1') return jsonResponse(albumDetail)
      if (url === '/admin/api/albums/album-1/extractions?limit=20') return jsonResponse({ runs: [extractionRun(2), extractionRun(1, 'failed')], nextCursor: 'run-page' })
      if (url === '/admin/api/albums/album-1/extractions?limit=20&cursor=run-page') return jsonResponse({ runs: [extractionRun(3)], nextCursor: null })
      if (url === '/admin/api/albums/album-1/entries?limit=50') return jsonResponse({
        run: extractionRun(2), runSelection: 'latest', entries: [extractionEntry(2, 0, 'cover.png', 'registered')], nextCursor: 'entry-page'
      })
      if (url === '/admin/api/albums/album-1/entries?limit=50&runId=2') return jsonResponse({
        run: extractionRun(2), runSelection: 'explicit', entries: [extractionEntry(2, 0, 'cover.png', 'registered')], nextCursor: 'entry-page'
      })
      if (url === '/admin/api/albums/album-1/entries?limit=50&runId=2&cursor=entry-page') return jsonResponse({
        run: extractionRun(2), runSelection: 'explicit', entries: [extractionEntry(2, 1, 'notes.txt', 'skipped', 'unsupported_extension')], nextCursor: null
      })
      if (url === '/admin/api/albums/album-1/entries?limit=50&runId=2&outcome=skipped') return jsonResponse({
        run: extractionRun(2), runSelection: 'explicit', entries: [extractionEntry(2, 1, 'notes.txt', 'skipped', 'unsupported_extension')], nextCursor: null
      })
      if (url === '/admin/api/albums/album-1/entries?limit=50&runId=1&outcome=skipped') return jsonResponse({
        run: extractionRun(1, 'failed'), runSelection: 'explicit', entries: [extractionEntry(1, 3, 'duplicate.jpg', 'skipped', 'invalid_image')], nextCursor: null
      })
      return jsonResponse({ entries: [], nextCursor: null })
    })
    renderAt('/albums/album-1')

    expect(await screen.findByText('cover.png')).toBeTruthy()
    expect(screen.getByText(/최신 실행/)).toBeTruthy()
    expect(screen.getByText(/후보 수는 현재 추출기가 검사하는 이미지 확장자 항목 기준/)).toBeTruthy()
    fireEvent.click(screen.getByRole('button', { name: '다음 항목 더 보기' }))
    expect(await screen.findByText('notes.txt')).toBeTruthy()
    await waitFor(() => expect(fetchMock).toHaveBeenCalledWith('/admin/api/albums/album-1/entries?limit=50&runId=2&cursor=entry-page', expect.anything()))

    fireEvent.change(screen.getByRole('combobox', { name: '추출 결과' }), { target: { value: 'skipped' } })
    expect(await screen.findByText('unsupported_extension')).toBeTruthy()
    fireEvent.click(screen.getAllByRole('button', { name: '항목 보기' })[0])
    expect(await screen.findByText('invalid_image')).toBeTruthy()
    expect(screen.getByText('duplicate.jpg')).toBeTruthy()
    expect(await screen.findByText('image decode failed')).toBeTruthy()
    await waitFor(() => expect(fetchMock).toHaveBeenCalledWith('/admin/api/albums/album-1/entries?limit=50&runId=1&outcome=skipped', expect.anything()))

    fireEvent.click(screen.getByRole('button', { name: '이전 실행 더 보기' }))
    expect(await screen.findByText('#3')).toBeTruthy()
    await waitFor(() => expect(fetchMock).toHaveBeenCalledWith('/admin/api/albums/album-1/extractions?limit=20&cursor=run-page', expect.anything()))
  })

  it('pins the latest response run id before paging when a newer run appears during the initial request', async () => {
    const defaultLatest = deferred<Response>()
    const newerRun = extractionRun(3)
    const fetchMock = mockFetch((url) => {
      if (url === '/admin/api/albums/album-1') return jsonResponse({ ...albumDetail, latestExtraction: extractionRun(1) })
      if (url === '/admin/api/albums/album-1/extractions?limit=20') return jsonResponse({ runs: [extractionRun(1)], nextCursor: null })
      if (url === '/admin/api/albums/album-1/entries?limit=50') return defaultLatest.promise
      if (url === '/admin/api/albums/album-1/entries?limit=50&runId=3') return jsonResponse({
        run: newerRun, runSelection: 'explicit', entries: [extractionEntry(3, 0, 'latest-cover.png', 'registered')], nextCursor: 'pinned-cursor'
      })
      if (url === '/admin/api/albums/album-1/entries?limit=50&runId=3&cursor=pinned-cursor') return jsonResponse({
        run: newerRun, runSelection: 'explicit', entries: [extractionEntry(3, 9, 'latest-tail.png', 'skipped', 'invalid_image')], nextCursor: null
      })
      return jsonResponse({ entries: [], nextCursor: null })
    })
    renderAt('/albums/album-1')
    await waitFor(() => expect(fetchMock).toHaveBeenCalledWith('/admin/api/albums/album-1/entries?limit=50', expect.anything()))

    // A new run finishes while the initial latest-run request is in flight.
    await act(async () => {
      defaultLatest.resolve(jsonResponse({
        run: newerRun, runSelection: 'latest', entries: [extractionEntry(3, 0, 'latest-cover.png', 'registered')], nextCursor: 'first-cursor'
      }))
      await Promise.resolve()
    })

    expect(await screen.findByText('latest-cover.png')).toBeTruthy()
    await waitFor(() => expect(fetchMock).toHaveBeenCalledWith('/admin/api/albums/album-1/entries?limit=50&runId=3', expect.anything()))
    expect(screen.getByText('#3 · succeeded')).toBeTruthy()
    const latestRows = screen.getAllByText('최신')
    expect(latestRows.length).toBeGreaterThan(0)
    fireEvent.click(screen.getByRole('button', { name: '다음 항목 더 보기' }))
    expect(await screen.findByText('latest-tail.png')).toBeTruthy()
    await waitFor(() => expect(fetchMock).toHaveBeenCalledWith('/admin/api/albums/album-1/entries?limit=50&runId=3&cursor=pinned-cursor', expect.anything()))
    expect(fetchMock.mock.calls.some(([url]) => String(url) === '/admin/api/albums/album-1/entries?limit=50&cursor=first-cursor')).toBe(false)
  })

  it('explains that running entry outcomes are pending and refreshes the matching provisional run summary', async () => {
    const runningRun = extractionRun(3, 'running')
    let runListCalls = 0
    const fetchMock = mockFetch((url) => {
      if (url === '/admin/api/albums/album-1') return jsonResponse({ ...albumDetail, latestExtraction: extractionRun(1) })
      if (url === '/admin/api/albums/album-1/extractions?limit=20') {
        runListCalls++
        return jsonResponse({ runs: [runListCalls === 1 ? extractionRun(1) : runningRun], nextCursor: null })
      }
      if (url === '/admin/api/albums/album-1/entries?limit=50') return jsonResponse({
        error: { code: 'EXTRACTION_IN_PROGRESS', message: 'Extraction is still running; entry outcomes can be browsed after it reaches a terminal status.' }
      }, 409)
      return jsonResponse({ entries: [], nextCursor: null })
    })
    renderAt('/albums/album-1')

    expect(await screen.findByText(/항목 페이지는 아직 제공되지 않습니다/)).toBeTruthy()
    expect(await screen.findByText('#3 · running')).toBeTruthy()
    expect(screen.getByText(/현재까지 기록된 값이며 계속 바뀔 수 있습니다/)).toBeTruthy()
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(4))
    expect(screen.getAllByText('최신').length).toBeGreaterThan(0)
  })

  it('explains that legacy albums have no extraction record instead of showing zero failures', async () => {
    const legacy: AlbumDetail = { ...albumDetail, extractionRecorded: false, latestExtraction: null }
    const fetchMock = mockFetch((url) => url === '/admin/api/albums/album-1'
      ? jsonResponse(legacy)
      : jsonResponse({ runs: [], nextCursor: null }))
    renderAt('/albums/album-1')
    expect(await screen.findByText(/기록 기능 도입 전의 기존 앨범이며 실패 항목이 0개라는 뜻은 아닙니다/)).toBeTruthy()
    expect(fetchMock.mock.calls.some(([url]) => String(url).includes('/entries'))).toBe(false)
  })

  it('shows current issue groups and drills into the matching paged resource and latest extraction entries', async () => {
    const issues = {
      measuredAt: '2026-10-14T12:00:00Z', totalGroups: 2, remainingGroups: 0, truncated: false,
      countScope: 'Groups are independent.',
      groups: [
        { stage: 'encoding', errorCode: 'invalid_source', failureCount: 1, blobCount: 1, photoCount: 2, albumCount: 1 },
        { stage: 'extraction_entry', errorCode: 'invalid_image', failureCount: 2, entryCount: 2, blobCount: 0, photoCount: 0, albumCount: 2 }
      ]
    }
    const fetchMock = mockFetch((url) => {
      if (url === '/admin/api/issues') return jsonResponse(issues)
      if (url === '/admin/api/blobs?encoding=failed&encodingErrorCode=invalid_source') return jsonResponse({ blobs: [], nextCursor: null })
      if (url === '/admin/api/extraction-failures?reasonCode=invalid_image&limit=50') return jsonResponse({
        reasonCode: 'invalid_image', failures: [{ albumId: 'album-2', originalFilename: 'broken.zip', runId: 9, ordinal: 2, name: 'bad.jpg', candidate: true, reasonCode: 'invalid_image' }], nextCursor: null
      })
      return jsonResponse({ blobs: [], nextCursor: null })
    })
    renderAt('/issues')

    expect(await screen.findByText('invalid_source')).toBeTruthy()
    expect(screen.getByText(/측정/)).toBeTruthy()
    expect(screen.getByText('Groups are independent.')).toBeTruthy()
    expect(screen.getByRole('link', { name: '이슈' })).toBeTruthy()
    const issueCount = screen.getByRole('link', { name: '1' })
    expect(issueCount.getAttribute('href')).toBe('/blobs?encoding=failed&encodingErrorCode=invalid_source')
    fireEvent.click(issueCount)
    await waitFor(() => expect(fetchMock).toHaveBeenCalledWith('/admin/api/blobs?encoding=failed&encodingErrorCode=invalid_source', expect.anything()))

    // Navigate through the extraction group, which loads entries only from
    // their albums' latest runs and uses a separately bounded page API.
    cleanup()
    renderAt('/issues?entryReasonCode=invalid_image')
    expect(await screen.findByText('bad.jpg')).toBeTruthy()
    expect(screen.getByText(/최신 추출 실행 실패 항목/)).toBeTruthy()
    await waitFor(() => expect(fetchMock).toHaveBeenCalledWith('/admin/api/extraction-failures?reasonCode=invalid_image&limit=50', expect.anything()))
  })

  it('previews only explicitly selected photos from the visible page and requires confirmation before queueing', async () => {
    const first: Photo = { ...missingPhoto, albumId: 'album-1', idx: 0, hash: 'hash-a', name: 'a.jpg', missingBlob: false, sizeBytes: 5, contentType: 'image/jpeg' }
    const second: Photo = { ...first, idx: 1, hash: 'hash-b', name: 'b.jpg' }
    let previewBody: Record<string, unknown> | null = null
    const fetchMock = mockFetchWithInit((url, init) => {
      if (url === '/admin/api/photos') return jsonResponse({ photos: [first, second], nextCursor: 'next-page' })
      if (url === '/admin/api/actions/preview') {
        previewBody = JSON.parse(String(init?.body)) as Record<string, unknown>
        return jsonResponse(recoveryPreview({ targetCount: 2, eligibleCount: 1, skippedCount: 1, selectedPhotos: 2, affectedPhotos: 5, sharedBlobWarning: true }), 201)
      }
      return jsonResponse({ photos: [], nextCursor: null })
    })
    renderAt('/photos')

    fireEvent.click(await screen.findByRole('checkbox', { name: '현재 페이지 복구 가능한 사진 모두 선택' }))
    expect((screen.getByRole('checkbox', { name: 'a.jpg 복구 선택' }) as HTMLInputElement).checked).toBe(true)
    expect((screen.getByRole('checkbox', { name: 'b.jpg 복구 선택' }) as HTMLInputElement).checked).toBe(true)
    fireEvent.click(screen.getByRole('button', { name: '미리보기 생성' }))
    expect(await screen.findByText(/대상 블롭 2 · 실행 가능 1 · 건너뜀 1/)).toBeTruthy()
    expect(screen.getByText(/공유 블롭 영향: 선택한 범위 밖의 사진도 같은 콘텐츠 블롭/)).toBeTruthy()
    expect(screen.getByText('not_failed')).toBeTruthy()
    expect(screen.getByRole('button', { name: '확인 후 작업 큐에 등록' })).toHaveProperty('disabled', true)
    expect(previewBody).toEqual({
      action: 'retry_failed_embedding',
      selection: { photos: [{ albumId: 'album-1', idx: 0 }, { albumId: 'album-1', idx: 1 }] }
    })
    expect(fetchMock.mock.calls.some(([url, init]) => String(url) === '/admin/api/operations' && init?.method === 'POST')).toBe(false)
    expect(screen.getByText(/복구 가능한 사진 2\/2개 선택/)).toBeTruthy()
  })

  it('clears page selection when the photo URL filters change', async () => {
    const first: Photo = { ...missingPhoto, albumId: 'album-1', idx: 0, hash: 'hash-a', name: 'a.jpg', missingBlob: false }
    const fetchMock = mockFetch((url) => url.startsWith('/admin/api/photos')
      ? jsonResponse({ photos: [first], nextCursor: null })
      : jsonResponse({ photos: [], nextCursor: null }))
    renderAt('/photos?hash=first')
    fireEvent.click(await screen.findByRole('checkbox', { name: '현재 페이지 복구 가능한 사진 모두 선택' }))
    expect(await screen.findByRole('heading', { name: '범위 지정 복구' })).toBeTruthy()

    fireEvent.change(screen.getByLabelText('해시'), { target: { value: 'second' } })
    fireEvent.click(screen.getByRole('button', { name: '검색' }))
    await waitFor(() => expect(fetchMock).toHaveBeenCalledWith('/admin/api/photos?hash=second', expect.anything()))
    await waitFor(() => expect(screen.queryByRole('heading', { name: '범위 지정 복구' })).toBeNull())
    expect(screen.getByText(/페이지나 필터를 바꾸면 선택이 초기화됩니다/)).toBeTruthy()
  })

  it('bulk-selects only blob-backed photos and disables page selection when all rows are missing blobs', async () => {
    const first: Photo = { ...missingPhoto, missingBlob: false, hash: 'hash-a', name: 'a.jpg', idx: 0 }
    const second: Photo = { ...first, hash: 'hash-b', name: 'b.jpg', idx: 1 }
    const missing: Photo = { ...missingPhoto, idx: 2, name: 'missing.jpg' }
    let previewSelection: unknown = null
    let previewPosts = 0
    const fetchMock = mockFetchWithInit((url, init) => {
      if (url === '/admin/api/photos') return jsonResponse({ photos: [first, second, missing], nextCursor: 'next-page' })
      if (url === '/admin/api/actions/preview') {
        previewPosts++
        previewSelection = (JSON.parse(String(init?.body)) as { selection: unknown }).selection
        return jsonResponse(recoveryPreview({ targetCount: 2, eligibleCount: 2, skippedCount: 0, selectedPhotos: 2 }), 201)
      }
      return jsonResponse({ photos: [], nextCursor: null })
    })
    renderAt('/photos')

    const selectVisible = await screen.findByRole('checkbox', { name: '현재 페이지 복구 가능한 사진 모두 선택' })
    expect((screen.getByRole('checkbox', { name: 'missing.jpg 복구 선택' }) as HTMLInputElement).disabled).toBe(true)
    fireEvent.click(screen.getByRole('checkbox', { name: 'a.jpg 복구 선택' }))
    await waitFor(() => expect((selectVisible as HTMLInputElement).indeterminate).toBe(true))
    expect(screen.getByText(/복구 가능한 사진 1\/2개 선택/)).toBeTruthy()
    fireEvent.click(selectVisible)
    expect((screen.getByRole('checkbox', { name: 'a.jpg 복구 선택' }) as HTMLInputElement).checked).toBe(true)
    expect((screen.getByRole('checkbox', { name: 'b.jpg 복구 선택' }) as HTMLInputElement).checked).toBe(true)
    expect((screen.getByRole('checkbox', { name: 'missing.jpg 복구 선택' }) as HTMLInputElement).checked).toBe(false)
    expect((selectVisible as HTMLInputElement).checked).toBe(true)
    expect(screen.getByText(/복구 가능한 사진 2\/2개 선택/)).toBeTruthy()
    fireEvent.click(screen.getByRole('button', { name: '미리보기 생성' }))
    await screen.findByRole('heading', { name: '미리보기 · 변경 없음' })
    expect(previewSelection).toEqual({ photos: [{ albumId: 'album-1', idx: 0 }, { albumId: 'album-1', idx: 1 }] })

    cleanup()
    previewPosts = 0
    mockFetchWithInit((url) => {
      if (url === '/admin/api/photos') return jsonResponse({ photos: [missing], nextCursor: null })
      if (url === '/admin/api/actions/preview') previewPosts++
      return jsonResponse({ photos: [], nextCursor: null })
    })
    renderAt('/photos')
    const allMissingSelect = await screen.findByRole('checkbox', { name: '현재 페이지 복구 가능한 사진 모두 선택' })
    expect((allMissingSelect as HTMLInputElement).disabled).toBe(true)
    expect(screen.queryByRole('heading', { name: '범위 지정 복구' })).toBeNull()
    expect(previewPosts).toBe(0)
    expect(fetchMock.mock.calls.some(([url]) => String(url) === '/admin/api/photos')).toBe(true)
  })

  it('selects only visible blob hashes and previews those hashes rather than the full filter', async () => {
    const blobs = ['hash-a', 'hash-b'].map((hash) => ({ ...detailBlob, hash }))
    let selectionBody: Record<string, unknown> | null = null
    const fetchMock = mockFetchWithInit((url, init) => {
      if (url === '/admin/api/blobs?encoding=failed') return jsonResponse({ blobs, nextCursor: 'next-hash-page' })
      if (url === '/admin/api/actions/preview') {
        selectionBody = JSON.parse(String(init?.body)) as Record<string, unknown>
        return jsonResponse(recoveryPreview(), 201)
      }
      return jsonResponse({ blobs: [], nextCursor: null })
    })
    renderAt('/blobs?encoding=failed')
    fireEvent.click(await screen.findByRole('button', { name: '현재 페이지 모두 선택' }))
    expect((screen.getByRole('checkbox', { name: 'hash-a 복구 선택' }) as HTMLInputElement).checked).toBe(true)
    expect((screen.getByRole('checkbox', { name: 'hash-b 복구 선택' }) as HTMLInputElement).checked).toBe(true)
    fireEvent.click(screen.getByRole('button', { name: '미리보기 생성' }))
    await screen.findByRole('heading', { name: '미리보기 · 변경 없음' })
    expect(selectionBody).toEqual({ action: 'retry_failed_embedding', selection: { hashes: ['hash-a', 'hash-b'] } })
    expect(screen.getByText(/현재 페이지의 해시만 선택합니다/)).toBeTruthy()
    expect(fetchMock.mock.calls.some(([url, init]) => String(url) === '/admin/api/actions/preview' && String(init?.body).includes('filter'))).toBe(false)
  })

  it('uses albumIds for album pipeline scope and explicitly says archive extraction retry is unavailable', async () => {
    let previewBody: Record<string, unknown> | null = null
    const fetchMock = mockFetchWithInit((url, init) => {
      if (url === '/admin/api/albums/album-1') return jsonResponse({ ...albumDetail, extractionRecorded: false, latestExtraction: null })
      if (url === '/admin/api/albums/album-1/extractions?limit=20') return jsonResponse({ runs: [], nextCursor: null })
      if (url === '/admin/api/actions/preview') {
        previewBody = JSON.parse(String(init?.body)) as Record<string, unknown>
        return jsonResponse(recoveryPreview(), 201)
      }
      return jsonResponse({})
    })
    renderAt('/albums/album-1')
    expect(await screen.findByText(/아카이브 추출 재시도가 아닙니다/)).toBeTruthy()
    expect(screen.getByText(/아직 제공되지 않습니다/)).toBeTruthy()
    fireEvent.click(screen.getByRole('button', { name: '미리보기 생성' }))
    await screen.findByRole('heading', { name: '미리보기 · 변경 없음' })
    expect(previewBody).toEqual({ action: 'retry_failed_embedding', selection: { albumIds: ['album-1'] } })
    expect(fetchMock.mock.calls.some(([url]) => String(url) === '/admin/api/recover')).toBe(false)
  })

  it('invalidates a preview when the action changes and does not silently apply blob q/cursor filters', async () => {
    const fetchMock = mockFetchWithInit((url) => {
      if (url.startsWith('/admin/api/blobs')) return jsonResponse({ blobs: [], nextCursor: null })
      if (url === '/admin/api/actions/preview') return jsonResponse(recoveryPreview(), 201)
      return jsonResponse({})
    })
    renderAt('/blobs?encoding=failed&q=needle&cursor=abc')
    expect(await screen.findByText(/지원하지 않는 URL 매개변수: q, cursor/)).toBeTruthy()
    expect(screen.getByRole('button', { name: '현재 블롭 필터 전체 작업 미리보기' })).toHaveProperty('disabled', true)
    expect(fetchMock.mock.calls.some(([url, init]) => String(url) === '/admin/api/actions/preview' && init?.method === 'POST')).toBe(false)

    cleanup()
    renderAt('/blobs?encoding=failed')
    fireEvent.click(await screen.findByRole('button', { name: '현재 블롭 필터 전체 작업 미리보기' }))
    fireEvent.click(screen.getByRole('button', { name: '미리보기 생성' }))
    expect(await screen.findByRole('heading', { name: '미리보기 · 변경 없음' })).toBeTruthy()
    const previews = fetchMock.mock.calls.filter(([url, init]) => String(url) === '/admin/api/actions/preview' && init?.method === 'POST')
    expect(previews[previews.length - 1]?.[1]?.body).toBe(JSON.stringify({ action: 'retry_failed_embedding', selection: { filter: { encoding: 'failed' } } }))
    expect(screen.queryByRole('button', { name: '확인 후 작업 큐에 등록' })).toHaveProperty('disabled', true)
    fireEvent.change(screen.getByRole('combobox', { name: '복구 동작' }), { target: { value: 'retry_failed_encoding' } })
    await waitFor(() => expect(screen.queryByRole('heading', { name: '미리보기 · 변경 없음' })).toBeNull())
    expect(screen.queryByLabelText(/미리보기를 확인했습니다/)).toBeNull()
  })

  it('explains the 10,000-target cap and expires a preview on a 409 without queueing it', async () => {
    let operationCalls = 0
    const fetchMock = mockFetchWithInit((url, init) => {
      if (url.startsWith('/admin/api/blobs')) return jsonResponse({ blobs: [], nextCursor: null })
      if (url === '/admin/api/actions/preview') return jsonResponse({ error: { message: 'invalid recovery selection: selection exceeds 10000 distinct blobs' } }, 400)
      if (url === '/admin/api/operations') {
        operationCalls++
        return jsonResponse({ error: { message: 'preview expired' } }, 409)
      }
      return jsonResponse({})
    })
    renderAt('/blobs?encoding=failed')
    fireEvent.click(await screen.findByRole('button', { name: '현재 블롭 필터 전체 작업 미리보기' }))
    fireEvent.click(screen.getByRole('button', { name: '미리보기 생성' }))
    expect(await screen.findByText(/10,000개 고유 블롭 한도를 초과/)).toBeTruthy()
    expect(operationCalls).toBe(0)

    cleanup()
    mockFetchWithInit((url, init) => {
      if (url === '/admin/api/photos') return jsonResponse({ photos: [{ ...missingPhoto, missingBlob: false, hash: 'hash-one' }], nextCursor: null })
      if (url === '/admin/api/actions/preview') return jsonResponse(recoveryPreview(), 201)
      if (url === '/admin/api/operations') return jsonResponse({ error: { message: 'preview, target generation, expiry, or idempotency binding changed' } }, 409)
      return jsonResponse({ photos: [], nextCursor: null })
    })
    renderAt('/photos')
    fireEvent.click(await screen.findByRole('checkbox', { name: 'lost.jpg 복구 선택' }))
    fireEvent.click(screen.getByRole('button', { name: '미리보기 생성' }))
    fireEvent.click(await screen.findByLabelText(/미리보기를 확인했습니다/))
    fireEvent.click(screen.getByRole('button', { name: '확인 후 작업 큐에 등록' }))
    expect(await screen.findByText(/미리보기가 만료되었거나 이미 사용/)).toBeTruthy()
    expect(screen.queryByRole('heading', { name: '미리보기 · 변경 없음' })).toBeNull()
  })

  it('guards double submits and reuses the same idempotency key after a lost operation response', async () => {
    const firstOperationRequest = deferred<Response>()
    let operationPosts = 0
    const keys: string[] = []
    const operation = recoveryOperation()
    const fetchMock = mockFetchWithInit((url, init) => {
      if (url === '/admin/api/photos') return jsonResponse({ photos: [{ ...missingPhoto, missingBlob: false, hash: 'hash-one' }], nextCursor: null })
      if (url === '/admin/api/actions/preview') return jsonResponse(recoveryPreview(), 201)
      if (url === '/admin/api/operations' && init?.method === 'POST') {
        operationPosts++
        keys.push(new Headers(init.headers).get('Idempotency-Key') || '')
        return operationPosts === 1 ? firstOperationRequest.promise : jsonResponse(operation, 202)
      }
      if (url === '/admin/api/operations/operation-1?limit=50') return jsonResponse({ operation, results: [], nextCursor: null })
      return jsonResponse({ photos: [], nextCursor: null })
    })
    renderAt('/photos')
    fireEvent.click(await screen.findByRole('checkbox', { name: 'lost.jpg 복구 선택' }))
    fireEvent.click(screen.getByRole('button', { name: '미리보기 생성' }))
    fireEvent.click(await screen.findByLabelText(/미리보기를 확인했습니다/))
    const submit = screen.getByRole('button', { name: '확인 후 작업 큐에 등록' })
    fireEvent.click(submit)
    fireEvent.click(submit)
    await waitFor(() => expect(operationPosts).toBe(1))
    const firstInit = fetchMock.mock.calls.find(([url, init]) => String(url) === '/admin/api/operations' && init?.method === 'POST')?.[1]
    expect(firstInit?.credentials).toBe('same-origin')
    expect(new Headers(firstInit?.headers).get('X-Admin-Action')).toBe('1')
    expect(new Headers(firstInit?.headers).get('Content-Type')).toBe('application/json')

    await act(async () => {
      firstOperationRequest.reject(new Error('connection lost'))
      await Promise.resolve()
    })
    expect(await screen.findByText('connection lost')).toBeTruthy()
    fireEvent.click(screen.getByRole('button', { name: '확인 후 작업 큐에 등록' }))
    expect(await screen.findByRole('heading', { name: '복구 작업 상세' })).toBeTruthy()
    await waitFor(() => expect(operationPosts).toBe(2))
    expect(keys[0]).toBeTruthy()
    expect(keys[1]).toBe(keys[0])
    expect(fetchMock.mock.calls.some(([url, init]) => String(url) === '/admin/api/operations/operation-1?limit=50' && init?.method === 'GET')).toBe(true)
  })

  it('aborts old operation polling and ignores a late result after selecting a different operation', async () => {
    const oldOperation = deferred<Response>()
    const clearIntervalSpy = vi.spyOn(window, 'clearInterval')
    const fetchMock = mockFetch((url) => {
      if (url === '/admin/api/operations?limit=50') return jsonResponse({ operations: [
        recoveryOperation({ id: 'operation-a' }), recoveryOperation({ id: 'operation-b' })
      ], nextCursor: null })
      if (url === '/admin/api/operations/operation-a?limit=50') return oldOperation.promise
      if (url === '/admin/api/operations/operation-b?limit=50') return jsonResponse({
        operation: recoveryOperation({ id: 'operation-b', status: 'completed' }),
        results: [{ ordinal: 2, hash: 'hash-b', status: 'succeeded', reason: '', updatedAt: '2026-10-14T12:00:00Z' }], nextCursor: null
      })
      return jsonResponse({ operations: [], nextCursor: null })
    })
    const view = renderAt('/?tab=recovery')
    fireEvent.click(await screen.findByRole('link', { name: 'operation-a' }))
    expect(await screen.findByRole('heading', { name: '복구 작업 상세' })).toBeTruthy()
    fireEvent.click(screen.getByRole('link', { name: '← 복구 감사 목록' }))
    fireEvent.click(await screen.findByRole('link', { name: 'operation-b' }))
    expect(await screen.findByRole('link', { name: 'hash-b' })).toBeTruthy()
    expect(clearIntervalSpy).toHaveBeenCalled()

    await act(async () => {
      oldOperation.resolve(jsonResponse({
        operation: recoveryOperation({ id: 'operation-a', status: 'running' }),
        results: [{ ordinal: 1, hash: 'stale-hash-a', status: 'queued', updatedAt: '2026-10-14T12:00:00Z' }], nextCursor: null
      }))
      await Promise.resolve()
    })
    expect(screen.queryByRole('link', { name: 'stale-hash-a' })).toBeNull()
    expect(screen.getByRole('link', { name: 'hash-b' })).toBeTruthy()
    view.unmount()
    expect(fetchMock.mock.calls.some(([url]) => String(url) === '/admin/api/operations/operation-a?limit=50')).toBe(true)
  })

  it('pages operation results by stable cursor and refreshes changed target statuses without appending stale rows', async () => {
    let firstPageCalls = 0
    const fetchMock = mockFetch((url) => {
      if (url === '/admin/api/operations/operation-page?limit=50') {
        firstPageCalls++
        const completed = firstPageCalls > 1
        return jsonResponse({
          operation: recoveryOperation({ id: 'operation-page', status: completed ? 'completed' : 'running', succeeded: completed ? 1 : 0 }),
          results: [{ ordinal: 1, hash: 'hash-page-1', status: completed ? 'succeeded' : 'queued', updatedAt: '2026-10-14T12:00:00Z' }],
          nextCursor: completed ? null : 'NTA'
        })
      }
      if (url === '/admin/api/operations/operation-page?limit=50&cursor=NTA') return jsonResponse({
        operation: recoveryOperation({ id: 'operation-page', status: 'running' }),
        results: [{ ordinal: 51, hash: 'hash-page-2', status: 'conflict', reason: 'state_changed', updatedAt: '2026-10-14T12:00:01Z' }], nextCursor: null
      })
      return jsonResponse({ operations: [], nextCursor: null })
    })
    renderAt('/?tab=recovery&operation=operation-page')
    expect(await screen.findByRole('link', { name: 'hash-page-1' })).toBeTruthy()
    expect(screen.getByText('대기열')).toBeTruthy()
    fireEvent.click(screen.getByRole('button', { name: '다음 결과 페이지 →' }))
    expect(await screen.findByRole('link', { name: 'hash-page-2' })).toBeTruthy()
    expect(screen.getAllByText('충돌').length).toBeGreaterThan(0)
    await waitFor(() => expect(fetchMock).toHaveBeenCalledWith('/admin/api/operations/operation-page?limit=50&cursor=NTA', expect.anything()))
    fireEvent.click(screen.getByRole('button', { name: '← 이전 결과 페이지' }))
    expect(await screen.findByRole('link', { name: 'hash-page-1' })).toBeTruthy()
    expect(screen.queryByRole('link', { name: 'hash-page-2' })).toBeNull()
    expect(screen.getAllByText('적용됨').length).toBeGreaterThan(0)
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(3))
  })

  it('labels diagnostics as measurements rather than worker heartbeats or full vector sync', async () => {
    const diagnostics = {
      measuredAt: '2026-10-14T12:00:00Z', encodingEnabled: false, modelEnabled: false, vectorStoreEnabled: true,
      encodingExpiredLeases: 1, embeddingExpiredLeases: 2,
      encodingQueue: { queuedCount: 3, oldestStatusChangedAt: null, oldestAgeSeconds: null, unknownAgeCount: 3 },
      embeddingQueue: { queuedCount: 4, oldestStatusChangedAt: '2026-10-14T10:00:00Z', oldestAgeSeconds: 7200, unknownAgeCount: 1 },
      embeddingBlockedQueue: { blockedCount: 2, oldestStatusChangedAt: '2026-10-14T09:00:00Z', oldestAgeSeconds: 10800, unknownAgeCount: 1 },
      encodingCommit: { committingCount: 1, reconcilingCount: 1, oldestKnownStartedAt: '2026-10-14T11:00:00Z', unknownStartCount: 0 },
      workerActivity: { lastClaimUnixMs: 0, lastRenewUnixMs: 0, lastResultsUnixMs: 0 },
      workerActivityScope: 'Since server restart; attempt stamps are not heartbeats.',
      vectorCount: { status: 'available', measuredAt: '2026-10-14T12:00:02Z', expectedPoints: 4, actualPoints: 4, countAgreement: true }
    }
    const fetchMock = mockFetch((url) => url === '/admin/api/diagnostics' ? jsonResponse(diagnostics) : jsonResponse({ photos: [], nextCursor: null }))
    renderAt('/diagnostics')

    expect(await screen.findByText('개수 일치')).toBeTruthy()
    expect(screen.getByText(/연령 미상 3/)).toBeTruthy()
    expect(screen.getByText(/attempt stamps are not heartbeats/)).toBeTruthy()
    expect(screen.getByText(/전체 동기화나 포인트 무결성을 검증하지 않습니다/)).toBeTruthy()
    expect(screen.getByRole('link', { name: '시스템 진단' })).toBeTruthy()
    expect(screen.getByRole('link', { name: '임베딩 차단 항목 보기' }).getAttribute('href')).toBe('/blobs?embedding=blocked')
    expect(screen.getByRole('link', { name: /인코딩 만료 리스/ }).getAttribute('href')).toBe('/blobs?encodingLeaseExpired=true')
    await waitFor(() => expect(fetchMock).toHaveBeenCalledWith('/admin/api/diagnostics', expect.anything()))
  })
})
