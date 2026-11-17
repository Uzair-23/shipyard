import type {
  App,
  AppSummary,
  DeployStarted,
  EnvVar,
  LogLine,
  Member,
  User,
  Visibility,
  ApiErrorBody,
} from './types.ts'

const BASE = '/api/v1'

export class ApiError extends Error {
  readonly status: number
  readonly code: string
  constructor(status: number, code: string, message: string) {
    super(message)
    this.status = status
    this.code = code
  }
}

async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
  const init: RequestInit = { method, credentials: 'same-origin' }
  if (body instanceof FormData) {
    init.body = body
  } else if (body !== undefined) {
    init.headers = { 'Content-Type': 'application/json' }
    init.body = JSON.stringify(body)
  }
  let res: Response
  try {
    res = await fetch(BASE + path, init)
  } catch {
    throw new ApiError(0, 'network_error', 'Could not reach the server. Check your connection and try again.')
  }
  if (res.status === 204) return undefined as T
  let data: unknown
  try {
    data = await res.json()
  } catch {
    data = undefined
  }
  if (!res.ok) {
    const e = (data as Partial<ApiErrorBody> | undefined)?.error
    throw new ApiError(res.status, e?.code ?? 'unknown', e?.message ?? `Request failed (${res.status}).`)
  }
  return data as T
}

const app = (slug: string) => `/apps/${encodeURIComponent(slug)}`

export const api = {
  signup: (email: string, password: string) => request<User>('POST', '/auth/signup', { email, password }),
  login: (email: string, password: string) => request<User>('POST', '/auth/login', { email, password }),
  logout: () => request<void>('POST', '/auth/logout'),
  /** Resolves to null when not logged in. */
  me: async (): Promise<User | null> => {
    try {
      return await request<User>('GET', '/me')
    } catch (e) {
      if (e instanceof ApiError && e.status === 401) return null
      throw e
    }
  },

  listApps: async () => (await request<{ apps: AppSummary[] }>('GET', '/apps')).apps,
  createApp: (slug: string) => request<App>('POST', '/apps', { slug }),
  getApp: (slug: string) => request<App>('GET', app(slug)),
  patchApp: (slug: string, patch: { idle_timeout_s?: number; visibility?: Visibility }) =>
    request<App>('PATCH', app(slug), patch),
  deleteApp: (slug: string) => request<void>('DELETE', app(slug)),
  deploy: (slug: string, file: File) => {
    const form = new FormData()
    form.append('source', file)
    return request<DeployStarted>('POST', `${app(slug)}/deploys`, form)
  },
  start: (slug: string) => request<App>('POST', `${app(slug)}/start`),
  stop: (slug: string) => request<App>('POST', `${app(slug)}/stop`),
  restart: (slug: string) => request<App>('POST', `${app(slug)}/restart`),

  listMembers: async (slug: string) =>
    (await request<{ members: Member[] }>('GET', `${app(slug)}/members`)).members,
  addMember: (slug: string, email: string) => request<{ email: string }>('POST', `${app(slug)}/members`, { email }),
  removeMember: (slug: string, email: string) =>
    request<void>('DELETE', `${app(slug)}/members/${encodeURIComponent(email)}`),

  listEnv: async (slug: string) => (await request<{ env: EnvVar[] }>('GET', `${app(slug)}/env`)).env,
  setEnv: (slug: string, key: string, value: string, isSecret: boolean) =>
    request<{ key: string; is_secret: boolean }>('PUT', `${app(slug)}/env/${encodeURIComponent(key)}`, {
      value,
      is_secret: isSecret,
    }),
  deleteEnv: (slug: string, key: string) =>
    request<void>('DELETE', `${app(slug)}/env/${encodeURIComponent(key)}`),
}

/**
 * Follows a deploy's build log over SSE. Closes itself on the `end` event
 * (otherwise EventSource would reconnect). Returns a function that stops it.
 */
export function followBuildLog(
  slug: string,
  seq: number,
  onLine: (l: LogLine) => void,
  onEnd: (state: string) => void,
): () => void {
  const es = new EventSource(`${BASE}${app(slug)}/deploys/${seq}/logs`)
  es.addEventListener('log', (ev) => {
    try {
      onLine(JSON.parse((ev as MessageEvent<string>).data) as LogLine)
    } catch {
      // ignore a malformed event
    }
  })
  es.addEventListener('end', (ev) => {
    es.close()
    try {
      onEnd((JSON.parse((ev as MessageEvent<string>).data) as { state: string }).state)
    } catch {
      onEnd('unknown')
    }
  })
  return () => es.close()
}
