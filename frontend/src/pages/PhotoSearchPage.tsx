import { useEffect, useRef, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { PageTopBar } from '../components/PageTopBar'
import { MasonryWall } from '../components/MasonryWall'
import { THUMBNAIL_WIDTH, imageByHashUrl, type PhotoSearchItem } from '../api/client'
import { usePhotoSearch } from '../hooks/usePhotoSearch'
import { usePhotoImageSearch } from '../hooks/usePhotoImageSearch'
import './albumSearch.css'

const SEARCH_LIMIT = 40
const SKELETON_COUNT = 12
const SEARCH_COLUMNS = 3

// ActiveSearchStatus merges the text hook's statuses with the image hook's:
// "loading" is a text search in flight, "searching" an image upload + embed.
type ActiveSearchStatus = 'idle' | 'loading' | 'searching' | 'ready' | 'error'

type ActiveSearch = {
  items: PhotoSearchItem[]
  status: ActiveSearchStatus
  error: string | null
  errorCode: string | null
  refresh: () => void
}

function SearchIcon() {
  return (
    <svg className="search-field-icon" viewBox="0 0 20 20" aria-hidden="true">
      <circle cx="8.5" cy="8.5" r="5.5" fill="none" stroke="currentColor" strokeWidth="1.6" />
      <path d="m13 13 4.2 4.2" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" />
    </svg>
  )
}

function ImageSearchIcon() {
  return (
    <svg viewBox="0 0 20 20" aria-hidden="true">
      <rect x="2.2" y="3.2" width="15.6" height="13.6" rx="2" fill="none" stroke="currentColor" strokeWidth="1.6" />
      <circle cx="7.4" cy="7.9" r="1.5" fill="none" stroke="currentColor" strokeWidth="1.4" />
      <path
        d="m3.2 14.6 3.9-3.9 3.1 3.1 2.6-2.6 4 4"
        fill="none"
        stroke="currentColor"
        strokeWidth="1.6"
        strokeLinecap="round"
        strokeLinejoin="round"
      />
    </svg>
  )
}

function CrossIcon() {
  return (
    <svg viewBox="0 0 14 14" aria-hidden="true">
      <path d="m3 3 8 8m0-8-8 8" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" />
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
  const [imageFile, setImageFile] = useState<File | null>(null)
  const [imageUrl, setImageUrl] = useState<string | null>(null)
  const inputRef = useRef<HTMLInputElement | null>(null)
  const fileInputRef = useRef<HTMLInputElement | null>(null)
  const imageUrlRef = useRef<string | null>(null)
  const imageActive = imageFile !== null

  // The two modes are exclusive: while an image search is active the text hook
  // is fed an empty query, so it neither fires debounced requests nor keeps
  // stale tiles — clearing the chip hands control straight back to the text
  // query (idle when that is empty, same gating as before).
  const textSearch = usePhotoSearch(imageActive ? '' : query, SEARCH_LIMIT)
  const imageSearch = usePhotoImageSearch(SEARCH_LIMIT)

  const active: ActiveSearch = imageActive
    ? {
        items: imageSearch.items,
        // "idle" only exists for the frame before the hook's effect kicks off;
        // a picked file always means a search is on its way.
        status: imageSearch.status === 'idle' ? 'searching' : imageSearch.status,
        error: imageSearch.error,
        errorCode: imageSearch.errorCode,
        refresh: imageSearch.refresh,
      }
    : {
        items: textSearch.items,
        status: textSearch.status,
        error: textSearch.error,
        errorCode: textSearch.errorCode,
        refresh: textSearch.refresh,
      }

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

  // The chip's object URL is owned by the ref so replace/clear never leaks the
  // previous one; the empty-up cleanup covers unmount.
  useEffect(
    () => () => {
      if (imageUrlRef.current) URL.revokeObjectURL(imageUrlRef.current)
    },
    [],
  )

  const onInputKeyDown = (event: React.KeyboardEvent<HTMLInputElement>) => {
    if (event.key === 'Escape' && query.length > 0) {
      setQuery('')
    }
  }

  const replaceChipImage = (file: File | null) => {
    if (imageUrlRef.current) {
      URL.revokeObjectURL(imageUrlRef.current)
      imageUrlRef.current = null
    }
    setImageFile(file)
    if (file !== null) {
      const url = URL.createObjectURL(file)
      imageUrlRef.current = url
      setImageUrl(url)
    } else {
      setImageUrl(null)
    }
  }

  const onImageInputChange = (event: React.ChangeEvent<HTMLInputElement>) => {
    const file = event.target.files?.[0] ?? null
    // Reset the input even on cancel, so picking the same file again re-fires
    // the change event.
    event.target.value = ''
    if (file === null) return
    replaceChipImage(file)
    imageSearch.search(file)
  }

  const clearImageSearch = () => {
    replaceChipImage(null)
    imageSearch.clear()
  }

  const hasQuery = query.trim().length > 0
  const isSearching = active.status === 'loading' || active.status === 'searching'
  const isUnavailable = active.status === 'error' && active.errorCode === 'UNAVAILABLE'
  const hasSection = imageActive || hasQuery
  const showResults = active.items.length > 0 && (imageActive ? active.status === 'ready' : hasQuery)
  const showEmpty =
    active.status === 'ready' && active.items.length === 0 && (imageActive || hasQuery)

  return (
    <div className="album-search-page" data-testid="photo-search-page">
      <div className="album-search-shell">
        <PageTopBar title="Search photos" />

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
          <button
            type="button"
            className="search-image-button"
            onClick={() => fileInputRef.current?.click()}
            aria-label="Search by image"
            title="Search by image"
            data-testid="photo-search-image-button"
          >
            <ImageSearchIcon />
          </button>
          <input
            ref={fileInputRef}
            type="file"
            accept="image/*"
            hidden
            onChange={onImageInputChange}
            aria-label="Search by image"
            data-testid="photo-search-image-input"
          />
          {query.length > 0 && (
            <button
              type="button"
              className="search-clear"
              onClick={() => setQuery('')}
              aria-label="Clear search"
              data-testid="photo-search-clear"
            >
              <CrossIcon />
            </button>
          )}
        </div>

        {imageActive && imageUrl !== null && (
          <div className="photo-search-chip-row">
            <div className="photo-search-chip" data-testid="photo-search-image-chip">
              <img src={imageUrl} alt="" />
              <span className="photo-search-chip-name">{imageFile?.name}</span>
              <button
                type="button"
                className="photo-search-chip-clear"
                onClick={clearImageSearch}
                aria-label="Clear image search"
                data-testid="photo-search-image-clear"
              >
                <CrossIcon />
              </button>
            </div>
          </div>
        )}

        <div className="album-search-toolbar">
          <p className="album-search-section">{hasSection ? 'Results' : 'Search photos'}</p>
          <p className="album-search-count tnum" role="status">
            {isSearching ? 'Searching…' : active.status === 'ready' ? `${active.items.length} photos` : ''}
          </p>
        </div>

        {!hasSection && (
          <div className="album-search-note-block" data-testid="photo-search-hint">
            <p className="album-search-note">
              Describe the photo you are looking for, for example “sunset over mountains” or “a dog
              playing on the beach”.
            </p>
          </div>
        )}

        {active.status === 'error' && isUnavailable && (
          <div className="album-search-note-block" data-testid="photo-search-unavailable">
            <p className="album-search-note">
              Photo search is not available. It needs vector search to be enabled on the server.
            </p>
          </div>
        )}

        {active.status === 'error' && !isUnavailable && (
          <div className="album-search-note-block" data-testid="photo-search-error">
            <p className="album-search-note album-search-note-error">{active.error}</p>
            <button type="button" className="photo-nav-button" onClick={active.refresh}>
              Try again
            </button>
          </div>
        )}

        {showEmpty && !imageActive && (
          <div className="album-search-note-block" data-testid="photo-search-empty">
            <p className="album-search-note">No photos match “{query.trim()}”.</p>
            <button type="button" className="photo-nav-button" onClick={() => setQuery('')}>
              Clear search
            </button>
          </div>
        )}

        {showEmpty && imageActive && (
          <div className="album-search-note-block" data-testid="photo-search-empty">
            <p className="album-search-note">No photos look similar to this image.</p>
            <button type="button" className="photo-nav-button" onClick={() => fileInputRef.current?.click()}>
              Choose another photo
            </button>
          </div>
        )}

        {hasSection && isSearching && active.items.length === 0 && <SkeletonGrid />}

        {showResults && (
          <MasonryWall
            items={active.items}
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
