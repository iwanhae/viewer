export class ApiError extends Error {
  readonly status: number
  readonly code: string
  readonly details?: unknown

  constructor(status: number, code: string, message: string, details?: unknown) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.code = code
    this.details = details
  }
}

type ErrorEnvelope = {
  error?: {
    code?: string
    message?: string
    details?: unknown
  }
}

function parseJson(text: string): unknown {
  try {
    return JSON.parse(text) as unknown
  } catch {
    return null
  }
}

function toApiError(res: Response, bodyText: string): ApiError {
  const parsed = bodyText ? (parseJson(bodyText) as ErrorEnvelope | null) : null
  const code = parsed?.error?.code || `HTTP_${res.status}`
  const message = parsed?.error?.message || `request failed: ${res.status}`
  return new ApiError(res.status, code, message, parsed?.error?.details)
}

export async function requestJSON<T>(input: RequestInfo | URL, init?: RequestInit): Promise<T> {
  const res = await fetch(input, init)
  const text = await res.text()

  if (!res.ok) {
    throw toApiError(res, text)
  }

  if (!text) {
    throw new ApiError(res.status, 'EMPTY_RESPONSE', 'empty response body')
  }

  const parsed = parseJson(text)
  if (parsed === null) {
    throw new ApiError(res.status, 'INVALID_JSON', 'invalid JSON response')
  }

  return parsed as T
}

export async function ensureOK(input: RequestInfo | URL, init?: RequestInit): Promise<void> {
  const res = await fetch(input, init)
  if (!res.ok) {
    const text = await res.text()
    throw toApiError(res, text)
  }
}
