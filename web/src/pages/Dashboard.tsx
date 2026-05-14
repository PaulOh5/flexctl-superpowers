import { Link } from 'react-router-dom'
import { useQuery } from '@tanstack/react-query'
import { api } from '@/lib/api'
import { Button } from '@/components/ui/button'
import { StatusBadge } from '@/components/StatusBadge'
import { Card, CardContent } from '@/components/ui/card'

export function Dashboard() {
  const { data, isLoading } = useQuery({
    queryKey: ['envs'], queryFn: api.listEnvs,
    refetchInterval: q => {
      const envs = q.state.data ?? []
      return envs.some(e => e.status === 'creating') ? 3000 : false
    },
  })

  if (isLoading) return <div className="text-muted-foreground">Loading…</div>

  const envs = data || []
  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between">
        <h1 className="text-2xl font-semibold">Environments</h1>
        <Link to="/envs/new"><Button>New env</Button></Link>
      </div>
      {envs.length === 0 ? (
        <Card><CardContent className="py-12 text-center text-muted-foreground">
          No environments yet. Click <strong>New env</strong> to create one.
        </CardContent></Card>
      ) : (
        <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
          {envs.map(e => (
            <Link key={e.id} to={`/envs/${e.id}`}>
              <Card className="hover:bg-accent transition">
                <CardContent className="p-4 flex items-center justify-between">
                  <div>
                    <div className="font-medium">{e.name}</div>
                    <div className="text-sm text-muted-foreground">{e.hostname}</div>
                  </div>
                  <StatusBadge status={e.status} message={e.status_message} />
                </CardContent>
              </Card>
            </Link>
          ))}
        </div>
      )}
    </div>
  )
}
