import { useState } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { api, ApiError } from '@/lib/api'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { useToast } from '@/hooks/use-toast'

export function Keys() {
  const qc = useQueryClient()
  const { toast } = useToast()
  const keys = useQuery({ queryKey: ['keys'], queryFn: api.listKeys })
  const [name, setName] = useState('')
  const [pub, setPub] = useState('')

  const onErr = (err: unknown) => toast({
    variant: 'destructive', title: 'Failed',
    description: err instanceof ApiError ? err.message : String(err),
  })

  const addMut = useMutation({
    mutationFn: () => api.addKey({ name, public_key: pub.trim() }),
    onSuccess: () => { qc.invalidateQueries({ queryKey: ['keys'] }); setName(''); setPub('') },
    onError: onErr,
  })
  const delMut = useMutation({
    mutationFn: (id: string) => api.deleteKey(id),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['keys'] }),
    onError: onErr,
  })

  return (
    <div className="space-y-6 max-w-3xl">
      <h1 className="text-2xl font-semibold">SSH keys</h1>

      <Card>
        <CardHeader><CardTitle>Add a key</CardTitle></CardHeader>
        <CardContent>
          <form onSubmit={e => { e.preventDefault(); addMut.mutate() }} className="space-y-3">
            <div className="space-y-2">
              <Label htmlFor="name">Name</Label>
              <Input id="name" required value={name} onChange={e => setName(e.target.value)} />
            </div>
            <div className="space-y-2">
              <Label htmlFor="pub">Public key (ssh-ed25519 / ssh-rsa)</Label>
              <textarea id="pub" required rows={3}
                className="flex w-full rounded-md border border-input bg-background px-3 py-2 text-sm font-mono"
                value={pub} onChange={e => setPub(e.target.value)} />
            </div>
            <Button type="submit" disabled={addMut.isPending}>
              {addMut.isPending ? 'Adding…' : 'Add key'}
            </Button>
          </form>
        </CardContent>
      </Card>

      <Card>
        <CardHeader><CardTitle>Registered keys</CardTitle></CardHeader>
        <CardContent>
          {keys.isLoading ? <p>Loading…</p> :
            (keys.data || []).length === 0 ? <p className="text-muted-foreground">None.</p> :
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Name</TableHead><TableHead>Fingerprint</TableHead><TableHead></TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {(keys.data || []).map(k => (
                  <TableRow key={k.id}>
                    <TableCell>{k.name}</TableCell>
                    <TableCell className="font-mono text-xs">{k.fingerprint}</TableCell>
                    <TableCell className="text-right">
                      <Button variant="ghost" size="sm" disabled={delMut.isPending}
                        onClick={() => { if (confirm(`Delete key ${k.name}?`)) delMut.mutate(k.id) }}>
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
