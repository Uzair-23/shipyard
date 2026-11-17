import { Navigate, Outlet, useLocation } from 'react-router-dom'
import { useMe } from '../useMe.ts'

export default function RequireAuth() {
  const me = useMe()
  const loc = useLocation()
  if (me.isPending) return <p className="p-8 text-sm text-slate-500">Loading…</p>
  if (me.isError) return <p className="p-8 text-sm text-red-600">Could not reach the server.</p>
  if (!me.data) {
    return <Navigate to={`/login?next=${encodeURIComponent(loc.pathname + loc.search)}`} replace />
  }
  return <Outlet />
}
