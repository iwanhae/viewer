import { useEffect, useRef, useState } from 'react'
import { useNavigate, useSearchParams } from 'react-router-dom'
import {
  createAlbum,
  finalizeAlbum,
  uploadZip,
  uploadZipFallback,
} from '../api/client'
import { useFeed } from '../hooks/useFeed'
import { readColumnPreference, writeColumnPreference } from '../utils/columnPreference'
import { writeLastWallSeed } from '../utils/wallSeed'
import { MasonryWall } from '../components/MasonryWall'

const columnOptions = [1, 2, 3, 4, 5, 6]
const WALL_COLUMNS_KEY = 'wall_columns'
const DEFAULT_COLUMNS = 3

function nextTimestampSeed(currentSeed?: string): string {
  const now = Date.now()
  const parsedCurrent = Number.parseInt(currentSeed ?? '', 10)
  if (Number.isFinite(parsedCurrent) && parsedCurrent >= now) {
    return String(parsedCurrent + 1)
  }
  return String(now)
}

export function WallPage() {
  const [columns, setColumns] = useState(() =>
    readColumnPreference(WALL_COLUMNS_KEY, columnOptions, DEFAULT_COLUMNS),
  )
  const [uploadError, setUploadError] = useState<string | null>(null)
  const [uploading, setUploading] = useState(false)

  const fileInputRef = useRef<HTMLInputElement | null>(null)
  const navigate = useNavigate()
  const [searchParams, setSearchParams] = useSearchParams()
  const seed = searchParams.get('seed') ?? ''

  const { items, loading, error: feedError, refetch } = useFeed(seed)
  const error = uploadError ?? feedError

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

  const onRefresh = () => {
    if (typeof window !== 'undefined') {
      window.scrollTo({ top: 0, behavior: 'auto' })
    }
    const nextSeed = nextTimestampSeed(seed)
    setSearchParams((prev) => {
      const next = new URLSearchParams(prev)
      next.set('seed', nextSeed)
      return next
    })
  }

  const onPickFile = async (event: React.ChangeEvent<HTMLInputElement>) => {
    const file = event.target.files?.[0]
    if (!file) return

    setUploading(true)
    setUploadError(null)
    try {
      const created = await createAlbum(file)
      try {
        await uploadZip(created.upload.url, file, created.upload.headers)
      } catch {
        await uploadZipFallback(created.albumId, file)
      }
      await finalizeAlbum(created.albumId)

      if (seed) {
        await refetch()
      } else {
        const nextSeed = nextTimestampSeed()
        setSearchParams(
          (prev) => {
            const next = new URLSearchParams(prev)
            next.set('seed', nextSeed)
            return next
          },
          { replace: true },
        )
      }
    } catch (err) {
      setUploadError((err as Error).message)
    } finally {
      setUploading(false)
      if (event.target) event.target.value = ''
    }
  }

  return (
    <div className="wall-page">
      <MasonryWall
        items={items}
        columnCount={columns}
        getItemWeight={(item) => item.h / Math.max(item.w, 1)}
        renderItem={(item) => (
          <button
            className="tile"
            onClick={() => navigate(`/album/${item.albumId}`)}
            data-testid="wall-tile"
          >
            <img
              src={item.src}
              alt=""
              loading="lazy"
              style={{ aspectRatio: `${item.w} / ${item.h}` }}
            />
          </button>
        )}
        getItemKey={(item, idx) => `${item.albumId}-${item.i}-${idx}`}
        containerClassName=""
        columnClassName=""
        containerTestId="wall-grid"
        columnTestId="masonry-column"
      />

      <div className="bottom-bar">
        <div className="columns">
          {columnOptions.map((option) => (
            <button
              key={option}
              className={option === columns ? 'active' : ''}
              onClick={() => {
                setColumns(option)
                writeColumnPreference(WALL_COLUMNS_KEY, option)
              }}
              data-testid={`columns-${option}`}
            >
              {option}
            </button>
          ))}
        </div>
        <button
          className="upload wall-refresh"
          onClick={onRefresh}
          disabled={loading || uploading}
          data-testid="wall-refresh"
        >
          Refresh
        </button>
        <button
          className="upload"
          onClick={() => fileInputRef.current?.click()}
          disabled={uploading}
          data-testid="upload-button"
        >
          {uploading ? 'Uploading...' : '+'}
        </button>
        <input
          ref={fileInputRef}
          type="file"
          accept=".zip"
          hidden
          onChange={onPickFile}
          data-testid="upload-input"
        />
      </div>

      {loading && <div className="status">Loading...</div>}
      {error && <div className="status error">{error}</div>}
    </div>
  )
}
