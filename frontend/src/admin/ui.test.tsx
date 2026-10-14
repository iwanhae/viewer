// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from 'vitest'
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'
import AdminApp from './ui'
import type { Blob, Overview, Photo } from './api'

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
})
