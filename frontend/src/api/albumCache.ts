import type { AlbumIndex } from './types'

const maxCachedAlbums = 64
const albumCache = new Map<string, AlbumIndex>()

function touch(albumId: string, album: AlbumIndex): void {
  albumCache.delete(albumId)
  albumCache.set(albumId, album)
}

export function getCachedAlbum(albumId: string): AlbumIndex | null {
  if (!albumId) return null
  const cached = albumCache.get(albumId)
  if (!cached) return null
  touch(albumId, cached)
  return cached
}

export function cacheAlbum(album: AlbumIndex): void {
  if (!album?.albumId) return
  touch(album.albumId, album)
  if (albumCache.size <= maxCachedAlbums) return

  const oldest = albumCache.keys().next().value
  if (typeof oldest === 'string') {
    albumCache.delete(oldest)
  }
}
