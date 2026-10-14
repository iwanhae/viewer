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
  width: 20, height: 30, sizeBytes: null, contentType: null, encodingRaw: '', encoding: '', encodingError: '',
  embeddingRaw: '', embedding: '', embeddingError: '', skippedReason: '', blockReason: ''
}

const detailBlob: Blob = {
  hash: 'history-hash', sizeBytes: 10, contentType: 'image/jpeg', encodingRaw: 'pending', encoding: 'queued',
  encodingError: '', embeddingRaw: 'pending', embedding: 'queued', embeddingError: '', skippedReason: '',
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
})
