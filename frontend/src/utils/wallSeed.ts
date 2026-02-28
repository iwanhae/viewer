const WALL_LAST_SEED_KEY = 'wall_last_seed'
const WALL_LAST_STATE_KEY = 'wall_last_state'

export type LastWallState = {
  mode: 'random' | 'latest'
  seed?: string
  latestPage?: number
  latestCursor?: string
}

export function readLastWallSeed(): string | null {
  if (typeof window === 'undefined') return null

  const seed = window.localStorage.getItem(WALL_LAST_SEED_KEY)
  if (!seed) return null
  if (!seed.trim()) return null
  return seed
}

export function writeLastWallSeed(seed: string): void {
  if (typeof window === 'undefined') return
  if (!seed.trim()) return
  window.localStorage.setItem(WALL_LAST_SEED_KEY, seed)
}

export function readLastWallState(): LastWallState | null {
  if (typeof window === 'undefined') return null
  const raw = window.localStorage.getItem(WALL_LAST_STATE_KEY)
  if (!raw) return null

  try {
    const parsed = JSON.parse(raw) as LastWallState | null
    if (!parsed) return null
    if (parsed.mode !== 'random' && parsed.mode !== 'latest') return null
    return parsed
  } catch {
    return null
  }
}

export function writeLastWallState(state: LastWallState): void {
  if (typeof window === 'undefined') return
  const payload: LastWallState = {
    mode: state.mode === 'latest' ? 'latest' : 'random',
  }
  if (typeof state.seed === 'string' && state.seed.trim()) {
    payload.seed = state.seed
  }
  if (typeof state.latestPage === 'number' && Number.isInteger(state.latestPage) && state.latestPage >= 1) {
    payload.latestPage = state.latestPage
  }
  if (typeof state.latestCursor === 'string' && state.latestCursor.trim()) {
    payload.latestCursor = state.latestCursor
  }
  window.localStorage.setItem(WALL_LAST_STATE_KEY, JSON.stringify(payload))
}
