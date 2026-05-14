import { useParams, useNavigate } from 'react-router-dom'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { api, ApiError } from '@/lib/api'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { useToast } from '@/hooks/use-toast'
import { StatusBadge } from '@/components/StatusBadge'

function CopyBox({ text }: { text: string }) {
  const { toast } = useToast()
  return (
    <div className="flex items-center gap-2">
      <code className="flex-1 bg-muted px-3 py-2 rounded font-mono text-sm">{text}</code>
      <Button variant="outline" size="sm" onClick={async () => {
        await navigator.clipboard.writeText(text)
        toast({ title: 'Copied' })
      }}>Copy</Button>
    </div>
  )
}

export function EnvDetail() {
  const { id } = useParams<{ id: string }>()
  const nav = useNavigate()
  const qc = useQueryClient()
  const { toast } = useToast()
  const env = useQuery({
    queryKey: ['env', id], queryFn: () => api.getEnv(id!),
    refetchInterval: q => q.state.data?.status === 'creating' ? 3000 : false,
    enabled: !!id,
  })

  const invalidate = () => {
    qc.invalidateQueries({ queryKey: ['envs'] })
    qc.invalidateQueries({ queryKey: ['env', id] })
  }

  const onErr = (err: unknown) => toast({
    variant: 'destructive', title: 'Action failed',
    description: err instanceof ApiError ? err.message : String(err),
  })

  const stopMut   = useMutation({ mutationFn: () => api.stopEnv(id!),   onSuccess: invalidate, onError: onErr })
  const startMut  = useMutation({ mutationFn: () => api.startEnv(id!),  onSuccess: invalidate, onError: onErr })
  const deleteMut = useMutation({ mutationFn: () => api.deleteEnv(id!),
    onSuccess: () => { qc.invalidateQueries({ queryKey: ['envs'] }); nav('/') }, onError: onErr })

  if (env.isLoading) return <div className="text-muted-foreground">Loading…</div>
  if (env.isError || !env.data) return <div className="text-destructive">Not found</div>

  const e = env.data
  return (
    <div className="space-y-6 max-w-2xl">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-semibold">{e.name}</h1>
          <p className="text-sm text-muted-foreground">{e.hostname}</p>
        </div>
        <StatusBadge status={e.status} message={e.status_message} />
      </div>

      {e.status === 'running' && (
        <Card>
          <CardHeader><CardTitle>Connect via SSH</CardTitle></CardHeader>
          <CardContent className="space-y-3">
            <CopyBox text={`flexctl ssh ${e.name}`} />
            <p className="text-sm text-muted-foreground">or:</p>
            <CopyBox text={`ssh dev@${e.hostname}.flex`} />
          </CardContent>
        </Card>
      )}

      {e.status === 'error' && (
        <Card>
          <CardHeader><CardTitle className="text-destructive">Error</CardTitle></CardHeader>
          <CardContent className="text-sm">{e.status_message || 'unknown error'}</CardContent>
        </Card>
      )}

      <Card>
        <CardHeader><CardTitle>Actions</CardTitle></CardHeader>
        <CardContent className="flex gap-2 flex-wrap">
          {e.status === 'running' && <Button variant="outline" disabled={stopMut.isPending} onClick={() => stopMut.mutate()}>Stop</Button>}
          {e.status === 'stopped' && <Button variant="outline" disabled={startMut.isPending} onClick={() => startMut.mutate()}>Start</Button>}
          <Button variant="destructive" disabled={deleteMut.isPending}
            onClick={() => { if (confirm(`Delete env "${e.name}"? Volume will be removed.`)) deleteMut.mutate() }}>
            Delete
          </Button>
        </CardContent>
      </Card>
    </div>
  )
}
