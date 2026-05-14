import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { api, ApiError } from '@/lib/api'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { useToast } from '@/hooks/use-toast'

export function Devices() {
  const qc = useQueryClient()
  const { toast } = useToast()
  const devs = useQuery({ queryKey: ['devices'], queryFn: api.listDevices })

  const delMut = useMutation({
    mutationFn: (id: string) => api.deleteDevice(id),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['devices'] }),
    onError: err => toast({
      variant: 'destructive', title: 'Failed',
      description: err instanceof ApiError ? err.message : String(err),
    }),
  })

  return (
    <div className="space-y-6 max-w-3xl">
      <h1 className="text-2xl font-semibold">Devices</h1>
      <Card>
        <CardHeader><CardTitle>Registered devices</CardTitle></CardHeader>
        <CardContent>
          {devs.isLoading ? <p>Loading…</p> :
            (devs.data || []).length === 0 ?
              <p className="text-muted-foreground">
                None. Run <code className="bg-muted px-1 py-0.5 rounded">flexctl login</code> on a laptop.
              </p> :
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Name</TableHead><TableHead>Hostname (tailnet)</TableHead>
                  <TableHead>Last seen</TableHead><TableHead></TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {(devs.data || []).map(d => (
                  <TableRow key={d.id}>
                    <TableCell>{d.name}</TableCell>
                    <TableCell className="font-mono text-xs">{d.hostname}</TableCell>
                    <TableCell className="text-sm text-muted-foreground">
                      {d.last_seen_at ? new Date(d.last_seen_at).toLocaleString() : '—'}
                    </TableCell>
                    <TableCell className="text-right">
                      <Button variant="ghost" size="sm" disabled={delMut.isPending}
                        onClick={() => { if (confirm(`Delete device ${d.name}?`)) delMut.mutate(d.id) }}>
                        Delete
                      </Button>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          }
        </CardContent>
      </Card>
    </div>
  )
}
