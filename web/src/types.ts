// Hand-written to match the ROADMAP API table. Keep in sync with it.

export interface User {
  id: string
  email: string
}

export type AppStatus = 'stopped' | 'starting' | 'running' | 'sleeping' | 'crashed'
export type Visibility = 'private' | 'public'
export type DeployState =
  | 'queued'
  | 'building'
  | 'built'
  | 'starting'
  | 'health_check'
  | 'live'
  | 'failed'
  | 'superseded'

export interface Deploy {
  seq: number
  state: DeployState
  /** Untrusted text (may contain app output). Render as text only. */
  error?: string
  created_at?: string
  finished_at?: string
}

export interface AppSummary {
  slug: string
  status: AppStatus
  url: string
  created_at: string
}

export interface App extends AppSummary {
  visibility: Visibility
  idle_timeout_s: number
  /** How long the last wake from sleep took, in ms; null if it never slept. */
  last_cold_start_ms: number | null
  current_deploy: Deploy | null
  latest_deploy: Deploy | null
}

export interface Member {
  email: string
  created_at: string
}

export interface EnvVar {
  key: string
  is_secret: boolean
  /** Present only for non-secret variables. */
  value?: string
  updated_at: string
}

export interface DeployStarted {
  slug: string
  seq: number
  state: DeployState
}

export interface LogLine {
  id: number
  stream: 'build' | 'system'
  /** Untrusted text. Render as text only. */
  line: string
  ts: string
}

export interface ApiErrorBody {
  error: { code: string; message: string }
}
