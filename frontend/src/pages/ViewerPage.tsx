import { useEffect, useMemo, useRef, useState } from 'react'
import { useNavigate, useParams, useSearchParams } from 'react-router-dom'
import { useAlbum } from '../hooks/useAlbum'
import { readColumnPreference, writeColumnPreference } from '../utils/columnPreference'
import { distributeMasonry } from '../utils/masonry'
import { readLastWallSeed } from '../utils/wallSeed'

const COLUMN_OPTIONS = [1, 2, 3, 4, 5, 6]
const VIEWER_COLUMNS_KEY = 'viewer_columns'
const DEFAULT_COLUMNS = 3

export function ViewerPage() {
  const { albumId = '' } = useParams()
  const [params] = useSearchParams()
  const initialIndexValue = Number(params.get('i') ?? '0')
  const initialIndex = Number.isFinite(initialIndexValue) ? initialIndexValue : 0

  const { album, loading, error } = useAlbum(albumId)
  const [columnCount, setColumnCount] = useState(() =>
    readColumnPreference(VIEWER_COLUMNS_KEY, COLUMN_OPTIONS, DEFAULT_COLUMNS),
  )

  const anchorRef = useRef<HTMLButtonElement | null>(null)
  const anchoredRef = useRef(false)
  const navigate = useNavigate()

  useEffect(() => {
    anchoredRef.current = false
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

  const masonryColumns = useMemo(
    () => distributeMasonry(album?.photos ?? [], columnCount, (photo) => photo.h / Math.max(photo.w, 1)),
    [album?.photos, columnCount],
  )

  const onBackToWall = () => {
    const seed = readLastWallSeed()
    if (!seed) {
      navigate('/')
      return
    }

    const query = new URLSearchParams()
    query.set('seed', seed)
    navigate(`/?${query.toString()}`)
  }

  if (error) {
    return <div className="viewer-error">{error}</div>
  }
  if (loading && !album) {
    return <div className="viewer-loading">Loading album...</div>
  }
  if (!album) {
    return <div className="viewer-loading">Loading album...</div>
  }

  return (
    <div className="album-page">
      <div className="album-grid wall-grid" data-testid="album-grid">
        {masonryColumns.map((columnPhotos, columnIndex) => (
          <div className="masonry-column" data-testid="masonry-column" key={columnIndex}>
            {columnPhotos.map((photo) => (
              <button
                key={photo.i}
                className="tile album-photo-tile"
                data-testid="album-tile"
                ref={photo.i === anchorIndex ? anchorRef : null}
                onClick={() => navigate(`/album/${album.albumId}/${photo.i}`, { state: { album } })}
                aria-label={`Open details for image ${photo.i + 1}`}
              >
                <img
                  src={`/api/image/${album.albumId}/${photo.i}?mode=wall&w=${imageWidth}`}
                  alt=""
                  loading="lazy"
                  style={{ aspectRatio: `${photo.w} / ${photo.h}` }}
                />
              </button>
            ))}
          </div>
        ))}
      </div>

      <div className="bottom-bar">
        <div className="columns">
          {COLUMN_OPTIONS.map((value) => (
            <button
              key={value}
              className={value === columnCount ? 'active' : ''}
              onClick={() => {
                setColumnCount(value)
                writeColumnPreference(VIEWER_COLUMNS_KEY, value)
              }}
              data-testid={`album-columns-${value}`}
            >
              {value}
            </button>
          ))}
        </div>
        <button className="upload album-back" onClick={onBackToWall} data-testid="album-back">
          Back
        </button>
      </div>
    </div>
  )
}
