import { useCallback, useEffect, useRef, useState } from 'react'
import { ApiError } from '../api/http'
import { searchPhotosByImage, type PhotoSearchItem } from '../api/client'

export type PhotoImageSearchStatus = 'idle' | 'searching' | 'ready' | 'error'

type PhotoImageSearchState = {
  items: PhotoSearchItem[]
  status: PhotoImageSearchStatus
  error: string | null
  // errorCode carries the server's error code when the failure is a typed API
  // error ("UNAVAILABLE" means vector search is switched off); it is null for
  // network-level failures, where only a message exists.
  errorCode: string | null
  search: (file: File) => void
  clear: () => void
  refresh: () => void
}

// usePhotoImageSearch runs the search-by-image request for the Search Photos
// page: the page hands it the file the user picked, the hook owns the request
// lifecycle. Each search gets its own AbortController and every setState is
// guarded, so a slow response can never overwrite a newer one; picking a new
// file, clearing, or unmounting cancels whatever is in flight. The file lives
// in a ref next to a request tick in state: picking the same file twice must
// still re-run the search, which a state-only file (compared by reference)
// would skip. Like usePhotoSearch, an error surfaces the server's code so
// "UNAVAILABLE" stays distinguishable on the page.
export function usePhotoImageSearch(limit: number): PhotoImageSearchState {
  const [items, setItems] = useState<PhotoSearchItem[]>([])
  const [status, setStatus] = useState<PhotoImageSearchStatus>('idle')
  const [error, setError] = useState<string | null>(null)
  const [errorCode, setErrorCode] = useState<string | null>(null)
  const [requestTick, setRequestTick] = useState(0)
  const fileRef = useRef<File | null>(null)

  useEffect(() => {
    const file = fileRef.current
    if (file === null) {
      setItems([])
      setStatus('idle')
      setError(null)
      setErrorCode(null)
      return
    }

    const controller = new AbortController()
    setStatus('searching')
    setError(null)
    setErrorCode(null)

    void (async () => {
      try {
        const response = await searchPhotosByImage({
          file,
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
  }, [limit, requestTick])

  const search = useCallback((file: File) => {
    fileRef.current = file
    setRequestTick((tick) => tick + 1)
  }, [])

  const clear = useCallback(() => {
    fileRef.current = null
    setRequestTick((tick) => tick + 1)
  }, [])

  const refresh = useCallback(() => {
    setRequestTick((tick) => tick + 1)
  }, [])

  return { items, status, error, errorCode, search, clear, refresh }
}
