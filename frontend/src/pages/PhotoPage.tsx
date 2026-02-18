import { useEffect, useMemo, useState } from 'react'
import { useLocation, useNavigate, useParams } from 'react-router-dom'
import { seedCachedAlbum, type AlbumIndex, type RecommendationItem } from '../api/client'
import { useAlbum } from '../hooks/useAlbum'
import { useRecommendations } from '../hooks/useRecommendations'
import { MasonryWall } from '../components/MasonryWall'

type RecommendationGridItem =
  | { kind: 'photo'; recommendation: RecommendationItem }
  | { kind: 'load-more' }

export function PhotoPage() {
  const recommendationPageSize = 12
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
  const [recommendationLimit, setRecommendationLimit] = useState(recommendationPageSize)
  const [displayedRecommendations, setDisplayedRecommendations] = useState<RecommendationItem[]>([])
  const [hasMoreRecommendations, setHasMoreRecommendations] = useState(true)

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
    status: recommendationStatus,
    error: recommendationError,
  } = useRecommendations(album?.albumId ?? '', photo?.i ?? null, recommendationLimit)

  useEffect(() => {
    setRecommendationLimit(recommendationPageSize)
    setDisplayedRecommendations([])
    setHasMoreRecommendations(true)
  }, [album?.albumId, photo?.i])

  useEffect(() => {
    if (recommendations.length === 0) return
    setDisplayedRecommendations((current) => {
      if (current.length === 0) return recommendations
      const existingKeys = new Set(current.map((item) => `${item.albumId}-${item.i}`))
      const appended = recommendations.filter(
        (item) => !existingKeys.has(`${item.albumId}-${item.i}`),
      )
      if (appended.length === 0) return current
      return [...current, ...appended]
    })
  }, [recommendations])

  useEffect(() => {
    if (recommendationStatus !== 'ready' && recommendationStatus !== 'partial') return
    setHasMoreRecommendations(recommendations.length >= recommendationLimit)
  }, [recommendations.length, recommendationLimit, recommendationStatus])

  const isRecommendationLoading = recommendationStatus === 'loading'
  const canShowLoadMore =
    displayedRecommendations.length > 0 && hasMoreRecommendations && recommendationError === null

  const recommendationGridItems = useMemo<RecommendationGridItem[]>(() => {
    const photoItems: RecommendationGridItem[] = displayedRecommendations.map((item) => ({
      kind: 'photo',
      recommendation: item,
    }))
    if (canShowLoadMore) {
      photoItems.push({ kind: 'load-more' })
    }
    return photoItems
  }, [canShowLoadMore, displayedRecommendations])

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
                {displayedRecommendations.length} shown
                {isRecommendationLoading ? '  Loading more...' : ''}
              </p>
            </div>
            {recommendationStatus === 'loading' && (
              <p className="photo-recommendations-note">Finding similar images...</p>
            )}
            {recommendationStatus === 'pending' && (
              <p className="photo-recommendations-note">
                Recommendations are being prepared in background.
              </p>
            )}
            {recommendationStatus === 'failed' && (
              <p className="photo-recommendations-note">
                Embedding failed for this photo. Skipping recommendations.
              </p>
            )}
            {recommendationError && (
              <p className="photo-recommendations-note">Recommendations unavailable right now.</p>
            )}
            {(recommendationStatus === 'ready' || recommendationStatus === 'partial') &&
              displayedRecommendations.length === 0 && (
                <p className="photo-recommendations-note">No similar images found yet.</p>
              )}
            {recommendationGridItems.length > 0 && (
              <MasonryWall
                items={recommendationGridItems}
                columnCount={recommendationColumnCount}
                getItemWeight={(item) =>
                  item.kind === 'photo'
                    ? item.recommendation.h / Math.max(item.recommendation.w, 1)
                    : 0.32
                }
                renderItem={(item) => (
                  item.kind === 'photo' ? (
                    <button
                      className="photo-recommendation-tile"
                      onClick={() =>
                        navigate(`/album/${item.recommendation.albumId}/${item.recommendation.i}`)
                      }
                      aria-label={`Open similar image ${item.recommendation.i + 1}`}
                      data-testid="photo-recommendation-tile"
                    >
                      <img
                        src={item.recommendation.src}
                        alt=""
                        loading="lazy"
                        style={{
                          aspectRatio: `${item.recommendation.w} / ${item.recommendation.h}`,
                        }}
                      />
                    </button>
                  ) : (
                    <button
                      className="photo-recommendation-load-more-tile"
                      type="button"
                      onClick={() =>
                        setRecommendationLimit((current) => current + recommendationPageSize)
                      }
                      disabled={isRecommendationLoading}
                      data-testid="photo-recommendations-load-more"
                    >
                      <span className="photo-recommendation-load-more-title">
                        {isRecommendationLoading
                          ? 'Loading more...'
                          : 'Load more similar images'}
                      </span>
                      <span className="photo-recommendation-load-more-subtitle">
                        {isRecommendationLoading
                          ? 'Searching for more albums'
                          : `Show ${recommendationPageSize} more`}
                      </span>
                    </button>
                  )
                )}
                getItemKey={(item) =>
                  item.kind === 'photo'
                    ? `${item.recommendation.albumId}-${item.recommendation.i}`
                    : 'load-more'
                }
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
