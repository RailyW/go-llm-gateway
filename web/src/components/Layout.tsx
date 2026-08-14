import { useEffect, useState } from 'react'
import { Link, NavLink, Outlet } from 'react-router-dom'
import { Activity, Boxes, Cable, KeyRound, LayoutDashboard, LogOut, Moon, Settings, Sun, Users } from 'lucide-react'
import { useAuth } from '@/lib/auth'
import { Button } from '@/components/ui/button'
import { Badge } from '@/components/ui/misc'
import { cn } from '@/lib/utils'

const nav = [
  { to: '/', label: '概览', icon: LayoutDashboard, admin: false },
  { to: '/channels', label: '上游', icon: Cable, admin: true },
  { to: '/models', label: '模型', icon: Boxes, admin: true },
  { to: '/keys', label: 'API Key', icon: KeyRound, admin: false },
  { to: '/logs', label: '日志', icon: Activity, admin: false },
  { to: '/users', label: '用户', icon: Users, admin: true },
  { to: '/settings', label: '设置', icon: Settings, admin: true },
]

export default function Layout() {
  const { user, logout } = useAuth()
  const [dark, setDark] = useState(() => localStorage.getItem('gw_theme') === 'dark')

  useEffect(() => {
    document.documentElement.classList.toggle('dark', dark)
    localStorage.setItem('gw_theme', dark ? 'dark' : 'light')
  }, [dark])

  const items = nav.filter((n) => !n.admin || user?.role === 'admin')

  return (
    <div className="flex min-h-screen">
      <aside className="flex w-56 shrink-0 flex-col border-r border-border bg-card">
        <Link to="/" className="flex h-14 items-center gap-2 border-b border-border px-5 font-semibold">
          <Cable className="size-5" /> LLM Gateway
        </Link>
        <nav className="flex-1 space-y-1 p-3">
          {items.map(({ to, label, icon: Icon }) => (
            <NavLink
              key={to}
              to={to}
              end={to === '/'}
              className={({ isActive }) =>
                cn(
                  'flex items-center gap-2 rounded-md px-3 py-2 text-sm transition-colors',
                  isActive ? 'bg-secondary font-medium text-secondary-foreground' : 'text-muted-foreground hover:bg-accent',
                )
              }
            >
              <Icon className="size-4" />
              {label}
            </NavLink>
          ))}
        </nav>
        <div className="border-t border-border p-3">
          <div className="mb-2 flex items-center gap-2 px-1 text-sm">
            <span className="truncate">{user?.username}</span>
            <Badge variant={user?.role === 'admin' ? 'default' : 'secondary'}>{user?.role}</Badge>
          </div>
          <div className="flex gap-2">
            <Button variant="outline" size="sm" className="flex-1" onClick={logout}>
              <LogOut className="size-4" /> 退出
            </Button>
            <Button variant="outline" size="icon" onClick={() => setDark((d) => !d)} title="切换主题">
              {dark ? <Sun className="size-4" /> : <Moon className="size-4" />}
            </Button>
          </div>
        </div>
      </aside>
      <main className="flex-1 overflow-x-hidden bg-background p-6">
        <Outlet />
      </main>
    </div>
  )
}
