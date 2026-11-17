import { useRef, useState } from 'react'
import { toast } from '../lib/toast.ts'

const MAX_UPLOAD_BYTES = 50 * 1024 * 1024 // same as the API's limit

/** Drag-and-drop (or click) picker for a .zip or .tar.gz source archive. */
export default function UploadDrop({
  onFile,
  busy,
  label = 'Drop a .zip of your app here, or click to choose',
}: {
  onFile: (f: File) => void
  busy?: boolean
  label?: string
}) {
  const input = useRef<HTMLInputElement>(null)
  const [over, setOver] = useState(false)

  function pick(f: File | undefined) {
    if (!f) return
    if (!/\.(zip|tar\.gz|tgz)$/i.test(f.name)) {
      toast('error', 'Please upload a .zip (right-click your folder and compress it) or a .tar.gz.')
      return
    }
    if (f.size > MAX_UPLOAD_BYTES) {
      toast('error', `That file is ${Math.round(f.size / 1048576)} MB; the limit is 50 MB. Leave out node_modules, virtual environments and build output.`)
      return
    }
    onFile(f)
  }

  return (
    <div
      onClick={() => !busy && input.current?.click()}
      onDragOver={(e) => {
        e.preventDefault()
        setOver(true)
      }}
      onDragLeave={() => setOver(false)}
      onDrop={(e) => {
        e.preventDefault()
        setOver(false)
        if (!busy) pick(e.dataTransfer.files[0])
      }}
      className={`cursor-pointer rounded-lg border-2 border-dashed p-8 text-center text-sm ${
        over ? 'border-blue-500 bg-blue-50 dark:bg-blue-950' : 'border-slate-300 dark:border-slate-700'
      } ${busy ? 'opacity-50' : ''}`}
    >
      {busy ? 'Uploading…' : label}
      <input
        ref={input}
        type="file"
        accept=".zip,.gz,.tgz,application/zip,application/gzip"
        className="hidden"
        onChange={(e) => {
          pick(e.target.files?.[0])
          e.target.value = ''
        }}
      />
    </div>
  )
}
