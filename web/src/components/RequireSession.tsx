import { useQuery } from '@tanstack/react-query'
import { Navigate, useLocation } from 'react-router-dom'
import { api, ApiError } from '@/lib/api'

export function RequireSession({ children }: { children: React.ReactNode }) {
  const loc = useLocation()
  const { data, isLoading, isError, error } = useQuery({
    queryKey: ['me'], queryFn: api.me, retry: false, staleTime: 60_000,
  })
  if (isLoading) return <div className="p-8 text-muted-foreground">Loading…</div>
  if (isError) {
    const apiErr = error instanceof ApiError ? error : null
    if (apiErr?.status === 401) {
      return <Navigate to={`/login?from=${encodeURIComponent(loc.pathname)}`} replace />
    }
    return <div className="p-8 text-destructive">Failed to load session: {String(error)}</div>
  }
  if (!data) return <Navigate to="/login" replace />
  return <>{children}</>
}
