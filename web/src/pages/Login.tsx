import { useEffect, useState } from 'react'
import { useNavigate, useSearchParams } from 'react-router-dom'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Card, CardContent, CardHeader, CardTitle, CardDescription } from '@/components/ui/card'
import { useToast } from '@/hooks/use-toast'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { api, ApiError } from '@/lib/api'

export function Login() {
  const me = useQuery({ queryKey: ['me'], queryFn: api.me, retry: false, staleTime: 60_000 })
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const [busy, setBusy] = useState(false)
  const nav = useNavigate()
  const [params] = useSearchParams()

  useEffect(() => {
    if (me.data) nav(params.get('from') || '/', { replace: true })
  }, [me.data, nav, params])
  const qc = useQueryClient()
  const { toast } = useToast()

  async function onSubmit(e: React.FormEvent) {
    e.preventDefault()
    setBusy(true)
    try {
      await api.login({ email, password })
      qc.invalidateQueries({ queryKey: ['me'] })
      nav(params.get('from') || '/')
    } catch (err) {
      toast({ variant: 'destructive', title: 'Login failed',
        description: err instanceof ApiError ? err.message : String(err) })
    } finally { setBusy(false) }
  }

  return (
    <div className="min-h-screen grid place-items-center bg-muted/40 p-4">
      <Card className="w-full max-w-md">
        <CardHeader>
          <CardTitle>Log in</CardTitle>
          <CardDescription>flexctl</CardDescription>
        </CardHeader>
        <CardContent>
          <form onSubmit={onSubmit} className="space-y-4">
            <div className="space-y-2">
              <Label htmlFor="email">Email</Label>
              <Input id="email" name="email" type="email" required
                value={email} onChange={e => setEmail(e.target.value)} />
            </div>
            <div className="space-y-2">
              <Label htmlFor="password">Password</Label>
              <Input id="password" name="password" type="password" required
                value={password} onChange={e => setPassword(e.target.value)} />
            </div>
            <Button type="submit" className="w-full" disabled={busy}>
              {busy ? 'Signing in…' : 'Log in'}
            </Button>
            <p className="text-sm text-muted-foreground text-center">
              New here? <a href="/signup" className="underline">Sign up</a>
            </p>
          </form>
        </CardContent>
      </Card>
    </div>
  )
}
