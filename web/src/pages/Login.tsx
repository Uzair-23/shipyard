import { useMutation, useQueryClient } from '@tanstack/react-query'
import { useState, type FormEvent } from 'react'
import { useNavigate, useSearchParams } from 'react-router-dom'
import { ApiError, api } from '../api.ts'
import { btn, card, input } from '../lib/ui.ts'

/** Same-site path (+query) from ?next=, or '/'. Accepts a full URL only on this origin. */
function safeNext(next: string | null): string {
  if (!next) return '/'
  try {
    const u = new URL(next, window.location.origin)
    if (u.origin !== window.location.origin) return '/'
    return u.pathname + u.search
  } catch {
    return '/'
  }
}

export default function Login() {
  const [params] = useSearchParams()
  const nav = useNavigate()
  const qc = useQueryClient()
  const [signup, setSignup] = useState(false)
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')

  const submit = useMutation({
    mutationFn: () => (signup ? api.signup(email, password) : api.login(email, password)),
    onSuccess: async (user) => {
      qc.setQueryData(['me'], user)
      const next = safeNext(params.get('next'))
      // /auth/app is a server-side redirect chain, not a dashboard route.
      if (next.startsWith('/auth/') || next.startsWith('/api/')) window.location.assign(next)
      else nav(next, { replace: true })
    },
  })

  function onSubmit(e: FormEvent) {
    e.preventDefault()
    submit.mutate()
  }

  return (
    <div className="mx-auto mt-24 max-w-sm px-4">
      <h1 className="mb-1 text-2xl font-semibold">Shipyard</h1>
      <p className="mb-6 text-sm text-slate-500">Private, shareable URLs for small internal tools.</p>
      <form onSubmit={onSubmit} className={`${card} space-y-3`}>
        <h2 className="font-medium">{signup ? 'Create an account' : 'Log in'}</h2>
        <label className="block text-sm">
          Email
          <input className={input} type="email" required autoComplete="email" value={email} onChange={(e) => setEmail(e.target.value)} />
        </label>
        <label className="block text-sm">
          Password
          <input
            className={input}
            type="password"
            required
            autoComplete={signup ? 'new-password' : 'current-password'}
            value={password}
            onChange={(e) => setPassword(e.target.value)}
          />
        </label>
        {submit.error instanceof ApiError && <p className="text-sm text-red-600">{submit.error.message}</p>}
        <button className={`${btn} w-full`} disabled={submit.isPending}>
          {signup ? 'Sign up' : 'Log in'}
        </button>
        <button type="button" className="w-full text-center text-sm underline" onClick={() => { setSignup(!signup); submit.reset() }}>
          {signup ? 'I already have an account' : 'Create an account'}
        </button>
      </form>
    </div>
  )
}
