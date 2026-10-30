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

    const section = (await screen.findByRole('heading', { name: 'WebP encoding' })).parentElement!
    const row = await within(section).findByRole('row', { name: /Queued \(queued\)/ })
    const countLink = within(row).getByRole('link', { name: '1' })
    // The counts are unique-blob counts, but the blobs list is retired: the
    // drilldown opens photo rows filtered by the category.
    expect(countLink.getAttribute('href')).toBe('/photos?encoding=queued')
    expect(screen.getByRole('link', { name: /Unique blobs/ }).getAttribute('href')).toBe('/photos')
    expect(screen.getByText(/do not split photo or album reference counts by state/)).toBeTruthy()
    expect(screen.getByText(/a state the embedding worker can process regardless of the encoding state/)).toBeTruthy()
    expect(within(section).getByRole('columnheader', { name: 'Unique blobs' })).toBeTruthy()
    expect(screen.queryByRole('columnheader', { name: 'Photo rows' })).toBeNull()
    fireEvent.click(countLink)
    await waitFor(() => expect(fetchMock).toHaveBeenCalledWith('/admin/api/photos?encoding=queued', expect.anything()))
  })

  it('keeps supported list filters in the URL and renders missing-blob nullable fields', async () => {
    const fetchMock = mockFetch((url) => url === '/admin/api/photos?hash=gone'
      ? jsonResponse({ photos: [missingPhoto], nextCursor: null })
      : jsonResponse(overview))
    renderAt('/photos?hash=gone')

    expect((await screen.findAllByText('No blob')).length).toBe(2)
    expect(screen.getByText('Unavailable')).toBeTruthy()
    expect(screen.queryByRole('img')).toBeNull()
    expect(fetchMock).toHaveBeenCalledWith('/admin/api/photos?hash=gone', expect.anything())
    expect(screen.getByLabelText('Hash')).toHaveProperty('value', 'gone')
    expect(screen.queryByRole('option', { name: 'Blocked' })).toBeNull()
  })

  it('exposes exactly the five form fields on the photos page', async () => {
    mockFetch((url) => jsonResponse({ photos: [], nextCursor: null }))
    renderAt('/photos')

    const form = document.querySelector('.filter-panel') as HTMLElement
    expect(form).toBeTruthy()
    expect(within(form).getAllByRole('textbox')).toHaveLength(3)
    expect(within(form).getAllByRole('combobox')).toHaveLength(2)
    for (const label of ['Name search', 'Album ID', 'Hash', 'Encoding', 'Embedding']) {
      expect(within(form).getByLabelText(label)).toBeTruthy()
    }
    // The retired raw-state, error-code, lease, phase, and skipped-reason
    // fields must not come back as hidden inputs.
    for (const label of ['Raw encoding state', 'Raw embedding state', 'Encoding error code', 'Embedding error code', 'Encoding lease expired', 'Embedding lease expired', 'Encoding phase', 'Skipped format', 'Referenced']) {
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
    const notice = (await screen.findByText('Photos without blobs')).closest('.notice') as HTMLElement
    expect(notice).toBeTruthy()
    expect(within(notice).getByText('true')).toBeTruthy()
    expect(within(notice).getByText('Encoding error code')).toBeTruthy()
    expect(within(notice).getByText('invalid_source')).toBeTruthy()
    // The form keeps only its five fields; the URL-only ones are not inputs.
    expect(screen.getByLabelText('Hash')).toHaveProperty('value', 'gone')
    expect(screen.queryByLabelText('Encoding error code')).toBeNull()

    // Clearing drops exactly the URL-only filters and keeps the form's.
    fireEvent.click(within(notice).getByRole('link', { name: 'Clear URL filters' }))
    await waitFor(() => expect(fetchMock).toHaveBeenCalledWith('/admin/api/photos?hash=gone', expect.anything()))
    await waitFor(() => expect(screen.queryByText('Photos without blobs')).toBeNull())

    // Submitting the form preserves the URL-only filters it did not show.
    cleanup()
    const secondMock = mockFetch((url) => jsonResponse({ photos: [], nextCursor: null }))
    renderAt('/photos?missingBlob=true')
    await waitFor(() => expect(secondMock).toHaveBeenCalledWith('/admin/api/photos?missingBlob=true', expect.anything()))
    fireEvent.change(screen.getByLabelText('Name search'), { target: { value: 'pic' } })
    fireEvent.click(screen.getByRole('button', { name: 'Search' }))
    await waitFor(() => expect(secondMock).toHaveBeenCalledWith('/admin/api/photos?missingBlob=true&q=pic', expect.anything()))
  })

  it('shows request errors without dropping the current route', async () => {
    mockFetch(() => jsonResponse({ error: { message: 'catalog unavailable' } }, 500))
    renderAt('/albums?status=FAILED')
    expect((await screen.findByRole('alert')).textContent).toContain('catalog unavailable')
    expect(screen.getByRole('combobox', { name: 'Album status' })).toHaveProperty('value', 'FAILED')
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
    expect(screen.getByText('Waiting for embedding processing regardless of the encoding state.')).toBeTruthy()
    // One bounded fetch of the newest 20 events: no stage filter and no
    // load-more machinery anymore.
    expect(screen.getByText(/Only the latest 20 events are shown/)).toBeTruthy()
    expect(screen.queryByRole('combobox', { name: 'Stage' })).toBeNull()
    expect(screen.queryByRole('button', { name: 'More history' })).toBeNull()
  })

  it('does not show stale rows when a changed filter fails and the aborted request resolves late', async () => {
    const firstRequest = deferred<Response>()
    const secondRequest = deferred<Response>()
    const fetchMock = mockFetch((url) => url === '/admin/api/photos?hash=first'
      ? firstRequest.promise
      : secondRequest.promise)
    renderAt('/photos?hash=first')
    await waitFor(() => expect(fetchMock).toHaveBeenCalledWith('/admin/api/photos?hash=first', expect.anything()))

    fireEvent.change(await screen.findByLabelText('Hash'), { target: { value: 'second' } })
    fireEvent.click(screen.getByRole('button', { name: 'Search' }))
    await waitFor(() => expect(fetchMock).toHaveBeenCalledWith('/admin/api/photos?hash=second', expect.anything()))
    expect(screen.getByRole('status').textContent).toContain('Loading')

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
    expect(screen.getByText(/Browse this album's photos/)).toBeTruthy()
    expect(screen.getByRole('link', { name: "Browse this album's photos" }).getAttribute('href')).toBe('/photos?albumId=album-1')
    expect(screen.getByRole('link', { name: 'Encoding-failed photos' }).getAttribute('href')).toBe('/photos?albumId=album-1&encoding=failed')
    expect(screen.getByRole('link', { name: 'Embedding-failed photos' }).getAttribute('href')).toBe('/photos?albumId=album-1&embedding=failed')
    expect(screen.getByRole('link', { name: /Photos without blobs/ }).getAttribute('href')).toBe('/photos?albumId=album-1&missingBlob=true')
    expect(screen.getByText('extract failed')).toBeTruthy()

    // The latest run summary: identity, timing, and the three outcome counts.
    expect(screen.getByText('#2 · succeeded')).toBeTruthy()
    expect(screen.getByText('Registered')).toBeTruthy()
    expect(screen.getByText(/Started /)).toBeTruthy()
    expect(screen.getByText(/entry recording complete/)).toBeTruthy()
    expect(screen.queryByRole('button', { name: 'More previous runs' })).toBeNull()
    expect(screen.queryByRole('combobox', { name: 'Extraction results' })).toBeNull()
    // No pipeline tables: the photos deep links replaced them.
    expect(screen.queryByRole('heading', { name: 'Album encoding status' })).toBeNull()
    expect(screen.queryByRole('heading', { name: 'Album embedding status' })).toBeNull()
    expect(screen.queryByRole('heading', { name: 'Skipped format' })).toBeNull()
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
    expect(screen.getByText(/recorded so far and may still change/)).toBeTruthy()
    expect(screen.queryByText(/entries page is available once the run completes/)).toBeNull()
  })

  it('explains that legacy albums have no extraction record instead of showing zero failures', async () => {
    const legacy: AlbumDetail = { ...albumDetail, extractionRecorded: false, latestExtraction: null }
    mockFetch((url) => url === '/admin/api/albums/album-1'
      ? jsonResponse(legacy)
      : jsonResponse({ photos: [], nextCursor: null }))
    renderAt('/albums/album-1')
    expect(await screen.findByText(/which does not mean it has zero failed entries/)).toBeTruthy()
    expect(screen.queryByText(/Latest run/)).toBeNull()
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
    expect(screen.getByText(/Measured/)).toBeTruthy()
    expect(screen.getByText('Groups are independent.')).toBeTruthy()
    const encodingRow = screen.getByRole('row', { name: /Encoding invalid_source/ })
    expect(within(encodingRow).getByText('Unique blobs')).toBeTruthy()
    expect(screen.getByText('Failed entries')).toBeTruthy()
    expect(screen.getByRole('link', { name: 'Issues' })).toBeTruthy()
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
    expect(screen.getByText(/Latest extraction run failures/)).toBeTruthy()
    await waitFor(() => expect(fetchMock).toHaveBeenCalledWith('/admin/api/extraction-failures?reasonCode=invalid_image&limit=50', expect.anything()))
  })

  it('offers only the sync recovery actions that match the blob pipeline states', async () => {
    mockFetch((url) => url === '/admin/api/blobs/recover-hash' ? jsonResponse(recoverBlob) : jsonResponse({}))
    renderAt('/blobs/recover-hash')
    expect(await screen.findByRole('heading', { name: 'Recover this blob' })).toBeTruthy()
    expect(screen.getByRole('button', { name: "Retry this blob's embedding" })).toBeTruthy()
    expect(screen.getByRole('button', { name: "Retry this blob's encoding" })).toBeTruthy()
    expect(screen.queryByRole('button', { name: "Release this blob's embedding lease" })).toBeNull()
    expect(screen.queryByRole('button', { name: "Reset this blob's encoding" })).toBeNull()

    cleanup()
    const claimed: Blob = {
      ...recoverBlob, hash: 'claimed-hash', encoding: 'processing', encodingRaw: 'leased',
      embedding: 'processing', embeddingRaw: 'processing'
    }
    mockFetch((url) => url === '/admin/api/blobs/claimed-hash' ? jsonResponse(claimed) : jsonResponse({}))
    renderAt('/blobs/claimed-hash')
    expect(await screen.findByRole('button', { name: "Release this blob's embedding lease" })).toBeTruthy()
    expect(screen.getByRole('button', { name: "Reset this blob's encoding" })).toBeTruthy()
    expect(screen.queryByRole('button', { name: "Retry this blob's embedding" })).toBeNull()
    expect(screen.queryByRole('button', { name: "Retry this blob's encoding" })).toBeNull()

    cleanup()
    const webp: Blob = { ...recoverBlob, hash: 'webp-hash', contentType: 'image/webp', encoding: 'processing', encodingRaw: 'received' }
    mockFetch((url) => url === '/admin/api/blobs/webp-hash' ? jsonResponse(webp) : jsonResponse({}))
    renderAt('/blobs/webp-hash')
    expect(await screen.findByRole('button', { name: "Retry this blob's embedding" })).toBeTruthy()
    expect(screen.getByRole('button', { name: "Reset this blob's encoding" })).toBeTruthy()
    expect(screen.queryByRole('button', { name: "Retry this blob's encoding" })).toBeNull()
    expect(screen.queryByRole('checkbox')).toBeNull()
  })

  it('scopes photo recovery to the photo blob and hides the panel when the blob is missing', async () => {
    const failedPhoto: Photo = {
      ...missingPhoto, hash: 'photo-hash', missingBlob: false, sizeBytes: 5, contentType: 'image/jpeg',
      encoding: 'failed', encodingRaw: 'failed', embedding: 'failed', embeddingRaw: 'failed'
    }
    mockFetch((url) => url === '/admin/api/photos/album-1/0' ? jsonResponse(failedPhoto) : jsonResponse({}))
    renderAt('/photos/album-1/0')
    expect(await screen.findByRole('heading', { name: 'Recover this blob' })).toBeTruthy()
    expect(screen.getByRole('button', { name: "Retry this blob's embedding" })).toBeTruthy()
    expect(screen.getByRole('button', { name: "Retry this blob's encoding" })).toBeTruthy()

    cleanup()
    mockFetch((url) => url === '/admin/api/photos/album-1/0' ? jsonResponse(missingPhoto) : jsonResponse({}))
    renderAt('/photos/album-1/0')
    expect((await screen.findAllByText('Blob unavailable')).length).toBeGreaterThan(0)
    expect(screen.queryByRole('heading', { name: 'Recover this blob' })).toBeNull()
  })

  it('confirms before posting a hash-scoped recovery and reports the moved rows', async () => {
    const confirmSpy = vi.spyOn(window, 'confirm').mockReturnValue(true)
    const fetchMock = mockFetchWithInit((url) => {
      if (url === '/admin/api/blobs/recover-hash') return jsonResponse(recoverBlob)
      if (url === '/admin/api/recover') return jsonResponse({ target: 'retry_failed_embeddings', retry: { reset: 1, counts: embeddingCounts } })
      return jsonResponse({})
    })
    renderAt('/blobs/recover-hash')
    fireEvent.click(await screen.findByRole('button', { name: "Retry this blob's embedding" }))
    expect(confirmSpy).toHaveBeenCalledTimes(1)
    expect(confirmSpy.mock.calls[0][0]).toContain("this blob's failed embedding")
    expect(await screen.findByText('Retried 1 row(s)')).toBeTruthy()
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
    fireEvent.click(await screen.findByRole('button', { name: "Retry this blob's encoding" }))
    expect(confirmSpy).toHaveBeenCalledTimes(1)
    expect(fetchMock.mock.calls.some(([url]) => String(url) === '/admin/api/recover')).toBe(false)
    expect(screen.queryByText(/Retried \d+ row\(s\)/)).toBeNull()
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
    const embeddingRetry = await screen.findByRole('button', { name: "Retry this blob's embedding" })
    const encodingRetry = screen.getByRole('button', { name: "Retry this blob's encoding" })
    fireEvent.click(embeddingRetry)
    expect(embeddingRetry.textContent).toBe('Running…')
    expect(encodingRetry.hasAttribute('disabled')).toBe(true)
    fireEvent.click(embeddingRetry)
    await act(async () => {
      inFlight.resolve(jsonResponse({ target: 'retry_failed_embeddings', retry: { reset: 1, counts: embeddingCounts } }))
      await Promise.resolve()
    })
    expect(await screen.findByText('Retried 1 row(s)')).toBeTruthy()
    await waitFor(() => expect(screen.getByRole('button', { name: "Retry this blob's encoding" }).hasAttribute('disabled')).toBe(false))
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
    fireEvent.click(await screen.findByRole('button', { name: "Retry this blob's embedding" }))
    expect((await screen.findByRole('alert')).textContent).toContain('catalog unavailable')
    await waitFor(() => expect(screen.getByRole('button', { name: "Retry this blob's embedding" }).hasAttribute('disabled')).toBe(false))
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
    expect(await screen.findByRole('heading', { name: 'Recovery actions' })).toBeTruthy()
    for (const label of ['Retry failed embeddings', 'Retry failed encodings', 'Release expired embedding claims', 'Reset encoding queue', 'Re-embed everything']) {
      expect(screen.getByRole('button', { name: label })).toBeTruthy()
    }
    expect(screen.queryByRole('button', { name: /Release all claims|Release every claim/ })).toBeNull()
    fireEvent.click(screen.getByRole('button', { name: 'Release expired embedding claims' }))
    expect(await screen.findByText('Released 2 row(s)')).toBeTruthy()
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
    fireEvent.click(await screen.findByRole('button', { name: 'Re-embed everything' }))
    expect(confirmSpy).toHaveBeenCalledTimes(1)
    expect(confirmSpy.mock.calls[0][0]).toContain('Re-embeds every ready blob')
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
    fireEvent.click(await screen.findByRole('button', { name: 'Re-embed everything' }))
    expect(await screen.findByText(/Full re-embedding scheduled/)).toBeTruthy()
    expect(screen.getByText(/queued 10/)).toBeTruthy()
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
    fireEvent.click(await screen.findByRole('button', { name: 'Reset encoding queue' }))
    expect((await screen.findByRole('alert')).textContent).toContain('reset failed')
    expect(screen.getByRole('heading', { name: 'Library' })).toBeTruthy()
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
    const panel = screen.getByRole('region', { name: 'Run control' })
    // Enabled but not running yet: the muted hint explains the model-load lag
    // instead of reading as a broken worker.
    await within(panel).findByText('not running')
    expect(within(panel).getByText('enabled')).toBeTruthy()
    expect(within(panel).getByText(/starts automatically once the model has loaded/)).toBeTruthy()
    // The toggle is reversible, so no confirm() is consulted.
    const confirmSpy = vi.spyOn(window, 'confirm').mockReturnValue(false)
    fireEvent.click(within(panel).getByRole('button', { name: 'Pause automatic embedding' }))
    await within(panel).findByRole('button', { name: 'Resume automatic embedding' })
    expect(within(panel).getByText('paused')).toBeTruthy()
    expect(confirmSpy).not.toHaveBeenCalled()
    fireEvent.click(within(panel).getByRole('button', { name: 'Resume automatic embedding' }))
    await within(panel).findByRole('button', { name: 'Pause automatic embedding' })
    expect(within(panel).getByText('running')).toBeTruthy()
    expect(within(panel).queryByText(/starts automatically once the model has loaded/)).toBeNull()
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
    const panel = screen.getByRole('region', { name: 'Run control' })
    expect(await within(panel).findByText(/automatic unavailable/)).toBeTruthy()
    // The local encoding trigger stays usable even without the status.
    expect(within(panel).getByRole('button', { name: 'Run local encoding' })).toBeTruthy()
  })

  it('starts the local encoding drain only after confirmation and reports the outcome', async () => {
    const confirmSpy = vi.spyOn(window, 'confirm').mockReturnValue(false)
    const declined = mockFetchWithInit((url) => {
      if (url === '/admin/api/overview') return jsonResponse(overview)
      if (url === '/admin/api/automatic/embedding') return jsonResponse(automaticStatus)
      return jsonResponse({})
    })
    renderAt('/')
    fireEvent.click(await screen.findByRole('button', { name: 'Run local encoding' }))
    expect(confirmSpy).toHaveBeenCalledTimes(1)
    expect(confirmSpy.mock.calls[0][0]).toContain('Encodes every queued WebP conversion target')
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
    fireEvent.click(await screen.findByRole('button', { name: 'Run local encoding' }))
    expect(await screen.findByText('Local encoding started in the background.')).toBeTruthy()
    const runCall = fetchMock.mock.calls.find(([url]) => String(url) === '/admin/api/encoding/run-local')
    expect(runCall?.[1]?.method).toBe('POST')
    expect(runCall?.[1]?.body).toBe('{}')

    // started=false is the already-running answer, surfaced as a result line
    // rather than an error.
    fireEvent.click(screen.getByRole('button', { name: 'Run local encoding' }))
    await screen.findByText('Local encoding is already running.')
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
    fireEvent.click(await screen.findByRole('button', { name: 'Run local encoding' }))
    expect((await screen.findByRole('alert')).textContent).toContain('cwebp is not on the server PATH')
    expect(screen.getByRole('button', { name: 'Run local encoding' }).hasAttribute('disabled')).toBe(false)
    expect(screen.getByRole('heading', { name: 'Library' })).toBeTruthy()
  })

  it('does not render the retired local runs, diagnostics, or blobs-list pages or their navigation entries', async () => {
    mockFetch((url) => url === '/admin/api/overview'
      ? jsonResponse(overview)
      : url === '/admin/api/automatic/embedding'
        ? jsonResponse(automaticStatus)
        : jsonResponse({ photos: [], nextCursor: null }))
    renderAt('/')
    expect(await screen.findByRole('heading', { name: 'Run control' })).toBeTruthy()
    expect(screen.queryByRole('link', { name: 'Local runs' })).toBeNull()
    expect(screen.queryByRole('region', { name: 'Local run detail' })).toBeNull()
    expect(screen.queryByText(/drains a finite set of targets and exits/)).toBeNull()
    expect(screen.queryByRole('link', { name: 'System diagnostics' })).toBeNull()
    // The blobs LIST page is retired; blob detail stays reachable from photo
    // rows, so the nav link is gone but the route keeps its splat form.
    expect(screen.queryByRole('link', { name: 'Blobs' })).toBeNull()
    expect(screen.getByRole('link', { name: 'Photos' })).toBeTruthy()

    for (const target of ['/diagnostics', '/blobs']) {
      cleanup()
      renderAt(target)
      expect(await screen.findByText('Page not found')).toBeTruthy()
    }
  })

  it('renders the health section with integrity links, flags, and the vector agreement', async () => {
    mockFetch((url) => url === '/admin/api/overview'
      ? jsonResponse(overview)
      : url === '/admin/api/automatic/embedding'
        ? jsonResponse(automaticStatus)
        : jsonResponse({ photos: [], nextCursor: null }))
    renderAt('/')

    expect(await screen.findByRole('region', { name: 'System health' })).toBeTruthy()
    expect(screen.getByRole('link', { name: /Expired embedding leases/ }).getAttribute('href')).toBe('/photos?embeddingLeaseExpired=true')
    expect(screen.getByRole('link', { name: /Expired encoding leases/ }).getAttribute('href')).toBe('/photos?encodingLeaseExpired=true')
    // Unreferenced blobs lost their drilldown with the blobs list: a plain number.
    const unreferencedStat = screen.getByText('Unreferenced blobs').closest('.stat') as HTMLElement
    expect(unreferencedStat.tagName).toBe('DIV')
    expect(within(unreferencedStat).getByText('3')).toBeTruthy()
    // Photos without blobs gained one: the photos missingBlob filter.
    expect(screen.getByRole('link', { name: /Photos without blobs/ }).getAttribute('href')).toBe('/photos?missingBlob=true')
    expect(screen.getByText('Match')).toBeTruthy()
    expect(screen.getByText(/Expected 5 · Actual 5/)).toBeTruthy()
    expect(screen.getByText(/it does not verify full synchronization or point integrity/)).toBeTruthy()
    expect(screen.getByText(/Encoding worker API enabled · Embedding model enabled · Vector store enabled/)).toBeTruthy()
    // The retired overview tables stay retired.
    expect(screen.queryByText(/raw state values/)).toBeNull()
    expect(screen.queryByRole('heading', { name: /Skipped/ })).toBeNull()
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

    expect(await screen.findByText(/Unavailable/)).toBeTruthy()
    expect(screen.getByText(/Vector-store count is temporarily unavailable/)).toBeTruthy()
    // The catalog measurements still render alongside the degraded verdict.
    expect(screen.getByRole('heading', { name: 'Library' })).toBeTruthy()

    cleanup()
    const disabled = withStatus({ status: 'disabled', measuredAt: null, expectedPoints: 5, actualPoints: null, countAgreement: null }, false)
    mockFetch((url) => url === '/admin/api/overview'
      ? jsonResponse(disabled)
      : url === '/admin/api/automatic/embedding'
        ? jsonResponse(automaticStatus)
        : jsonResponse({ photos: [], nextCursor: null }))
    renderAt('/')
    expect(await screen.findByText(/Inactive — the vector store is not configured, so counts are not measured/)).toBeTruthy()
    expect(screen.getByText(/Encoding worker API enabled · Embedding model enabled · Vector store disabled/)).toBeTruthy()

    cleanup()
    const mismatch = withStatus({ ...overview.health.vectorCount, actualPoints: 6, countAgreement: false })
    mockFetch((url) => url === '/admin/api/overview'
      ? jsonResponse(mismatch)
      : url === '/admin/api/automatic/embedding'
        ? jsonResponse(automaticStatus)
        : jsonResponse({ photos: [], nextCursor: null }))
    renderAt('/')
    expect(await screen.findByText('Mismatch')).toBeTruthy()
    expect(screen.getByText(/Expected 5 · Actual 6/)).toBeTruthy()
  })
})
