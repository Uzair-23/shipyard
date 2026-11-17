import { useMutation, useQueryClient } from '@tanstack/react-query'
import { Link, Outlet, useNavigate } from 'react-router-dom'
import { api } from '../api.ts'
import { useMe } from '../useMe.ts'

export default function Layout() {
  const me = useMe()
  const qc = useQueryClient()
  const nav = useNavigate()
  const logout = useMutation({
    mutationFn: api.logout,
    onSuccess: () => {
      qc.clear()
      nav('/login')
    },
  })
  return (
    <>
      <header className="border-b border-slate-200 bg-white dark:border-slate-800 dark:bg-slate-900">
        <div className="mx-auto flex max-w-4xl items-center justify-between px-4 py-3">
          <Link to="/" className="font-semibold">
            Shipyard
          </Link>
          <div className="flex items-center gap-3 text-sm">
            <span className="text-slate-500">{me.data?.email}</span>
            <button onClick={() => logout.mutate()} className="underline">
              Log out
            </button>
          </div>
        </div>
      </header>
      <main className="mx-auto max-w-4xl px-4 py-6">
        <Outlet />
      </main>
    </>
  )
}
