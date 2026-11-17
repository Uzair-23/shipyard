import { useSyncExternalStore } from 'react'
import { ApiError } from '../api.ts'

export interface ToastItem {
  id: number
  kind: 'error' | 'info'
  text: string
}

let items: ToastItem[] = []
let nextId = 1
const listeners = new Set<() => void>()

function emit() {
  items = [...items]
  listeners.forEach((l) => l())
}

export function toast(kind: ToastItem['kind'], text: string) {
  const id = nextId++
  items.push({ id, kind, text })
  emit()
  setTimeout(() => dismiss(id), 6000)
}

export function dismiss(id: number) {
  items = items.filter((t) => t.id !== id)
  emit()
}

export function toastError(e: unknown) {
  toast('error', e instanceof ApiError ? e.message : 'Something went wrong. Try again.')
}

export function useToasts(): ToastItem[] {
  return useSyncExternalStore(
    (cb) => {
      listeners.add(cb)
      return () => listeners.delete(cb)
    },
    () => items,
  )
}
