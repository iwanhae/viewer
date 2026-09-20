export function formatBytes(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes <= 0) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  let value = bytes
  let idx = 0
  while (value >= 1024 && idx < units.length - 1) {
    value /= 1024
    idx++
  }
  const precision = value >= 100 || idx === 0 ? 0 : value >= 10 ? 1 : 2
  return `${value.toFixed(precision)} ${units[idx]}`
}
