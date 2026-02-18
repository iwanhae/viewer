import { useEffect, useMemo, useState } from 'react'
import { useLocation, useNavigate, useParams } from 'react-router-dom'
import { AlbumIndex, RecommendationItem, RecommendationStatus, fetchAlbum, fetchRecommendations, getCachedAlbum, seedCachedAlbum } from '../api/client'

export function PhotoPage() {
  const { albumId = '', photoIndex: rawPhotoIndex = '' } = useParams<{ albumId: string; photoIndex: string }>()
  const navigate = useNavigate()
  const location = useLocation()

  const photoIndex = useMemo(() => {
    if (!rawPhotoIndex) return null
    const parsed = Number(rawPhotoIndex)
    if (!Number.isInteger(parsed) || parsed < 0) return null
    return parsed
  }, [rawPhotoIndex])

  const [album, setAlbum] = useState<AlbumIndex | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [recommendations, setRecommendations] = useState<RecommendationItem[]>([])
  const [recommendationStatus, setRecommendationStatus] = useState<RecommendationStatus | 'idle' | 'loading'>('idle')
  const [recommendationError, setRecommendationError] = useState<string | null>(null)
  const cachedAlbum = useMemo(() => getCachedAlbum(albumId), [albumId])
  const locationAlbum = useMemo(() => {
    const state = location.state as { album?: AlbumIndex } | null
    const candidate = state?.album
    if (!candidate || candidate.albumId !== albumId) {
      return null
    }
    seedCachedAlbum(candidate)
    return candidate
  }, [location.state, albumId])

  useEffect(() => {
    const seeded = locationAlbum ?? cachedAlbum
    setAlbum(seeded)
    setError(null)

    if (!albumId) {
      setError('Missing album ID')
      return
    }
    if (photoIndex === null) {
      setError('Invalid photo index')
      return
    }

    if (seeded) {
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
  }, [albumId, photoIndex, locationAlbum, cachedAlbum])

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

  useEffect(() => {
    if (!album || !photo) {
      setRecommendations([])
      setRecommendationStatus('idle')
      setRecommendationError(null)
      return
    }
    let cancelled = false
    setRecommendationStatus('loading')
    setRecommendationError(null)
    void (async () => {
      try {
        const result = await fetchRecommendations(album.albumId, photo.i, 12)
        if (cancelled) return
        setRecommendations(Array.isArray(result.items) ? result.items : [])
        setRecommendationStatus(result.status)
      } catch (err) {
        if (cancelled) return
        setRecommendations([])
        setRecommendationStatus('idle')
        setRecommendationError((err as Error).message)
      }
    })()
    return () => {
      cancelled = true
    }
  }, [album, photo])

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
          <section className="photo-recommendations" data-testid="photo-recommendations">
            <h2 className="photo-recommendations-title">Similar images</h2>
            {recommendationStatus === 'loading' && <p className="photo-recommendations-note">Finding similar images...</p>}
            {recommendationStatus === 'pending' && (
              <p className="photo-recommendations-note">Recommendations are being prepared in background.</p>
            )}
            {recommendationStatus === 'failed' && (
              <p className="photo-recommendations-note">Embedding failed for this photo. Skipping recommendations.</p>
            )}
            {recommendationError && <p className="photo-recommendations-note">Recommendations unavailable right now.</p>}
            {(recommendationStatus === 'ready' || recommendationStatus === 'partial') && recommendations.length === 0 && (
              <p className="photo-recommendations-note">No similar images found yet.</p>
            )}
            {recommendations.length > 0 && (
              <div className="photo-recommendation-grid">
                {recommendations.map((item) => (
                  <button
                    key={`${item.albumId}-${item.i}`}
                    className="photo-recommendation-tile"
                    onClick={() => navigate(`/photo/${item.albumId}/${item.i}`)}
                    aria-label={`Open similar image ${item.i + 1}`}
                    data-testid="photo-recommendation-tile"
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
            )}
          </section>
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
