import { useEffect, useMemo, useRef, useState } from 'react'
import { useNavigate, useSearchParams } from 'react-router-dom'
import { imageByHashUrl, type FeedMode } from '../api/client'
import { useFeed } from '../hooks/useFeed'
import { useEmbeddingProgress } from '../hooks/useEmbeddingProgress'
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
import { ColumnsIcon, ModeIcon, NextIcon, PrevIcon, RefreshIcon, ShortcutIcon } from '../components/IslandIcons'

const WALL_COLUMNS_KEY = 'wall_columns'
const DEFAULT_COLUMNS = 3
const WALL_FEED_LIMIT = 40
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

  const embedding = useEmbeddingProgress()

  const { items, loading, error, pageInfo } = useFeed(
    mode === 'random' ? seed : '',
    mode,
    mode === 'latest' ? latestCursor : '',
  )
  const visibleItems = useMemo(() => items.slice(0, WALL_FEED_LIMIT), [items])
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
                src={imageByHashUrl(item.hash)}
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
          // Embedding keeps running after an album is indexed, so the wall
          // says so while there is work left instead of silently showing a
          // feed that recommendations cannot use yet.
          ...(embedding && embedding.enabled && embedding.pending > 0
            ? [
                {
                  kind: 'indicator' as const,
                  id: 'wall-embedding-indicator',
                  label: (
                    <span>
                      Embedding {embedding.ready}/{embedding.total}
                    </span>
                  ),
                  testId: 'wall-embedding-indicator',
                  ariaLabel: `Embedding ${embedding.ready} of ${embedding.total} images`,
                },
              ]
            : []),
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
        ]}
      />

      {loading && <div className="status">Loading...</div>}
      {error && <div className="status error">{error}</div>}
    </div>
  )
}
