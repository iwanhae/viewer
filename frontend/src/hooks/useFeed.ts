import { useCallback, useEffect, useRef, useState } from 'react'
import { fetchFeed, type FeedItem, type FeedMode } from '../api/client'

type FeedPageInfo = {
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
}

const defaultPageInfo: FeedPageInfo = {
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
  // latestRequest is what keeps concurrent loads honest: only the most recent
  // call may touch state, so a superseded response is discarded on arrival.
  // In-flight requests are deliberately NOT aborted - the response is a few KB,
  // so cancelling saves nothing, and an abort hooked into effects' cleanup
  // fires on React StrictMode's dev double-mount, killing the first fetch as
  // soon as it starts.
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

  return { items, loading, error, pageInfo }
}
