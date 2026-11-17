const colors: Record<string, string> = {
  running: 'bg-green-100 text-green-800 dark:bg-green-900 dark:text-green-100',
  starting: 'bg-amber-100 text-amber-800 dark:bg-amber-900 dark:text-amber-100',
  sleeping: 'bg-sky-100 text-sky-800 dark:bg-sky-900 dark:text-sky-100',
  stopped: 'bg-slate-200 text-slate-700 dark:bg-slate-700 dark:text-slate-200',
  crashed: 'bg-red-100 text-red-800 dark:bg-red-900 dark:text-red-100',
}

export default function StatusBadge({ status }: { status: string }) {
  return (
    <span className={`rounded-full px-2 py-0.5 text-xs font-medium ${colors[status] ?? colors.stopped}`}>
      {status}
    </span>
  )
}
