import { useEffect, useMemo, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { fetchAlbumSearch, type AlbumSearchItem } from '../api/client'

const SEARCH_LIMIT = 20
const SEARCH_DEBOUNCE_MS = 200

function formatCreatedAt(value: string): string {
  const parsed = new Date(value)
  if (Number.isNaN(parsed.getTime())) {
    return value
  }
  return parsed.toLocaleString()
}

export function AlbumSearchPage() {
  const [query, setQuery] = useState('')
  const [debouncedQuery, setDebouncedQuery] = useState('')
  const [items, setItems] = useState<AlbumSearchItem[]>([])
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const navigate = useNavigate()

  useEffect(() => {
    const timeout = window.setTimeout(() => {
      setDebouncedQuery(query)
    }, SEARCH_DEBOUNCE_MS)
    return () => window.clearTimeout(timeout)
  }, [query])

  useEffect(() => {
    const abortController = new AbortController()
    setLoading(true)
    setError(null)

    void (async () => {
      try {
        const response = await fetchAlbumSearch({
          q: debouncedQuery,
          limit: SEARCH_LIMIT,
          signal: abortController.signal,
        })
        setItems(Array.isArray(response.albums) ? response.albums : [])
      } catch (err) {
        if (abortController.signal.aborted) return
        setError((err as Error).message)
      } finally {
        if (!abortController.signal.aborted) {
          setLoading(false)
        }
      }
    })()

    return () => {
      abortController.abort()
    }
  }, [debouncedQuery])

  const hasResults = items.length > 0
  const hasQuery = debouncedQuery.trim().length > 0

  const suggestions = useMemo(() => {
    const seen = new Set<string>()
    const values: string[] = []
    for (const item of items) {
      if (!item.originalFilename || seen.has(item.originalFilename)) continue
      seen.add(item.originalFilename)
      values.push(item.originalFilename)
      if (values.length >= 10) break
    }
    return values
  }, [items])

  const onBack = () => {
    if (typeof window !== 'undefined' && window.history.length > 1) {
      navigate(-1)
      return
    }
    navigate('/')
  }

  return (
    <div className="album-search-page" data-testid="album-search-page">
      <div className="album-search-shell">
        <header className="album-search-header">
          <button className="photo-nav-button" onClick={onBack} data-testid="album-search-back">
            Back
          </button>
          <h1 className="album-search-title">Find albums</h1>
        </header>

        <label className="album-search-label" htmlFor="album-search-input">
          Album name
        </label>
        <input
          id="album-search-input"
          className="album-search-input"
          list="album-search-suggestions"
          type="search"
          value={query}
          onChange={(event) => setQuery(event.target.value)}
          placeholder="Type album name prefix..."
          autoComplete="off"
          spellCheck={false}
          data-testid="album-search-input"
        />
        <datalist id="album-search-suggestions">
          {suggestions.map((value) => (
            <option key={value} value={value} />
          ))}
        </datalist>

        {loading && <p className="album-search-note">Searching albums...</p>}
        {!loading && error && <p className="album-search-note album-search-note-error">{error}</p>}
        {!loading && !error && !hasResults && hasQuery && (
          <p className="album-search-note">No albums match that prefix.</p>
        )}
        {!loading && !error && !hasResults && !hasQuery && (
          <p className="album-search-note">Type a prefix or pick a recent album below.</p>
        )}

        <div className="album-search-list" data-testid="album-search-list">
          {items.map((item) => (
            <button
              key={item.albumId}
              className="album-search-item"
              type="button"
              onClick={() => navigate(`/album/${item.albumId}`)}
              data-testid="album-search-item"
            >
              <div className="album-search-item-head">
                <p className="album-search-name">{item.originalFilename || '(untitled album)'}</p>
              </div>
              <p className="album-search-item-meta">
                {item.photoCount} photos  {formatCreatedAt(item.createdAt)}
              </p>
              <p className="album-search-item-meta album-search-item-meta-id">ID: {item.albumId}</p>
            </button>
          ))}
        </div>
      </div>
    </div>
  )
}
