import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { createAlbum, finalizeAlbum, uploadAlbumObject } from '../api/client'

type UploadStatus = 'uploading' | 'submitted' | 'failed' | 'canceled'
const uploadWorkerCount = 3

type UploadItem = {
  id: string
  file: File
  name: string
  sizeBytes: number
  status: UploadStatus
  uploadedBytes: number
  albumId?: string
  error?: string
}

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
    case 'uploading':
      return 'Uploading'
    case 'submitted':
      return 'Submitted'
    case 'failed':
      return 'Failed'
    case 'canceled':
      return 'Canceled'
    default:
      return status
  }
}

export function UploadPage() {
  const navigate = useNavigate()
  const fileInputRef = useRef<HTMLInputElement | null>(null)
  const controllersRef = useRef(new Map<string, AbortController>())
  const itemsRef = useRef<UploadItem[]>([])
  const pendingQueueRef = useRef<string[]>([])
  const activeWorkersRef = useRef(0)

  const [items, setItems] = useState<UploadItem[]>([])
  const [pageError, setPageError] = useState<string | null>(null)

  const updateItem = useCallback((itemID: string, next: Partial<UploadItem>) => {
    setItems((prev) => {
      const updated = prev.map((item) => (item.id === itemID ? { ...item, ...next } : item))
      itemsRef.current = updated
      return updated
    })
  }, [])

  useEffect(() => {
    itemsRef.current = items
  }, [items])

  const removeQueuedItem = useCallback((itemID: string): boolean => {
    const index = pendingQueueRef.current.indexOf(itemID)
    if (index < 0) {
      return false
    }
    pendingQueueRef.current.splice(index, 1)
    return true
  }, [])

  const runItemUpload = useCallback(
    async (item: UploadItem) => {
      const controller = new AbortController()
      controllersRef.current.set(item.id, controller)

      let albumID = item.albumId ?? ''
      try {
        const created = await createAlbum(item.file)
        albumID = created.albumId
        updateItem(item.id, { albumId: albumID, status: 'uploading' })

        await uploadAlbumObject(created.uploadUrl, item.file, created.uploadHeaders, controller.signal)
        updateItem(item.id, { uploadedBytes: item.sizeBytes })

        await finalizeAlbum(albumID, { signal: controller.signal })
        updateItem(item.id, { status: 'submitted', error: undefined })
      } catch (err) {
        if (isAbortError(err)) {
          updateItem(item.id, { status: 'canceled', error: undefined })
        } else {
          updateItem(item.id, { status: 'failed', error: toErrorMessage(err) })
        }
      } finally {
        controllersRef.current.delete(item.id)
      }
    },
    [updateItem],
  )

  const runQueuedUpload = useCallback(
    async (itemID: string) => {
      const item = itemsRef.current.find((candidate) => candidate.id === itemID)
      if (!item || item.status !== 'uploading' || controllersRef.current.has(itemID)) {
        return
      }
      await runItemUpload(item)
    },
    [runItemUpload],
  )

  const pumpQueue = useCallback(() => {
    while (activeWorkersRef.current < uploadWorkerCount && pendingQueueRef.current.length > 0) {
      const nextID = pendingQueueRef.current.shift()
      if (!nextID) {
        continue
      }

      const item = itemsRef.current.find((candidate) => candidate.id === nextID)
      if (!item || item.status !== 'uploading' || controllersRef.current.has(nextID)) {
        continue
      }

      activeWorkersRef.current += 1
      void runQueuedUpload(nextID)
        .catch(() => undefined)
        .finally(() => {
          activeWorkersRef.current = Math.max(0, activeWorkersRef.current - 1)
          pumpQueue()
        })
    }
  }, [runQueuedUpload])

  const enqueueUpload = useCallback(
    (itemID: string) => {
      if (controllersRef.current.has(itemID) || pendingQueueRef.current.includes(itemID)) {
        return
      }
      pendingQueueRef.current.push(itemID)
      pumpQueue()
    },
    [pumpQueue],
  )

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
        status: 'uploading',
        uploadedBytes: 0,
      }))

    if (picked.length === 0) {
      setPageError('Select at least one .zip file.')
    } else {
      setPageError(null)
      setItems((prev) => {
        const updated = [...prev, ...picked]
        itemsRef.current = updated
        return updated
      })
      for (const item of picked) {
        enqueueUpload(item.id)
      }
    }

    if (event.target) {
      event.target.value = ''
    }
  }

  const onCancelItem = (itemID: string) => {
    const controller = controllersRef.current.get(itemID)
    if (controller) {
      controller.abort()
      return
    }
    if (removeQueuedItem(itemID)) {
      updateItem(itemID, { status: 'canceled', error: undefined })
    }
  }

  const onRemoveItem = (itemID: string) => {
    const controller = controllersRef.current.get(itemID)
    if (controller) {
      controller.abort()
    }
    removeQueuedItem(itemID)
    setItems((prev) => {
      const updated = prev.filter((item) => item.id !== itemID)
      itemsRef.current = updated
      return updated
    })
  }

  const onRetryFailedUploads = () => {
    const retryable = items.filter((item) => item.status === 'canceled' || item.status === 'failed')
    if (retryable.length === 0) {
      return
    }

    setItems((prev) => {
      const updated = prev.map((item) => {
        const isUploadFailure = item.status === 'canceled' || item.status === 'failed'
        if (!isUploadFailure) return item
        return {
          ...item,
          status: 'uploading',
          uploadedBytes: 0,
          albumId: undefined,
          error: undefined,
        }
      })
      itemsRef.current = updated
      return updated
    })
    for (const item of retryable) {
      removeQueuedItem(item.id)
      enqueueUpload(item.id)
    }
  }

  const summary = useMemo(() => {
    const totalFiles = items.length
    const submittedFiles = items.filter((item) => item.status === 'submitted').length
    const failedFiles = items.filter((item) => item.status === 'failed' || item.status === 'canceled').length
    const totalBytes = items.reduce((sum, item) => sum + item.sizeBytes, 0)
    const uploadedBytes = items.reduce(
      (sum, item) =>
        sum + (item.status === 'submitted' ? item.sizeBytes : Math.min(item.uploadedBytes, item.sizeBytes)),
      0,
    )
    const progressPct = totalBytes > 0 ? Math.round((uploadedBytes / totalBytes) * 100) : 0
    return {
      totalFiles,
      submittedFiles,
      failedFiles,
      totalBytes,
      uploadedBytes,
      progressPct,
    }
  }, [items])

  const hasRetryable = items.some((item) => item.status === 'canceled' || item.status === 'failed')

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
          Upload ZIP files directly to object storage. Files are submitted for background indexing after upload.
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
            onClick={onRetryFailedUploads}
            disabled={!hasRetryable}
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
            {summary.submittedFiles}/{summary.totalFiles} submitted, {summary.failedFiles} failed
          </p>
          <p>
            {formatBytes(summary.uploadedBytes)} / {formatBytes(summary.totalBytes)} ({summary.progressPct}%)
          </p>
          <p>Use Find albums to open albums once indexing is complete.</p>
        </section>

        {pageError && <p className="upload-page-error">{pageError}</p>}

        <div className="upload-list" data-testid="upload-list">
          {items.length === 0 && (
            <p className="upload-empty">No files selected yet. Add one or more ZIP files to begin.</p>
          )}
          {items.map((item) => {
            const pct = item.sizeBytes > 0 ? Math.min(100, Math.round((item.uploadedBytes / item.sizeBytes) * 100)) : 0
            return (
              <article className="upload-item" data-testid="upload-item" key={item.id}>
                <div className="upload-item-head">
                  <p className="upload-item-name">{item.name}</p>
                  <span
                    className={`upload-item-status upload-item-status-${item.status}`}
                    data-testid="upload-status"
                  >
                    {statusLabel(item.status)}
                  </span>
                </div>
                <p className="upload-item-meta">
                  {formatBytes(item.sizeBytes)} | {pct}% uploaded
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
