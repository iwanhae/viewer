import { useMemo } from 'react'
import { useLocation, useNavigate, useParams } from 'react-router-dom'
import { seedCachedAlbum, type AlbumIndex } from '../api/client'
import { useAlbum } from '../hooks/useAlbum'
import { useRecommendations } from '../hooks/useRecommendations'
import { MasonryWall } from '../components/MasonryWall'

const recommendationLimit = 24

export function PhotoPage() {
  const { albumId = '', photoIndex: rawPhotoIndex = '' } = useParams<{
    albumId: string
    photoIndex: string
  }>()
  const navigate = useNavigate()
  const location = useLocation()

  const photoIndex = useMemo(() => {
    if (!rawPhotoIndex) return null
    const parsed = Number(rawPhotoIndex)
    if (!Number.isInteger(parsed) || parsed < 0) return null
    return parsed
  }, [rawPhotoIndex])

  const locationAlbum = useMemo(() => {
    const state = location.state as { album?: AlbumIndex } | null
    const candidate = state?.album
    if (!candidate || candidate.albumId !== albumId) {
      return null
    }
    seedCachedAlbum(candidate)
    return candidate
  }, [location.state, albumId])

  const { album, loading, error } = useAlbum(albumId, locationAlbum)
  const recommendationColumnCount = 3

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

  const {
    items: recommendations,
    status: recommendationLoadStatus,
    error: recommendationError,
  } = useRecommendations(album?.albumId ?? '', photo?.i ?? null, recommendationLimit)

  const isRecommendationLoading = recommendationLoadStatus === 'loading'

  if (!albumId) {
    return <div className="photo-error">Missing album ID</div>
  }
  if (photoIndex === null) {
    return <div className="photo-error">Invalid photo index</div>
  }
  if (error) {
    return <div className="photo-error">{error}</div>
  }
  if (loading && !album) {
    return <div className="photo-loading">Loading photo...</div>
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
            <div className="photo-recommendations-header">
              <h2 className="photo-recommendations-title">Similar images</h2>
              <p className="photo-recommendations-meta">
                {recommendations.length} shown
                {isRecommendationLoading ? '  Loading...' : ''}
              </p>
            </div>
            {recommendationLoadStatus === 'loading' && (
              <p className="photo-recommendations-note">Finding similar images...</p>
            )}
            {recommendationError && (
              <p className="photo-recommendations-note">Recommendations unavailable right now.</p>
            )}
            {recommendationLoadStatus === 'ready' && recommendations.length === 0 && (
              <p className="photo-recommendations-note">No similar images found yet.</p>
            )}
            {recommendations.length > 0 && (
              <MasonryWall
                items={recommendations}
                columnCount={recommendationColumnCount}
                getItemWeight={(item) => item.h / Math.max(item.w, 1)}
                renderItem={(item) => (
                  <button
                    className="photo-recommendation-tile"
                    onClick={() => navigate(`/album/${item.albumId}/${item.i}`)}
                    aria-label={`Open similar image ${item.i + 1}`}
                    data-testid="photo-recommendation-tile"
                  >
                    <img
                      src={`/api/image/${item.albumId}/${item.i}`}
                      alt=""
                      loading="lazy"
                      style={{
                        aspectRatio: `${item.w} / ${item.h}`,
                      }}
                    />
                  </button>
                )}
                getItemKey={(item) => `${item.albumId}-${item.i}`}
                containerClassName="photo-recommendation-grid"
                containerTestId="photo-recommendation-grid"
                columnTestId="photo-recommendation-column"
              />
            )}
          </section>
        </aside>
      </div>
      <div className="photo-bottom-actions">
        <button
          className="photo-nav-button"
          onClick={() => navigate(`/album/${album.albumId}`)}
          data-testid="photo-back"
        >
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
