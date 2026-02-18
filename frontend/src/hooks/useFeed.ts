import { useCallback, useEffect, useRef, useState } from 'react'
import { fetchFeed, type FeedItem } from '../api/client'

type UseFeedResult = {
  items: FeedItem[]
  loading: boolean
  error: string | null
  refetch: () => Promise<void>
}

export function useFeed(seed: string): UseFeedResult {
  const [items, setItems] = useState<FeedItem[]>([])
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const latestRequest = useRef(0)

  const load = useCallback(async (seedValue: string): Promise<void> => {
    if (!seedValue) {
      setItems([])
      setError(null)
      setLoading(false)
      return
    }

    const requestID = latestRequest.current + 1
    latestRequest.current = requestID

    setLoading(true)
    setError(null)

    try {
      const data = await fetchFeed({ seed: seedValue })
      if (latestRequest.current !== requestID) return
      setItems(data.items)
    } catch (err) {
      if (latestRequest.current !== requestID) return
      setError((err as Error).message)
    } finally {
      if (latestRequest.current === requestID) {
        setLoading(false)
      }
    }
  }, [])

  useEffect(() => {
    void load(seed)
  }, [load, seed])

  const refetch = useCallback(async (): Promise<void> => {
    await load(seed)
  }, [load, seed])

  return { items, loading, error, refetch }
}
