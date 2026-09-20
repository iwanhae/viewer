import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { createAlbum, fetchFinalizeStatus, finalizeAlbum, uploadAlbumObject } from '../api/client'
import type { EmbeddingProgress } from '../api/types'
import { useEmbeddingProgress } from '../hooks/useEmbeddingProgress'
import { formatBytes } from '../utils/format'

type UploadStatus = 'uploading' | 'submitted' | 'embedding' | 'ready' | 'failed' | 'canceled'
const uploadWorkerCount = 3
const finalizePollIntervalMs = 2000
const finalizePollTimeoutMs = 30 * 60 * 1000

type UploadItem = {
  id: string
  file: File
  name: string
  sizeBytes: number
  status: UploadStatus
  uploadedBytes: number
  albumId?: string
  embedding?: EmbeddingProgress
  error?: string
}

// isEmbeddingPending reports whether the server still has images of this album
// to embed. A disabled embedder means the counts will never move, so there is
// nothing to wait for.
function isEmbeddingPending(progress?: EmbeddingProgress): boolean {
  return progress !== undefined && progress.enabled && progress.pending > 0
}

function embeddingPercent(progress?: EmbeddingProgress): number {
  if (!progress || progress.total <= 0) return 0
  return Math.min(100, Math.round((progress.ready / progress.total) * 100))
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

function abortedError(): Error {
  if (typeof DOMException !== 'undefined') {
    return new DOMException('Aborted', 'AbortError')
  }
  const err = new Error('Aborted')
  err.name = 'AbortError'
  return err
}

// delay waits for the poll interval but rejects as soon as the upload's
// AbortController fires, so cancellation is not held up by a pending timer.
function delay(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal.aborted) {
      reject(abortedError())
      return
    }
    let timer = 0
    const onAbort = () => {
      window.clearTimeout(timer)
      reject(abortedError())
    }
    timer = window.setTimeout(() => {
      signal.removeEventListener('abort', onAbort)
      resolve()
    }, ms)
    signal.addEventListener('abort', onAbort, { once: true })
  })
}

function statusLabel(status: UploadStatus): string {
  switch (status) {
    case 'uploading':
      return 'Uploading'
    case 'submitted':
      return 'Submitted'
    case 'embedding':
      return 'Embedding'
    case 'ready':
      return 'Ready'
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
  const embedding = useEmbeddingProgress()

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

        // Indexing runs in the background pipeline: keep polling until the
        // album succeeds or fails. Embedding is a second, slower stage that
        // continues after the album is indexed, so an indexed album stays
        // "embedding" until the server reports nothing left pending. A deadline
        // leaves the item submitted rather than falsely reporting a failure.
        const deadline = Date.now() + finalizePollTimeoutMs
        for (;;) {
          const state = await fetchFinalizeStatus(albumID, { signal: controller.signal })
          if (state.status === 'FAILED') {
            updateItem(item.id, { status: 'failed', error: state.error })
            return
          }
          if (state.status === 'SUCCEEDED') {
            if (isEmbeddingPending(state.embedding)) {
              updateItem(item.id, { status: 'embedding', embedding: state.embedding, error: undefined })
            } else {
              updateItem(item.id, { status: 'ready', embedding: state.embedding, error: undefined })
              return
            }
          } else if (state.embedding) {
            updateItem(item.id, { embedding: state.embedding })
          }
          if (Date.now() >= deadline) {
            return
          }
          await delay(finalizePollIntervalMs, controller.signal)
        }
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
    const isSubmitted = (status: UploadStatus) => status === 'submitted' || status === 'ready'
    const submittedFiles = items.filter((item) => isSubmitted(item.status)).length
    const failedFiles = items.filter((item) => item.status === 'failed' || item.status === 'canceled').length
    const totalBytes = items.reduce((sum, item) => sum + item.sizeBytes, 0)
    const uploadedBytes = items.reduce(
      (sum, item) =>
        sum + (isSubmitted(item.status) ? item.sizeBytes : Math.min(item.uploadedBytes, item.sizeBytes)),
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
          {embedding && embedding.enabled && embedding.pending > 0 && (
            <p data-testid="upload-embedding-summary">
              Embedding {embedding.ready}/{embedding.total} images ({Math.round(embedding.ratio * 100)}%)
              {embedding.failed > 0 ? `, ${embedding.failed} failed` : ''}
            </p>
          )}
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
                  {item.embedding && item.embedding.enabled && item.embedding.total > 0
                    ? ` | embedded ${item.embedding.ready}/${item.embedding.total}${
                        item.embedding.failed > 0 ? `, ${item.embedding.failed} failed` : ''
                      }`
                    : ''}
                  {item.embedding && !item.embedding.enabled && item.embedding.pending > 0
                    ? ' | embeddings unavailable on this server'
                    : ''}
                </p>
                {item.status === 'embedding' && (
                  <div
                    className="progress"
                    data-testid="upload-embedding-progress"
                    role="progressbar"
                    aria-label="Embedding progress"
                    aria-valuemin={0}
                    aria-valuemax={item.embedding?.total ?? 0}
                    aria-valuenow={item.embedding?.ready ?? 0}
                  >
                    <div className="progress-bar" style={{ width: `${embeddingPercent(item.embedding)}%` }} />
                  </div>
                )}
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
