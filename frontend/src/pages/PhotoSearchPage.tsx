import { useEffect, useRef, useState } from 'react'
import { Link, useNavigate } from 'react-router-dom'
import { PageTopBar } from '../components/PageTopBar'
import { MasonryWall } from '../components/MasonryWall'
import { THUMBNAIL_WIDTH, imageByHashUrl } from '../api/client'
import { usePhotoSearch } from '../hooks/usePhotoSearch'
import './albumSearch.css'

const SEARCH_LIMIT = 40
const SKELETON_COUNT = 12
const SEARCH_COLUMNS = 3

function SearchIcon() {
  return (
    <svg className="search-field-icon" viewBox="0 0 20 20" aria-hidden="true">
      <circle cx="8.5" cy="8.5" r="5.5" fill="none" stroke="currentColor" strokeWidth="1.6" />
      <path d="m13 13 4.2 4.2" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" />
    </svg>
  )
}

function SkeletonGrid() {
  return (
    <ul className="album-card-grid" aria-hidden="true" data-testid="photo-search-loading">
      {Array.from({ length: SKELETON_COUNT }, (_, index) => (
        <li key={index}>
          <div className="album-card is-skeleton">
            <div className="album-card-cover skeleton" />
          </div>
        </li>
      ))}
    </ul>
  )
}

export function PhotoSearchPage() {
  const navigate = useNavigate()
  const [query, setQuery] = useState('')
  const inputRef = useRef<HTMLInputElement | null>(null)
  const { items, status, error, errorCode, refresh } = usePhotoSearch(query, SEARCH_LIMIT)

  // "/" focuses the search field from anywhere on the page, unless the
  // keystroke is already going into some text input.
  useEffect(() => {
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key !== '/') return
      const target = event.target as HTMLElement | null
      if (
        target &&
        (target.tagName === 'INPUT' || target.tagName === 'TEXTAREA' || target.isContentEditable)
      ) {
        return
      }
      event.preventDefault()
      inputRef.current?.focus()
    }
    window.addEventListener('keydown', onKeyDown)
    return () => window.removeEventListener('keydown', onKeyDown)
  }, [])

  const onInputKeyDown = (event: React.KeyboardEvent<HTMLInputElement>) => {
    if (event.key === 'Escape' && query.length > 0) {
      setQuery('')
    }
  }

  const hasQuery = query.trim().length > 0
  const isSearching = status === 'loading'
  const isUnavailable = status === 'error' && errorCode === 'UNAVAILABLE'

  return (
    <div className="album-search-page" data-testid="photo-search-page">
      <div className="album-search-shell">
        <PageTopBar
          title="Search photos"
          actions={
            <Link className="photo-nav-button" to="/albums/find">
              Find albums
            </Link>
          }
        />

        <div className="search-field">
          <SearchIcon />
          <input
            ref={inputRef}
            type="search"
            value={query}
            onChange={(event) => setQuery(event.target.value)}
            onKeyDown={onInputKeyDown}
            placeholder="Search photos by description…"
            aria-label="Search photos by description"
            autoComplete="off"
            spellCheck={false}
            data-testid="photo-search-input"
          />
          {query.length > 0 && (
            <button
              type="button"
              className="search-clear"
              onClick={() => setQuery('')}
              aria-label="Clear search"
              data-testid="photo-search-clear"
            >
              <svg viewBox="0 0 14 14" aria-hidden="true">
                <path d="m3 3 8 8m0-8-8 8" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" />
              </svg>
            </button>
          )}
        </div>

        <div className="album-search-toolbar">
          <p className="album-search-section">{hasQuery ? 'Results' : 'Search photos'}</p>
          <p className="album-search-count tnum" role="status">
            {isSearching ? 'Searching…' : status === 'ready' ? `${items.length} photos` : ''}
          </p>
        </div>

        {!hasQuery && (
          <div className="album-search-note-block" data-testid="photo-search-hint">
            <p className="album-search-note">
              Describe the photo you are looking for, for example “sunset over mountains” or “a dog
              playing on the beach”.
            </p>
          </div>
        )}

        {status === 'error' && isUnavailable && (
          <div className="album-search-note-block" data-testid="photo-search-unavailable">
            <p className="album-search-note">
              Photo search is not available. It needs vector search to be enabled on the server.
            </p>
          </div>
        )}

        {status === 'error' && !isUnavailable && (
          <div className="album-search-note-block" data-testid="photo-search-error">
            <p className="album-search-note album-search-note-error">{error}</p>
            <button type="button" className="photo-nav-button" onClick={refresh}>
              Try again
            </button>
          </div>
        )}

        {status === 'ready' && hasQuery && items.length === 0 && (
          <div className="album-search-note-block" data-testid="photo-search-empty">
            <p className="album-search-note">No photos match “{query.trim()}”.</p>
            <button type="button" className="photo-nav-button" onClick={() => setQuery('')}>
              Clear search
            </button>
          </div>
        )}

        {hasQuery && isSearching && items.length === 0 && <SkeletonGrid />}

        {hasQuery && items.length > 0 && (
          <MasonryWall
            items={items}
            columnCount={SEARCH_COLUMNS}
            getItemWeight={(item) => item.h / Math.max(item.w, 1)}
            renderItem={(item) => (
              <button
                className="tile"
                onClick={() => navigate(`/album/${item.albumId}/${item.i}`)}
                aria-label={`Open photo ${item.i + 1}`}
                data-testid="photo-search-tile"
              >
                <img
                  src={imageByHashUrl(item.hash, THUMBNAIL_WIDTH)}
                  alt=""
                  loading="lazy"
                  decoding="async"
                  style={{ aspectRatio: `${item.w} / ${item.h}` }}
                />
              </button>
            )}
            getItemKey={(item, index) => `${item.albumId}-${item.i}-${index}`}
            containerClassName=""
            columnClassName=""
            containerTestId="photo-search-grid"
            columnTestId="photo-search-column"
          />
        )}
      </div>
    </div>
  )
}
