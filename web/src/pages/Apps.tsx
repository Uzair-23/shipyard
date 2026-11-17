import { useQuery } from '@tanstack/react-query'
import { Link } from 'react-router-dom'
import { api } from '../api.ts'
import StatusBadge from '../components/StatusBadge.tsx'
import { btn, card } from '../lib/ui.ts'

export default function Apps() {
  const apps = useQuery({ queryKey: ['apps'], queryFn: api.listApps, refetchInterval: 5000 })
  return (
    <>
      <div className="mb-4 flex items-center justify-between">
        <h1 className="text-xl font-semibold">Your apps</h1>
        <Link to="/new" className={btn}>
          New app
        </Link>
      </div>
      {apps.isPending && <p className="text-sm text-slate-500">Loading…</p>}
      {apps.isError && <p className="text-sm text-red-600">Could not load apps.</p>}
      {apps.data?.length === 0 && (
        <div className={`${card} text-center`}>
          <p className="mb-3 text-sm text-slate-500">No apps yet. Deploy your first one.</p>
          <Link to="/new" className={btn}>
            New app
          </Link>
        </div>
      )}
      <ul className="space-y-2">
        {apps.data?.map((a) => (
          <li key={a.slug} className={`${card} flex items-center justify-between`}>
            <div>
              <Link to={`/apps/${a.slug}`} className="font-medium underline">
                {a.slug}
              </Link>
              <div>
                <a href={a.url} target="_blank" rel="noopener noreferrer" className="text-xs text-slate-500 underline">
                  {a.url}
                </a>
              </div>
            </div>
            <StatusBadge status={a.status} />
          </li>
        ))}
      </ul>
    </>
  )
}
