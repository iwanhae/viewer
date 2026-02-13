import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { createAlbum, fetchFeed, FeedItem, finalizeAlbum, uploadZip, uploadZipFallback } from '../api/client'

const columnOptions = [1, 2, 3, 4]

export function WallPage() {
  const [items, setItems] = useState<FeedItem[]>([])
  const [cursor, setCursor] = useState<string | undefined>(undefined)
  const [loading, setLoading] = useState(false)
  const [hasMore, setHasMore] = useState(true)
  const [columns, setColumns] = useState(3)
  const [error, setError] = useState<string | null>(null)
  const [uploading, setUploading] = useState(false)

  const sentinelRef = useRef<HTMLDivElement | null>(null)
  const fileInputRef = useRef<HTMLInputElement | null>(null)
  const navigate = useNavigate()

  const columnStyle = useMemo(() => ({ columnCount: columns }), [columns])

  const loadMore = useCallback(async () => {
    if (loading || !hasMore) return
    setLoading(true)
    setError(null)
    try {
      const data = await fetchFeed(cursor)
      setItems((prev) => [...prev, ...data.items])
      setCursor(data.nextCursor)
      setHasMore(Boolean(data.nextCursor))
    } catch (err) {
      setError((err as Error).message)
    } finally {
      setLoading(false)
    }
  }, [cursor, loading, hasMore])

  useEffect(() => {
    void loadMore()
  }, [])

  useEffect(() => {
    const target = sentinelRef.current
    if (!target) return

    const observer = new IntersectionObserver((entries) => {
      for (const entry of entries) {
        if (entry.isIntersecting) {
          void loadMore()
        }
      }
    }, { rootMargin: '1200px' })

    observer.observe(target)
    return () => observer.disconnect()
  }, [loadMore])

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

      const data = await fetchFeed()
      setItems(data.items)
      setCursor(data.nextCursor)
      setHasMore(Boolean(data.nextCursor))
    } catch (err) {
      setError((err as Error).message)
    } finally {
      setUploading(false)
      if (event.target) event.target.value = ''
    }
  }

  return (
    <div className="wall-page">
      <div className="wall-grid" style={columnStyle} data-testid="wall-grid">
        {items.map((item, idx) => (
          <button
            className="tile"
            key={`${item.albumId}-${item.i}-${idx}`}
            onClick={() => navigate(`/album/${item.albumId}?i=${item.i}`)}
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

      <div ref={sentinelRef} className="sentinel" />

      <div className="bottom-bar">
        <div className="columns">
          {columnOptions.map((option) => (
            <button
              key={option}
              className={option === columns ? 'active' : ''}
              onClick={() => setColumns(option)}
              data-testid={`columns-${option}`}
            >
              {option}
            </button>
          ))}
        </div>
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
