import { useEffect, useState } from 'react'
import { fetchEmbeddingStatus } from '../api/client'
import type { EmbeddingProgress } from '../api/types'

// Embedding runs in the background long after an upload has been indexed, so
// the indicator polls: quickly while images are still pending, slowly once the
// catalog has settled, which keeps an idle viewer quiet without going blind to
// work that starts later.
const busyPollIntervalMs = 2000
const idlePollIntervalMs = 15000

export function useEmbeddingProgress(): EmbeddingProgress | null {
  const [progress, setProgress] = useState<EmbeddingProgress | null>(null)

  useEffect(() => {
    let stopped = false
    let timer = 0
    let controller: AbortController | null = null

    const schedule = (delayMs: number) => {
      if (stopped) return
      timer = window.setTimeout(() => {
        void poll()
      }, delayMs)
    }

    const poll = async () => {
      controller = new AbortController()
      let delayMs = idlePollIntervalMs
      try {
        const next = await fetchEmbeddingStatus({ signal: controller.signal })
        if (stopped) return
        setProgress(next)
        if (next.enabled && (next.active || next.pending > 0)) {
          delayMs = busyPollIntervalMs
        }
      } catch {
        // Keep the last known state on screen and retry on the slow interval.
      }
      schedule(delayMs)
    }

    void poll()
    return () => {
      stopped = true
      window.clearTimeout(timer)
      controller?.abort()
    }
  }, [])

  return progress
}
