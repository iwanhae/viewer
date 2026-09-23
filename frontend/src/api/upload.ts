import { apiErrorFromResponse } from './http'

// The server rejects anything bigger when the album is registered; checking
// here keeps that failure local instead of one round trip away.
export const MAX_UPLOAD_BYTES = 1024 * 1024 * 1024

export type UploadProgress = { bytes: number; total: number }

export function abortError(): Error {
  if (typeof DOMException !== 'undefined') {
    return new DOMException('Aborted', 'AbortError')
  }
  const err = new Error('Aborted')
  err.name = 'AbortError'
  return err
}

export function isAbortError(err: unknown): boolean {
  return err instanceof Error && err.name === 'AbortError'
}

type UploadAlbumObjectOptions = {
  signal?: AbortSignal
  onProgress?: (progress: UploadProgress) => void
}

// uploadAlbumObject PUTs a file straight to the presigned URL. It uses XHR
// because fetch cannot report upload progress, and the progress bar is the
// point. The presigned PUT needs no extra headers: the signature pins only the
// Host header the URL itself carries.
export function uploadAlbumObject(
  uploadURL: string,
  file: File,
  options: UploadAlbumObjectOptions = {},
): Promise<void> {
  return new Promise((resolve, reject) => {
    if (options.signal?.aborted) {
      reject(abortError())
      return
    }
    const xhr = new XMLHttpRequest()
    xhr.open('PUT', uploadURL, true)
    xhr.upload.onprogress = (event) => {
      if (!event.lengthComputable) return
      options.onProgress?.({ bytes: event.loaded, total: event.total })
    }
    xhr.onload = () => {
      if (xhr.status >= 200 && xhr.status < 300) {
        resolve()
        return
      }
      reject(apiErrorFromResponse(xhr.status, xhr.responseText))
    }
    xhr.onerror = () => reject(new Error('network error during upload'))
    xhr.onabort = () => reject(abortError())
    options.signal?.addEventListener('abort', () => xhr.abort(), { once: true })
    xhr.send(file)
  })
}
