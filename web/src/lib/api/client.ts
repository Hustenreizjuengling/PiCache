// Fetch wrapper for /api/v1: same-origin credentials, JSON bodies, typed
// errors and global handling of expired sessions (401) and unknown host
// names (421).

import type { ErrorCode } from './types'

/** Query parameters; undefined, null and '' are skipped, arrays repeat the key. */
export type Query = Record<string, string | number | boolean | undefined | null | readonly (string | number)[]>

/** Options every endpoint function accepts. */
export interface ReqOpts {
  signal?: AbortSignal
}

export interface RequestOptions extends ReqOpts {
  query?: Query
  /** JSON request body. */
  body?: unknown
  /** Raw request body, sent as application/octet-stream (restore). */
  raw?: Blob
  /** Timeout in ms (default 30 s; 0 = none). */
  timeoutMs?: number
  /** Do not treat 401 as "session expired" (status, login, setup). */
  allowUnauthorized?: boolean
  /** Extra request headers (e.g. the password confirmation of a restore). */
  headers?: Record<string, string>
}

/** An API or transport error. `field` names the invalid input (e.g. "dns.upstreams[1]"). */
export class ApiError extends Error {
  readonly status: number
  readonly code: ErrorCode
  readonly field?: string

  constructor(status: number, code: ErrorCode, message: string, field?: string) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.code = code
    this.field = field || undefined
  }
}

/** Narrows unknown errors; with `code`, also checks the error code. */
export function isApiError(err: unknown, code?: ErrorCode): err is ApiError {
  return err instanceof ApiError && (code === undefined || err.code === code)
}

/** Converts anything thrown into an ApiError (non-API errors become `internal`). */
export function toApiError(err: unknown): ApiError {
  if (err instanceof ApiError) return err
  if (err instanceof DOMException && (err.name === 'AbortError' || err.name === 'TimeoutError')) {
    return err.name === 'AbortError'
      ? new ApiError(0, 'aborted', 'request aborted')
      : new ApiError(0, 'network', 'request timed out')
  }
  return new ApiError(0, 'internal', err instanceof Error ? err.message : String(err))
}

interface Hooks {
  /** A request returned 401: the session expired or was revoked. */
  unauthorized?: () => void
  /** The server does not accept this host name (DNS-rebinding guard). */
  misdirected?: (message: string) => void
  /** A 403: the rights shown may be out of date (a changed role, the host's configuration lock). */
  forbidden?: () => void
}

const hooks: Hooks = {}

/** Installs global handlers (set once by the app shell). */
export function setApiHooks(h: Hooks): void {
  Object.assign(hooks, h)
}

/** Reports an expired session to the app (used by streams, which cannot see HTTP status codes). */
export function notifyUnauthorized(): void {
  hooks.unauthorized?.()
}

const BASE = 'api/v1'
const DEFAULT_TIMEOUT = 30_000
const MAX_ERROR_TEXT = 300

/** Builds an absolute URL for an API path ("/dns/records") relative to the page, so sub-path hosting works. */
export function apiUrl(path: string, query?: Query): string {
  const url = new URL(BASE + path, document.baseURI)
  if (query) {
    for (const [k, v] of Object.entries(query)) {
      if (v === undefined || v === null || v === '') continue
      if (Array.isArray(v)) {
        for (const item of v) url.searchParams.append(k, String(item))
      } else {
        url.searchParams.set(k, String(v))
      }
    }
  }
  return url.toString()
}

const statusCodes: Record<number, ErrorCode> = {
  400: 'invalid',
  401: 'unauthorized',
  403: 'forbidden',
  404: 'not_found',
  409: 'conflict',
  421: 'misdirected',
  429: 'too_many_requests',
  503: 'unavailable',
}

async function readError(res: Response): Promise<ApiError> {
  const fallback: ErrorCode = statusCodes[res.status] ?? 'internal'
  const ct = res.headers.get('Content-Type') ?? ''
  try {
    if (ct.includes('application/json')) {
      const body = (await res.json()) as { error?: { code?: string; message?: string; field?: string } }
      const e = body.error
      if (e && typeof e.message === 'string') {
        const code = (typeof e.code === 'string' ? e.code : fallback) as ErrorCode
        return new ApiError(res.status, code, e.message, typeof e.field === 'string' ? e.field : undefined)
      }
    } else {
      const text = (await res.text()).trim().slice(0, MAX_ERROR_TEXT)
      if (text) return new ApiError(res.status, fallback, text)
    }
  } catch {
    /* unreadable body: use the status */
  }
  return new ApiError(res.status, fallback, `HTTP ${res.status}`)
}

/** Sends one API request and returns the successful response (errors are thrown as ApiError). */
async function send(method: string, path: string, opts: RequestOptions, accept: string): Promise<Response> {
  const headers: Record<string, string> = { ...opts.headers, Accept: accept }
  let body: BodyInit | undefined
  if (opts.raw !== undefined) {
    headers['Content-Type'] = 'application/octet-stream'
    body = opts.raw
  } else if (opts.body !== undefined) {
    headers['Content-Type'] = 'application/json'
    body = JSON.stringify(opts.body)
  }
  const signals: AbortSignal[] = []
  if (opts.signal) signals.push(opts.signal)
  const timeout = opts.timeoutMs ?? DEFAULT_TIMEOUT
  if (timeout > 0) signals.push(AbortSignal.timeout(timeout))
  const signal = signals.length > 1 ? AbortSignal.any(signals) : signals[0]

  let res: Response
  try {
    res = await fetch(apiUrl(path, opts.query), {
      method,
      headers,
      body,
      signal,
      credentials: 'same-origin',
      cache: 'no-store',
      redirect: 'error',
    })
  } catch (err) {
    if (opts.signal?.aborted) throw new ApiError(0, 'aborted', 'request aborted')
    if (err instanceof DOMException && err.name === 'TimeoutError') {
      throw new ApiError(0, 'network', 'request timed out')
    }
    throw new ApiError(0, 'network', 'PiCache could not be reached')
  }

  if (!res.ok) {
    const err = await readError(res)
    // 401 with field "password" is a wrong password confirmation (restore),
    // not an ended session.
    if (res.status === 401 && !opts.allowUnauthorized && err.field !== 'password') hooks.unauthorized?.()
    if (res.status === 421) hooks.misdirected?.(err.message)
    if (res.status === 403) hooks.forbidden?.()
    throw err
  }
  return res
}

/** Performs one API request. Resolves with the decoded JSON body (undefined for 204). */
export async function request<T>(method: string, path: string, opts: RequestOptions = {}): Promise<T> {
  const res = await send(method, path, opts, 'application/json')
  const ct = res.headers.get('Content-Type') ?? ''
  if (res.status === 204 || !ct.includes('application/json')) return undefined as T
  try {
    return (await res.json()) as T
  } catch {
    throw new ApiError(res.status, 'internal', 'invalid JSON in response')
  }
}

/** A file fetched from the API (for downloads that need a request body, such as a password). */
export interface FetchedFile {
  blob: Blob
  /** The name from Content-Disposition, if the server sent one. */
  filename?: string
}

/** The file name of a Content-Disposition header (`attachment; filename="x.zip"`), without any path. */
function dispositionName(header: string | null): string | undefined {
  const m = /filename="?([^";]+)"?/i.exec(header ?? '')
  const name = m?.[1].split(/[/\\]/).pop()?.trim()
  return name || undefined
}

/** Performs one API request whose answer is a file (errors are JSON as usual). */
export async function requestFile(method: string, path: string, opts: RequestOptions = {}): Promise<FetchedFile> {
  const res = await send(method, path, opts, '*/*')
  let blob: Blob
  try {
    blob = await res.blob()
  } catch {
    if (opts.signal?.aborted) throw new ApiError(0, 'aborted', 'request aborted')
    throw new ApiError(0, 'network', 'the download was interrupted')
  }
  return { blob, filename: dispositionName(res.headers.get('Content-Disposition')) }
}

/** Shorthands used by the endpoint modules. */
export const http = {
  get: <T>(path: string, opts?: RequestOptions) => request<T>('GET', path, opts),
  post: <T>(path: string, body?: unknown, opts?: RequestOptions) => request<T>('POST', path, { ...opts, body }),
  put: <T>(path: string, body?: unknown, opts?: RequestOptions) => request<T>('PUT', path, { ...opts, body }),
  patch: <T>(path: string, body?: unknown, opts?: RequestOptions) => request<T>('PATCH', path, { ...opts, body }),
  del: <T = void>(path: string, opts?: RequestOptions) => request<T>('DELETE', path, opts),
  /** POST with a JSON body whose answer is a file. */
  postFile: (path: string, body?: unknown, opts?: RequestOptions) => requestFile('POST', path, { ...opts, body }),
}

/** Encodes one path segment (ids, hosts, service ids). */
export function seg(v: string | number): string {
  return encodeURIComponent(String(v))
}
