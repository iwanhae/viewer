import { useCallback, useEffect, useState } from 'react'
import { fetchAlbum, getCachedAlbum, seedCachedAlbum, type AlbumIndex } from '../api/client'

type UseAlbumResult = {
  album: AlbumIndex | null
  loading: boolean
  error: string | null
  refetch: () => Promise<void>
}

function resolveSeededAlbum(albumId: string, preferredAlbum?: AlbumIndex | null): AlbumIndex | null {
  if (preferredAlbum && preferredAlbum.albumId === albumId) {
    return preferredAlbum
  }
  return getCachedAlbum(albumId)
}

export function useAlbum(albumId: string, preferredAlbum?: AlbumIndex | null): UseAlbumResult {
  const [album, setAlbum] = useState<AlbumIndex | null>(() => resolveSeededAlbum(albumId, preferredAlbum))
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    const seeded = resolveSeededAlbum(albumId, preferredAlbum)
    setAlbum(seeded)
    setError(null)

    if (!albumId) {
      setLoading(false)
      setError('Missing album ID')
      return
    }

    if (seeded) {
      setLoading(false)
      return
    }

    const abortController = new AbortController()
    setLoading(true)

    void (async () => {
      try {
        const fetched = await fetchAlbum(albumId, { signal: abortController.signal })
        setAlbum(fetched)
      } catch (err) {
        if (abortController.signal.aborted) return
        setError((err as Error).message)
      } finally {
        if (!abortController.signal.aborted) {
          setLoading(false)
        }
      }
    })()

    return () => {
      abortController.abort()
    }
  }, [albumId, preferredAlbum])

  const refetch = useCallback(async (): Promise<void> => {
    if (!albumId) {
      setError('Missing album ID')
      return
    }

    setLoading(true)
    setError(null)

    try {
      const fetched = await fetchAlbum(albumId)
      seedCachedAlbum(fetched)
      setAlbum(fetched)
    } catch (err) {
      setError((err as Error).message)
    } finally {
      setLoading(false)
    }
  }, [albumId])

  return { album, loading, error, refetch }
}
