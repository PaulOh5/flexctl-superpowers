import { useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { api, ApiError } from '@/lib/api'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { useToast } from '@/hooks/use-toast'

export function EnvNew() {
  const nav = useNavigate()
  const qc = useQueryClient()
  const { toast } = useToast()
  const [name, setName] = useState('')
  const [templateId, setTemplateId] = useState('')
  const [nodeId, setNodeId] = useState('')
  const [gpu, setGpu] = useState(1)

  const tpls = useQuery({ queryKey: ['templates'], queryFn: api.listTemplates })
  const nodes = useQuery({ queryKey: ['nodes'], queryFn: api.listNodes })

  const mut = useMutation({
    mutationFn: () => api.createEnv({ name, template_id: templateId, node_id: nodeId, gpu_request: gpu }),
    onSuccess: env => {
      qc.invalidateQueries({ queryKey: ['envs'] })
      nav(`/envs/${env.id}`)
    },
    onError: err => toast({
      variant: 'destructive', title: 'Failed to create env',
      description: err instanceof ApiError ? err.message : String(err),
    }),
  })

  if (tpls.isLoading || nodes.isLoading) return <div className="text-muted-foreground">Loading…</div>

  const noNodes = (nodes.data || []).length === 0
  if (noNodes) {
    return (
      <Card><CardContent className="py-12 text-center space-y-2">
        <p>You haven't joined any GPU nodes yet.</p>
        <p className="text-sm text-muted-foreground">
          Run <code className="bg-muted px-1 py-0.5 rounded">flexctl join FX-XXXX-YYYY</code> on a GPU machine first.
        </p>
      </CardContent></Card>
    )
  }

  return (
    <Card className="max-w-xl">
      <CardHeader><CardTitle>New environment</CardTitle></CardHeader>
      <CardContent>
        <form onSubmit={e => { e.preventDefault(); mut.mutate() }} className="space-y-4">
          <div className="space-y-2">
            <Label htmlFor="name">Name (lowercase, hyphens)</Label>
            <Input id="name" name="name" required pattern="[a-z][a-z0-9-]*"
              value={name} onChange={e => setName(e.target.value)} />
          </div>
          <div className="space-y-2">
            <Label htmlFor="template_id">Template</Label>
            <select id="template_id" name="template_id" required
              className="block w-full rounded-md border border-input bg-background px-3 py-2 text-sm"
              value={templateId} onChange={e => setTemplateId(e.target.value)}>
              <option value="">— select —</option>
              {(tpls.data || []).map(t =>
                <option key={t.id} value={t.id}>{t.display_name || t.id}</option>)}
            </select>
          </div>
          <div className="space-y-2">
            <Label htmlFor="node_id">GPU node</Label>
            <select id="node_id" name="node_id" required
              className="block w-full rounded-md border border-input bg-background px-3 py-2 text-sm"
              value={nodeId} onChange={e => setNodeId(e.target.value)}>
              <option value="">— select —</option>
              {(nodes.data || []).map(n =>
                <option key={n.id} value={n.id}>{n.name} ({n.status})</option>)}
            </select>
          </div>
          <div className="space-y-2">
            <Label htmlFor="gpu_request">GPU count</Label>
            <Input id="gpu_request" name="gpu_request" type="number" min={0} max={8} required
              value={gpu} onChange={e => setGpu(Number(e.target.value))} />
          </div>
          <Button type="submit" disabled={mut.isPending}>
            {mut.isPending ? 'Creating…' : 'Create environment'}
          </Button>
        </form>
      </CardContent>
    </Card>
  )
}
