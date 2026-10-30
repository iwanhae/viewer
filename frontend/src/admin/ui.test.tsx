// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from 'vitest'
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'
import AdminApp from './ui'
import type { AlbumDetail, Blob, ExtractionRun, Overview, Photo } from './api'

const pipeline = { blobCount: 1, photoCount: 2, albumCount: 1 }
const blobCount = { blobCount: 1 }
const noBlobs = { blobCount: 0 }
const overview: Overview = {
  measuredAt: '2026-10-14T12:00:00Z',
  library: { albums: 1, photos: 3, blobs: 1 },
  encoding: {
    categories: { queued: blobCount, processing: noBlobs, validating: noBlobs, committing: noBlobs, completed: noBlobs, skipped: noBlobs, failed: noBlobs, unknown: noBlobs }
  },
  embedding: {
    categories: { queued: blobCount, processing: noBlobs, completed: noBlobs, failed: noBlobs, unknown: noBlobs }
  },
  health: {
    encodingExpiredLeases: 1,
    embeddingExpiredLeases: 2,
    unreferencedBlobs: 3,
    missingBlobPhotos: 4,
    encodingEnabled: true,
    modelEnabled: true,
    vectorStoreEnabled: true,
    vectorCount: { status: 'available', measuredAt: '2026-10-14T12:00:02Z', expectedPoints: 5, actualPoints: 5, countAgreement: true }
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
  missingBlobPhotos: 0, extractionRecorded: true, latestExtraction: extractionRun(2)
}

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

// The overview's execution-control panel also reads the automatic-embedding
// status; the default shape every overview test serves.
const automaticStatus = { enabled: true, running: true }

describe('admin React UI', () => {
  afterEach(() => { cleanup(); vi.unstubAllGlobals(); vi.restoreAllMocks() })

  it('drills overview unique-blob counts into the photos list', async () => {
    const fetchMock = mockFetch((url) => url === '/admin/api/overview'
      ? jsonResponse(overview)
      : url === '/admin/api/automatic/embedding'
        ? jsonResponse(automaticStatus)
        : jsonResponse({ photos: [], nextCursor: null }))
    renderAt('/')

    const section = (await screen.findByRole('heading', { name: 'WebP 인코딩' })).parentElement!
    const row = await within(section).findByRole('row', { name: /대기 \(queued\)/ })
    const countLink = within(row).getByRole('link', { name: '1' })
    // The counts are unique-blob counts, but the blobs list is retired: the
    // drilldown opens photo rows filtered by the category.
    expect(countLink.getAttribute('href')).toBe('/photos?encoding=queued')
    expect(screen.getByRole('link', { name: /고유 블롭/ }).getAttribute('href')).toBe('/photos')
    expect(screen.getByText(/사진·앨범 참조 수를 상태별로 나누지 않습니다/)).toBeTruthy()
    expect(screen.getByText(/인코딩 상태와 관계없이 임베딩 작업자가 처리할 수 있는 상태/)).toBeTruthy()
    expect(within(section).getByRole('columnheader', { name: '고유 블롭' })).toBeTruthy()
    expect(screen.queryByRole('columnheader', { name: '사진 행' })).toBeNull()
    fireEvent.click(countLink)
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
    expect(screen.queryByRole('option', { name: '차단됨' })).toBeNull()
  })

  it('exposes exactly the five form fields on the photos page', async () => {
    mockFetch((url) => jsonResponse({ photos: [], nextCursor: null }))
    renderAt('/photos')

    const form = document.querySelector('.filter-panel') as HTMLElement
    expect(form).toBeTruthy()
    expect(within(form).getAllByRole('textbox')).toHaveLength(3)
    expect(within(form).getAllByRole('combobox')).toHaveLength(2)
    for (const label of ['이름 검색', '앨범 ID', '해시', '인코딩', '임베딩']) {
      expect(within(form).getByLabelText(label)).toBeTruthy()
    }
    // The retired raw-state, error-code, lease, phase, and skipped-reason
    // fields must not come back as hidden inputs.
    for (const label of ['원본 인코딩 상태', '원본 임베딩 상태', '인코딩 오류 코드', '임베딩 오류 코드', '인코딩 리스 만료', '임베딩 리스 만료', '인코딩 단계', '건너뜀 형식', '참조 여부']) {
      expect(within(form).queryByLabelText(label)).toBeNull()
    }
  })

  it('honors URL-only photo filters and explains them as chips with a clear link', async () => {
    const fetchMock = mockFetch((url) => url === '/admin/api/photos?hash=gone&missingBlob=true&encodingErrorCode=invalid_source'
      ? jsonResponse({ photos: [missingPhoto], nextCursor: null })
      : jsonResponse({ photos: [], nextCursor: null }))
    renderAt('/photos?hash=gone&missingBlob=true&encodingErrorCode=invalid_source')

    // The deep link's filters reach the API and the page explains itself.
    await waitFor(() => expect(fetchMock).toHaveBeenCalledWith('/admin/api/photos?hash=gone&missingBlob=true&encodingErrorCode=invalid_source', expect.anything()))
    const notice = (await screen.findByText('블롭 없는 사진')).closest('.notice') as HTMLElement
    expect(notice).toBeTruthy()
    expect(within(notice).getByText('true')).toBeTruthy()
    expect(within(notice).getByText('인코딩 오류 코드')).toBeTruthy()
    expect(within(notice).getByText('invalid_source')).toBeTruthy()
    // The form keeps only its five fields; the URL-only ones are not inputs.
    expect(screen.getByLabelText('해시')).toHaveProperty('value', 'gone')
    expect(screen.queryByLabelText('인코딩 오류 코드')).toBeNull()

    // Clearing drops exactly the URL-only filters and keeps the form's.
    fireEvent.click(within(notice).getByRole('link', { name: 'URL 필터 지우기' }))
    await waitFor(() => expect(fetchMock).toHaveBeenCalledWith('/admin/api/photos?hash=gone', expect.anything()))
    await waitFor(() => expect(screen.queryByText('블롭 없는 사진')).toBeNull())

    // Submitting the form preserves the URL-only filters it did not show.
    cleanup()
    const secondMock = mockFetch((url) => jsonResponse({ photos: [], nextCursor: null }))
    renderAt('/photos?missingBlob=true')
    await waitFor(() => expect(secondMock).toHaveBeenCalledWith('/admin/api/photos?missingBlob=true', expect.anything()))
    fireEvent.change(screen.getByLabelText('이름 검색'), { target: { value: 'pic' } })
    fireEvent.click(screen.getByRole('button', { name: '검색' }))
    await waitFor(() => expect(secondMock).toHaveBeenCalledWith('/admin/api/photos?missingBlob=true&q=pic', expect.anything()))
  })

  it('shows request errors without dropping the current route', async () => {
    mockFetch(() => jsonResponse({ error: { message: 'catalog unavailable' } }, 500))
    renderAt('/albums?status=FAILED')
    expect((await screen.findByRole('alert')).textContent).toContain('catalog unavailable')
    expect(screen.getByRole('combobox', { name: '앨범 상태' })).toHaveProperty('value', 'FAILED')
  })

  it('renders nullable observations as unavailable and shows the bounded blob history', async () => {
    mockFetch((url) => {
      if (url === '/admin/api/blobs/history-hash') return jsonResponse(detailBlob)
      if (url === '/admin/api/blobs/history-hash/history?limit=20') return jsonResponse({
        events: [{ id: 1, stage: 'encoding', event: 'claimed', status: 'leased', phase: null, attemptId: 1, errorCode: '', retryability: 'unknown', occurredAt: '2026-10-14T12:00:00Z' }],
        nextCursor: '1'
      })
      return jsonResponse({ events: [], nextCursor: null })
    })
    renderAt('/blobs/history-hash')

    expect(await screen.findByText(/encoding · claimed/)).toBeTruthy()
    expect(screen.getAllByText('—').length).toBeGreaterThan(0)
    expect(screen.getAllByText('unknown').length).toBeGreaterThan(0)
    expect(screen.getByText('legacy_unknown')).toBeTruthy()
    expect(screen.getByText('인코딩 상태와 관계없이 임베딩 처리 대기 중입니다.')).toBeTruthy()
    // One bounded fetch of the newest 20 events: no stage filter and no
    // load-more machinery anymore.
    expect(screen.getByText(/최근 20개 이벤트만 표시합니다/)).toBeTruthy()
    expect(screen.queryByRole('combobox', { name: '단계' })).toBeNull()
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

  it('renders the slim album detail with scoped counts, drilldown links, and the latest extraction run', async () => {
    const fetchMock = mockFetch((url) => url === '/admin/api/albums/album-1'
      ? jsonResponse({ ...albumDetail, missingBlobPhotos: 2, error: 'extract failed' })
      : jsonResponse({ photos: [], nextCursor: null }))
    renderAt('/albums/album-1')

    expect(await screen.findByText('archive.zip')).toBeTruthy()
    expect(screen.getByText(/앨범의 사진을 페이지로 보기/)).toBeTruthy()
    expect(screen.getByRole('link', { name: '앨범의 사진을 페이지로 보기' }).getAttribute('href')).toBe('/photos?albumId=album-1')
    expect(screen.getByRole('link', { name: '인코딩 실패 사진' }).getAttribute('href')).toBe('/photos?albumId=album-1&encoding=failed')
    expect(screen.getByRole('link', { name: '임베딩 실패 사진' }).getAttribute('href')).toBe('/photos?albumId=album-1&embedding=failed')
    expect(screen.getByRole('link', { name: /블롭 없는 사진/ }).getAttribute('href')).toBe('/photos?albumId=album-1&missingBlob=true')
    expect(screen.getByText('extract failed')).toBeTruthy()

    // The latest run summary: identity, timing, and the three outcome counts.
    expect(screen.getByText('#2 · succeeded')).toBeTruthy()
    expect(screen.getByText('등록됨')).toBeTruthy()
    expect(screen.getByText(/시작 /)).toBeTruthy()
    expect(screen.getByText(/항목 기록 완료/)).toBeTruthy()
    expect(screen.queryByRole('button', { name: '이전 실행 더 보기' })).toBeNull()
    expect(screen.queryByRole('combobox', { name: '추출 결과' })).toBeNull()
    // No pipeline tables: the photos deep links replaced them.
    expect(screen.queryByRole('heading', { name: '앨범 인코딩 상태' })).toBeNull()
    expect(screen.queryByRole('heading', { name: '앨범 임베딩 상태' })).toBeNull()
    expect(screen.queryByRole('heading', { name: '건너뜀 형식' })).toBeNull()
    expect(fetchMock.mock.calls.filter(([url]) => String(url).includes('/extractions') || String(url).includes('/entries')).length).toBe(0)
  })

  it('renders the failed latest run reason and provisional counts while it is running', async () => {
    const failedRun = { ...extractionRun(4, 'failed'), failureReasonCode: 'entry_failed', errorDetail: 'safe failure detail' }
    mockFetch((url) => url === '/admin/api/albums/album-1'
      ? jsonResponse({ ...albumDetail, latestExtraction: failedRun })
      : jsonResponse({}))
    renderAt('/albums/album-1')
    expect(await screen.findByText('#4 · failed')).toBeTruthy()
    expect(screen.getByText(/entry_failed · safe failure detail/)).toBeTruthy()

    cleanup()
    const runningRun = extractionRun(5, 'running')
    mockFetch((url) => url === '/admin/api/albums/album-1'
      ? jsonResponse({ ...albumDetail, latestExtraction: runningRun })
      : jsonResponse({}))
    renderAt('/albums/album-1')
    expect(await screen.findByText('#5 · running')).toBeTruthy()
    expect(screen.getByText(/등록·건너뜀·실패 수는 현재까지 기록된 값이며 계속 바뀔 수 있습니다/)).toBeTruthy()
    expect(screen.queryByText(/항목 페이지는 실행 완료 후 확인할 수 있습니다/)).toBeNull()
  })

  it('explains that legacy albums have no extraction record instead of showing zero failures', async () => {
    const legacy: AlbumDetail = { ...albumDetail, extractionRecorded: false, latestExtraction: null }
    mockFetch((url) => url === '/admin/api/albums/album-1'
      ? jsonResponse(legacy)
      : jsonResponse({ photos: [], nextCursor: null }))
    renderAt('/albums/album-1')
    expect(await screen.findByText(/기록 기능 도입 전의 기존 앨범이며 실패 항목이 0개라는 뜻은 아닙니다/)).toBeTruthy()
    expect(screen.queryByText(/최신 실행/)).toBeNull()
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
      if (url === '/admin/api/photos?encoding=failed&encodingErrorCode=invalid_source') return jsonResponse({ photos: [], nextCursor: null })
      if (url === '/admin/api/extraction-failures?reasonCode=invalid_image&limit=50') return jsonResponse({
        reasonCode: 'invalid_image', failures: [{ albumId: 'album-2', originalFilename: 'broken.zip', runId: 9, ordinal: 2, name: 'bad.jpg', candidate: true, reasonCode: 'invalid_image' }], nextCursor: null
      })
      return jsonResponse({ photos: [], nextCursor: null })
    })
    renderAt('/issues')

    expect(await screen.findByText('invalid_source')).toBeTruthy()
    expect(screen.getByText(/측정/)).toBeTruthy()
    expect(screen.getByText('Groups are independent.')).toBeTruthy()
    const encodingRow = screen.getByRole('row', { name: /인코딩 invalid_source/ })
    expect(within(encodingRow).getByText('고유 블롭')).toBeTruthy()
    expect(screen.getByText('실패 항목')).toBeTruthy()
    expect(screen.getByRole('link', { name: '이슈' })).toBeTruthy()
    const issueCount = screen.getByRole('link', { name: '1' })
    // The blobs list is retired: pipeline issue groups drill into photo rows.
    expect(issueCount.getAttribute('href')).toBe('/photos?encoding=failed&encodingErrorCode=invalid_source')
    fireEvent.click(issueCount)
    await waitFor(() => expect(fetchMock).toHaveBeenCalledWith('/admin/api/photos?encoding=failed&encodingErrorCode=invalid_source', expect.anything()))

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
      if (url === '/admin/api/automatic/embedding') return jsonResponse(automaticStatus)
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
    await waitFor(() => expect(fetchMock.mock.calls.filter(([url]) => String(url) === '/admin/api/overview').length).toBe(2))
    expect(confirmSpy).toHaveBeenCalledTimes(1)
  })

  it('registers the full re-embed only after confirming its cost and reports the fresh counts', async () => {
    const confirmSpy = vi.spyOn(window, 'confirm').mockReturnValue(false)
    const declined = mockFetchWithInit((url) => url === '/admin/api/overview'
      ? jsonResponse(overview)
      : url === '/admin/api/automatic/embedding' ? jsonResponse(automaticStatus) : jsonResponse({}))
    renderAt('/')
    fireEvent.click(await screen.findByRole('button', { name: '전체 재임베딩' }))
    expect(confirmSpy).toHaveBeenCalledTimes(1)
    expect(confirmSpy.mock.calls[0][0]).toContain('모든 준비(ready) 블롭을 다시 임베딩')
    expect(declined.mock.calls.some(([url]) => String(url) === '/admin/api/reindex')).toBe(false)

    cleanup()
    confirmSpy.mockReturnValue(true)
    const fetchMock = mockFetchWithInit((url) => {
      if (url === '/admin/api/overview') return jsonResponse(overview)
      if (url === '/admin/api/automatic/embedding') return jsonResponse(automaticStatus)
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
    await waitFor(() => expect(fetchMock.mock.calls.filter(([url]) => String(url) === '/admin/api/overview').length).toBe(2))
  })

  it('shows global recovery errors without dropping the overview', async () => {
    vi.spyOn(window, 'confirm').mockReturnValue(true)
    mockFetchWithInit((url) => {
      if (url === '/admin/api/overview') return jsonResponse(overview)
      if (url === '/admin/api/automatic/embedding') return jsonResponse(automaticStatus)
      if (url === '/admin/api/recover') return jsonResponse({ error: { message: 'reset failed' } }, 500)
      return jsonResponse({})
    })
    renderAt('/')
    fireEvent.click(await screen.findByRole('button', { name: '인코딩 큐 리셋' }))
    expect((await screen.findByRole('alert')).textContent).toContain('reset failed')
    expect(screen.getByRole('heading', { name: '라이브러리' })).toBeTruthy()
  })

  it('shows the automatic embedding status and pauses/resumes it without confirmation', async () => {
    let status = { enabled: true, running: false }
    const fetchMock = mockFetchWithInit((url, init) => {
      if (url !== '/admin/api/automatic/embedding') return jsonResponse(overview)
      if (init?.method === 'POST') {
        const { enabled } = JSON.parse(String(init.body)) as { enabled: boolean }
        status = { enabled, running: enabled }
        return jsonResponse(status)
      }
      return jsonResponse(status)
    })
    renderAt('/')
    const panel = screen.getByRole('region', { name: '실행 제어' })
    // Enabled but not running yet: the muted hint explains the model-load lag
    // instead of reading as a broken worker.
    await within(panel).findByText('실행 중 아님')
    expect(within(panel).getByText('활성')).toBeTruthy()
    expect(within(panel).getByText(/모델 로드 후 자동으로 시작됩니다/)).toBeTruthy()
    // The toggle is reversible, so no confirm() is consulted.
    const confirmSpy = vi.spyOn(window, 'confirm').mockReturnValue(false)
    fireEvent.click(within(panel).getByRole('button', { name: '자동 임베딩 일시정지' }))
    await within(panel).findByRole('button', { name: '자동 임베딩 재개' })
    expect(within(panel).getByText('일시정지')).toBeTruthy()
    expect(confirmSpy).not.toHaveBeenCalled()
    fireEvent.click(within(panel).getByRole('button', { name: '자동 임베딩 재개' }))
    await within(panel).findByRole('button', { name: '자동 임베딩 일시정지' })
    expect(within(panel).getByText('실행 중')).toBeTruthy()
    expect(within(panel).queryByText(/모델 로드 후 자동으로 시작됩니다/)).toBeNull()
    const posts = fetchMock.mock.calls.filter(([, init]) => init?.method === 'POST')
    expect(posts.map(([url, init]) => [url, init?.body])).toEqual([
      ['/admin/api/automatic/embedding', '{"enabled":false}'],
      ['/admin/api/automatic/embedding', '{"enabled":true}']
    ])
  })

  it('keeps serving the execution control panel when the automatic status read fails', async () => {
    mockFetchWithInit((url) => {
      if (url === '/admin/api/overview') return jsonResponse(overview)
      if (url === '/admin/api/automatic/embedding') return jsonResponse({ error: { message: 'automatic unavailable' } }, 500)
      return jsonResponse({})
    })
    renderAt('/')
    const panel = screen.getByRole('region', { name: '실행 제어' })
    expect(await within(panel).findByText(/automatic unavailable/)).toBeTruthy()
    // The local encoding trigger stays usable even without the status.
    expect(within(panel).getByRole('button', { name: '로컬 인코딩 실행' })).toBeTruthy()
  })

  it('starts the local encoding drain only after confirmation and reports the outcome', async () => {
    const confirmSpy = vi.spyOn(window, 'confirm').mockReturnValue(false)
    const declined = mockFetchWithInit((url) => {
      if (url === '/admin/api/overview') return jsonResponse(overview)
      if (url === '/admin/api/automatic/embedding') return jsonResponse(automaticStatus)
      return jsonResponse({})
    })
    renderAt('/')
    fireEvent.click(await screen.findByRole('button', { name: '로컬 인코딩 실행' }))
    expect(confirmSpy).toHaveBeenCalledTimes(1)
    expect(confirmSpy.mock.calls[0][0]).toContain('대기 중인 WebP 변환 대상 전체를 인코딩')
    expect(declined.mock.calls.some(([url]) => String(url) === '/admin/api/encoding/run-local')).toBe(false)

    cleanup()
    confirmSpy.mockReturnValue(true)
    let runs = 0
    const fetchMock = mockFetchWithInit((url) => {
      if (url === '/admin/api/overview') return jsonResponse(overview)
      if (url === '/admin/api/automatic/embedding') return jsonResponse(automaticStatus)
      if (url === '/admin/api/encoding/run-local') {
        runs++
        // The second trigger arrives while the first drain is still running.
        return jsonResponse({ started: runs === 1 })
      }
      return jsonResponse({})
    })
    renderAt('/')
    fireEvent.click(await screen.findByRole('button', { name: '로컬 인코딩 실행' }))
    expect(await screen.findByText('로컬 인코딩이 백그라운드에서 시작되었습니다.')).toBeTruthy()
    const runCall = fetchMock.mock.calls.find(([url]) => String(url) === '/admin/api/encoding/run-local')
    expect(runCall?.[1]?.method).toBe('POST')
    expect(runCall?.[1]?.body).toBe('{}')

    // started=false is the already-running answer, surfaced as a result line
    // rather than an error.
    fireEvent.click(screen.getByRole('button', { name: '로컬 인코딩 실행' }))
    await screen.findByText('이미 로컬 인코딩이 실행 중입니다.')
  })

  it('shows local encoding drain errors inline and keeps the panel usable', async () => {
    vi.spyOn(window, 'confirm').mockReturnValue(true)
    mockFetchWithInit((url) => {
      if (url === '/admin/api/overview') return jsonResponse(overview)
      if (url === '/admin/api/automatic/embedding') return jsonResponse(automaticStatus)
      if (url === '/admin/api/encoding/run-local') return jsonResponse({ error: { message: 'cwebp is not on the server PATH' } }, 503)
      return jsonResponse({})
    })
    renderAt('/')
    fireEvent.click(await screen.findByRole('button', { name: '로컬 인코딩 실행' }))
    expect((await screen.findByRole('alert')).textContent).toContain('cwebp is not on the server PATH')
    expect(screen.getByRole('button', { name: '로컬 인코딩 실행' }).hasAttribute('disabled')).toBe(false)
    expect(screen.getByRole('heading', { name: '라이브러리' })).toBeTruthy()
  })

  it('does not render the retired local runs, diagnostics, or blobs-list pages or their navigation entries', async () => {
    mockFetch((url) => url === '/admin/api/overview'
      ? jsonResponse(overview)
      : url === '/admin/api/automatic/embedding'
        ? jsonResponse(automaticStatus)
        : jsonResponse({ photos: [], nextCursor: null }))
    renderAt('/')
    expect(await screen.findByRole('heading', { name: '실행 제어' })).toBeTruthy()
    expect(screen.queryByRole('link', { name: '로컬 실행' })).toBeNull()
    expect(screen.queryByRole('region', { name: '로컬 실행 상세' })).toBeNull()
    expect(screen.queryByText(/유한한 대상만 소진한 뒤 종료/)).toBeNull()
    expect(screen.queryByRole('link', { name: '시스템 진단' })).toBeNull()
    // The blobs LIST page is retired; blob detail stays reachable from photo
    // rows, so the nav link is gone but the route keeps its splat form.
    expect(screen.queryByRole('link', { name: '블롭' })).toBeNull()
    expect(screen.getByRole('link', { name: '사진' })).toBeTruthy()

    for (const target of ['/diagnostics', '/blobs']) {
      cleanup()
      renderAt(target)
      expect(await screen.findByText('페이지를 찾을 수 없습니다')).toBeTruthy()
    }
  })

  it('renders the health section with integrity links, flags, and the vector agreement', async () => {
    mockFetch((url) => url === '/admin/api/overview'
      ? jsonResponse(overview)
      : url === '/admin/api/automatic/embedding'
        ? jsonResponse(automaticStatus)
        : jsonResponse({ photos: [], nextCursor: null }))
    renderAt('/')

    expect(await screen.findByRole('region', { name: '시스템 상태' })).toBeTruthy()
    expect(screen.getByRole('link', { name: /만료된 임베딩 리스/ }).getAttribute('href')).toBe('/photos?embeddingLeaseExpired=true')
    expect(screen.getByRole('link', { name: /만료된 인코딩 리스/ }).getAttribute('href')).toBe('/photos?encodingLeaseExpired=true')
    // 참조 없는 블롭 lost its drilldown with the blobs list: a plain number.
    const unreferencedStat = screen.getByText('참조 없는 블롭').closest('.stat') as HTMLElement
    expect(unreferencedStat.tagName).toBe('DIV')
    expect(within(unreferencedStat).getByText('3')).toBeTruthy()
    // 블롭 없는 사진 gained one: the photos missingBlob filter.
    expect(screen.getByRole('link', { name: /블롭 없는 사진/ }).getAttribute('href')).toBe('/photos?missingBlob=true')
    expect(screen.getByText('일치')).toBeTruthy()
    expect(screen.getByText(/기대 5 · 실제 5/)).toBeTruthy()
    expect(screen.getByText(/개수 비교일 뿐 전체 동기화나 포인트 무결성을 검증하지 않습니다/)).toBeTruthy()
    expect(screen.getByText(/인코딩 워커 API 사용 가능 · 임베딩 모델 사용 가능 · 벡터 스토어 사용 가능/)).toBeTruthy()
    // The retired overview tables stay retired.
    expect(screen.queryByText(/원본 상태 값/)).toBeNull()
    expect(screen.queryByRole('heading', { name: /건너뜀/ })).toBeNull()
  })

  it('degrades the vector verdict without failing the overview', async () => {
    const withStatus = (vectorCount: Overview['health']['vectorCount'], vectorStoreEnabled = true): Overview => ({
      ...overview,
      health: { ...overview.health, vectorStoreEnabled, vectorCount }
    })
    const unavailable = withStatus({
      status: 'unavailable', measuredAt: null, expectedPoints: 5, actualPoints: null, countAgreement: null,
      message: 'Vector-store count is temporarily unavailable; the catalog measurements are still current.'
    })
    mockFetch((url) => url === '/admin/api/overview'
      ? jsonResponse(unavailable)
      : url === '/admin/api/automatic/embedding'
        ? jsonResponse(automaticStatus)
        : jsonResponse({ photos: [], nextCursor: null }))
    renderAt('/')

    expect(await screen.findByText(/측정 불가/)).toBeTruthy()
    expect(screen.getByText(/Vector-store count is temporarily unavailable/)).toBeTruthy()
    // The catalog measurements still render alongside the degraded verdict.
    expect(screen.getByRole('heading', { name: '라이브러리' })).toBeTruthy()

    cleanup()
    const disabled = withStatus({ status: 'disabled', measuredAt: null, expectedPoints: 5, actualPoints: null, countAgreement: null }, false)
    mockFetch((url) => url === '/admin/api/overview'
      ? jsonResponse(disabled)
      : url === '/admin/api/automatic/embedding'
        ? jsonResponse(automaticStatus)
        : jsonResponse({ photos: [], nextCursor: null }))
    renderAt('/')
    expect(await screen.findByText(/비활성 — 벡터 저장소가 설정되지 않아 개수는 측정되지 않습니다/)).toBeTruthy()
    expect(screen.getByText(/인코딩 워커 API 사용 가능 · 임베딩 모델 사용 가능 · 벡터 스토어 비활성/)).toBeTruthy()

    cleanup()
    const mismatch = withStatus({ ...overview.health.vectorCount, actualPoints: 6, countAgreement: false })
    mockFetch((url) => url === '/admin/api/overview'
      ? jsonResponse(mismatch)
      : url === '/admin/api/automatic/embedding'
        ? jsonResponse(automaticStatus)
        : jsonResponse({ photos: [], nextCursor: null }))
    renderAt('/')
    expect(await screen.findByText('불일치')).toBeTruthy()
    expect(screen.getByText(/기대 5 · 실제 6/)).toBeTruthy()
  })
})
