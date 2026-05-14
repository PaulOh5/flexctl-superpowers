import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { BrowserRouter, Routes, Route, Navigate } from 'react-router-dom'
import { RequireSession } from '@/components/RequireSession'
import { Layout } from '@/components/Layout'
import { Toaster } from '@/components/ui/toaster'

import { Signup } from '@/pages/Signup'
import { Login } from '@/pages/Login'
import { Dashboard } from '@/pages/Dashboard'
import { EnvNew } from '@/pages/EnvNew'
import { EnvDetail } from '@/pages/EnvDetail'
import { Keys } from '@/pages/Keys'
import { Devices } from '@/pages/Devices'

const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } })

export default function App() {
  return (
    <QueryClientProvider client={qc}>
      <BrowserRouter>
        <Routes>
          <Route path="/signup" element={<Signup />} />
          <Route path="/login" element={<Login />} />
          <Route element={<RequireSession><Layout /></RequireSession>}>
            <Route path="/" element={<Dashboard />} />
            <Route path="/envs/new" element={<EnvNew />} />
            <Route path="/envs/:id" element={<EnvDetail />} />
            <Route path="/keys" element={<Keys />} />
            <Route path="/devices" element={<Devices />} />
          </Route>
          <Route path="*" element={<Navigate to="/" replace />} />
        </Routes>
      </BrowserRouter>
      <Toaster />
    </QueryClientProvider>
  )
}
