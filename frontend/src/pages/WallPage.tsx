import { useEffect, useMemo, useRef, useState } from 'react'
import { useNavigate, useSearchParams } from 'react-router-dom'
import { THUMBNAIL_WIDTH, imageByHashUrl, type FeedMode } from '../api/client'
import { useFeed } from '../hooks/useFeed'
import { COLUMN_OPTIONS, pageForPhotoIndex, wallFocusKey } from '../utils/albumPaging'
import { materializeWallParams, resolveWallParams } from '../utils/wallParams'
import {
  readLastWallState,
  readNumberPreference,
  writeLastWallSeed,
  writeLastWallState,
  writeNumberPreference,
} from '../utils/storage'
import { MasonryWall } from '../components/MasonryWall'
import { BottomIsland } from '../components/BottomIsland'
import { ColumnsIcon, ModeIcon, NextIcon, PrevIcon, RefreshIcon } from '../components/IslandIcons'
import { AlbumsIcon, SearchIcon, UploadIcon } from '../components/IslandIcons'

const WALL_COLUMNS_KEY = 'wall_columns'
const DEFAULT_COLUMNS = 3
const wallModes: FeedMode[] = ['random', 'latest']

function nextTimestampSeed(currentSeed?: string): string {
  const now = Date.now()
  const parsedCurrent = Number.parseInt(currentSeed ?? '', 10)
  if (Number.isFinite(parsedCurrent) && parsedCurrent >= now) {
    return String(parsedCurrent + 1)
  }
  return String(now)
}

export function WallPage() {
  const navigate = useNavigate()
  const [searchParams, setSearchParams] = useSearchParams()
  const [columns, setColumns] = useState(() =>
    readNumberPreference(WALL_COLUMNS_KEY, COLUMN_OPTIONS, DEFAULT_COLUMNS),
  )
  const lastWallState = useMemo(() => readLastWallState(), [])
  // One shuffle seed per page lifetime, used when the URL has no seed to offer.
  const mountSeed = useRef(nextTimestampSeed())

  // URL params resolve through one pure function: mode, seed, latest page and
  // cursor, including the localStorage restore for a bare "/" visit.
  const wall = useMemo(
    () => resolveWallParams(searchParams, lastWallState, mountSeed.current),
    [searchParams, lastWallState],
  )
  const { mode, seed, latestPage, latestCursor } = wall
  const focus = searchParams.get('focus')

  const { items, loading, error, pageInfo } = useFeed(
    mode === 'random' ? seed : '',
    mode,
    mode === 'latest' ? latestCursor : '',
  )
  const tileRefs = useRef(new Map<string, HTMLButtonElement>())

  // The single URL-hygiene effect: when the raw query does not spell out the
  // resolved params, write them once (replace) and leave navigation alone.
  // There is deliberately no effect syncing server responses back into the
  // URL - the requested cursor is the position, so there is nothing to sync,
  // and an echo-rewriting effect races the in-flight navigation it reacts to.
  useEffect(() => {
    if (!wall.dirty) return
    setSearchParams((prev) => materializeWallParams(prev, wall), { replace: true })
  }, [wall, setSearchParams])

  useEffect(() => {
    if (mode === 'random') {
      writeLastWallSeed(seed)
      writeLastWallState({ mode: 'random', seed })
      return
    }
    writeLastWallState({ mode: 'latest', latestPage, latestCursor })
  }, [latestCursor, latestPage, mode, seed])

  useEffect(() => {
    if (!focus || loading || items.length === 0) return
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
  }, [focus, loading, items.length, setSearchParams])

  const setWallMode = (nextMode: FeedMode) => {
    setSearchParams((prev) => {
      const next = new URLSearchParams(prev)
      next.delete('focus')

      if (nextMode === 'latest') {
        next.set('mode', 'latest')
        next.set('lp', '1')
        next.delete('lc')
        next.delete('seed')
        return next
      }

      next.set('mode', 'random')
      next.delete('lp')
      next.delete('lc')
      if (!next.get('seed')) {
        next.set('seed', nextTimestampSeed())
      }
      return next
    })
  }

  const changeLatestPage = (targetPage: number, cursor: string | null) => {
    if (mode !== 'latest' || loading) return

    const target = Math.max(1, targetPage)
    const targetCursor = cursor?.trim() ?? ''
    if (target === latestPage && targetCursor === latestCursor) return

    if (target > latestPage) {
      if (!pageInfo.hasNext || !targetCursor) return
    } else if (target > 1) {
      if (!pageInfo.hasPrev || !targetCursor) return
    }

    if (typeof window !== 'undefined') {
      window.scrollTo({ top: 0, behavior: 'auto' })
    }

    setSearchParams((prev) => {
      const next = new URLSearchParams(prev)
      next.set('mode', 'latest')
      next.set('lp', String(target))
      // The first page is cursor-less by definition.
      if (targetCursor && target > 1) {
        next.set('lc', targetCursor)
      } else {
        next.delete('lc')
      }
      next.delete('seed')
      next.delete('focus')
      return next
    })
  }

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
        items={items}
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
                src={imageByHashUrl(item.hash, THUMBNAIL_WIDTH)}
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
          ...(mode === 'latest'
            ? [
                {
                  id: 'wall-page-prev',
                  icon: <PrevIcon />,
                  ariaLabel: 'Previous latest page',
                  tooltip: 'Previous page',
                  testId: 'wall-page-prev',
                  onClick: () => changeLatestPage(latestPage - 1, pageInfo.prevCursor),
                  disabled: loading || !pageInfo.hasPrev,
                },
              ]
            : []),
          {
            id: 'wall-columns',
            icon: <ColumnsIcon />,
            ariaLabel: 'Change columns',
            tooltip: 'Columns',
            testId: 'wall-columns',
            renderPopup: ({ close }) => (
              <div className="bottom-island-popup-grid" data-testid="wall-columns-popup">
                {COLUMN_OPTIONS.map((option) => (
                  <button
                    type="button"
                    key={option}
                    className={`bottom-island-popup-option ${option === columns ? 'active' : ''}`.trim()}
                    data-testid={`columns-${option}`}
                    onClick={() => {
                      setColumns(option)
                      writeNumberPreference(WALL_COLUMNS_KEY, option)
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
            id: 'wall-mode',
            icon: <ModeIcon />,
            ariaLabel: 'Change wall mode',
            tooltip: 'Mode',
            testId: 'wall-mode',
            renderPopup: ({ close }) => (
              <div className="bottom-island-popup-stack" data-testid="wall-mode-popup">
                {wallModes.map((option) => (
                  <button
                    type="button"
                    key={option}
                    className={`bottom-island-popup-option ${option === mode ? 'active' : ''}`.trim()}
                    data-testid={`wall-mode-${option}`}
                    onClick={() => {
                      setWallMode(option)
                      close()
                    }}
                  >
                    {option === 'random' ? 'Random' : 'Latest'}
                  </button>
                ))}
              </div>
            ),
          },
          ...(mode === 'latest'
            ? [
                {
                  kind: 'indicator' as const,
                  id: 'wall-page-indicator',
                  label: <span>Page {latestPage}</span>,
                  testId: 'wall-page-indicator',
                  ariaLabel: `Latest page ${latestPage}`,
                },
              ]
            : []),
          ...(mode === 'random'
            ? [
                {
                  id: 'wall-refresh',
                  icon: <RefreshIcon />,
                  ariaLabel: 'Refresh wall',
                  tooltip: 'Refresh',
                  testId: 'wall-refresh',
                  onClick: onRefresh,
                  disabled: loading,
                },
              ]
            : []),
          ...(mode === 'latest'
            ? [
                {
                  id: 'wall-page-next',
                  icon: <NextIcon />,
                  ariaLabel: 'Next latest page',
                  tooltip: 'Next page',
                  testId: 'wall-page-next',
                  onClick: () => changeLatestPage(latestPage + 1, pageInfo.nextCursor),
                  disabled: loading || !pageInfo.hasNext,
                },
              ]
            : []),
          // Every destination lives here now — the sub-pages only know the way
          // back to the wall. Admin stays off the toolbar on purpose: the only
          // way to detect it is probing /admin/api/stats, an admin-only query
          // that takes seconds, and that probe would fire on every wall load.
          {
            kind: 'divider' as const,
            id: 'wall-nav-divider',
          },
          {
            id: 'wall-nav-albums',
            icon: <AlbumsIcon />,
            ariaLabel: 'Find albums',
            tooltip: 'Find albums',
            testId: 'wall-nav-albums',
            onClick: () => navigate('/albums/find'),
          },
          {
            id: 'wall-nav-search',
            icon: <SearchIcon />,
            ariaLabel: 'Search photos',
            tooltip: 'Search photos',
            testId: 'wall-nav-search',
            onClick: () => navigate('/search'),
          },
          {
            id: 'wall-nav-upload',
            icon: <UploadIcon />,
            ariaLabel: 'Upload',
            tooltip: 'Upload',
            testId: 'wall-nav-upload',
            onClick: () => navigate('/upload'),
          },
        ]}
      />

      {loading && <div className="status">Loading...</div>}
      {error && <div className="status error">{error}</div>}
    </div>
  )
}
