// Shared album-wall paging helpers. The wall, the album viewer and the photo
// page all page through the same album listing, so the page size and the
// index-to-page conversion live in one place.

export const ALBUM_PAGE_SIZE = 50

// COLUMN_OPTIONS is the set of masonry column counts every grid offers.
export const COLUMN_OPTIONS = [1, 2, 3, 4, 5, 6]

export function pageForPhotoIndex(index: number): number {
  return Math.floor(index / ALBUM_PAGE_SIZE) + 1
}

export function parsePositiveInt(value: string | null, fallback: number): number {
  const parsed = Number(value)
  if (!Number.isInteger(parsed) || parsed < 1) return fallback
  return parsed
}

// wallFocusKey identifies the wall tile a focus request should scroll to.
export function wallFocusKey(albumId: string, photoIndex: number): string {
  return `${albumId}:${photoIndex}`
}
