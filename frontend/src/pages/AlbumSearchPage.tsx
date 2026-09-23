import { useEffect, useRef, useState } from 'react'
import { Link } from 'react-router-dom'
import { PageTopBar } from '../components/PageTopBar'
import { THUMBNAIL_WIDTH, imageByHashUrl, type AlbumSearchItem } from '../api/client'
import { useAlbumSearch } from '../hooks/useAlbumSearch'
import { formatBytes } from '../utils/format'
import './albumSearch.css'

const SEARCH_LIMIT = 40
const SKELETON_COUNT = 12

const dateFormatter = new Intl.DateTimeFormat(undefined, {
  year: 'numeric',
  month: 'short',
  day: 'numeric',
})

function formatCreatedAt(value: string): string {
  const parsed = new Date(value)
  if (Number.isNaN(parsed.getTime())) {
    return value
  }
  return dateFormatter.format(parsed)
}

function SearchIcon() {
  return (
    <svg className="search-field-icon" viewBox="0 0 20 20" aria-hidden="true">
      <circle cx="8.5" cy="8.5" r="5.5" fill="none" stroke="currentColor" strokeWidth="1.6" />
      <path d="m13 13 4.2 4.2" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" />
    </svg>
  )
}

function AlbumCard({ item }: { item: AlbumSearchItem }) {
  const name = item.originalFilename || '(untitled album)'
  return (
    <li>
      <Link className="album-card" to={`/album/${item.albumId}`} data-testid="album-search-item">
        <div className={`album-card-cover${item.cover ? '' : ' is-empty'}`}>
          {item.cover && (
            <img
              src={imageByHashUrl(item.cover.hash, THUMBNAIL_WIDTH)}
              alt=""
              width={item.cover.w}
              height={item.cover.h}
              loading="lazy"
              decoding="async"
            />
          )}
        </div>
        <p className="album-card-name">{name}</p>
        <p className="album-card-meta tnum">
          {item.photoCount} photos · {formatBytes(item.sizeBytes)} · {formatCreatedAt(item.createdAt)}
        </p>
      </Link>
    </li>
  )
}

function SkeletonGrid() {
  return (
    <ul className="album-card-grid" aria-hidden="true" data-testid="album-search-loading">
      {Array.from({ length: SKELETON_COUNT }, (_, index) => (
        <li key={index}>
          <div className="album-card is-skeleton">
            <div className="album-card-cover skeleton" />
            <div className="skeleton-line skeleton" />
            <div className="skeleton-line is-short skeleton" />
          </div>
        </li>
      ))}
    </ul>
  )
}

export function AlbumSearchPage() {
  const [query, setQuery] = useState('')
  const inputRef = useRef<HTMLInputElement | null>(null)
  const { items, status, error, refresh } = useAlbumSearch(query, SEARCH_LIMIT)

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
  const isTruncated = items.length >= SEARCH_LIMIT
  const sectionLabel = hasQuery ? 'Results' : 'Recent albums'

  return (
    <div className="album-search-page" data-testid="album-search-page">
      <div className="album-search-shell">
        <PageTopBar
          title="Find albums"
          actions={
            <Link className="photo-nav-button" to="/upload">
              Upload
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
            placeholder="Search album names…"
            aria-label="Search albums by name"
            autoComplete="off"
            spellCheck={false}
            data-testid="album-search-input"
          />
          {query.length > 0 && (
            <button
              type="button"
              className="search-clear"
              onClick={() => setQuery('')}
              aria-label="Clear search"
              data-testid="album-search-clear"
            >
              <svg viewBox="0 0 14 14" aria-hidden="true">
                <path d="m3 3 8 8m0-8-8 8" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" />
              </svg>
            </button>
          )}
        </div>

        <div className="album-search-toolbar">
          <p className="album-search-section">{sectionLabel}</p>
          <p className="album-search-count tnum" role="status">
            {isSearching
              ? 'Searching…'
              : status === 'ready'
                ? `${items.length} albums${isTruncated ? ' · most recent matches' : ''}`
                : ''}
          </p>
        </div>

        {status === 'error' && (
          <div className="album-search-note-block" data-testid="album-search-error">
            <p className="album-search-note album-search-note-error">{error}</p>
            <button type="button" className="photo-nav-button" onClick={refresh}>
              Try again
            </button>
          </div>
        )}

        {status === 'ready' && items.length === 0 && hasQuery && (
          <div className="album-search-note-block" data-testid="album-search-empty">
            <p className="album-search-note">No album name contains “{query.trim()}”.</p>
            <button type="button" className="photo-nav-button" onClick={() => setQuery('')}>
              Clear search
            </button>
          </div>
        )}

        {status === 'ready' && items.length === 0 && !hasQuery && (
          <div className="album-search-note-block" data-testid="album-search-no-albums">
            <p className="album-search-note">No albums yet. Upload a ZIP to start the archive.</p>
            <Link className="photo-primary-action" to="/upload">
              Upload
            </Link>
          </div>
        )}

        {isSearching && items.length === 0 && <SkeletonGrid />}

        {items.length > 0 && (
          <ul className="album-card-grid" data-testid="album-search-list">
            {items.map((item) => (
              <AlbumCard key={item.albumId} item={item} />
            ))}
          </ul>
        )}
      </div>
    </div>
  )
}
