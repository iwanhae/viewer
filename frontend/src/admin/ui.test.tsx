// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from 'vitest'
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'
import AdminApp from './ui'
import type { AlbumDetail, Blob, ExtractionEntry, ExtractionRun, Overview, Photo } from './api'

const pipeline = { blobCount: 1, photoCount: 2, albumCount: 1 }
const blobCount = { blobCount: 1 }
const noBlobs = { blobCount: 0 }
const overview: Overview = {
  measuredAt: '2026-10-14T12:00:00Z',
  units: { counts: 'counts', measuredAt: 'RFC3339 UTC' },
  library: { albums: 1, photos: 3, blobs: 1 },
  encoding: {
    categories: { queued: blobCount, processing: noBlobs, validating: noBlobs, committing: noBlobs, completed: noBlobs, skipped: noBlobs, failed: noBlobs, unknown: noBlobs },
    skippedReasons: { webp: noBlobs, nonWebp: noBlobs },
    rawStates: { future_encode_state: blobCount }
  },
  embedding: {
    categories: { queued: blobCount, processing: noBlobs, completed: noBlobs, failed: noBlobs, unknown: noBlobs },
    rawStates: { future_embed_state: blobCount }
  }
}

const missingPhoto: Photo = {
  albumId: 'album-1', albumName: 'archive.zip', idx: 0, name: 'lost.jpg', hash: 'gone', missingBlob: true,
  width: 20, height: 30, sizeBytes: null, contentType: null, encodingRaw: '', encoding: '', encodingError: '', encodingErrorCode: '',
  embeddingRaw: '', embedding: '', embeddingError: '', embeddingErrorCode: '', skippedReason: ''
}

const detailBlob: Blob = {
  hash: 'history-hash', sizeBytes: 10, contentType: 'image/jpeg', encodingRaw: 'pending', encoding: 'queued',
  encodingError: '', encodingErrorCode: '', embeddingRaw: 'pending', embedding: 'queued', embeddingError: '', embeddingErrorCode: '', skippedReason: '',
  photoCount: 0, albumCount: 0,
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

const recoverBlob: Blob = {
  hash: 'recover-hash', sizeBytes: 10, contentType: 'image/jpeg',
  encodingRaw: 'failed', encoding: 'failed', encodingError: 'encode failed', encodingErrorCode: 'invalid_source',
  embeddingRaw: 'failed', embedding: 'failed', embeddingError: 'embed failed', embeddingErrorCode: 'legacy_unknown', skippedReason: '',
  photoCount: 1, albumCount: 1
}

const embeddingCounts = { total: 2, ready: 0, failed: 0, processing: 0, pending: 2 }

describe('admin React UI', () => {
  afterEach(() => { cleanup(); vi.unstubAllGlobals(); vi.restoreAllMocks() })

  it('drills overview unique-blob counts into blob filters and preserves unknown raw states', async () => {
    const fetchMock = mockFetch((url) => url === '/admin/api/overview'
      ? jsonResponse(overview)
      : jsonResponse({ photos: [], nextCursor: null }))
    renderAt('/')

    const section = (await screen.findByRole('heading', { name: 'WebP 인코딩' })).parentElement!
    const row = await within(section).findByRole('row', { name: /대기 \(queued\)/ })
    const countLink = within(row).getByRole('link', { name: '1' })
    expect(countLink.getAttribute('href')).toBe('/blobs?encoding=queued')
    const rawLink = screen.getAllByRole('link', { name: '1' }).find((link) => link.getAttribute('href')?.includes('encodingRaw=future_encode_state'))
    expect(rawLink?.getAttribute('href')).toContain('encodingRaw=future_encode_state')
    expect(screen.getByText(/사진·앨범 참조 수를 상태별로 나누지 않습니다/)).toBeTruthy()
    expect(screen.queryByText(/블롭이 없는 사진/)).toBeNull()
    expect(screen.getByText(/인코딩 상태와 관계없이 임베딩 작업자가 처리할 수 있는 상태/)).toBeTruthy()
    expect(screen.queryByRole('link', { name: /차단됨/ })).toBeNull()
    fireEvent.click(countLink)
    await waitFor(() => expect(fetchMock).toHaveBeenCalledWith('/admin/api/blobs?encoding=queued', expect.anything()))
    expect(fetchMock.mock.calls.some(([url]) => String(url) === '/admin/api/diagnostics')).toBe(false)
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
    expect(screen.queryByRole('option', { name: '차단됨' })).toBeNull()
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
    expect(screen.getByText('인코딩 상태와 관계없이 임베딩 처리 대기 중입니다.')).toBeTruthy()
    expect(screen.queryByText(/인코딩 게이트/)).toBeNull()
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
    await waitFor(() => expect(fetchMock).toHaveBeenCalledWith('/admin/api/albums/album-1/entries?limit=50&runId=2', expect.anything()))
    fireEvent.click(await screen.findByRole('button', { name: '다음 항목 더 보기' }))
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
        { stage: 'encoding', errorCode: 'invalid_source', failureCount: 1, countUnit: 'blobs' },
        { stage: 'extraction_entry', errorCode: 'invalid_image', failureCount: 2, countUnit: 'entries' }
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
    const encodingRow = screen.getByRole('row', { name: /인코딩 invalid_source/ })
    expect(within(encodingRow).getByText('고유 블롭')).toBeTruthy()
    expect(screen.queryByRole('columnheader', { name: '사진 행' })).toBeNull()
    expect(screen.getByText('실패 항목')).toBeTruthy()
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

  it('offers only the sync recovery actions that match the blob pipeline states', async () => {
    mockFetch((url) => url === '/admin/api/blobs/recover-hash' ? jsonResponse(recoverBlob) : jsonResponse({}))
    renderAt('/blobs/recover-hash')
    expect(await screen.findByRole('heading', { name: '이 블롭 복구' })).toBeTruthy()
    expect(screen.getByRole('button', { name: '이 블롭 임베딩 재시도' })).toBeTruthy()
    expect(screen.getByRole('button', { name: '이 블롭 인코딩 재시도' })).toBeTruthy()
    expect(screen.queryByRole('button', { name: '이 블롭 임베딩 리스 해제' })).toBeNull()
    expect(screen.queryByRole('button', { name: '이 블롭 인코딩 리셋' })).toBeNull()

    cleanup()
    const claimed: Blob = {
      ...recoverBlob, hash: 'claimed-hash', encoding: 'processing', encodingRaw: 'leased',
      embedding: 'processing', embeddingRaw: 'processing'
    }
    mockFetch((url) => url === '/admin/api/blobs/claimed-hash' ? jsonResponse(claimed) : jsonResponse({}))
    renderAt('/blobs/claimed-hash')
    expect(await screen.findByRole('button', { name: '이 블롭 임베딩 리스 해제' })).toBeTruthy()
    expect(screen.getByRole('button', { name: '이 블롭 인코딩 리셋' })).toBeTruthy()
    expect(screen.queryByRole('button', { name: '이 블롭 임베딩 재시도' })).toBeNull()
    expect(screen.queryByRole('button', { name: '이 블롭 인코딩 재시도' })).toBeNull()

    cleanup()
    const webp: Blob = { ...recoverBlob, hash: 'webp-hash', contentType: 'image/webp', encoding: 'processing', encodingRaw: 'received' }
    mockFetch((url) => url === '/admin/api/blobs/webp-hash' ? jsonResponse(webp) : jsonResponse({}))
    renderAt('/blobs/webp-hash')
    expect(await screen.findByRole('button', { name: '이 블롭 임베딩 재시도' })).toBeTruthy()
    expect(screen.getByRole('button', { name: '이 블롭 인코딩 리셋' })).toBeTruthy()
    expect(screen.queryByRole('button', { name: '이 블롭 인코딩 재시도' })).toBeNull()
    expect(screen.queryByRole('checkbox')).toBeNull()
  })

  it('scopes photo recovery to the photo blob and hides the panel when the blob is missing', async () => {
    const failedPhoto: Photo = {
      ...missingPhoto, hash: 'photo-hash', missingBlob: false, sizeBytes: 5, contentType: 'image/jpeg',
      encoding: 'failed', encodingRaw: 'failed', embedding: 'failed', embeddingRaw: 'failed'
    }
    mockFetch((url) => url === '/admin/api/photos/album-1/0' ? jsonResponse(failedPhoto) : jsonResponse({}))
    renderAt('/photos/album-1/0')
    expect(await screen.findByRole('heading', { name: '이 블롭 복구' })).toBeTruthy()
    expect(screen.getByRole('button', { name: '이 블롭 임베딩 재시도' })).toBeTruthy()
    expect(screen.getByRole('button', { name: '이 블롭 인코딩 재시도' })).toBeTruthy()

    cleanup()
    mockFetch((url) => url === '/admin/api/photos/album-1/0' ? jsonResponse(missingPhoto) : jsonResponse({}))
    renderAt('/photos/album-1/0')
    expect((await screen.findAllByText('블롭을 사용할 수 없습니다')).length).toBeGreaterThan(0)
    expect(screen.queryByRole('heading', { name: '이 블롭 복구' })).toBeNull()
  })

  it('confirms before posting a hash-scoped recovery and reports the moved rows', async () => {
    const confirmSpy = vi.spyOn(window, 'confirm').mockReturnValue(true)
    const fetchMock = mockFetchWithInit((url) => {
      if (url === '/admin/api/blobs/recover-hash') return jsonResponse(recoverBlob)
      if (url === '/admin/api/recover') return jsonResponse({ target: 'retry_failed_embeddings', retry: { reset: 1, counts: embeddingCounts } })
      return jsonResponse({})
    })
    renderAt('/blobs/recover-hash')
    fireEvent.click(await screen.findByRole('button', { name: '이 블롭 임베딩 재시도' }))
    expect(confirmSpy).toHaveBeenCalledTimes(1)
    expect(confirmSpy.mock.calls[0][0]).toContain('이 블롭의 실패한 임베딩')
    expect(await screen.findByText('1행 재시도됨')).toBeTruthy()
    const recoverCall = fetchMock.mock.calls.find(([url]) => String(url) === '/admin/api/recover')
    expect(recoverCall?.[1]?.method).toBe('POST')
    expect(recoverCall?.[1]?.body).toBe(JSON.stringify({ target: 'retry_failed_embeddings', hashes: ['recover-hash'] }))
    expect(new Headers(recoverCall?.[1]?.headers).get('X-Admin-Action')).toBe('1')
    await waitFor(() => expect(fetchMock.mock.calls.filter(([url]) => String(url) === '/admin/api/blobs/recover-hash').length).toBe(2))
  })

  it('skips the recovery POST when the operator cancels the confirmation', async () => {
    const confirmSpy = vi.spyOn(window, 'confirm').mockReturnValue(false)
    const fetchMock = mockFetchWithInit((url) => {
      if (url === '/admin/api/blobs/recover-hash') return jsonResponse(recoverBlob)
      return jsonResponse({})
    })
    renderAt('/blobs/recover-hash')
    fireEvent.click(await screen.findByRole('button', { name: '이 블롭 인코딩 재시도' }))
    expect(confirmSpy).toHaveBeenCalledTimes(1)
    expect(fetchMock.mock.calls.some(([url]) => String(url) === '/admin/api/recover')).toBe(false)
    expect(screen.queryByText(/행 재시도됨/)).toBeNull()
  })

  it('disables sibling actions while a recovery is in flight and ignores double submits', async () => {
    vi.spyOn(window, 'confirm').mockReturnValue(true)
    const inFlight = deferred<Response>()
    const fetchMock = mockFetchWithInit((url) => {
      if (url === '/admin/api/blobs/recover-hash') return jsonResponse(recoverBlob)
      if (url === '/admin/api/recover') return inFlight.promise
      return jsonResponse({})
    })
    renderAt('/blobs/recover-hash')
    const embeddingRetry = await screen.findByRole('button', { name: '이 블롭 임베딩 재시도' })
    const encodingRetry = screen.getByRole('button', { name: '이 블롭 인코딩 재시도' })
    fireEvent.click(embeddingRetry)
    expect(embeddingRetry.textContent).toBe('실행 중…')
    expect(encodingRetry.hasAttribute('disabled')).toBe(true)
    fireEvent.click(embeddingRetry)
    await act(async () => {
      inFlight.resolve(jsonResponse({ target: 'retry_failed_embeddings', retry: { reset: 1, counts: embeddingCounts } }))
      await Promise.resolve()
    })
    expect(await screen.findByText('1행 재시도됨')).toBeTruthy()
    await waitFor(() => expect(screen.getByRole('button', { name: '이 블롭 인코딩 재시도' }).hasAttribute('disabled')).toBe(false))
    expect(fetchMock.mock.calls.filter(([url]) => String(url) === '/admin/api/recover').length).toBe(1)
  })

  it('shows recovery errors inline and keeps the panel usable', async () => {
    vi.spyOn(window, 'confirm').mockReturnValue(true)
    mockFetchWithInit((url) => {
      if (url === '/admin/api/blobs/recover-hash') return jsonResponse(recoverBlob)
      if (url === '/admin/api/recover') return jsonResponse({ error: { message: 'catalog unavailable' } }, 500)
      return jsonResponse({})
    })
    renderAt('/blobs/recover-hash')
    fireEvent.click(await screen.findByRole('button', { name: '이 블롭 임베딩 재시도' }))
    expect((await screen.findByRole('alert')).textContent).toContain('catalog unavailable')
    await waitFor(() => expect(screen.getByRole('button', { name: '이 블롭 임베딩 재시도' }).hasAttribute('disabled')).toBe(false))
  })

  it('runs confirm-gated global recovery actions from the overview and refetches it', async () => {
    const confirmSpy = vi.spyOn(window, 'confirm').mockReturnValue(true)
    const fetchMock = mockFetchWithInit((url) => {
      if (url === '/admin/api/overview') return jsonResponse(overview)
      if (url === '/admin/api/recover') return jsonResponse({ target: 'release_expired_claims', release: { released: 2, counts: embeddingCounts } })
      return jsonResponse({})
    })
    renderAt('/')
    expect(await screen.findByRole('heading', { name: '복구 작업' })).toBeTruthy()
    for (const label of ['실패한 임베딩 재시도', '실패한 인코딩 재시도', '만료된 임베딩 클레임 해제', '인코딩 큐 리셋', '전체 재임베딩']) {
      expect(screen.getByRole('button', { name: label })).toBeTruthy()
    }
    expect(screen.queryByRole('button', { name: /전체 클레임 해제|모든 클레임 해제/ })).toBeNull()
    fireEvent.click(screen.getByRole('button', { name: '만료된 임베딩 클레임 해제' }))
    expect(await screen.findByText('2행 해제됨')).toBeTruthy()
    const recoverCall = fetchMock.mock.calls.find(([url]) => String(url) === '/admin/api/recover')
    expect(recoverCall?.[1]?.method).toBe('POST')
    expect(recoverCall?.[1]?.body).toBe(JSON.stringify({ target: 'release_expired_claims' }))
    expect(new Headers(recoverCall?.[1]?.headers).get('X-Admin-Action')).toBe('1')
    await waitFor(() => expect(fetchMock.mock.calls.filter(([url]) => String(url) === '/admin/api/overview').length).toBe(2))
    expect(confirmSpy).toHaveBeenCalledTimes(1)
  })

  it('registers the full re-embed only after confirming its cost and reports the fresh counts', async () => {
    const confirmSpy = vi.spyOn(window, 'confirm').mockReturnValue(false)
    const declined = mockFetchWithInit((url) => url === '/admin/api/overview' ? jsonResponse(overview) : jsonResponse({}))
    renderAt('/')
    fireEvent.click(await screen.findByRole('button', { name: '전체 재임베딩' }))
    expect(confirmSpy).toHaveBeenCalledTimes(1)
    expect(confirmSpy.mock.calls[0][0]).toContain('모든 준비(ready) 블롭을 다시 임베딩')
    expect(declined.mock.calls.some(([url]) => String(url) === '/admin/api/reindex')).toBe(false)

    cleanup()
    confirmSpy.mockReturnValue(true)
    const fetchMock = mockFetchWithInit((url) => {
      if (url === '/admin/api/overview') return jsonResponse(overview)
      if (url === '/admin/api/reindex') return jsonResponse({ total: 12, ready: 0, failed: 2, processing: 0, pending: 10 })
      return jsonResponse({})
    })
    renderAt('/')
    fireEvent.click(await screen.findByRole('button', { name: '전체 재임베딩' }))
    expect(await screen.findByText(/전체 재임베딩이 등록되었습니다/)).toBeTruthy()
    expect(screen.getByText(/대기 10/)).toBeTruthy()
    const reindexCall = fetchMock.mock.calls.find(([url]) => String(url) === '/admin/api/reindex')
    expect(reindexCall?.[1]?.method).toBe('POST')
    expect(reindexCall?.[1]?.body).toBe('{}')
    expect(new Headers(reindexCall?.[1]?.headers).get('X-Admin-Action')).toBe('1')
    await waitFor(() => expect(fetchMock.mock.calls.filter(([url]) => String(url) === '/admin/api/overview').length).toBe(2))
  })

  it('shows global recovery errors without dropping the overview', async () => {
    vi.spyOn(window, 'confirm').mockReturnValue(true)
    mockFetchWithInit((url) => {
      if (url === '/admin/api/overview') return jsonResponse(overview)
      if (url === '/admin/api/recover') return jsonResponse({ error: { message: 'reset failed' } }, 500)
      return jsonResponse({})
    })
    renderAt('/')
    fireEvent.click(await screen.findByRole('button', { name: '인코딩 큐 리셋' }))
    expect((await screen.findByRole('alert')).textContent).toContain('reset failed')
    expect(screen.getByRole('heading', { name: '라이브러리' })).toBeTruthy()
  })

  it('labels diagnostics as measurements rather than worker heartbeats or full vector sync', async () => {
    const diagnostics = {
      measuredAt: '2026-10-14T12:00:00Z', encodingEnabled: false, modelEnabled: false, vectorStoreEnabled: true,
      encodingExpiredLeases: 1, embeddingExpiredLeases: 2,
      encodingQueue: { queuedCount: 3, oldestStatusChangedAt: null, oldestAgeSeconds: null, unknownAgeCount: 3 },
      embeddingQueue: { queuedCount: 4, oldestStatusChangedAt: '2026-10-14T10:00:00Z', oldestAgeSeconds: 7200, unknownAgeCount: 1 },
      encodingCommit: { committingCount: 1, reconcilingCount: 1, oldestKnownStartedAt: '2026-10-14T11:00:00Z', unknownStartCount: 0 },
      integrity: { unreferencedBlobs: 2, missingBlobPhotos: 3 },
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
    expect(screen.getByRole('link', { name: '임베딩 대기 항목 보기' }).getAttribute('href')).toBe('/blobs?embedding=queued')
    expect(screen.queryByRole('link', { name: '임베딩 차단 항목 보기' })).toBeNull()
    expect(screen.getByRole('link', { name: /인코딩 만료 리스/ }).getAttribute('href')).toBe('/blobs?encodingLeaseExpired=true')
    expect(screen.getByRole('link', { name: /미참조 블롭/ }).getAttribute('href')).toBe('/blobs?unreferenced=true')
    expect(screen.getByText('블롭 없는 사진')).toBeTruthy()
    await waitFor(() => expect(fetchMock).toHaveBeenCalledWith('/admin/api/diagnostics', expect.anything()))
  })
})
