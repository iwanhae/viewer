import { useEffect, useMemo, useRef, useState } from 'react'
import { useNavigate, useParams, useSearchParams } from 'react-router-dom'
import { AlbumIndex, fetchAlbum } from '../api/client'

const COLUMN_OPTIONS = [1, 2, 3, 4]

export function ViewerPage() {
  const { albumId = '' } = useParams()
  const [params] = useSearchParams()
  const initialIndexValue = Number(params.get('i') ?? '0')
  const initialIndex = Number.isFinite(initialIndexValue) ? initialIndexValue : 0

  const [album, setAlbum] = useState<AlbumIndex | null>(null)
  const [columnCount, setColumnCount] = useState(3)
  const [error, setError] = useState<string | null>(null)

  const anchorRef = useRef<HTMLDivElement | null>(null)
  const anchoredRef = useRef(false)
  const navigate = useNavigate()

  useEffect(() => {
    setAlbum(null)
    setError(null)
    anchoredRef.current = false

    let cancelled = false
    void (async () => {
      try {
        const fetched = await fetchAlbum(albumId)
        if (!cancelled) {
          setAlbum(fetched)
        }
      } catch (err) {
        if (!cancelled) setError((err as Error).message)
      }
    })()
    return () => {
      cancelled = true
    }
  }, [albumId])

  const anchorIndex = useMemo(() => {
    if (!album) return null
    if (initialIndex < 0 || initialIndex >= album.photoCount) return 0
    return initialIndex
  }, [album, initialIndex])

  useEffect(() => {
    if (!album || anchoredRef.current || anchorIndex === null) return
    const target = anchorRef.current
    if (!target) return

    requestAnimationFrame(() => {
      target.scrollIntoView({ block: 'nearest', inline: 'nearest' })
      anchoredRef.current = true
    })
  }, [album, anchorIndex])

  const imageWidth = useMemo(() => {
    if (typeof window === 'undefined') return 480

    const estimated = Math.floor(window.innerWidth / Math.max(1, columnCount))
    if (estimated < 64) return 64
    if (estimated > 2048) return 2048
    return estimated
  }, [columnCount])

  if (error) {
    return <div className="viewer-error">{error}</div>
  }
  if (!album) {
    return <div className="viewer-loading">Loading album...</div>
  }

  return (
    <div className="album-page">
      <div className="album-grid wall-grid" style={{ columnCount }} data-testid="album-grid">
        {album.photos.map((photo) => (
          <div
            key={photo.i}
            className="tile album-tile"
            data-testid="album-tile"
            ref={photo.i === anchorIndex ? anchorRef : null}
          >
            <img
              src={`/api/image/${album.albumId}/${photo.i}?mode=wall&w=${imageWidth}`}
              alt=""
              loading="lazy"
              style={{ aspectRatio: `${photo.w} / ${photo.h}` }}
            />
          </div>
        ))}
      </div>

      <div className="bottom-bar">
        <div className="columns">
          {COLUMN_OPTIONS.map((value) => (
            <button
              key={value}
              className={value === columnCount ? 'active' : ''}
              onClick={() => setColumnCount(value)}
              data-testid={`album-columns-${value}`}
            >
              {value}
            </button>
          ))}
        </div>
        <button className="upload album-back" onClick={() => navigate(-1)} data-testid="album-back">
          Back
        </button>
      </div>
    </div>
  )
}
