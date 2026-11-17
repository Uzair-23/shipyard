import { dismiss, useToasts } from '../lib/toast.ts'

export default function Toasts() {
  const toasts = useToasts()
  return (
    <div className="fixed right-4 bottom-4 z-50 flex max-w-sm flex-col gap-2" role="status">
      {toasts.map((t) => (
        <button
          key={t.id}
          onClick={() => dismiss(t.id)}
          className={`rounded-md px-4 py-3 text-left text-sm shadow-lg ${
            t.kind === 'error' ? 'bg-red-600 text-white' : 'bg-slate-800 text-white'
          }`}
        >
          {t.text}
        </button>
      ))}
    </div>
  )
}
