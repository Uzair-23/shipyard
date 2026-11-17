import { useQuery } from '@tanstack/react-query'
import { api } from './api.ts'

export function useMe() {
  return useQuery({ queryKey: ['me'], queryFn: api.me, staleTime: 60_000 })
}
