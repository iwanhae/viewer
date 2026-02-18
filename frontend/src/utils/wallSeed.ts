const WALL_LAST_SEED_KEY = 'wall_last_seed'

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
