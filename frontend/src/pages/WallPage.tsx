import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { useNavigate, useSearchParams } from 'react-router-dom'
import { createAlbum, fetchFeed, FeedItem, finalizeAlbum, uploadZip, uploadZipFallback } from '../api/client'
import { readColumnPreference, writeColumnPreference } from '../utils/columnPreference'
import { distributeMasonry } from '../utils/masonry'

const columnOptions = [1, 2, 3, 4, 5, 6]
const WALL_COLUMNS_KEY = 'wall_columns'
const DEFAULT_COLUMNS = 3

function nextTimestampSeed(currentSeed?: string): string {
  const now = Date.now()
  const parsedCurrent = Number.parseInt(currentSeed ?? '', 10)
  if (Number.isFinite(parsedCurrent) && parsedCurrent >= now) {
    return String(parsedCurrent + 1)
  }
  return String(now)
}

export function WallPage() {
  const [items, setItems] = useState<FeedItem[]>([])
  const [loading, setLoading] = useState(false)
  const [columns, setColumns] = useState(() =>
    readColumnPreference(WALL_COLUMNS_KEY, columnOptions, DEFAULT_COLUMNS),
  )
  const [error, setError] = useState<string | null>(null)
  const [uploading, setUploading] = useState(false)
  const [isAtBottom, setIsAtBottom] = useState(false)

  const sentinelRef = useRef<HTMLDivElement | null>(null)
  const fileInputRef = useRef<HTMLInputElement | null>(null)
  const loadingRef = useRef(false)
  const navigate = useNavigate()
  const [searchParams, setSearchParams] = useSearchParams()
  const seed = searchParams.get('seed') ?? ''

  const masonryColumns = useMemo(
    () =>
      distributeMasonry(
        items.map((item, idx) => ({ item, idx })),
        columns,
        ({ item }) => item.h / Math.max(item.w, 1),
      ),
    [columns, items],
  )

  const loadFeed = useCallback(async (seedValue: string) => {
    if (loadingRef.current) return
    loadingRef.current = true
    setLoading(true)
    setError(null)
    try {
      const data = await fetchFeed({ seed: seedValue })
      setItems(data.items)
    } catch (err) {
      setError((err as Error).message)
    } finally {
      loadingRef.current = false
      setLoading(false)
    }
  }, [])

  useEffect(() => {
    if (!seed) {
      const nextSeed = nextTimestampSeed()
      setSearchParams((prev) => {
        const next = new URLSearchParams(prev)
        next.set('seed', nextSeed)
        return next
      }, { replace: true })
      return
    }
    void loadFeed(seed)
  }, [loadFeed, seed, setSearchParams])

  useEffect(() => {
    const target = sentinelRef.current
    if (!target) return

    const observer = new IntersectionObserver((entries) => {
      for (const entry of entries) {
        if (entry.target === target) setIsAtBottom(entry.isIntersecting)
      }
    })

    observer.observe(target)
    return () => observer.disconnect()
  }, [])

  const onRefresh = () => {
    const nextSeed = nextTimestampSeed(seed)
    setSearchParams((prev) => {
      const next = new URLSearchParams(prev)
      next.set('seed', nextSeed)
      return next
    })
  }

  const onPickFile = async (event: React.ChangeEvent<HTMLInputElement>) => {
    const file = event.target.files?.[0]
    if (!file) return

    setUploading(true)
    setError(null)
    try {
      const created = await createAlbum(file)
      try {
        await uploadZip(created.upload.url, file, created.upload.headers)
      } catch {
        await uploadZipFallback(created.albumId, file)
      }
      await finalizeAlbum(created.albumId)

      if (seed) {
        await loadFeed(seed)
      } else {
        const nextSeed = nextTimestampSeed()
        setSearchParams((prev) => {
          const next = new URLSearchParams(prev)
          next.set('seed', nextSeed)
          return next
        }, { replace: true })
      }
    } catch (err) {
      setError((err as Error).message)
    } finally {
      setUploading(false)
      if (event.target) event.target.value = ''
    }
  }

  return (
    <div className="wall-page">
      <div className="wall-grid" data-testid="wall-grid">
        {masonryColumns.map((columnItems, columnIndex) => (
          <div className="masonry-column" data-testid="masonry-column" key={columnIndex}>
            {columnItems.map(({ item, idx }) => (
              <button
                className="tile"
                key={`${item.albumId}-${item.i}-${idx}`}
                onClick={() => navigate(`/album/${item.albumId}`)}
                data-testid="wall-tile"
              >
                <img
                  src={item.src}
                  alt=""
                  loading="lazy"
                  style={{ aspectRatio: `${item.w} / ${item.h}` }}
                />
              </button>
            ))}
          </div>
        ))}
      </div>

      <div ref={sentinelRef} className="sentinel" />

      <div className="bottom-bar">
        <div className="columns">
          {columnOptions.map((option) => (
            <button
              key={option}
              className={option === columns ? 'active' : ''}
              onClick={() => {
                setColumns(option)
                writeColumnPreference(WALL_COLUMNS_KEY, option)
              }}
              data-testid={`columns-${option}`}
            >
              {option}
            </button>
          ))}
        </div>
        {isAtBottom && (
          <button
            className="upload wall-refresh"
            onClick={onRefresh}
            disabled={loading || uploading}
            data-testid="wall-refresh"
          >
            Refresh
          </button>
        )}
        <button className="upload" onClick={() => fileInputRef.current?.click()} disabled={uploading} data-testid="upload-button">
          {uploading ? 'Uploading...' : '+'}
        </button>
        <input
          ref={fileInputRef}
          type="file"
          accept=".zip"
          hidden
          onChange={onPickFile}
          data-testid="upload-input"
        />
      </div>

      {loading && <div className="status">Loading...</div>}
      {error && <div className="status error">{error}</div>}
    </div>
  )
}
