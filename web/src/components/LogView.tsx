import { useEffect, useRef, useState } from 'react'
import { followBuildLog } from '../api.ts'
import type { LogLine } from '../types.ts'

/** Live build log. Lines are untrusted: rendered as plain text only. */
/** Remount with a new `key` when slug/seq change. */
export default function LogView({
  slug,
  seq,
  onEnd,
}: {
  slug: string
  seq: number
  onEnd?: (state: string) => void
}) {
  const [lines, setLines] = useState<LogLine[]>([])
  const [ended, setEnded] = useState<string | null>(null)
  const box = useRef<HTMLPreElement>(null)
  const onEndRef = useRef(onEnd)
  useEffect(() => {
    onEndRef.current = onEnd
  })

  // Callers remount this component (key) when the deploy changes.
  useEffect(() => {
    return followBuildLog(
      slug,
      seq,
      (l) => setLines((prev) => (prev.some((p) => p.id === l.id) ? prev : [...prev, l])),
      (state) => {
        setEnded(state)
        onEndRef.current?.(state)
      },
    )
  }, [slug, seq])

  useEffect(() => {
    box.current?.scrollTo({ top: box.current.scrollHeight })
  }, [lines])

  return (
    <div>
      <pre
        ref={box}
        className="h-72 overflow-auto rounded-md bg-slate-900 p-3 font-mono text-xs whitespace-pre-wrap text-slate-100"
      >
        {lines.length === 0 && !ended ? 'Waiting for the build…' : null}
        {lines.map((l) => (
          <div key={l.id} className={l.stream === 'system' ? 'text-sky-300' : undefined}>
            {l.line}
          </div>
        ))}
      </pre>
      {ended && <p className="mt-1 text-xs text-slate-500">Deploy {ended}.</p>}
    </div>
  )
}
