import { useEffect, useMemo, useRef, useState } from 'react'
import { useNavigate, useSearchParams } from 'react-router-dom'
import { useFeed } from '../hooks/useFeed'
import { readColumnPreference, writeColumnPreference } from '../utils/columnPreference'
import { writeLastWallSeed } from '../utils/wallSeed'
import { MasonryWall } from '../components/MasonryWall'
import { BottomIsland } from '../components/BottomIsland'
import { ColumnsIcon, RefreshIcon, ShortcutIcon } from '../components/IslandIcons'

const columnOptions = [1, 2, 3, 4, 5, 6]
const WALL_COLUMNS_KEY = 'wall_columns'
const DEFAULT_COLUMNS = 3
const WALL_FEED_LIMIT = 40
const ALBUM_PAGE_SIZE = 50

function nextTimestampSeed(currentSeed?: string): string {
  const now = Date.now()
  const parsedCurrent = Number.parseInt(currentSeed ?? '', 10)
  if (Number.isFinite(parsedCurrent) && parsedCurrent >= now) {
    return String(parsedCurrent + 1)
  }
  return String(now)
}

function pageForPhotoIndex(index: number): number {
  return Math.floor(index / ALBUM_PAGE_SIZE) + 1
}

function wallFocusKey(albumId: string, photoIndex: number): string {
  return `${albumId}:${photoIndex}`
}

export function WallPage() {
  const [columns, setColumns] = useState(() =>
    readColumnPreference(WALL_COLUMNS_KEY, columnOptions, DEFAULT_COLUMNS),
  )

  const navigate = useNavigate()
  const [searchParams, setSearchParams] = useSearchParams()
  const seed = searchParams.get('seed') ?? ''
  const focus = searchParams.get('focus')

  const { items, loading, error } = useFeed(seed)
  const visibleItems = useMemo(() => items.slice(0, WALL_FEED_LIMIT), [items])
  const tileRefs = useRef(new Map<string, HTMLButtonElement>())

  useEffect(() => {
    if (seed) return
    const nextSeed = nextTimestampSeed()
    setSearchParams(
      (prev) => {
        const next = new URLSearchParams(prev)
        next.set('seed', nextSeed)
        return next
      },
      { replace: true },
    )
  }, [seed, setSearchParams])

  useEffect(() => {
    if (!seed) return
    writeLastWallSeed(seed)
  }, [seed])

  useEffect(() => {
    if (!focus || loading || visibleItems.length === 0) return
    const target = tileRefs.current.get(focus)
    if (!target) {
      setSearchParams(
        (prev) => {
          const next = new URLSearchParams(prev)
          next.delete('focus')
          return next
        },
        { replace: true },
      )
      return
    }

    requestAnimationFrame(() => {
      target.scrollIntoView({ block: 'center', inline: 'nearest' })
    })

    setSearchParams(
      (prev) => {
        const next = new URLSearchParams(prev)
        next.delete('focus')
        return next
      },
      { replace: true },
    )
  }, [focus, loading, visibleItems.length, setSearchParams])

  const onRefresh = () => {
    if (typeof window !== 'undefined') {
      window.scrollTo({ top: 0, behavior: 'auto' })
    }
    const nextSeed = nextTimestampSeed(seed)
    setSearchParams((prev) => {
      const next = new URLSearchParams(prev)
      next.set('seed', nextSeed)
      next.delete('focus')
      return next
    })
  }

  return (
    <div className="wall-page">
      <MasonryWall
        items={visibleItems}
        columnCount={columns}
        getItemWeight={(item) => item.h / Math.max(item.w, 1)}
        renderItem={(item) => {
          const focusKey = wallFocusKey(item.albumId, item.i)
          return (
            <button
              className="tile"
              onClick={() => navigate(`/album/${item.albumId}?i=${item.i}&p=${pageForPhotoIndex(item.i)}`)}
              data-testid="wall-tile"
              ref={(node) => {
                if (node) {
                  tileRefs.current.set(focusKey, node)
                } else {
                  tileRefs.current.delete(focusKey)
                }
              }}
            >
              <img
                src={`/api/image/${item.albumId}/${item.i}`}
                alt=""
                loading="lazy"
                style={{ aspectRatio: `${item.w} / ${item.h}` }}
              />
            </button>
          )
        }}
        getItemKey={(item, idx) => `${item.albumId}-${item.i}-${idx}`}
        containerClassName=""
        columnClassName=""
        containerTestId="wall-grid"
        columnTestId="masonry-column"
      />

      <BottomIsland
        actions={[
          {
            id: 'wall-columns',
            icon: <ColumnsIcon />,
            ariaLabel: 'Change columns',
            tooltip: 'Columns',
            testId: 'wall-columns',
            renderPopup: ({ close }) => (
              <div className="bottom-island-popup-grid" data-testid="wall-columns-popup">
                {columnOptions.map((option) => (
                  <button
                    type="button"
                    key={option}
                    className={`bottom-island-popup-option ${option === columns ? 'active' : ''}`.trim()}
                    data-testid={`columns-${option}`}
                    onClick={() => {
                      setColumns(option)
                      writeColumnPreference(WALL_COLUMNS_KEY, option)
                      close()
                    }}
                  >
                    {option}
                  </button>
                ))}
              </div>
            ),
          },
          {
            id: 'wall-shortcut',
            icon: <ShortcutIcon />,
            ariaLabel: 'Open shortcuts',
            tooltip: 'Shortcut',
            testId: 'wall-shortcut',
            renderPopup: ({ close }) => (
              <div className="bottom-island-popup-stack" data-testid="wall-shortcut-popup">
                <button
                  type="button"
                  className="bottom-island-popup-option"
                  data-testid="wall-find"
                  onClick={() => {
                    close()
                    navigate('/albums/find')
                  }}
                >
                  Find albums
                </button>
                <button
                  type="button"
                  className="bottom-island-popup-option"
                  data-testid="wall-upload"
                  onClick={() => {
                    close()
                    navigate('/upload')
                  }}
                >
                  Upload
                </button>
              </div>
            ),
          },
          {
            id: 'wall-refresh',
            icon: <RefreshIcon />,
            ariaLabel: 'Refresh wall',
            tooltip: 'Refresh',
            testId: 'wall-refresh',
            onClick: onRefresh,
            disabled: loading,
          },
        ]}
      />

      {loading && <div className="status">Loading...</div>}
      {error && <div className="status error">{error}</div>}
    </div>
  )
}
