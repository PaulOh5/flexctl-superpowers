import { NavLink, Outlet, useNavigate } from 'react-router-dom'
import { useQueryClient } from '@tanstack/react-query'
import { api } from '@/lib/api'

export function Layout() {
  const nav = useNavigate()
  const qc = useQueryClient()
  const items = [
    { to: '/', label: 'Dashboard' },
    { to: '/keys', label: 'SSH keys' },
    { to: '/devices', label: 'Devices' },
  ]
  return (
    <div className="flex min-h-screen">
      <aside className="w-56 border-r p-4 flex flex-col gap-1">
        <div className="font-semibold text-lg mb-4">flexctl</div>
        {items.map(it => (
          <NavLink key={it.to} to={it.to} end
            className={({ isActive }) =>
              'rounded px-3 py-2 hover:bg-accent ' +
              (isActive ? 'bg-accent font-medium' : '')
            }>
            {it.label}
          </NavLink>
        ))}
        <button className="mt-auto rounded px-3 py-2 hover:bg-accent text-left"
          onClick={async () => { await api.logout(); qc.clear(); nav('/login') }}>
          Logout
        </button>
      </aside>
      <main className="flex-1 p-8"><Outlet /></main>
    </div>
  )
}
