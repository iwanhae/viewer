import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import {
  abortMultipartUpload,
  completeMultipartUpload,
  createAlbum,
  finalizeAlbum,
  initiateMultipartUpload,
  presignMultipartPart,
  uploadMultipartPart,
} from '../api/client'

type UploadStatus =
  | 'queued'
  | 'creating'
  | 'initiating'
  | 'uploading'
  | 'uploaded'
  | 'finalizing'
  | 'ready'
  | 'failed'
  | 'aborted'

type UploadItem = {
  id: string
  file: File
  name: string
  sizeBytes: number
  status: UploadStatus
  uploadedBytes: number
  albumId?: string
  uploadId?: string
  partCount?: number
  partSizeBytes?: number
  photoCount?: number
  error?: string
}

const uploadConcurrency = 1

function nextItemID(): string {
  if (typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function') {
    return crypto.randomUUID()
  }
  return `${Date.now()}-${Math.random().toString(16).slice(2)}`
}

function toErrorMessage(err: unknown): string {
  if (err instanceof Error) {
    return err.message
  }
  return 'request failed'
}

function isAbortError(err: unknown): boolean {
  if (err instanceof DOMException) {
    return err.name === 'AbortError'
  }
  if (typeof err === 'object' && err !== null && 'name' in err) {
    return String((err as { name?: unknown }).name) === 'AbortError'
  }
  return false
}

function formatBytes(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes <= 0) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  let value = bytes
  let idx = 0
  while (value >= 1024 && idx < units.length - 1) {
    value /= 1024
    idx++
  }
  const precision = value >= 100 || idx === 0 ? 0 : value >= 10 ? 1 : 2
  return `${value.toFixed(precision)} ${units[idx]}`
}

function statusLabel(status: UploadStatus): string {
  switch (status) {
    case 'queued':
      return 'Queued'
    case 'creating':
      return 'Creating album'
    case 'initiating':
      return 'Initiating upload'
    case 'uploading':
      return 'Uploading'
    case 'uploaded':
      return 'Uploaded'
    case 'finalizing':
      return 'Processing (manual refresh)'
    case 'ready':
      return 'Ready'
    case 'failed':
      return 'Failed'
    case 'aborted':
      return 'Canceled'
    default:
      return status
  }
}

export function UploadPage() {
  const navigate = useNavigate()
  const fileInputRef = useRef<HTMLInputElement | null>(null)
  const controllersRef = useRef(new Map<string, AbortController>())

  const [items, setItems] = useState<UploadItem[]>([])
  const [queueRunning, setQueueRunning] = useState(false)
  const [activeUploads, setActiveUploads] = useState(0)
  const [pageError, setPageError] = useState<string | null>(null)

  const updateItem = useCallback((itemID: string, next: Partial<UploadItem>) => {
    setItems((prev) => prev.map((item) => (item.id === itemID ? { ...item, ...next } : item)))
  }, [])

  const applyFinalizeStatus = useCallback(
    (
      itemID: string,
      result: { status: string; photoCount: number; lastError?: string },
    ) => {
      const normalized = String(result.status || '').toLowerCase()
      if (normalized === 'ready') {
        updateItem(itemID, {
          status: 'ready',
          photoCount: result.photoCount,
          error: undefined,
        })
        return
      }
      if (normalized === 'failed') {
        updateItem(itemID, {
          status: 'failed',
          error: result.lastError || 'finalization failed',
        })
        return
      }
      updateItem(itemID, {
        status: 'finalizing',
        error: undefined,
      })
    },
    [updateItem],
  )

  const runItemUpload = useCallback(
    async (item: UploadItem) => {
      const controller = new AbortController()
      controllersRef.current.set(item.id, controller)
      setActiveUploads((count) => count + 1)

      let albumID = item.albumId ?? ''
      let uploadID = item.uploadId ?? ''
      try {
        updateItem(item.id, { status: 'creating', error: undefined, uploadedBytes: 0 })
        const created = await createAlbum(item.file)
        albumID = created.albumId
        updateItem(item.id, { albumId: albumID, status: 'initiating' })

        const initiated = await initiateMultipartUpload(albumID, item.file)
        uploadID = initiated.uploadId
        updateItem(item.id, {
          uploadId: uploadID,
          partCount: initiated.partCount,
          partSizeBytes: initiated.partSizeBytes,
          status: 'uploading',
        })

        let uploadedBytes = 0
        for (let partNumber = 1; partNumber <= initiated.partCount; partNumber++) {
          const offset = (partNumber - 1) * initiated.partSizeBytes
          const chunk = item.file.slice(offset, Math.min(offset + initiated.partSizeBytes, item.file.size))
          const signed = await presignMultipartPart(albumID, uploadID, partNumber)
          await uploadMultipartPart(signed.url, chunk, signed.headers, controller.signal)
          uploadedBytes += chunk.size
          updateItem(item.id, { uploadedBytes })
        }

        await completeMultipartUpload(albumID, uploadID, [])
        updateItem(item.id, { status: 'uploaded', uploadedBytes: item.sizeBytes, error: undefined })

        const finalize = await finalizeAlbum(albumID)
        applyFinalizeStatus(item.id, finalize)
      } catch (err) {
        if (albumID && uploadID) {
          try {
            await abortMultipartUpload(albumID, uploadID)
          } catch {
            // Ignore abort cleanup failures; original error is reported below.
          }
        }

        if (isAbortError(err)) {
          updateItem(item.id, { status: 'aborted', error: undefined })
        } else {
          updateItem(item.id, { status: 'failed', error: toErrorMessage(err) })
        }
      } finally {
        controllersRef.current.delete(item.id)
        setActiveUploads((count) => Math.max(0, count - 1))
      }
    },
    [applyFinalizeStatus, updateItem],
  )

  useEffect(() => {
    if (!queueRunning) return
    if (activeUploads >= uploadConcurrency) return

    const availableSlots = uploadConcurrency - activeUploads
    const queued = items.filter((item) => item.status === 'queued').slice(0, availableSlots)
    if (queued.length === 0) return

    for (const item of queued) {
      void runItemUpload(item)
    }
  }, [activeUploads, items, queueRunning, runItemUpload])

  const onPickFiles = (event: React.ChangeEvent<HTMLInputElement>) => {
    const files = Array.from(event.target.files ?? [])
    if (files.length === 0) return

    const picked = files
      .filter((file) => file.name.toLowerCase().endsWith('.zip'))
      .map<UploadItem>((file) => ({
        id: nextItemID(),
        file,
        name: file.name,
        sizeBytes: file.size,
        status: 'queued',
        uploadedBytes: 0,
      }))

    if (picked.length === 0) {
      setPageError('Select at least one .zip file.')
    } else {
      setPageError(null)
      setItems((prev) => [...prev, ...picked])
    }

    if (event.target) {
      event.target.value = ''
    }
  }

  const onCancelItem = (itemID: string) => {
    const controller = controllersRef.current.get(itemID)
    if (!controller) return
    controller.abort()
  }

  const onRemoveItem = (itemID: string) => {
    const controller = controllersRef.current.get(itemID)
    if (controller) {
      controller.abort()
    }
    setItems((prev) => prev.filter((item) => item.id !== itemID))
  }

  const onRetryFailedUploads = () => {
    setItems((prev) =>
      prev.map((item) => {
        const isUploadFailure =
          item.status === 'aborted' || (item.status === 'failed' && item.uploadedBytes < item.sizeBytes)
        if (!isUploadFailure) return item
        return {
          ...item,
          status: 'queued',
          uploadedBytes: 0,
          albumId: undefined,
          uploadId: undefined,
          partCount: undefined,
          partSizeBytes: undefined,
          error: undefined,
        }
      }),
    )
    setQueueRunning(true)
  }

  const onRetryFinalize = useCallback(
    async (itemID: string, albumID: string) => {
      updateItem(itemID, { status: 'finalizing', error: undefined })
      try {
        const result = await finalizeAlbum(albumID)
        applyFinalizeStatus(itemID, result)
      } catch (err) {
        updateItem(itemID, { status: 'failed', error: toErrorMessage(err) })
      }
    },
    [applyFinalizeStatus, updateItem],
  )

  const summary = useMemo(() => {
    const totalFiles = items.length
    const uploadedFiles = items.filter(
      (item) => item.status === 'uploaded' || item.status === 'finalizing' || item.status === 'ready',
    ).length
    const readyFiles = items.filter((item) => item.status === 'ready').length
    const failedFiles = items.filter((item) => item.status === 'failed').length
    const totalBytes = items.reduce((sum, item) => sum + item.sizeBytes, 0)
    const uploadedBytes = items.reduce((sum, item) => {
      if (item.status === 'uploaded' || item.status === 'ready' || item.status === 'finalizing') {
        return sum + item.sizeBytes
      }
      return sum + Math.min(item.uploadedBytes, item.sizeBytes)
    }, 0)

    return {
      totalFiles,
      uploadedFiles,
      readyFiles,
      failedFiles,
      totalBytes,
      uploadedBytes,
      progressPct: totalBytes > 0 ? Math.round((uploadedBytes / totalBytes) * 100) : 0,
    }
  }, [items])

  const hasQueued = items.some((item) => item.status === 'queued')
  const hasRetryableUploadFailure = items.some(
    (item) => item.status === 'aborted' || (item.status === 'failed' && item.uploadedBytes < item.sizeBytes),
  )

  return (
    <div className="upload-page" data-testid="upload-page">
      <div className="upload-shell">
        <header className="upload-header">
          <button
            type="button"
            className="photo-nav-button"
            onClick={() => navigate('/')}
            data-testid="upload-back-wall"
          >
            Back to wall
          </button>
          <h1 className="upload-title">Album Uploads</h1>
        </header>

        <p className="upload-subtitle">
          Queue many ZIP files, upload directly to S3, then process each album automatically.
        </p>

        <div className="upload-actions">
          <button
            type="button"
            className="photo-primary-action"
            onClick={() => fileInputRef.current?.click()}
            data-testid="upload-add-button"
          >
            Add ZIP files
          </button>
          <button
            type="button"
            className="photo-nav-button"
            onClick={() => setQueueRunning(true)}
            disabled={!hasQueued}
            data-testid="upload-start"
          >
            Start uploads
          </button>
          <button
            type="button"
            className="photo-nav-button"
            onClick={() => setQueueRunning(false)}
            disabled={!queueRunning}
            data-testid="upload-pause"
          >
            Pause queue
          </button>
          <button
            type="button"
            className="photo-nav-button"
            onClick={onRetryFailedUploads}
            disabled={!hasRetryableUploadFailure}
            data-testid="upload-retry-failed"
          >
            Retry failed
          </button>
          <button
            type="button"
            className="photo-nav-button"
            onClick={() => navigate('/albums/find')}
            data-testid="upload-go-find"
          >
            Find albums
          </button>
        </div>

        <input
          ref={fileInputRef}
          type="file"
          accept=".zip"
          multiple
          hidden
          onChange={onPickFiles}
          data-testid="upload-pick-input"
        />

        <section className="upload-summary" data-testid="upload-summary">
          <p>
            {summary.uploadedFiles}/{summary.totalFiles} uploaded, {summary.readyFiles} ready,{' '}
            {summary.failedFiles} failed
          </p>
          <p>
            {formatBytes(summary.uploadedBytes)} / {formatBytes(summary.totalBytes)} ({summary.progressPct}%)
          </p>
        </section>

        {pageError && <p className="upload-page-error">{pageError}</p>}

        <div className="upload-list" data-testid="upload-list">
          {items.length === 0 && (
            <p className="upload-empty">No files queued yet. Add one or more ZIP files to begin.</p>
          )}
          {items.map((item) => {
            const progressPct =
              item.sizeBytes > 0 ? Math.min(100, Math.round((item.uploadedBytes / item.sizeBytes) * 100)) : 0
            return (
              <article key={item.id} className="upload-item" data-testid="upload-item">
                <div className="upload-item-head">
                  <p className="upload-item-name">{item.name}</p>
                  <span className={`upload-item-status upload-item-status-${item.status}`} data-testid="upload-status">
                    {statusLabel(item.status)}
                  </span>
                </div>
                <p className="upload-item-meta">
                  {formatBytes(item.sizeBytes)} | {progressPct}% uploaded
                  {item.photoCount !== undefined ? ` | ${item.photoCount} photos indexed` : ''}
                </p>
                {item.error && <p className="upload-item-error">{item.error}</p>}
                <div className="upload-item-actions">
                  {item.status === 'uploading' && (
                    <button
                      type="button"
                      className="photo-nav-button"
                      onClick={() => onCancelItem(item.id)}
                      data-testid="upload-cancel-item"
                    >
                      Cancel
                    </button>
                  )}
                  {item.status === 'failed' && item.albumId && item.uploadedBytes >= item.sizeBytes && (
                    <button
                      type="button"
                      className="photo-nav-button"
                      onClick={() => void onRetryFinalize(item.id, item.albumId!)}
                      data-testid="upload-retry-finalize"
                    >
                      Retry finalize
                    </button>
                  )}
                  {item.albumId && (item.status === 'ready' || item.status === 'uploaded' || item.status === 'finalizing') && (
                    <button
                      type="button"
                      className="photo-nav-button"
                      onClick={() => navigate(`/album/${item.albumId}`)}
                      data-testid="upload-open-album"
                    >
                      Open album
                    </button>
                  )}
                  <button
                    type="button"
                    className="photo-nav-button"
                    onClick={() => onRemoveItem(item.id)}
                    data-testid="upload-remove-item"
                  >
                    Remove
                  </button>
                </div>
              </article>
            )
          })}
        </div>
      </div>
    </div>
  )
}
