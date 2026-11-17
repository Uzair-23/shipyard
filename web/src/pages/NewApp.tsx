import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState, type FormEvent } from 'react'
import { Link } from 'react-router-dom'
import { ApiError, api } from '../api.ts'
import LogView from '../components/LogView.tsx'
import UploadDrop from '../components/UploadDrop.tsx'
import { slugProblem } from '../lib/slug.ts'
import { btn, card, input } from '../lib/ui.ts'

export default function NewApp() {
  const qc = useQueryClient()
  const [slug, setSlug] = useState('')
  const [created, setCreated] = useState<string | null>(null)
  const [seq, setSeq] = useState<number | null>(null)
  const [ended, setEnded] = useState<string | null>(null)
  const problem = slug ? slugProblem(slug) : ''

  const create = useMutation({
    mutationFn: () => api.createApp(slug),
    onSuccess: (a) => {
      setCreated(a.slug)
      void qc.invalidateQueries({ queryKey: ['apps'] })
    },
  })
  const upload = useMutation({
    mutationFn: (f: File) => api.deploy(created!, f),
    onSuccess: (d) => {
      setEnded(null)
      setSeq(d.seq)
    },
  })
  // After the build ends, fetch the app for its URL / failure text.
  const app = useQuery({
    queryKey: ['app', created],
    queryFn: () => api.getApp(created!),
    enabled: !!created && ended !== null,
  })

  function onCreate(e: FormEvent) {
    e.preventDefault()
    if (!problem && slug) create.mutate()
  }

  return (
    <div className="space-y-4">
      <h1 className="text-xl font-semibold">New app</h1>

      {!created && (
        <form onSubmit={onCreate} className={`${card} space-y-3`}>
          <label className="block text-sm">
            App name (becomes <code>{slug || 'your-app'}.apps.localhost</code>)
            <input className={input} value={slug} onChange={(e) => setSlug(e.target.value.toLowerCase())} autoFocus />
          </label>
          {problem && <p className="text-sm text-red-600">{problem}</p>}
          {create.error instanceof ApiError && <p className="text-sm text-red-600">{create.error.message}</p>}
          <button className={btn} disabled={!slug || !!problem || create.isPending}>
            Create app
          </button>
        </form>
      )}

      {created && seq === null && (
        <div className={`${card} space-y-3`}>
          <p className="text-sm">
            <strong>{created}</strong> created. Upload your code as a <code>.tar.gz</code>.
          </p>
          <UploadDrop busy={upload.isPending} onFile={(f) => upload.mutate(f)} />
          {upload.error instanceof ApiError && <p className="text-sm text-red-600">{upload.error.message}</p>}
        </div>
      )}

      {created && seq !== null && (
        <div className={`${card} space-y-3`}>
          <LogView key={seq} slug={created} seq={seq} onEnd={setEnded} />
          {ended === 'live' && app.data && (
            <p className="text-sm">
              Live at{' '}
              <a href={app.data.url} target="_blank" rel="noopener noreferrer" className="font-medium underline">
                {app.data.url}
              </a>{' '}
              · <Link to={`/apps/${created}`} className="underline">Manage app</Link>
            </p>
          )}
          {ended && ended !== 'live' && (
            <div className="space-y-2 text-sm text-red-600">
              <p>The deploy {ended}.</p>
              {app.data?.latest_deploy?.error && (
                <pre className="overflow-auto whitespace-pre-wrap">{app.data.latest_deploy.error}</pre>
              )}
              <button className={btn} onClick={() => { setSeq(null); setEnded(null) }}>
                Upload again
              </button>
            </div>
          )}
        </div>
      )}
    </div>
  )
}
