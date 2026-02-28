import { useEffect, useMemo, useRef, useState } from 'react'
import { useNavigate, useSearchParams } from 'react-router-dom'
import type { FeedMode } from '../api/client'
import { useFeed } from '../hooks/useFeed'
import { readColumnPreference, writeColumnPreference } from '../utils/columnPreference'
import { writeLastWallSeed, writeLastWallState } from '../utils/wallSeed'
import { MasonryWall } from '../components/MasonryWall'
import { BottomIsland } from '../components/BottomIsland'
import { ColumnsIcon, ModeIcon, NextIcon, PrevIcon, RefreshIcon, ShortcutIcon } from '../components/IslandIcons'

const columnOptions = [1, 2, 3, 4, 5, 6]
const WALL_COLUMNS_KEY = 'wall_columns'
const DEFAULT_COLUMNS = 3
const WALL_FEED_LIMIT = 40
const ALBUM_PAGE_SIZE = 50
const defaultMode: FeedMode = 'random'
const wallModes: FeedMode[] = ['random', 'latest']

function nextTimestampSeed(currentSeed?: string): string {
  const now = Date.now()
  const parsedCurrent = Number.parseInt(currentSeed ?? '', 10)
  if (Number.isFinite(parsedCurrent) && parsedCurrent >= now) {
    return String(parsedCurrent + 1)
  }
  return String(now)
}

function parsePositiveInt(value: string | null, fallback: number): number {
  const parsed = Number(value)
  if (!Number.isInteger(parsed) || parsed < 1) return fallback
  return parsed
}

function pageForPhotoIndex(index: number): number {
  return Math.floor(index / ALBUM_PAGE_SIZE) + 1
}

function wallFocusKey(albumId: string, photoIndex: number): string {
  return `${albumId}:${photoIndex}`
}

function parseWallMode(modeParam: string | null): FeedMode {
  return modeParam === 'latest' ? 'latest' : defaultMode
}

export function WallPage() {
  const [columns, setColumns] = useState(() =>
    readColumnPreference(WALL_COLUMNS_KEY, columnOptions, DEFAULT_COLUMNS),
  )

  const navigate = useNavigate()
  const [searchParams, setSearchParams] = useSearchParams()
  const rawMode = searchParams.get('mode')
  const mode = parseWallMode(rawMode)
  const seed = searchParams.get('seed') ?? ''
  const focus = searchParams.get('focus')
  const rawLatestPage = searchParams.get('lp')
  const latestPage = parsePositiveInt(rawLatestPage, 1)
  const latestCursor = searchParams.get('lc') ?? ''

  const { items, loading, error, pageInfo, refetch } = useFeed(
    seed,
    mode,
    mode === 'latest' ? latestCursor : '',
  )
  const visibleItems = useMemo(() => items.slice(0, WALL_FEED_LIMIT), [items])
  const tileRefs = useRef(new Map<string, HTMLButtonElement>())

  useEffect(() => {
    if (rawMode === mode) return
    setSearchParams(
      (prev) => {
        const next = new URLSearchParams(prev)
        next.set('mode', mode)
        return next
      },
      { replace: true },
    )
  }, [mode, rawMode, setSearchParams])

  useEffect(() => {
    if (mode !== 'random') return

    const needsCleanup = rawLatestPage !== null || latestCursor !== ''
    if (!seed) {
      const nextSeed = nextTimestampSeed()
      setSearchParams(
        (prev) => {
          const next = new URLSearchParams(prev)
          next.set('seed', nextSeed)
          next.delete('lp')
          next.delete('lc')
          return next
        },
        { replace: true },
      )
      return
    }

    if (!needsCleanup) return
    setSearchParams(
      (prev) => {
        const next = new URLSearchParams(prev)
        next.delete('lp')
        next.delete('lc')
        return next
      },
      { replace: true },
    )
  }, [latestCursor, mode, rawLatestPage, seed, setSearchParams])

  useEffect(() => {
    if (mode !== 'latest') return

    const normalizedPage = String(latestPage)
    const needsPageParam = rawLatestPage !== normalizedPage
    const hasSeed = seed !== ''
    if (!needsPageParam && !hasSeed) return

    setSearchParams(
      (prev) => {
        const next = new URLSearchParams(prev)
        next.set('lp', normalizedPage)
        next.delete('seed')
        return next
      },
      { replace: true },
    )
  }, [latestPage, mode, rawLatestPage, seed, setSearchParams])

  useEffect(() => {
    if (mode !== 'latest') return
    if (latestPage <= 1 || latestCursor) return
    setSearchParams(
      (prev) => {
        const next = new URLSearchParams(prev)
        next.set('lp', '1')
        return next
      },
      { replace: true },
    )
  }, [latestCursor, latestPage, mode, setSearchParams])

  useEffect(() => {
    if (mode === 'random' && seed) {
      writeLastWallSeed(seed)
      writeLastWallState({ mode: 'random', seed })
      return
    }

    if (mode === 'latest') {
      writeLastWallState({
        mode: 'latest',
        latestPage,
        latestCursor,
      })
    }
  }, [latestCursor, latestPage, mode, seed])

  useEffect(() => {
    if (mode !== 'latest' || loading || error) return
    const normalizedCursor = pageInfo.cursor ?? ''
    if (normalizedCursor === latestCursor) return

    setSearchParams(
      (prev) => {
        const next = new URLSearchParams(prev)
        next.set('lp', String(latestPage))
        if (normalizedCursor) {
          next.set('lc', normalizedCursor)
        } else {
          next.delete('lc')
        }
        return next
      },
      { replace: true },
    )
  }, [error, latestCursor, latestPage, loading, mode, pageInfo.cursor, setSearchParams])

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
      next.set('mode', nextMode)
      next.delete('focus')

      if (nextMode === 'latest') {
        next.set('lp', '1')
        next.delete('lc')
        next.delete('seed')
        return next
      }

      next.delete('lp')
      next.delete('lc')
      if (!next.get('seed')) {
        next.set('seed', nextTimestampSeed())
      }
      return next
    })
  }

  const onChangeLatestPage = (nextPage: number, nextCursor: string | null) => {
    if (typeof window !== 'undefined') {
      window.scrollTo({ top: 0, behavior: 'auto' })
    }

    setSearchParams((prev) => {
      const next = new URLSearchParams(prev)
      next.set('mode', 'latest')
      next.set('lp', String(Math.max(1, nextPage)))
      if (nextCursor && nextCursor.trim()) {
        next.set('lc', nextCursor)
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
    if (mode === 'latest') {
      setSearchParams(
        (prev) => {
          const next = new URLSearchParams(prev)
          next.delete('focus')
          return next
        },
        { replace: true },
      )
      void refetch()
      return
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
          ...(mode === 'latest'
            ? [
                {
                  id: 'wall-page-prev',
                  icon: <PrevIcon />,
                  ariaLabel: 'Previous latest page',
                  tooltip: 'Previous page',
                  testId: 'wall-page-prev',
                  onClick: () => onChangeLatestPage(latestPage - 1, pageInfo.prevCursor),
                  disabled: !pageInfo.hasPrev,
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
          ...(mode === 'latest'
            ? [
                {
                  id: 'wall-page-next',
                  icon: <NextIcon />,
                  ariaLabel: 'Next latest page',
                  tooltip: 'Next page',
                  testId: 'wall-page-next',
                  onClick: () => onChangeLatestPage(latestPage + 1, pageInfo.nextCursor),
                  disabled: !pageInfo.hasNext,
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
