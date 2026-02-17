import { useEffect, useMemo, useState } from 'react'
import { useNavigate, useParams } from 'react-router-dom'
import { AlbumIndex, fetchAlbum } from '../api/client'

export function PhotoPage() {
  const { albumId = '', photoIndex: rawPhotoIndex = '' } = useParams<{ albumId: string; photoIndex: string }>()
  const navigate = useNavigate()

  const photoIndex = useMemo(() => {
    if (!rawPhotoIndex) return null
    const parsed = Number(rawPhotoIndex)
    if (!Number.isInteger(parsed) || parsed < 0) return null
    return parsed
  }, [rawPhotoIndex])

  const [album, setAlbum] = useState<AlbumIndex | null>(null)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    setAlbum(null)
    setError(null)

    if (!albumId) {
      setError('Missing album ID')
      return
    }
    if (photoIndex === null) {
      setError('Invalid photo index')
      return
    }

    let cancelled = false
    void (async () => {
      try {
        const fetched = await fetchAlbum(albumId)
        if (!cancelled) {
          setAlbum(fetched)
        }
      } catch (err) {
        if (!cancelled) {
          setError((err as Error).message)
        }
      }
    })()
    return () => {
      cancelled = true
    }
  }, [albumId, photoIndex])

  const photo = useMemo(() => {
    if (!album || photoIndex === null) return null
    return album.photos.find((item) => item.i === photoIndex) ?? null
  }, [album, photoIndex])
  const createdAtLabel = useMemo(() => {
    if (!album) return ''
    const parsed = new Date(album.createdAt)
    if (Number.isNaN(parsed.getTime())) return album.createdAt
    return parsed.toLocaleString()
  }, [album])

  if (error) {
    return <div className="photo-error">{error}</div>
  }
  if (!album) {
    return <div className="photo-loading">Loading photo...</div>
  }
  if (!photo) {
    return <div className="photo-error">Photo not found</div>
  }

  return (
    <div className="photo-page" data-testid="photo-page">
      <div className="photo-shell">
        <img
          className="photo-view-image"
          src={`/api/image/${album.albumId}/${photo.i}`}
          alt={photo.name || `Photo ${photo.i + 1}`}
        />
        <aside className="photo-panel">
          <p className="photo-context">
            Album {album.albumId} / Photo {photo.i + 1}
          </p>
          <h1 className="photo-title">{photo.name || `Photo ${photo.i + 1}`}</h1>
          <dl className="photo-meta-list">
            <div className="photo-meta-item">
              <dt>Filename</dt>
              <dd>{photo.name || '-'}</dd>
            </div>
            <div className="photo-meta-item">
              <dt>Dimensions</dt>
              <dd>
                {photo.w} x {photo.h}
              </dd>
            </div>
            <div className="photo-meta-item">
              <dt>Aspect ratio</dt>
              <dd>{photo.ratio.toFixed(2)}</dd>
            </div>
            <div className="photo-meta-item">
              <dt>Created</dt>
              <dd>{createdAtLabel}</dd>
            </div>
            <div className="photo-meta-item">
              <dt>Album ID</dt>
              <dd>{album.albumId}</dd>
            </div>
            <div className="photo-meta-item">
              <dt>Zero-based index</dt>
              <dd>{photo.i}</dd>
            </div>
          </dl>
        </aside>
      </div>
      <div className="photo-bottom-actions">
        <button className="photo-nav-button" onClick={() => navigate(-1)} data-testid="photo-back">
          Back to album
        </button>
        <a
          className="photo-primary-action"
          href={`/api/image/${album.albumId}/${photo.i}`}
          target="_blank"
          rel="noopener noreferrer"
          data-testid="photo-open-original"
        >
          Open original
        </a>
      </div>
    </div>
  )
}
