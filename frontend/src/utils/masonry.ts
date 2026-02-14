function normalizeColumnCount(columnCount: number): number {
  if (!Number.isFinite(columnCount)) return 1
  const rounded = Math.floor(columnCount)
  return rounded > 0 ? rounded : 1
}

function normalizeWeight(weight: number): number {
  if (!Number.isFinite(weight) || weight <= 0) return 1
  return weight
}

export function distributeMasonry<T>(
  items: T[],
  columnCount: number,
  weight: (item: T) => number,
): T[][] {
  const normalizedColumnCount = normalizeColumnCount(columnCount)
  const columns = Array.from({ length: normalizedColumnCount }, () => [] as T[])
  const heights = Array.from({ length: normalizedColumnCount }, () => 0)

  for (const item of items) {
    let targetColumn = 0
    let shortestHeight = heights[0]
    for (let index = 1; index < normalizedColumnCount; index++) {
      if (heights[index] < shortestHeight) {
        shortestHeight = heights[index]
        targetColumn = index
      }
    }

    columns[targetColumn].push(item)
    heights[targetColumn] += normalizeWeight(weight(item))
  }

  return columns
}
