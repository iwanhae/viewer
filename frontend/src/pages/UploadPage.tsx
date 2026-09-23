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
import { PageTopBar } from '../components/PageTopBar'
import { formatBytes } from '../utils/format'
import './upload.css'

// A file walks the stages left to right: it waits in the queue, the pump PUTs
// it to storage, the server extracts it, and then the album is ready - the
// server marks an album ready as soon as its images are extracted.
type UploadStatus = 'queued' | 'uploading' | 'indexing' | 'ready' | 'failed' | 'canceled'

// The pump owns a single slot: one zip's bytes move to storage at a time.
// Everything after the PUT (finalize and status polling) runs detached, so a
// slow extraction never decides when the next file may start uploading.
// Finalize only moves an already-uploaded zip from "waits for the scan" to "in
// the queue now"; the upload scan queues any staged zip within a minute anyway,
// so a slow or parked finalize is dropped instead of holding a connection open.
const finalizeRequestTimeoutMs = 15 * 1000
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
  error?: string
}

const statusLabels: Record<UploadStatus, string> = {
  queued: 'Queued', uploading: 'Uploading', indexing: 'Indexing',
  ready: 'Ready', failed: 'Failed', canceled: 'Canceled',
}

function nextItemID(): string {
  if (typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function') {
    return crypto.randomUUID()
  }
  return `${Date.now()}-${Math.random().toString(16).slice(2)}`
}

function toErrorMessage(err: unknown): string {
  return err instanceof Error ? err.message : 'request failed'
}

// delay waits for the poll interval but rejects as soon as the upload's
// AbortController fires, so cancellation is not held up by a pending timer.
function delay(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal.aborted) return reject(abortError())
    let timer = 0
    const onAbort = () => { window.clearTimeout(timer); reject(abortError()) }
    timer = window.setTimeout(() => {
      signal.removeEventListener('abort', onAbort)
      resolve()
    }, ms)
    signal.addEventListener('abort', onAbort, { once: true })
  })
}

// revive resets a failed or canceled item for another run through the pump.
function revive(item: UploadItem): UploadItem {
  return { ...item, status: 'queued', uploadedBytes: 0, albumId: undefined, error: undefined }
}

function hasDraggedFiles(event: React.DragEvent): boolean {
  return Array.from(event.dataTransfer?.types ?? []).includes('Files')
}

export function UploadPage() {
  const fileInputRef = useRef<HTMLInputElement | null>(null)
  const controllersRef = useRef(new Map<string, AbortController>())
  // The queue holds whole UploadItem snapshots, so enqueueing never reads
  // component state; activeItemRef is the single slot - the id of the item
  // whose PUT is in flight, null while it is free.
  const pendingQueueRef = useRef<UploadItem[]>([])
  const activeItemRef = useRef<string | null>(null)
  // XHR progress events fire far more often than React should re-render, so
  // they land in this map and a single ticker flushes it into state.
  const progressRef = useRef(new Map<string, UploadProgress>())
  const dragDepthRef = useRef(0)

  const [items, setItems] = useState<UploadItem[]>([])
  const [notice, setNotice] = useState<string | null>(null)
  const [isDragActive, setIsDragActive] = useState(false)

  const updateItem = useCallback((itemID: string, next: Partial<UploadItem>) => {
    setItems((prev) => prev.map((item) => (item.id === itemID ? { ...item, ...next } : item)))
  }, [])

  const isUploading = items.some((item) => item.status === 'uploading')

  useEffect(() => {
    if (!isUploading) return
    const timer = window.setInterval(() => {
      // Single flight: at most one row is ever uploading, so a tick flushes
      // at most one item's progress into state.
      setItems((prev) => {
        const active = prev.find((item) => item.status === 'uploading')
        const progress = active ? progressRef.current.get(active.id) : undefined
        if (!active || !progress) return prev
        const bytes = Math.min(progress.bytes, active.sizeBytes)
        if (bytes === active.uploadedBytes) return prev
        return prev.map((item) => (item.id === active.id ? { ...item, uploadedBytes: bytes } : item))
      })
    }, progressTickMs)
    return () => window.clearInterval(timer)
  }, [isUploading])

  const releaseController = useCallback((itemID: string, controller: AbortController) => {
    // Only drop the entry this run owns: a retried item may already have
    // installed a fresh controller, and deleting that one would leave the
    // retry uncancellable.
    if (controllersRef.current.get(itemID) === controller) {
      controllersRef.current.delete(itemID)
    }
  }, [])

  // watchIndexing follows one uploaded album until it is ready. It runs outside
  // the upload slot on purpose: the zip is durable in the bucket the moment the
  // PUT returns and the server indexes it on its own, so extraction speed must
  // not decide when the next file may start uploading. A run that used to sit
  // here made a batch of ten uploads crawl - only the first few ever moved
  // while the rest waited for someone to finish indexing.
  const watchIndexing = useCallback(
    async (itemID: string, albumID: string, controller: AbortController) => {
      try {
        // Best effort: the finalize request itself can park on a busy server,
        // so it gets its own short deadline and nobody awaits it. Polling and
        // the one-minute upload scan cover the album either way.
        void finalizeAlbum(albumID, {
          signal: AbortSignal.any([controller.signal, AbortSignal.timeout(finalizeRequestTimeoutMs)]),
        }).catch(() => undefined)

        // Keep polling until the album succeeds or fails: success is what
        // makes it visible, so the item turns ready right away and the poll
        // ends with it. The deadline only bounds the loop when the server
        // stays stuck between QUEUED and SUCCEEDED.
        const deadline = Date.now() + finalizePollTimeoutMs
        for (;;) {
          const state = await fetchFinalizeStatus(albumID, { signal: controller.signal })
          if (state.status === 'FAILED') { updateItem(itemID, { status: 'failed', error: state.error }); return }
          if (state.status === 'SUCCEEDED') { updateItem(itemID, { status: 'ready', error: undefined }); return }
          if (Date.now() >= deadline) return
          await delay(finalizePollIntervalMs, controller.signal)
        }
      } catch (err) {
        // An abort here ends a canceled poll only - the zip itself is durable
        // in the bucket, so the album may still become ready server-side.
        if (isAbortError(err)) {
          updateItem(itemID, { status: 'canceled', error: undefined })
        } else {
          updateItem(itemID, { status: 'failed', error: toErrorMessage(err) })
        }
      } finally {
        releaseController(itemID, controller)
      }
    },
    [releaseController, updateItem],
  )

  // runUpload owns the slot only while the bytes move: it registers the album,
  // PUTs the zip to the presigned URL, and hands the album to a detached
  // watcher. It resolves right after the handoff, which is what lets the pump
  // start the next PUT while this album is still indexing.
  const runUpload = useCallback(
    async (item: UploadItem) => {
      const controller = new AbortController()
      controllersRef.current.set(item.id, controller)

      let albumID = item.albumId ?? ''
      try {
        updateItem(item.id, { status: 'uploading', error: undefined })
        const created = await createAlbum(item.file)
        albumID = created.albumId
        updateItem(item.id, { albumId: albumID })

        await uploadAlbumObject(created.uploadUrl, item.file, {
          signal: controller.signal,
          // Progress lands in the ref map; the 200ms ticker moves it into state.
          onProgress: (progress) => progressRef.current.set(item.id, progress),
        })
        progressRef.current.delete(item.id)
        updateItem(item.id, { uploadedBytes: item.sizeBytes, status: 'indexing' })
        void watchIndexing(item.id, albumID, controller)
      } catch (err) {
        // Release before the state flip: once the row reads failed/canceled a
        // retry may run, and it must not trip over a leftover controller.
        releaseController(item.id, controller)
        progressRef.current.delete(item.id)
        if (isAbortError(err)) {
          updateItem(item.id, { status: 'canceled', error: undefined })
        } else {
          updateItem(item.id, { status: 'failed', error: toErrorMessage(err) })
        }
      }
    },
    [releaseController, updateItem, watchIndexing],
  )

  // pumpQueue starts the next queued snapshot whenever the slot is free. It is
  // called ONLY from event handlers (file ingest, retry, cancel, remove) and
  // from runUpload's .finally continuation - never from a useEffect: React
  // StrictMode double-invokes effects on remount, and an effect-driven pump
  // would abort the PUT it already started.
  const pumpQueue = useCallback(() => {
    if (activeItemRef.current !== null) return
    const snapshot = pendingQueueRef.current.shift()
    if (!snapshot) return
    activeItemRef.current = snapshot.id
    void runUpload(snapshot).finally(() => {
      activeItemRef.current = null
      pumpQueue()
    })
  }, [runUpload])

  const enqueueUpload = useCallback(
    (snapshot: UploadItem) => {
      // Duplicate guard: skip an id already in flight, already waiting, or
      // still finishing under its previous run's controller.
      const waiting = pendingQueueRef.current.some((queued) => queued.id === snapshot.id)
      if (activeItemRef.current === snapshot.id || waiting || controllersRef.current.has(snapshot.id)) {
        return
      }
      pendingQueueRef.current.push(snapshot)
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
        } else if (file.size > MAX_UPLOAD_BYTES) {
          rejectedOversize.push(file.name)
        } else {
          accepted.push({ id: nextItemID(), file, name: file.name, sizeBytes: file.size, status: 'queued', uploadedBytes: 0 })
        }
      }

      const rejections: string[] = []
      if (rejectedNonZip.length > 0) {
        const names = rejectedNonZip.slice(0, 3).join(', ')
        const more = rejectedNonZip.length > 3 ? ` and ${rejectedNonZip.length - 3} more` : ''
        rejections.push(
          `Skipped ${rejectedNonZip.length} non-ZIP file${rejectedNonZip.length > 1 ? 's' : ''}: ${names}${more}`,
        )
      }
      for (const name of rejectedOversize.slice(0, 3)) {
        rejections.push(`${name} is over the 1 GiB limit`)
      }

      // The queue stores snapshots, so React batching cannot lose an enqueue:
      // state only mirrors the queue for rendering, and the pump starts the
      // first file synchronously right here in the handler.
      if (accepted.length > 0) {
        setItems((prev) => [...prev, ...accepted])
        for (const item of accepted) enqueueUpload(item)
      }
      setNotice(rejections.length > 0 ? rejections.join(' ') : null)
    },
    [enqueueUpload],
  )

  const onPickFiles = (event: React.ChangeEvent<HTMLInputElement>) => {
    ingestFiles(Array.from(event.target.files ?? []))
    // Reset so picking the same file again re-fires onChange.
    event.target.value = ''
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
    if (dragDepthRef.current === 0) setIsDragActive(false)
  }

  const onDrop = (event: React.DragEvent<HTMLDivElement>) => {
    if (!hasDraggedFiles(event)) return
    event.preventDefault()
    dragDepthRef.current = 0
    setIsDragActive(false)
    ingestFiles(Array.from(event.dataTransfer.files ?? []))
  }

  // Cancel and Remove deliberately do different things to the queue.
  const onCancelItem = (itemID: string) => {
    if (activeItemRef.current === itemID) {
      // Whole-batch stop: abort the in-flight PUT (its catch marks this item
      // canceled) and mark every still-queued item canceled with it.
      controllersRef.current.get(itemID)?.abort()
      pendingQueueRef.current = []
      setItems((prev) =>
        prev.map((item) => (item.status === 'queued' ? { ...item, status: 'canceled', error: undefined } : item)),
      )
      return
    }
    const controller = controllersRef.current.get(itemID)
    if (controller) {
      // A detached watcher is indexing this album: aborting ends its poll
      // only. The album may still become ready on the server even though the
      // row now shows canceled.
      controller.abort()
      return
    }
    const index = pendingQueueRef.current.findIndex((queued) => queued.id === itemID)
    if (index >= 0) {
      pendingQueueRef.current.splice(index, 1)
      updateItem(itemID, { status: 'canceled', error: undefined })
    }
  }

  // Remove acts on one row only: abort its PUT or watcher, drop just its own
  // queue entry, and let the pump continue with the next snapshot.
  const onRemoveItem = (itemID: string) => {
    controllersRef.current.get(itemID)?.abort()
    const index = pendingQueueRef.current.findIndex((queued) => queued.id === itemID)
    if (index >= 0) pendingQueueRef.current.splice(index, 1)
    progressRef.current.delete(itemID)
    setItems((prev) => prev.filter((item) => item.id !== itemID))
  }

  const onRetryFailedUploads = () => {
    const retryable = items.filter((item) => item.status === 'canceled' || item.status === 'failed')
    if (retryable.length === 0) return
    // Each retry enqueues a fresh snapshot of its item, so it re-enters the
    // pump exactly like a newly picked file.
    setItems((prev) => prev.map((item) => (item.status === 'canceled' || item.status === 'failed' ? revive(item) : item)))
    for (const item of retryable) enqueueUpload(revive(item))
  }

  const summary = useMemo(() => {
    const totalBytes = items.reduce((sum, item) => sum + item.sizeBytes, 0)
    const uploadedBytes = items.reduce(
      (sum, item) => sum + (item.status === 'ready' ? item.sizeBytes : Math.min(item.uploadedBytes, item.sizeBytes)),
      0,
    )
    return {
      totalFiles: items.length,
      doneFiles: items.filter((item) => item.status === 'ready').length,
      failedFiles: items.filter((item) => item.status === 'failed' || item.status === 'canceled').length,
      totalBytes,
      uploadedBytes,
      progressPct: totalBytes > 0 ? Math.round((uploadedBytes / totalBytes) * 100) : 0,
    }
  }, [items])

  const hasRetryable = items.some((item) => item.status === 'canceled' || item.status === 'failed')

  return (
    <div className="upload-page" data-testid="upload-page" onDragEnter={onDragEnter} onDragOver={onDragOver} onDragLeave={onDragLeave} onDrop={onDrop}>
      <div className="upload-shell">
        <PageTopBar
          title="Upload"
          actions={<Link className="photo-nav-button" to="/albums/find" data-testid="upload-go-find">Find albums</Link>}
        />

        <input ref={fileInputRef} type="file" accept=".zip" multiple hidden onChange={onPickFiles} data-testid="upload-pick-input" />

        {items.length === 0 ? (
          <button type="button" className={`upload-hero${isDragActive ? ' is-active' : ''}`} onClick={() => fileInputRef.current?.click()} data-testid="upload-dropzone">
            <span className="upload-hero-title">Drop ZIP files here</span>
            <span className="upload-hero-sub">or browse files</span>
            <span className="upload-hero-meta tnum">Each ZIP becomes one album · up to 1 GiB per file</span>
          </button>
        ) : (
          <div className="upload-toolbar">
            <button type="button" className="photo-primary-action" onClick={() => fileInputRef.current?.click()} data-testid="upload-add-button">
              Add ZIP files
            </button>
            <button type="button" className="photo-nav-button" onClick={onRetryFailedUploads} disabled={!hasRetryable} data-testid="upload-retry-failed">
              Retry failed
            </button>
          </div>
        )}

        {notice && (
          <p className="upload-notice" role="status" data-testid="upload-notices">{notice}</p>
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
          </section>
        )}

        <div className="upload-list" data-testid="upload-list">
          {items.map((item) => {
            const pct = item.sizeBytes > 0 ? Math.min(100, Math.round((item.uploadedBytes / item.sizeBytes) * 100)) : 0
            return (
              <article className="upload-item" data-testid="upload-item" key={item.id}>
                <div className="upload-item-head">
                  <p className="upload-item-name">{item.name}</p>
                  <span className={`upload-item-status upload-item-status-${item.status}`} data-testid="upload-status">
                    {statusLabels[item.status]}
                  </span>
                </div>
                <p className="upload-item-meta tnum">
                  {formatBytes(item.sizeBytes)}
                  {item.status === 'uploading' ? ` · ${pct}% uploaded` : ''}
                </p>
                {item.status === 'uploading' && (
                  <div className="progress" data-testid="upload-item-progress" role="progressbar" aria-label={`Uploading ${item.name}`} aria-valuemin={0} aria-valuemax={100} aria-valuenow={pct}>
                    <div className="progress-bar" style={{ width: `${pct}%` }} />
                  </div>
                )}
                {item.status === 'indexing' && (
                  <div className="progress is-indeterminate" data-testid="upload-item-indexing" role="progressbar" aria-label={`Indexing ${item.name}`}>
                    <div className="progress-bar" />
                  </div>
                )}
                {item.error && <p className="upload-item-error">{item.error}</p>}
                <div className="upload-item-actions">
                  {(item.status === 'queued' || item.status === 'uploading' || item.status === 'indexing') && (
                    <button type="button" className="photo-nav-button" onClick={() => onCancelItem(item.id)} data-testid="upload-cancel-item">
                      Cancel
                    </button>
                  )}
                  {item.status === 'ready' && item.albumId && (
                    <Link className="photo-nav-button" to={`/album/${item.albumId}`} data-testid="upload-open-album">
                      Open album
                    </Link>
                  )}
                  <button type="button" className="photo-nav-button" onClick={() => onRemoveItem(item.id)} data-testid="upload-remove-item">
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
