import { useCallback, useEffect, useState } from 'react'
import { ApiError } from '../api/http'
import { fetchPhotoSearch, type PhotoSearchItem } from '../api/client'

const SEARCH_DEBOUNCE_MS = 200

export type PhotoSearchStatus = 'idle' | 'loading' | 'ready' | 'error'

type PhotoSearchState = {
  items: PhotoSearchItem[]
  status: PhotoSearchStatus
  error: string | null
  // errorCode carries the server's error code when the failure is a typed API
  // error ("UNAVAILABLE" means vector search is switched off); it is null for
  // network-level failures, where only a message exists.
  errorCode: string | null
  refresh: () => void
}

// usePhotoSearch runs the debounced natural-language photo search for the
// Search Photos page. Unlike the album search, an empty (trimmed) query is not
// a request: the hook resets to a clean idle state, so clearing the field can
// never leave stale results on screen. Each query gets its own AbortController
// and every setState is guarded, so a slow response can never overwrite a
// newer one.
export function usePhotoSearch(query: string, limit: number): PhotoSearchState {
  const [debouncedQuery, setDebouncedQuery] = useState(query)
  const [items, setItems] = useState<PhotoSearchItem[]>([])
  const [status, setStatus] = useState<PhotoSearchStatus>('idle')
  const [error, setError] = useState<string | null>(null)
  const [errorCode, setErrorCode] = useState<string | null>(null)
  const [refreshTick, setRefreshTick] = useState(0)

  useEffect(() => {
    const timer = window.setTimeout(() => {
      setDebouncedQuery(query)
    }, SEARCH_DEBOUNCE_MS)
    return () => window.clearTimeout(timer)
  }, [query])

  useEffect(() => {
    const trimmed = debouncedQuery.trim()
    if (trimmed.length === 0) {
      setItems([])
      setStatus('idle')
      setError(null)
      setErrorCode(null)
      return
    }

    const controller = new AbortController()
    setStatus('loading')
    setError(null)
    setErrorCode(null)

    void (async () => {
      try {
        const response = await fetchPhotoSearch({
          q: trimmed,
          limit,
          signal: controller.signal,
        })
        if (controller.signal.aborted) return
        setItems(Array.isArray(response.items) ? response.items : [])
        setStatus('ready')
      } catch (err) {
        if (controller.signal.aborted) return
        setError((err as Error).message)
        setErrorCode(err instanceof ApiError ? err.code : null)
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

  return { items, status, error, errorCode, refresh }
}
