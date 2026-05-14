import { useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Card, CardContent, CardHeader, CardTitle, CardDescription } from '@/components/ui/card'
import { useToast } from '@/hooks/use-toast'
import { api, ApiError } from '@/lib/api'

export function Signup() {
  const [email, setEmail] = useState('')
  const [slug, setSlug] = useState('')
  const [password, setPassword] = useState('')
  const [busy, setBusy] = useState(false)
  const nav = useNavigate()
  const { toast } = useToast()

  async function onSubmit(e: React.FormEvent) {
    e.preventDefault()
    setBusy(true)
    try {
      await api.signup({ email, slug, password })
      nav('/')
    } catch (err) {
      toast({ variant: 'destructive', title: 'Signup failed',
        description: err instanceof ApiError ? err.message : String(err) })
    } finally { setBusy(false) }
  }

  return (
    <div className="min-h-screen grid place-items-center bg-muted/40 p-4">
      <Card className="w-full max-w-md">
        <CardHeader>
          <CardTitle>Create an account</CardTitle>
          <CardDescription>flexctl self-sovereign GPU cloud</CardDescription>
        </CardHeader>
        <CardContent>
          <form onSubmit={onSubmit} className="space-y-4">
            <div className="space-y-2">
              <Label htmlFor="email">Email</Label>
              <Input id="email" name="email" type="email" required
                value={email} onChange={e => setEmail(e.target.value)} />
            </div>
            <div className="space-y-2">
              <Label htmlFor="slug">Slug (lowercase, hyphens)</Label>
              <Input id="slug" name="slug" required pattern="[a-z0-9][a-z0-9-]*"
                value={slug} onChange={e => setSlug(e.target.value)} />
            </div>
            <div className="space-y-2">
              <Label htmlFor="password">Password (12+ chars)</Label>
              <Input id="password" name="password" type="password" required minLength={12}
                value={password} onChange={e => setPassword(e.target.value)} />
            </div>
            <Button type="submit" className="w-full" disabled={busy}>
              {busy ? 'Creating…' : 'Sign up'}
            </Button>
            <p className="text-sm text-muted-foreground text-center">
              Already have an account? <a href="/login" className="underline">Log in</a>
            </p>
          </form>
        </CardContent>
      </Card>
    </div>
  )
}
