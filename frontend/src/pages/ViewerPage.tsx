import { useEffect, useMemo, useRef, useState } from 'react'
import { useNavigate, useParams, useSearchParams } from 'react-router-dom'
import { AlbumIndex, fetchAlbum } from '../api/client'

type TouchPoint = {
  x: number
  y: number
  t: number
}

export function ViewerPage() {
  const { albumId = '' } = useParams()
  const [params] = useSearchParams()
  const initialIndex = Number(params.get('i') ?? '0')

  const [album, setAlbum] = useState<AlbumIndex | null>(null)
  const [index, setIndex] = useState(Number.isFinite(initialIndex) ? initialIndex : 0)
  const [showOverlay, setShowOverlay] = useState(true)
  const [error, setError] = useState<string | null>(null)

  const startRef = useRef<TouchPoint | null>(null)
  const navigate = useNavigate()

  useEffect(() => {
    let cancelled = false
    void (async () => {
      try {
        const fetched = await fetchAlbum(albumId)
        if (!cancelled) {
          setAlbum(fetched)
          if (index < 0 || index >= fetched.photoCount) {
            setIndex(0)
          }
        }
      } catch (err) {
        if (!cancelled) setError((err as Error).message)
      }
    })()
    return () => {
      cancelled = true
    }
  }, [albumId])

  const src = useMemo(() => {
    if (!album) return ''
    return `/api/image/${album.albumId}/${index}?mode=viewer&max=0`
  }, [album, index])

  const onTouchStart = (ev: React.TouchEvent<HTMLDivElement>) => {
    const touch = ev.changedTouches[0]
    startRef.current = { x: touch.clientX, y: touch.clientY, t: Date.now() }
  }

  const onTouchEnd = (ev: React.TouchEvent<HTMLDivElement>) => {
    const start = startRef.current
    if (!start || !album) return

    const touch = ev.changedTouches[0]
    const dx = touch.clientX - start.x
    const dy = touch.clientY - start.y
    const dt = Date.now() - start.t

    if (dt > 600) return

    if (Math.abs(dx) > Math.abs(dy) && Math.abs(dx) > 40) {
      if (dx < 0 && index < album.photoCount - 1) setIndex((v) => v + 1)
      if (dx > 0 && index > 0) setIndex((v) => v - 1)
      return
    }

    if (dy > 70 && Math.abs(dy) > Math.abs(dx)) {
      navigate(-1)
    }
  }

  const onClickViewer = () => setShowOverlay((v) => !v)

  if (error) {
    return <div className="viewer-error">{error}</div>
  }
  if (!album) {
    return <div className="viewer-loading">Loading album...</div>
  }

  return (
    <div className="viewer" onTouchStart={onTouchStart} onTouchEnd={onTouchEnd} onClick={onClickViewer}>
      {showOverlay && (
        <div className="viewer-overlay top" data-testid="viewer-overlay">
          <button onClick={() => navigate(-1)} data-testid="viewer-close">Close</button>
          <span>{index + 1} / {album.photoCount}</span>
        </div>
      )}

      <img src={src} alt="" className="viewer-image" data-testid="viewer-image" />

      {showOverlay && (
        <div className="viewer-overlay bottom">
          <div className="progress">
            <div className="progress-bar" style={{ width: `${((index + 1) / album.photoCount) * 100}%` }} />
          </div>
        </div>
      )}
    </div>
  )
}
