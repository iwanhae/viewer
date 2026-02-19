import { useEffect, useState } from 'react'
import { fetchRecommendations, type RecommendationItem } from '../api/client'

type RecommendationLoadStatus = 'idle' | 'loading' | 'ready'

type UseRecommendationsResult = {
  items: RecommendationItem[]
  status: RecommendationLoadStatus
  error: string | null
}

export function useRecommendations(
  albumId: string,
  photoIndex: number | null,
  limit = 12,
): UseRecommendationsResult {
  const [items, setItems] = useState<RecommendationItem[]>([])
  const [status, setStatus] = useState<RecommendationLoadStatus>('idle')
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    if (!albumId || photoIndex === null) {
      setItems([])
      setStatus('idle')
      setError(null)
      return
    }

    const abortController = new AbortController()

    setStatus('loading')
    setError(null)

    void (async () => {
      try {
        const result = await fetchRecommendations(albumId, photoIndex, limit, {
          signal: abortController.signal,
        })
        if (abortController.signal.aborted) return
        if (!Array.isArray(result.items)) {
          setItems([])
        } else {
          setItems(result.items)
        }
        setStatus('ready')
      } catch (err) {
        if (abortController.signal.aborted) return
        setItems([])
        setStatus('idle')
        setError((err as Error).message)
      }
    })()

    return () => {
      abortController.abort()
    }
  }, [albumId, photoIndex, limit])

  return { items, status, error }
}
