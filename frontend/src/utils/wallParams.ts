// Pure URL-parameter logic for the wall page. The wall's query string owns
// exactly four keys - mode, seed, lp, lc - and every rule about fallbacks,
// restores and canonical forms lives here so the page component only renders.
//
//   mode=latest pages with lc, the opaque keyset cursor the server hands out;
//   lp is the cosmetic "Page N" counter. A missing lc means the first page,
//   so a cursor-less latest URL is always lp=1.
//
//   mode=random pages with seed, the shuffle seed.
//
// On first visit with no params at all, the last wall state from localStorage
// is restored into the URL (once, with replace) so sharing or reloading the
// link lands on the same wall.

import type { FeedMode } from '../api/client'
import { parsePositiveInt } from './albumPaging'
import type { LastWallState } from './storage'

export type WallParams = {
  mode: FeedMode
  // random mode: the shuffle seed.
  seed: string
  // latest mode: cosmetic page counter for the indicator.
  latestPage: number
  // latest mode: the keyset cursor; '' is the first page.
  latestCursor: string
}

export type ResolvedWallParams = WallParams & {
  // dirty is true when the raw query string does not yet spell out the
  // resolved params - bare "/" restoring state, or a partial/hand-edited query.
  dirty: boolean
}

// OWNED_KEYS is every query key the wall claims; normalization rewrites these
// and leaves anything else (focus, future keys) untouched.
const OWNED_KEYS = ['mode', 'seed', 'lp', 'lc'] as const

function normalizeStoredPage(value: unknown): number {
  return typeof value === 'number' && Number.isInteger(value) && value >= 1 ? value : 1
}

function normalizeStoredCursor(value: unknown): string {
  return typeof value === 'string' && value.trim() ? value : ''
}

export function resolveWallParams(
  searchParams: URLSearchParams,
  stored: LastWallState | null,
  fallbackSeed: string,
): ResolvedWallParams {
  const rawMode = searchParams.get('mode')
  const explicitMode: FeedMode | null = rawMode === 'latest' || rawMode === 'random' ? rawMode : null
  const restoring = explicitMode === null
  const storedMode: FeedMode = stored?.mode === 'latest' ? 'latest' : 'random'

  if (explicitMode === 'latest' || (restoring && storedMode === 'latest')) {
    const restoredCursor = restoring ? normalizeStoredCursor(stored?.latestCursor) : ''
    const latestCursor = searchParams.get('lc')?.trim() || restoredCursor
    // A cursor-less latest URL is the first page; a page number without a
    // cursor cannot be represented, so it is forced back to 1.
    const latestPage =
      latestCursor === ''
        ? 1
        : parsePositiveInt(searchParams.get('lp'), restoring ? normalizeStoredPage(stored?.latestPage) : 1)
    const params: WallParams = { mode: 'latest', seed: '', latestPage, latestCursor }
    return { ...params, dirty: isDirty(searchParams, params) }
  }

  const rawSeed = searchParams.get('seed')?.trim() || ''
  const restoredSeed = restoring ? normalizeStoredCursor(stored?.seed) : ''
  const params: WallParams = { mode: 'random', seed: rawSeed || restoredSeed || fallbackSeed, latestPage: 1, latestCursor: '' }
  return { ...params, dirty: isDirty(searchParams, params) }
}

// materializeWallParams writes the resolved params over a copy of prev,
// preserving keys the wall does not own (focus, anything future).
export function materializeWallParams(prev: URLSearchParams, params: WallParams): URLSearchParams {
  const next = new URLSearchParams(prev)
  next.set('mode', params.mode)
  if (params.mode === 'random') {
    next.set('seed', params.seed)
    next.delete('lp')
    next.delete('lc')
  } else {
    next.set('lp', String(params.latestPage))
    if (params.latestCursor) {
      next.set('lc', params.latestCursor)
    } else {
      next.delete('lc')
    }
    next.delete('seed')
  }
  return next
}

function isDirty(searchParams: URLSearchParams, params: WallParams): boolean {
  const canonical = materializeWallParams(new URLSearchParams(), params)
  for (const key of OWNED_KEYS) {
    if ((searchParams.get(key) ?? '') !== (canonical.get(key) ?? '')) return true
  }
  return false
}
