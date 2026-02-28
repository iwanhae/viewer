import { useCallback, useEffect, useRef, useState } from 'react'
import { fetchFeed, type FeedItem, type FeedMode } from '../api/client'

type FeedPageInfo = {
  cursor: string | null
  nextCursor: string | null
  prevCursor: string | null
  hasNext: boolean
  hasPrev: boolean
}

type UseFeedResult = {
  items: FeedItem[]
  loading: boolean
  error: string | null
  pageInfo: FeedPageInfo
  refetch: () => Promise<void>
}

const defaultPageInfo: FeedPageInfo = {
  cursor: null,
  nextCursor: null,
  prevCursor: null,
  hasNext: false,
  hasPrev: false,
}

export function useFeed(seed: string, mode: FeedMode, afterCursor: string): UseFeedResult {
  const [items, setItems] = useState<FeedItem[]>([])
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [pageInfo, setPageInfo] = useState<FeedPageInfo>(defaultPageInfo)
  const latestRequest = useRef(0)

  const load = useCallback(async (seedValue: string, modeValue: FeedMode, latestAfter: string): Promise<void> => {
    if (modeValue === 'random' && !seedValue.trim()) {
      setItems([])
      setError(null)
      setLoading(false)
      setPageInfo(defaultPageInfo)
      return
    }

    const requestID = latestRequest.current + 1
    latestRequest.current = requestID

    setLoading(true)
    setError(null)

    try {
      const data = await fetchFeed({
        mode: modeValue,
        seed: modeValue === 'random' ? seedValue : undefined,
        after: modeValue === 'latest' ? latestAfter : undefined,
      })
      if (latestRequest.current !== requestID) return
      setItems(data.items)
      setPageInfo({
        cursor: data.cursor ?? null,
        nextCursor: data.nextCursor ?? null,
        prevCursor: data.prevCursor ?? null,
        hasNext: Boolean(data.hasNext),
        hasPrev: Boolean(data.hasPrev),
      })
    } catch (err) {
      if (latestRequest.current !== requestID) return
      setError((err as Error).message)
      setPageInfo(defaultPageInfo)
    } finally {
      if (latestRequest.current === requestID) {
        setLoading(false)
      }
    }
  }, [])

  useEffect(() => {
    void load(seed, mode, afterCursor)
  }, [afterCursor, load, mode, seed])

  const refetch = useCallback(async (): Promise<void> => {
    await load(seed, mode, afterCursor)
  }, [afterCursor, load, mode, seed])

  return { items, loading, error, pageInfo, refetch }
}
