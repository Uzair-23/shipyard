import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState, type FormEvent } from 'react'
import { useNavigate, useParams } from 'react-router-dom'
import { ApiError, api } from '../api.ts'
import LogView from '../components/LogView.tsx'
import StatusBadge from '../components/StatusBadge.tsx'
import UploadDrop from '../components/UploadDrop.tsx'
import { btn, btnDanger, btnGhost, card, input } from '../lib/ui.ts'
import type { App, Visibility } from '../types.ts'

type Tab = 'overview' | 'access' | 'settings'
const DONE = ['live', 'failed', 'superseded']

export default function AppDetail() {
  const { slug = '' } = useParams()
  const [tab, setTab] = useState<Tab>('overview')
  // Start/stop/restart return at once; the worker changes the status a moment later.
  const [pollUntil, setPollUntil] = useState(0)
  const app = useQuery({
    queryKey: ['app', slug],
    queryFn: () => api.getApp(slug),
    refetchInterval: (q) => {
      const a = q.state.data
      if (!a) return false
      const d = a.latest_deploy
      return a.status === 'starting' || (d && !DONE.includes(d.state)) || Date.now() < pollUntil ? 3000 : false
    },
  })

  if (app.isPending) return <p className="text-sm text-slate-500">Loading…</p>
  if (app.isError) {
    const notFound = app.error instanceof ApiError && app.error.status === 404
    return <p className="text-sm text-red-600">{notFound ? 'App not found.' : 'Could not load the app.'}</p>
  }
  const a = app.data
  return (
    <div className="space-y-4">
      <div className="flex items-center gap-3">
        <h1 className="text-xl font-semibold">{a.slug}</h1>
        <StatusBadge status={a.status} />
      </div>
      <nav className="flex gap-1 border-b border-slate-200 dark:border-slate-800">
        {(['overview', 'access', 'settings'] as const).map((t) => (
          <button
            key={t}
            onClick={() => setTab(t)}
            className={`px-3 py-2 text-sm capitalize ${tab === t ? 'border-b-2 border-slate-900 font-medium dark:border-slate-100' : 'text-slate-500'}`}
          >
            {t}
          </button>
        ))}
      </nav>
      {tab === 'overview' && <Overview app={a} onAction={() => setPollUntil(Date.now() + 30_000)} />}
      {tab === 'access' && <Access app={a} />}
      {tab === 'settings' && <Settings app={a} />}
    </div>
  )
}

function useRefresh(slug: string) {
  const qc = useQueryClient()
  return () => {
    void qc.invalidateQueries({ queryKey: ['app', slug] })
    void qc.invalidateQueries({ queryKey: ['apps'] })
  }
}

function Overview({ app, onAction }: { app: App; onAction: () => void }) {
  const nav = useNavigate()
  const qc = useQueryClient()
  const refresh = useRefresh(app.slug)
  const action = useMutation({
    mutationFn: (kind: 'start' | 'stop' | 'restart') => api[kind](app.slug),
    onSuccess: () => {
      onAction()
      refresh()
    },
  })
  const del = useMutation({
    mutationFn: () => api.deleteApp(app.slug),
    onSuccess: () => {
      qc.removeQueries({ queryKey: ['app', app.slug] })
      void qc.invalidateQueries({ queryKey: ['apps'] })
      nav('/')
    },
  })
  const redeploy = useMutation({ mutationFn: (f: File) => api.deploy(app.slug, f), onSuccess: refresh })
  const cur = app.current_deploy
  const latest = app.latest_deploy
  const building = !!latest && !DONE.includes(latest.state)

  return (
    <div className="space-y-4">
      <div className={`${card} space-y-2 text-sm`}>
        <div>
          URL:{' '}
          <a href={app.url} target="_blank" rel="noopener noreferrer" className="underline">
            {app.url}
          </a>
        </div>
        <div>Visibility: {app.visibility}</div>
        <div>Serving deploy: {cur ? `#${cur.seq}` : 'none yet'}</div>
        {app.last_cold_start_ms !== null && <div>Last wake from sleep: {app.last_cold_start_ms} ms</div>}
        <div className="flex flex-wrap gap-2 pt-2">
          <button className={btnGhost} disabled={action.isPending} onClick={() => action.mutate('start')}>
            Start
          </button>
          <button className={btnGhost} disabled={action.isPending} onClick={() => action.mutate('stop')}>
            Stop
          </button>
          <button className={btnGhost} disabled={action.isPending} onClick={() => action.mutate('restart')}>
            Restart
          </button>
          <button
            className={btnDanger}
            disabled={del.isPending}
            onClick={() => {
              if (window.confirm(`Delete ${app.slug}? This stops the app and frees the name.`)) del.mutate()
            }}
          >
            Delete
          </button>
        </div>
      </div>

      <div className={`${card} space-y-2`}>
        <h2 className="text-sm font-medium">{cur ? 'Redeploy' : 'Deploy'}</h2>
        <UploadDrop busy={redeploy.isPending || building} onFile={(f) => redeploy.mutate(f)} label={building ? 'A deploy is in progress…' : undefined} />
      </div>

      {latest && (
        <div className={`${card} space-y-2`}>
          <h2 className="text-sm font-medium">
            Deploy #{latest.seq} · {latest.state}
          </h2>
          {latest.error && <pre className="overflow-auto text-sm whitespace-pre-wrap text-red-600">{latest.error}</pre>}
          <LogView key={latest.seq} slug={app.slug} seq={latest.seq} onEnd={refresh} />
        </div>
      )}
    </div>
  )
}

function Access({ app }: { app: App }) {
  const qc = useQueryClient()
  const refresh = useRefresh(app.slug)
  const [email, setEmail] = useState('')
  const members = useQuery({ queryKey: ['members', app.slug], queryFn: () => api.listMembers(app.slug) })
  const reload = () => qc.invalidateQueries({ queryKey: ['members', app.slug] })
  const setVis = useMutation({ mutationFn: (v: Visibility) => api.patchApp(app.slug, { visibility: v }), onSuccess: refresh })
  const add = useMutation({
    mutationFn: () => api.addMember(app.slug, email),
    onSuccess: () => {
      setEmail('')
      void reload()
    },
  })
  const remove = useMutation({ mutationFn: (e: string) => api.removeMember(app.slug, e), onSuccess: reload })

  function onAdd(e: FormEvent) {
    e.preventDefault()
    add.mutate()
  }

  return (
    <div className="space-y-4">
      <div className={`${card} space-y-2 text-sm`}>
        <h2 className="font-medium">Who can open this app</h2>
        <select
          className={input}
          value={app.visibility}
          disabled={setVis.isPending}
          onChange={(e) => setVis.mutate(e.target.value as Visibility)}
        >
          <option value="private">Private: you and the people below, after login</option>
          <option value="public">Public: anyone with the link</option>
        </select>
      </div>
      <div className={`${card} space-y-3 text-sm`}>
        <h2 className="font-medium">Members</h2>
        <form onSubmit={onAdd} className="flex gap-2">
          <input className={input} type="email" required placeholder="teammate@example.com" value={email} onChange={(e) => setEmail(e.target.value)} />
          <button className={btn} disabled={add.isPending}>
            Add
          </button>
        </form>
        {members.data?.length === 0 && <p className="text-slate-500">No members yet.</p>}
        <ul className="space-y-1">
          {members.data?.map((m) => (
            <li key={m.email} className="flex items-center justify-between">
              <span>{m.email}</span>
              <button className={btnGhost} disabled={remove.isPending} onClick={() => remove.mutate(m.email)}>
                Remove
              </button>
            </li>
          ))}
        </ul>
      </div>
    </div>
  )
}

const IDLE_OPTIONS = [
  [300, '5 minutes'],
  [600, '10 minutes'],
  [900, '15 minutes'],
  [1800, '30 minutes'],
] as const

function Settings({ app }: { app: App }) {
  const qc = useQueryClient()
  const refresh = useRefresh(app.slug)
  const [key, setKey] = useState('')
  const [value, setValue] = useState('')
  const [secret, setSecret] = useState(true)
  const env = useQuery({ queryKey: ['env', app.slug], queryFn: () => api.listEnv(app.slug), retry: false })
  const reload = () => qc.invalidateQueries({ queryKey: ['env', app.slug] })
  const set = useMutation({
    mutationFn: () => api.setEnv(app.slug, key, value, secret),
    onSuccess: () => {
      setKey('')
      setValue('')
      void reload()
    },
  })
  const del = useMutation({ mutationFn: (k: string) => api.deleteEnv(app.slug, k), onSuccess: reload })
  const idle = useMutation({ mutationFn: (s: number) => api.patchApp(app.slug, { idle_timeout_s: s }), onSuccess: refresh })
  const options = IDLE_OPTIONS.some(([s]) => s === app.idle_timeout_s)
    ? IDLE_OPTIONS
    : [...IDLE_OPTIONS, [app.idle_timeout_s, `${Math.round(app.idle_timeout_s / 60)} minutes`] as const]

  function onSet(e: FormEvent) {
    e.preventDefault()
    set.mutate()
  }

  return (
    <div className="space-y-4">
      <div className={`${card} space-y-2 text-sm`}>
        <h2 className="font-medium">Sleep after idle</h2>
        <select className={input} value={app.idle_timeout_s} disabled={idle.isPending} onChange={(e) => idle.mutate(Number(e.target.value))}>
          {options.map(([s, label]) => (
            <option key={s} value={s}>
              {label}
            </option>
          ))}
        </select>
      </div>
      <div className={`${card} space-y-3 text-sm`}>
        <h2 className="font-medium">Environment variables and secrets</h2>
        <p className="text-slate-500">Changing a variable restarts a running app. Secret values are never shown again.</p>
        {env.isError && (
          <p className="text-red-600">
            {env.error instanceof ApiError ? env.error.message : 'Could not load variables.'}
          </p>
        )}
        <form onSubmit={onSet} className="flex flex-wrap items-center gap-2">
          <input className={`${input} max-w-48`} placeholder="NAME" required value={key} onChange={(e) => setKey(e.target.value)} />
          <input className={`${input} max-w-64`} placeholder="value" value={value} onChange={(e) => setValue(e.target.value)} />
          <label className="flex items-center gap-1">
            <input type="checkbox" checked={secret} onChange={(e) => setSecret(e.target.checked)} /> secret
          </label>
          <button className={btn} disabled={set.isPending}>
            Save
          </button>
        </form>
        <ul className="space-y-1">
          {env.data?.map((v) => (
            <li key={v.key} className="flex items-center justify-between gap-2">
              <span className="font-mono">
                {v.key} = {v.is_secret ? '••••' : (v.value ?? '')}
              </span>
              <button className={btnGhost} disabled={del.isPending} onClick={() => del.mutate(v.key)}>
                Delete
              </button>
            </li>
          ))}
        </ul>
      </div>
    </div>
  )
}
