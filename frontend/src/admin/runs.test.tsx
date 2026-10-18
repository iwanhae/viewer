// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from 'vitest'
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'
import AdminApp from './ui'
import type { AutomaticEmbedding, JobRun, JobRunItem } from './api'

const time = '2026-10-18T12:00:00Z'
const run = (overrides: Partial<JobRun> = {}): JobRun => ({
  id: 'run-1', stage: 'embedding', actor: 'operator', executor: 'local-embedding', status: 'running', cancelRequested: false,
  createdAt: time, updatedAt: time, startedAt: time, finishedAt: null, lastError: '',
  counts: { total: 2, pending: 1, running: 1, succeeded: 0, failed: 0, busy: 0, skipped: 0, cancelled: 0, interrupted: 0 },
  ...overrides
})
const item = (overrides: Partial<JobRunItem> = {}): JobRunItem => ({
  hash: 'hash-1', status: 'running', startedAt: time, finishedAt: null, updatedAt: time, lastError: '', ...overrides
})
const auto: AutomaticEmbedding = { enabled: true, running: false, pausedByManual: false }
const json = (body: unknown, status = 200) => ({ ok: status >= 200 && status < 300, status, json: async () => body }) as Response
function deferred<T>() {
  let resolve!: (value: T) => void
  let reject!: (reason: unknown) => void
  const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no })
  return { promise, resolve, reject }
}
function mockAPI(responder?: (url: string, init?: RequestInit) => Response | Promise<Response> | undefined) {
  const mock = vi.fn((url: string, init?: RequestInit) => Promise.resolve(responder?.(url, init) ?? (
    url === '/admin/api/automatic/embedding' ? json(auto) : json({ runs: [], nextCursor: null })
  )))
  vi.stubGlobal('fetch', mock)
  vi.stubGlobal('crypto', { randomUUID: vi.fn(() => 'test-uuid') })
  return mock
}
function renderRuns() {
  return render(<MemoryRouter initialEntries={['/runs']} future={{ v7_startTransition: true, v7_relativeSplatPath: true }}><AdminApp /></MemoryRouter>)
}
const autoPanel = () => screen.getByRole('region', { name: '자동 임베딩 제어' })
const detailPanel = () => screen.getByRole('region', { name: '로컬 실행 상세' })

describe('local runs Admin UI', () => {
  afterEach(() => { cleanup(); vi.useRealTimers(); vi.unstubAllGlobals(); vi.restoreAllMocks() })

  it.each(['embedding', 'encoding'] as const)('submits an exact %s snapshot request with Admin headers and an idempotency key', async (stage) => {
    const created = run({ stage, executor: `local-${stage}` })
    const post = deferred<Response>()
    const fetchMock = mockAPI((url, init) => {
      if (url === '/admin/api/runs' && init?.method === 'POST') return post.promise
      if (url === '/admin/api/runs/run-1?limit=50') return json({ run: created, items: [item()], nextCursor: null })
    })
    renderRuns()
    expect(screen.getByRole('link', { name: '로컬 실행' }).getAttribute('href')).toBe('/runs')
    expect(screen.getByText(/유한한 대상만 소진한 뒤 종료/)).toBeTruthy()
    expect(screen.getByText(/외부 워커를 제어하거나 취소하지 않습니다/)).toBeTruthy()
    await screen.findByRole('button', { name: '자동 임베딩 일시정지' })
    const button = screen.getByRole('button', { name: `${stage === 'embedding' ? '임베딩' : '인코딩'} 수동 실행` })
    fireEvent.click(button)
    fireEvent.click(button)
    const calls = fetchMock.mock.calls.filter(([url]) => url === '/admin/api/runs')
    expect(calls).toHaveLength(1)
    expect(calls[0][1]).toMatchObject({ method: 'POST', body: JSON.stringify({ stage }), credentials: 'same-origin', cache: 'no-store' })
    const headers = new Headers(calls[0][1]?.headers)
    expect(headers.get('Idempotency-Key')).toBe('admin-run-test-uuid')
    expect(headers.get('X-Admin-Action')).toBe('1')
    expect(headers.get('Content-Type')).toBe('application/json')
    await act(async () => { post.resolve(json(created, 202)) })
    expect(await within(detailPanel()).findByRole('link', { name: 'hash-1' })).toBeTruthy()
    expect(fetchMock).toHaveBeenCalledWith('/admin/api/runs?limit=50', expect.objectContaining({ method: 'GET' }))
  })

  it('reuses the snapshot key after an ambiguous lost response, including a 200 replay', async () => {
    let posts = 0
    const fetchMock = mockAPI((url, init) => {
      if (url === '/admin/api/runs' && init?.method === 'POST') {
        posts++
        if (posts === 1) return Promise.reject(new Error('connection lost'))
        return json(run(), 200)
      }
      if (url === '/admin/api/runs/run-1?limit=50') return json({ run: run(), items: [], nextCursor: null })
    })
    renderRuns()
    fireEvent.click(screen.getByRole('button', { name: '임베딩 수동 실행' }))
    expect(await screen.findByText('connection lost')).toBeTruthy()
    fireEvent.click(screen.getByRole('button', { name: '임베딩 수동 실행' }))
    const detail = await screen.findByRole('region', { name: '로컬 실행 상세' })
    await within(detail).findByText(/실행기 local-embedding/)
    const keys = fetchMock.mock.calls.filter(([url]) => url === '/admin/api/runs').map(([, init]) => new Headers(init?.headers).get('Idempotency-Key'))
    expect(keys).toEqual(['admin-run-test-uuid', 'admin-run-test-uuid'])
  })

  it('shows enabled-but-not-running and manual-yield status, and separately pauses/resumes automatic embedding', async () => {
    let status = { ...auto, pausedByManual: true }
    const pause = deferred<Response>()
    const fetchMock = mockAPI((url, init) => {
      if (url !== '/admin/api/automatic/embedding') return
      if (init?.method === 'POST') {
        const { enabled } = JSON.parse(String(init.body)) as { enabled: boolean }
        status = { enabled, running: enabled, pausedByManual: false }
        return enabled ? json(status) : pause.promise
      }
      return json(status)
    })
    renderRuns()
    await screen.findByText('실행 중 아님')
    expect(within(autoPanel()).getByText('활성')).toBeTruthy()
    expect(within(autoPanel()).getByText('수동 실행으로 일시정지 중')).toBeTruthy()
    expect(screen.getByText(/모델 로딩 중에는 실행 중이 아닐 수 있습니다/)).toBeTruthy()
    fireEvent.click(screen.getByRole('button', { name: '자동 임베딩 일시정지' }))
    expect(screen.getByRole('button', { name: '자동 설정 변경 중…' })).toHaveProperty('disabled', true)
    expect(within(autoPanel()).getByText('활성')).toBeTruthy()
    await act(async () => { pause.resolve(json(status)) })
    fireEvent.click(await screen.findByRole('button', { name: '자동 임베딩 재개' }))
    await screen.findByRole('button', { name: '자동 임베딩 일시정지' })
    expect(within(autoPanel()).getByText('실행 중')).toBeTruthy()
    const posts = fetchMock.mock.calls.filter(([, init]) => init?.method === 'POST')
    expect(posts.map(([url, init]) => [url, init?.body])).toEqual([
      ['/admin/api/automatic/embedding', '{"enabled":false}'], ['/admin/api/automatic/embedding', '{"enabled":true}']
    ])
    expect(posts.every(([, init]) => new Headers(init?.headers).get('X-Admin-Action') === '1')).toBe(true)
    expect(posts.some(([url]) => url.includes('/cancel'))).toBe(false)
  })

  it('pages durable runs and item outcomes with encoded cursors, safe errors and timestamps', async () => {
    const completed = run({ status: 'failed', finishedAt: time, lastError: 'processing_failed' })
    const fetchMock = mockAPI((url) => {
      if (url === '/admin/api/runs?limit=50') return json({ runs: [completed], nextCursor: 'older+/=' })
      if (url === '/admin/api/runs?limit=50&cursor=older%2B%2F%3D') return json({ runs: [run({ id: 'old-run', status: 'completed' })], nextCursor: null })
      if (url === '/admin/api/runs/run-1?limit=50') return json({ run: completed, items: [item({ status: 'failed', lastError: 'processing_failed', finishedAt: time })], nextCursor: 'items+/=' })
      if (url === '/admin/api/runs/run-1?limit=50&cursor=items%2B%2F%3D') return json({ run: completed, items: [item({ hash: 'hash-2', status: 'busy', lastError: 'target_busy', startedAt: null })], nextCursor: null })
    })
    renderRuns()
    fireEvent.click(await screen.findByRole('button', { name: 'run-1' }))
    await within(detailPanel()).findByRole('link', { name: 'hash-1' })
    expect(within(detailPanel()).getAllByText('processing_failed').length).toBeGreaterThan(0)
    expect(within(detailPanel()).getByRole('row', { name: /hash-1/ }).textContent).toContain(new Date(time).toLocaleString())
    expect(within(detailPanel()).queryByRole('checkbox')).toBeNull()
    fireEvent.click(screen.getByRole('button', { name: '다음 항목 페이지 →' }))
    expect(await screen.findByRole('link', { name: 'hash-2' })).toBeTruthy()
    expect(screen.queryByRole('link', { name: 'hash-1' })).toBeNull()
    expect(screen.getByText('target_busy')).toBeTruthy()
    fireEvent.click(screen.getByRole('button', { name: '← 이전 항목 페이지' }))
    await screen.findByRole('link', { name: 'hash-1' })
    fireEvent.click(screen.getByRole('button', { name: '이전 실행 더 보기 →' }))
    await screen.findByRole('button', { name: 'old-run' })
    fireEvent.click(screen.getByRole('button', { name: '← 이전 실행 페이지' }))
    await screen.findByRole('button', { name: 'run-1' })
    expect(fetchMock).toHaveBeenCalledWith('/admin/api/runs/run-1?limit=50&cursor=items%2B%2F%3D', expect.anything())
    expect(fetchMock).toHaveBeenCalledWith('/admin/api/runs?limit=50&cursor=older%2B%2F%3D', expect.anything())
    expect(screen.queryByText(/claimToken|claim_token/)).toBeNull()
  })

  it('requires explicit confirmation and cancels only the selected active local run', async () => {
    let current = run()
    const fetchMock = mockAPI((url) => {
      if (url === '/admin/api/runs?limit=50') return json({ runs: [current], nextCursor: null })
      if (url === '/admin/api/runs/run-1?limit=50') return json({ run: current, items: [], nextCursor: null })
      if (url === '/admin/api/runs/run-1/cancel') { current = { ...current, cancelRequested: true }; return json(current) }
    })
    renderRuns()
    fireEvent.click(await screen.findByRole('button', { name: 'run-1' }))
    const button = await screen.findByRole('button', { name: '확인한 로컬 실행 취소' })
    expect(button).toHaveProperty('disabled', true)
    fireEvent.click(button)
    expect(fetchMock.mock.calls.some(([url]) => url.includes('/cancel'))).toBe(false)
    fireEvent.click(screen.getByRole('checkbox', { name: /이 로컬 실행만 취소합니다/ }))
    fireEvent.click(button)
    await screen.findByText('이 로컬 실행의 취소가 요청되었습니다.')
    const posts = fetchMock.mock.calls.filter(([, init]) => init?.method === 'POST')
    expect(posts).toHaveLength(1)
    expect(posts[0][0]).toBe('/admin/api/runs/run-1/cancel')
    expect(posts[0][1]?.body).toBe('{}')
    expect(new Headers(posts[0][1]?.headers).get('X-Admin-Action')).toBe('1')
    expect(screen.getByRole('button', { name: '임베딩 수동 실행' })).toHaveProperty('disabled', true)
  })

  it.each([503, 409, 401])('shows Admin action HTTP %s errors without losing durable records', async (status) => {
    mockAPI((url, init) => {
      if (url === '/admin/api/runs?limit=50') return json({ runs: [run({ id: 'saved-run', status: 'completed' })], nextCursor: null })
      if (init?.method === 'POST') return json({ error: { message: `safe-error-${status}` } }, status)
    })
    renderRuns()
    await screen.findByRole('button', { name: 'saved-run' })
    fireEvent.click(screen.getByRole('button', { name: '임베딩 수동 실행' }))
    expect((await screen.findByRole('alert')).textContent).toContain(`safe-error-${status}`)
    if (status === 503) expect(screen.getByRole('alert').textContent).toContain('모델 로딩 중')
    expect(screen.getByRole('button', { name: 'saved-run' })).toBeTruthy()
    await waitFor(() => expect(screen.getByRole('button', { name: '임베딩 수동 실행' })).toHaveProperty('disabled', false))
    fireEvent.click(screen.getByRole('button', { name: '자동 임베딩 일시정지' }))
    expect((await screen.findByRole('alert')).textContent).toContain(`safe-error-${status}`)
    if (status === 503) expect(screen.getByRole('alert').textContent).not.toContain('로컬 실행기를 사용할 수 없습니다')
    expect(within(autoPanel()).getByText('활성')).toBeTruthy()
  })

  it('keeps confirmed pause and cancellation responses visible if subsequent refreshes fail', async () => {
    let paused = false
    let cancelled = false
    mockAPI((url, init) => {
      if (url === '/admin/api/automatic/embedding') {
        if (init?.method === 'POST') { paused = true; return json({ ...auto, enabled: false }) }
        return paused ? json({ error: { message: 'automatic refresh unavailable' } }, 500) : json(auto)
      }
      if (url === '/admin/api/runs?limit=50') return json({ runs: [run()], nextCursor: null })
      if (url === '/admin/api/runs/run-1?limit=50') return cancelled
        ? json({ error: { message: 'cancel refresh unavailable' } }, 500)
        : json({ run: run(), items: [item()], nextCursor: null })
      if (url === '/admin/api/runs/run-1/cancel') { cancelled = true; return json(run({ cancelRequested: true })) }
    })
    renderRuns()
    fireEvent.click(await screen.findByRole('button', { name: '자동 임베딩 일시정지' }))
    await screen.findByText(/automatic refresh unavailable/)
    expect(screen.getByRole('button', { name: '자동 임베딩 재개' })).toBeTruthy()
    fireEvent.click(screen.getByRole('button', { name: 'run-1' }))
    fireEvent.click(await screen.findByRole('checkbox', { name: /이 로컬 실행만 취소합니다/ }))
    fireEvent.click(screen.getByRole('button', { name: '확인한 로컬 실행 취소' }))
    await screen.findByText(/cancel refresh unavailable/)
    expect(screen.getByText('이 로컬 실행의 취소가 요청되었습니다.')).toBeTruthy()
    expect(screen.getByRole('link', { name: 'hash-1' })).toBeTruthy()
  })

  it('keeps the selected run and its cancellation confirmation after a failed cancel request', async () => {
    mockAPI((url) => {
      if (url === '/admin/api/runs?limit=50') return json({ runs: [run()], nextCursor: null })
      if (url === '/admin/api/runs/run-1?limit=50') return json({ run: run(), items: [item()], nextCursor: null })
      if (url === '/admin/api/runs/run-1/cancel') return json({ error: { message: 'cancel unavailable' } }, 500)
    })
    renderRuns()
    fireEvent.click(await screen.findByRole('button', { name: 'run-1' }))
    fireEvent.click(await screen.findByRole('checkbox', { name: /이 로컬 실행만 취소합니다/ }))
    fireEvent.click(screen.getByRole('button', { name: '확인한 로컬 실행 취소' }))
    expect(await screen.findByText('cancel unavailable')).toBeTruthy()
    expect(screen.getByRole('link', { name: 'hash-1' })).toBeTruthy()
    expect(screen.getByRole('checkbox', { name: /이 로컬 실행만 취소합니다/ })).toHaveProperty('checked', true)
    expect(screen.getByRole('button', { name: '확인한 로컬 실행 취소' })).toHaveProperty('disabled', false)
  })

  it('polls active runs and item outcomes, retains last good data on errors, and stops terminal item polling', async () => {
    vi.useFakeTimers()
    let phase = 0
    const fetchMock = mockAPI((url) => {
      const current = run({ status: phase === 2 ? 'completed' : 'running' })
      if (url === '/admin/api/automatic/embedding' && phase === 1) return json({ error: { message: 'automatic unavailable' } }, 500)
      if (url === '/admin/api/runs?limit=50') return phase === 1 ? json({ error: { message: 'list unavailable' } }, 500) : json({ runs: [current], nextCursor: null })
      if (url === '/admin/api/runs/run-1?limit=50') return phase === 1 ? json({ error: { message: 'items unavailable' } }, 500) : json({ run: current, items: [item({ status: phase === 2 ? 'succeeded' : 'running' })], nextCursor: null })
    })
    renderRuns()
    await act(async () => {})
    fireEvent.click(screen.getByRole('button', { name: 'run-1' }))
    await act(async () => {})
    phase = 1
    await act(async () => { vi.advanceTimersByTime(5_000) })
    expect(screen.getByRole('button', { name: 'run-1' })).toBeTruthy()
    expect(screen.getByRole('link', { name: 'hash-1' })).toBeTruthy()
    expect(within(autoPanel()).getByText('활성')).toBeTruthy()
    expect(screen.getAllByRole('alert').map((node) => node.textContent).join(' ')).toContain('마지막 정상 데이터')
    phase = 2
    await act(async () => { vi.advanceTimersByTime(5_000) })
    expect(within(detailPanel()).getByText('성공 (succeeded)')).toBeTruthy()
    expect(screen.queryByRole('alert')).toBeNull()
    const itemCalls = () => fetchMock.mock.calls.filter(([url]) => url === '/admin/api/runs/run-1?limit=50').length
    expect(itemCalls()).toBe(3)
    await act(async () => { vi.advanceTimersByTime(5_000) })
    expect(itemCalls()).toBe(3)
  })

  it('ignores and aborts late results from a previously selected run and cleans polling on unmount', async () => {
    const late = deferred<Response>()
    const clear = vi.spyOn(window, 'clearInterval')
    const fetchMock = mockAPI((url) => {
      if (url === '/admin/api/runs?limit=50') return json({ runs: [run(), run({ id: 'run-2' })], nextCursor: null })
      if (url === '/admin/api/runs/run-1?limit=50') return late.promise
      if (url === '/admin/api/runs/run-2?limit=50') return json({ run: run({ id: 'run-2' }), items: [item({ hash: 'new-hash' })], nextCursor: null })
    })
    const view = renderRuns()
    fireEvent.click(await screen.findByRole('button', { name: 'run-1' }))
    await waitFor(() => expect(fetchMock.mock.calls.some(([url]) => url === '/admin/api/runs/run-1?limit=50')).toBe(true))
    fireEvent.click(screen.getByRole('button', { name: 'run-2' }))
    await screen.findByRole('link', { name: 'new-hash' })
    expect(fetchMock.mock.calls.find(([url]) => url === '/admin/api/runs/run-1?limit=50')?.[1]?.signal?.aborted).toBe(true)
    await act(async () => { late.resolve(json({ run: run(), items: [item({ hash: 'stale-hash' })], nextCursor: null })) })
    expect(screen.queryByText('stale-hash')).toBeNull()
    view.unmount()
    expect(clear.mock.calls.length).toBeGreaterThanOrEqual(4)
    expect(fetchMock.mock.calls.every(([, init]) => init?.signal?.aborted)).toBe(true)
  })

  it('does not let an old automatic poll overwrite a successful pause, and aborts pending actions on unmount', async () => {
    vi.useFakeTimers()
    let reads = 0
    let status = auto
    const oldPoll = deferred<Response>()
    const post = deferred<Response>()
    const fetchMock = mockAPI((url, init) => {
      if (url === '/admin/api/automatic/embedding') {
        if (init?.method === 'POST') { status = { enabled: false, running: false, pausedByManual: false }; return json(status) }
        if (++reads === 2) return oldPoll.promise
        return json(status)
      }
      if (url === '/admin/api/runs') return post.promise
    })
    const view = renderRuns()
    await act(async () => {})
    await act(async () => { vi.advanceTimersByTime(5_000) })
    fireEvent.click(screen.getByRole('button', { name: '자동 임베딩 일시정지' }))
    await act(async () => {})
    expect(screen.getByRole('button', { name: '자동 임베딩 재개' })).toBeTruthy()
    await act(async () => { oldPoll.resolve(json(auto)) })
    expect(screen.getByRole('button', { name: '자동 임베딩 재개' })).toBeTruthy()
    fireEvent.click(screen.getByRole('button', { name: '인코딩 수동 실행' }))
    view.unmount()
    const signal = fetchMock.mock.calls.find(([url]) => url === '/admin/api/runs')?.[1]?.signal
    expect(signal?.aborted).toBe(true)
    const calls = fetchMock.mock.calls.length
    await act(async () => { vi.advanceTimersByTime(10_000); post.resolve(json(run(), 202)) })
    expect(fetchMock).toHaveBeenCalledTimes(calls)
  })
})
