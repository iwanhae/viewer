import { useEffect, useMemo, useRef, useState } from 'react'
import { useNavigate, useParams, useSearchParams } from 'react-router-dom'
import { useAlbum } from '../hooks/useAlbum'
import { readColumnPreference, writeColumnPreference } from '../utils/columnPreference'
import { readLastWallSeed, readLastWallState } from '../utils/wallSeed'
import { MasonryWall } from '../components/MasonryWall'
import { BottomIsland } from '../components/BottomIsland'
import { BackToAlbumIcon, ColumnsIcon, NextIcon, PrevIcon } from '../components/IslandIcons'

const COLUMN_OPTIONS = [1, 2, 3, 4, 5, 6]
const VIEWER_COLUMNS_KEY = 'viewer_columns'
const DEFAULT_COLUMNS = 3
const ALBUM_PAGE_SIZE = 50

function parsePositiveInt(value: string | null, fallback: number): number {
  const parsed = Number(value)
  if (!Number.isInteger(parsed) || parsed < 1) return fallback
  return parsed
}

function parseNonNegativeInt(value: string | null): number | null {
  if (value === null) return null
  const parsed = Number(value)
  if (!Number.isInteger(parsed) || parsed < 0) return null
  return parsed
}

function pageForPhotoIndex(index: number): number {
  return Math.floor(index / ALBUM_PAGE_SIZE) + 1
}

function wallFocusKey(albumId: string, photoIndex: number): string {
  return `${albumId}:${photoIndex}`
}

export function ViewerPage() {
  const { albumId = '' } = useParams()
  const [params, setParams] = useSearchParams()
  const requestedPage = parsePositiveInt(params.get('p'), 1)
  const requestedIndex = parseNonNegativeInt(params.get('i'))

  const { album, loading, error } = useAlbum(albumId)
  const [columnCount, setColumnCount] = useState(() =>
    readColumnPreference(VIEWER_COLUMNS_KEY, COLUMN_OPTIONS, DEFAULT_COLUMNS),
  )

  const anchorRef = useRef<HTMLButtonElement | null>(null)
  const anchoredRef = useRef(false)
  const navigate = useNavigate()

  useEffect(() => {
    anchoredRef.current = false
  }, [albumId, requestedPage, requestedIndex])

  const totalPages = useMemo(() => {
    if (!album) return 1
    return Math.max(1, Math.ceil(album.photoCount / ALBUM_PAGE_SIZE))
  }, [album])

  const normalizedPage = useMemo(
    () => Math.min(Math.max(requestedPage, 1), totalPages),
    [requestedPage, totalPages],
  )

  const anchorIndex = useMemo(() => {
    if (!album || requestedIndex === null) return null
    if (requestedIndex >= album.photoCount) return 0
    return requestedIndex
  }, [album, requestedIndex])

  const anchorPage = useMemo(
    () => (anchorIndex === null ? null : pageForPhotoIndex(anchorIndex)),
    [anchorIndex],
  )

  useEffect(() => {
    if (requestedPage === normalizedPage) return
    setParams(
      (prev) => {
        const next = new URLSearchParams(prev)
        next.set('p', String(normalizedPage))
        return next
      },
      { replace: true },
    )
  }, [requestedPage, normalizedPage, setParams])

  useEffect(() => {
    if (!album || anchorPage === null) return
    if (normalizedPage === anchorPage) return
    setParams(
      (prev) => {
        const next = new URLSearchParams(prev)
        next.set('p', String(anchorPage))
        return next
      },
      { replace: true },
    )
  }, [album, anchorPage, normalizedPage, setParams])

  const pageStart = useMemo(() => (normalizedPage - 1) * ALBUM_PAGE_SIZE, [normalizedPage])

  const anchorVisibleIndex = useMemo(() => {
    if (anchorIndex === null) return null
    if (anchorIndex < pageStart || anchorIndex >= pageStart + ALBUM_PAGE_SIZE) return null
    return anchorIndex
  }, [anchorIndex, pageStart])

  useEffect(() => {
    if (!album || anchoredRef.current || anchorVisibleIndex === null) return
    const target = anchorRef.current
    if (!target) return

    requestAnimationFrame(() => {
      target.scrollIntoView({ block: 'nearest', inline: 'nearest' })
      anchoredRef.current = true
    })
  }, [album, anchorVisibleIndex])

  const albumPhotos = useMemo(() => {
    if (!album) return []
    return album.photos.slice(pageStart, pageStart + ALBUM_PAGE_SIZE)
  }, [album, pageStart])

  const onBackToWall = () => {
    const focusIndex = anchorIndex
    const lastState = readLastWallState()
    const seed = readLastWallSeed()

    const query = new URLSearchParams()
    if (lastState?.mode === 'latest') {
      query.set('mode', 'latest')
      if (typeof lastState.latestPage === 'number' && Number.isInteger(lastState.latestPage) && lastState.latestPage >= 1) {
        query.set('lp', String(lastState.latestPage))
      }
      if (lastState.latestCursor) {
        query.set('lc', lastState.latestCursor)
      }
    } else if (seed) {
      query.set('seed', seed)
    }
    if (focusIndex !== null) {
      query.set('focus', wallFocusKey(album.albumId, focusIndex))
    }
    const queryString = query.toString()
    navigate(queryString ? `/?${queryString}` : '/')
  }

  const onChangePage = (page: number) => {
    const clamped = Math.min(Math.max(page, 1), totalPages)
    setParams((prev) => {
      const next = new URLSearchParams(prev)
      next.set('p', String(clamped))
      next.delete('i')
      return next
    })

    if (typeof window !== 'undefined') {
      window.scrollTo({ top: 0, behavior: 'auto' })
    }
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
      <MasonryWall
        items={albumPhotos}
        columnCount={columnCount}
        getItemWeight={(photo) => photo.h / Math.max(photo.w, 1)}
        renderItem={(photo) => (
          <button
            className={`tile album-photo-tile ${photo.i === anchorVisibleIndex ? 'is-selected' : ''}`.trim()}
            data-testid="album-tile"
            data-selected={photo.i === anchorVisibleIndex ? 'true' : undefined}
            ref={photo.i === anchorVisibleIndex ? anchorRef : null}
            onClick={() => {
              if (typeof window !== 'undefined') {
                window.scrollTo({ top: 0, behavior: 'auto' })
              }
              navigate(`/album/${album.albumId}/${photo.i}`, { state: { album } })
            }}
            aria-label={`Open details for image ${photo.i + 1}`}
            aria-current={photo.i === anchorVisibleIndex ? 'true' : undefined}
          >
            <img
              src={`/api/image/${album.albumId}/${photo.i}`}
              alt=""
              loading="lazy"
              style={{ aspectRatio: `${photo.w} / ${photo.h}` }}
            />
          </button>
        )}
        getItemKey={(photo, idx) => `${album.albumId}-${photo.i}-${idx}`}
        containerClassName="album-grid"
        columnClassName=""
        containerTestId="album-grid"
        columnTestId="masonry-column"
      />

      <BottomIsland
        actions={[
          {
            id: 'album-page-prev',
            icon: <PrevIcon />,
            ariaLabel: 'Previous page',
            tooltip: 'Previous page',
            testId: 'album-page-prev',
            onClick: () => onChangePage(normalizedPage - 1),
            disabled: normalizedPage <= 1,
          },
          {
            id: 'album-columns',
            icon: <ColumnsIcon />,
            ariaLabel: 'Change columns',
            tooltip: 'Columns',
            testId: 'album-columns-toggle',
            renderPopup: ({ close }) => (
              <div className="bottom-island-popup-grid" data-testid="album-columns-popup">
                {COLUMN_OPTIONS.map((value) => (
                  <button
                    type="button"
                    key={value}
                    className={`bottom-island-popup-option ${value === columnCount ? 'active' : ''}`.trim()}
                    onClick={() => {
                      setColumnCount(value)
                      writeColumnPreference(VIEWER_COLUMNS_KEY, value)
                      close()
                    }}
                    data-testid={`album-columns-${value}`}
                  >
                    {value}
                  </button>
                ))}
              </div>
            ),
          },
          {
            kind: 'indicator',
            id: 'album-page-indicator',
            label: (
              <span>
                {normalizedPage} / {totalPages}
              </span>
            ),
            testId: 'album-page-indicator',
            ariaLabel: `Page ${normalizedPage} of ${totalPages}`,
          },
          {
            id: 'album-back',
            icon: <BackToAlbumIcon />,
            ariaLabel: 'Back to wall',
            tooltip: 'Back',
            testId: 'album-back',
            onClick: onBackToWall,
          },
          {
            id: 'album-page-next',
            icon: <NextIcon />,
            ariaLabel: 'Next page',
            tooltip: 'Next page',
            testId: 'album-page-next',
            onClick: () => onChangePage(normalizedPage + 1),
            disabled: normalizedPage >= totalPages,
          },
        ]}
      />
    </div>
  )
}
