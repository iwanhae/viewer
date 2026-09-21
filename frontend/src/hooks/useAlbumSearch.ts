import { useCallback, useEffect, useState } from 'react'
import { fetchAlbumSearch, type AlbumSearchItem } from '../api/client'

const SEARCH_DEBOUNCE_MS = 200

export type AlbumSearchStatus = 'loading' | 'ready' | 'error'

type AlbumSearchState = {
  items: AlbumSearchItem[]
  status: AlbumSearchStatus
  error: string | null
  refresh: () => void
}

// useAlbumSearch runs the debounced album-name search for the Find Albums
// page. An empty query is a real search: the server answers with its newest
// albums, which the page presents as "Recent albums". Each query gets its own
// AbortController and every setState is guarded, so a slow response can never
// overwrite a newer one.
export function useAlbumSearch(query: string, limit: number): AlbumSearchState {
  const [debouncedQuery, setDebouncedQuery] = useState(query)
  const [items, setItems] = useState<AlbumSearchItem[]>([])
  const [status, setStatus] = useState<AlbumSearchStatus>('loading')
  const [error, setError] = useState<string | null>(null)
  const [refreshTick, setRefreshTick] = useState(0)

  useEffect(() => {
    const timer = window.setTimeout(() => {
      setDebouncedQuery(query)
    }, SEARCH_DEBOUNCE_MS)
    return () => window.clearTimeout(timer)
  }, [query])

  useEffect(() => {
    const controller = new AbortController()
    setStatus('loading')
    setError(null)

    void (async () => {
      try {
        const trimmed = debouncedQuery.trim()
        const response = await fetchAlbumSearch({
          q: trimmed.length > 0 ? trimmed : undefined,
          limit,
          signal: controller.signal,
        })
        if (controller.signal.aborted) return
        setItems(Array.isArray(response.albums) ? response.albums : [])
        setStatus('ready')
      } catch (err) {
        if (controller.signal.aborted) return
        setError((err as Error).message)
        setStatus('error')
      }
    })()

    return () => {
      controller.abort()
    }
  }, [debouncedQuery, limit, refreshTick])

  const refresh = useCallback(() => {
    setRefreshTick((tick) => tick + 1)
  }, [])

  return { items, status, error, refresh }
}
