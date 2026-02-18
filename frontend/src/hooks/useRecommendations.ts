import { useEffect, useState } from 'react'
import {
  fetchRecommendations,
  type RecommendationItem,
  type RecommendationStatus,
} from '../api/client'

type RecommendationLoadStatus = RecommendationStatus | 'idle' | 'loading'

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
        const fetchLimit = Math.max(limit * 4, 12)
        const result = await fetchRecommendations(albumId, photoIndex, fetchLimit, {
          signal: abortController.signal,
        })
        if (abortController.signal.aborted) return
        if (!Array.isArray(result.items)) {
          setItems([])
        } else {
          const seenAlbumIds = new Set<string>()
          const dedupedItems: RecommendationItem[] = []
          for (const item of result.items) {
            if (seenAlbumIds.has(item.albumId)) continue
            seenAlbumIds.add(item.albumId)
            dedupedItems.push(item)
            if (dedupedItems.length >= limit) {
              break
            }
          }
          setItems(dedupedItems)
        }
        setStatus(result.status)
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
