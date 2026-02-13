export function readColumnPreference(key: string, allowed: number[], fallback: number): number {
  if (typeof window === 'undefined') return fallback

  const raw = window.localStorage.getItem(key)
  if (!raw) return fallback

  const parsed = Number(raw)
  if (!Number.isInteger(parsed)) return fallback
  if (!allowed.includes(parsed)) return fallback
  return parsed
}

export function writeColumnPreference(key: string, value: number): void {
  if (typeof window === 'undefined') return
  window.localStorage.setItem(key, String(value))
}
