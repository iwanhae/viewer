import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { Link } from 'react-router-dom'
import { createAlbum, fetchFinalizeStatus, finalizeAlbum } from '../api/client'
import {
  MAX_UPLOAD_BYTES,
  abortError,
  isAbortError,
  uploadAlbumObject,
  type UploadProgress,
} from '../api/upload'
import type { EmbeddingProgress } from '../api/types'
import { PageTopBar } from '../components/PageTopBar'
import { useEmbeddingProgress } from '../hooks/useEmbeddingProgress'
import { formatBytes } from '../utils/format'
import './upload.css'

// A file walks the stages left to right: it waits in the queue, a worker PUTs
// it to storage, the server extracts it, and then the album is ready - the
// server marks an album ready as soon as its images are extracted, and the
// poll loop only stays alive afterwards to refresh the embedding counts.
type UploadStatus = 'queued' | 'uploading' | 'indexing' | 'ready' | 'failed' | 'canceled'

const uploadWorkerCount = 3
const finalizePollIntervalMs = 2000
const finalizePollTimeoutMs = 30 * 60 * 1000
const progressTickMs = 200

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
// to embed. Embedding runs after an album is already visible, so a pending
// count only keeps the informational progress line alive - it never blocks the
// upload from finishing. A disabled embedder means the counts will never move,
// so there is nothing to watch.
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

// delay waits for the poll interval but rejects as soon as the upload's
// AbortController fires, so cancellation is not held up by a pending timer.
function delay(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal.aborted) {
      reject(abortError())
      return
    }
    let timer = 0
    const onAbort = () => {
      window.clearTimeout(timer)
      reject(abortError())
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
    case 'queued':
      return 'Queued'
    case 'uploading':
      return 'Uploading'
    case 'indexing':
      return 'Indexing'
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

function hasDraggedFiles(event: React.DragEvent): boolean {
  return Array.from(event.dataTransfer?.types ?? []).includes('Files')
}

export function UploadPage() {
  const fileInputRef = useRef<HTMLInputElement | null>(null)
  const controllersRef = useRef(new Map<string, AbortController>())
  const itemsRef = useRef<UploadItem[]>([])
  const pendingQueueRef = useRef<string[]>([])
  const activeWorkersRef = useRef(0)
  // XHR progress events fire far more often than React should re-render, so
  // they land in this map and a single ticker flushes it into state.
  const progressRef = useRef(new Map<string, UploadProgress>())
  const dragDepthRef = useRef(0)

  const [items, setItems] = useState<UploadItem[]>([])
  const [notice, setNotice] = useState<string | null>(null)
  const [isDragActive, setIsDragActive] = useState(false)
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

  const isUploading = items.some((item) => item.status === 'uploading')

  useEffect(() => {
    if (!isUploading) return
    const timer = window.setInterval(() => {
      setItems((prev) => {
        let changed = false
        const updated = prev.map((item) => {
          if (item.status !== 'uploading') return item
          const progress = progressRef.current.get(item.id)
          if (!progress) return item
          const bytes = Math.min(progress.bytes, item.sizeBytes)
          if (bytes === item.uploadedBytes) return item
          changed = true
          return { ...item, uploadedBytes: bytes }
        })
        if (changed) {
          itemsRef.current = updated
          return updated
        }
        return prev
      })
    }, progressTickMs)
    return () => window.clearInterval(timer)
  }, [isUploading])

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
        updateItem(item.id, { status: 'uploading', error: undefined })
        const created = await createAlbum(item.file)
        albumID = created.albumId
        updateItem(item.id, { albumId: albumID })

        await uploadAlbumObject(created.uploadUrl, item.file, created.uploadHeaders, {
          signal: controller.signal,
          onProgress: (progress) => {
            progressRef.current.set(item.id, progress)
          },
        })
        progressRef.current.delete(item.id)
        updateItem(item.id, { uploadedBytes: item.sizeBytes, status: 'indexing' })

        await finalizeAlbum(albumID, { signal: controller.signal })

        // Indexing runs in the background pipeline: keep polling until the
        // album succeeds or fails. Success is what makes the album visible, so
        // the item turns ready right away - embedding is a slower stage that
        // continues afterwards on the server, and the loop only stays alive to
        // refresh the embedded counts until nothing is left pending. A deadline
        // just stops that refresh; the item still reports ready.
        const deadline = Date.now() + finalizePollTimeoutMs
        for (;;) {
          const state = await fetchFinalizeStatus(albumID, { signal: controller.signal })
          if (state.status === 'FAILED') {
            updateItem(item.id, { status: 'failed', error: state.error })
            return
          }
          if (state.status === 'SUCCEEDED') {
            updateItem(item.id, { status: 'ready', embedding: state.embedding, error: undefined })
            if (!isEmbeddingPending(state.embedding)) {
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
        progressRef.current.delete(item.id)
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
      if (!item || item.status !== 'queued' || controllersRef.current.has(itemID)) {
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
      if (!item || item.status !== 'queued' || controllersRef.current.has(nextID)) {
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

  const ingestFiles = useCallback(
    (files: File[]) => {
      if (files.length === 0) return

      const accepted: UploadItem[] = []
      const rejectedNonZip: string[] = []
      const rejectedOversize: string[] = []
      for (const file of files) {
        if (!file.name.toLowerCase().endsWith('.zip')) {
          rejectedNonZip.push(file.name)
          continue
        }
        if (file.size > MAX_UPLOAD_BYTES) {
          rejectedOversize.push(file.name)
          continue
        }
        accepted.push({
          id: nextItemID(),
          file,
          name: file.name,
          sizeBytes: file.size,
          status: 'queued',
          uploadedBytes: 0,
        })
      }

      const rejections: string[] = []
      if (rejectedNonZip.length > 0) {
        const names = rejectedNonZip.slice(0, 3).join(', ')
        const suffix = rejectedNonZip.length > 3 ? ` and ${rejectedNonZip.length - 3} more` : ''
        rejections.push(`Skipped ${rejectedNonZip.length} non-ZIP file${rejectedNonZip.length > 1 ? 's' : ''}: ${names}${suffix}`)
      }
      for (const name of rejectedOversize.slice(0, 3)) {
        rejections.push(`${name} is over the 1 GiB limit`)
      }

      // itemsRef is the upload queue's source of truth and pumpQueue runs
      // synchronously in the loop below, so the ref must be updated before
      // enqueueing. setItems' updater may be deferred by React's batching,
      // and a stale ref makes pumpQueue drop the ids it cannot resolve - the
      // upload then never starts and no request is sent.
      if (accepted.length > 0) {
        const updated = [...itemsRef.current, ...accepted]
        itemsRef.current = updated
        setItems(updated)
        for (const item of accepted) {
          enqueueUpload(item.id)
        }
      }
      setNotice(rejections.length > 0 ? rejections.join(' ') : null)
    },
    [enqueueUpload],
  )

  const onPickFiles = (event: React.ChangeEvent<HTMLInputElement>) => {
    ingestFiles(Array.from(event.target.files ?? []))
    if (event.target) {
      event.target.value = ''
    }
  }

  const onDragEnter = (event: React.DragEvent<HTMLDivElement>) => {
    if (!hasDraggedFiles(event)) return
    event.preventDefault()
    dragDepthRef.current += 1
    setIsDragActive(true)
  }

  const onDragOver = (event: React.DragEvent<HTMLDivElement>) => {
    if (!hasDraggedFiles(event)) return
    // preventDefault is what marks the page as a drop target at all.
    event.preventDefault()
  }

  const onDragLeave = (event: React.DragEvent<HTMLDivElement>) => {
    if (!hasDraggedFiles(event)) return
    // Child boundaries fire dragleave too, so only the last one closes.
    dragDepthRef.current = Math.max(0, dragDepthRef.current - 1)
    if (dragDepthRef.current === 0) {
      setIsDragActive(false)
    }
  }

  const onDrop = (event: React.DragEvent<HTMLDivElement>) => {
    if (!hasDraggedFiles(event)) return
    event.preventDefault()
    dragDepthRef.current = 0
    setIsDragActive(false)
    ingestFiles(Array.from(event.dataTransfer.files ?? []))
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
    progressRef.current.delete(itemID)
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

    // Same invariant as ingestFiles: pumpQueue reads itemsRef.current
    // synchronously right after this, so the ref leads and state follows.
    const updated = itemsRef.current.map((item) => {
      const isUploadFailure = item.status === 'canceled' || item.status === 'failed'
      if (!isUploadFailure) return item
      return {
        ...item,
        status: 'queued' as const,
        uploadedBytes: 0,
        albumId: undefined,
        embedding: undefined,
        error: undefined,
      }
    })
    itemsRef.current = updated
    setItems(updated)
    for (const item of retryable) {
      removeQueuedItem(item.id)
      enqueueUpload(item.id)
    }
  }

  const summary = useMemo(() => {
    const totalFiles = items.length
    const isDone = (status: UploadStatus) => status === 'ready'
    const doneFiles = items.filter((item) => isDone(item.status)).length
    const failedFiles = items.filter((item) => item.status === 'failed' || item.status === 'canceled').length
    const totalBytes = items.reduce((sum, item) => sum + item.sizeBytes, 0)
    const uploadedBytes = items.reduce(
      (sum, item) =>
        sum + (isDone(item.status) ? item.sizeBytes : Math.min(item.uploadedBytes, item.sizeBytes)),
      0,
    )
    const progressPct = totalBytes > 0 ? Math.round((uploadedBytes / totalBytes) * 100) : 0
    return {
      totalFiles,
      doneFiles,
      failedFiles,
      totalBytes,
      uploadedBytes,
      progressPct,
    }
  }, [items])

  const hasRetryable = items.some((item) => item.status === 'canceled' || item.status === 'failed')

  return (
    <div
      className="upload-page"
      data-testid="upload-page"
      onDragEnter={onDragEnter}
      onDragOver={onDragOver}
      onDragLeave={onDragLeave}
      onDrop={onDrop}
    >
      <div className="upload-shell">
        <PageTopBar
          title="Upload"
          actions={
            <Link className="photo-nav-button" to="/albums/find" data-testid="upload-go-find">
              Find albums
            </Link>
          }
        />

        <input
          ref={fileInputRef}
          type="file"
          accept=".zip"
          multiple
          hidden
          onChange={onPickFiles}
          data-testid="upload-pick-input"
        />

        {items.length === 0 ? (
          <button
            type="button"
            className={`upload-hero${isDragActive ? ' is-active' : ''}`}
            onClick={() => fileInputRef.current?.click()}
            data-testid="upload-dropzone"
          >
            <span className="upload-hero-title">Drop ZIP files here</span>
            <span className="upload-hero-sub">or browse files</span>
            <span className="upload-hero-meta tnum">Each ZIP becomes one album · up to 1 GiB per file</span>
          </button>
        ) : (
          <div className="upload-toolbar">
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
          </div>
        )}

        {notice && (
          <p className="upload-notice" role="status" data-testid="upload-notices">
            {notice}
          </p>
        )}

        {items.length > 0 && (
          <section className="upload-summary" data-testid="upload-summary">
            <div className="upload-summary-head tnum">
              <p>
                {summary.doneFiles}/{summary.totalFiles} ready
                {summary.failedFiles > 0 ? `, ${summary.failedFiles} failed` : ''}
              </p>
              <p>
                {formatBytes(summary.uploadedBytes)} / {formatBytes(summary.totalBytes)} ({summary.progressPct}%)
              </p>
            </div>
            <div className="progress">
              <div className="progress-bar" style={{ width: `${summary.progressPct}%` }} />
            </div>
            {embedding && embedding.enabled && embedding.pending > 0 && (
              <p className="upload-summary-embedding tnum" data-testid="upload-embedding-summary">
                Embedding {embedding.ready}/{embedding.total} images ({Math.round(embedding.ratio * 100)}%)
                {embedding.failed > 0 ? `, ${embedding.failed} failed` : ''}
              </p>
            )}
          </section>
        )}

        <div className="upload-list" data-testid="upload-list">
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
                <p className="upload-item-meta tnum">
                  {formatBytes(item.sizeBytes)}
                  {item.status === 'uploading' ? ` · ${pct}% uploaded` : ''}
                  {item.embedding && item.embedding.enabled && item.embedding.total > 0
                    ? ` · embedded ${item.embedding.ready}/${item.embedding.total}${
                        item.embedding.failed > 0 ? `, ${item.embedding.failed} failed` : ''
                      }`
                    : ''}
                  {item.embedding && !item.embedding.enabled && item.embedding.pending > 0
                    ? ' · embeddings unavailable on this server'
                    : ''}
                </p>
                {item.status === 'uploading' && (
                  <div
                    className="progress"
                    data-testid="upload-item-progress"
                    role="progressbar"
                    aria-label={`Uploading ${item.name}`}
                    aria-valuemin={0}
                    aria-valuemax={100}
                    aria-valuenow={pct}
                  >
                    <div className="progress-bar" style={{ width: `${pct}%` }} />
                  </div>
                )}
                {item.status === 'indexing' && (
                  <div
                    className="progress is-indeterminate"
                    data-testid="upload-item-indexing"
                    role="progressbar"
                    aria-label={`Indexing ${item.name}`}
                  >
                    <div className="progress-bar" />
                  </div>
                )}
                {item.embedding && isEmbeddingPending(item.embedding) && (
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
                  {(item.status === 'queued' || item.status === 'uploading') && (
                    <button
                      type="button"
                      className="photo-nav-button"
                      onClick={() => onCancelItem(item.id)}
                      data-testid="upload-cancel-item"
                    >
                      Cancel
                    </button>
                  )}
                  {item.status === 'ready' && item.albumId && (
                    <Link
                      className="photo-nav-button"
                      to={`/album/${item.albumId}`}
                      data-testid="upload-open-album"
                    >
                      Open album
                    </Link>
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

      {isDragActive && (
        <div className="upload-drop-overlay" data-testid="upload-drop-overlay" aria-hidden="true">
          <p className="upload-drop-overlay-label">Drop to add ZIP files</p>
        </div>
      )}
    </div>
  )
}
