import { Navigate, Route, Routes } from 'react-router-dom'
import { useAuth } from '@/lib/auth'
import Layout from '@/components/Layout'
import LoginPage from '@/pages/LoginPage'
import DashboardPage from '@/pages/DashboardPage'
import ChannelsPage from '@/pages/ChannelsPage'
import ModelsPage from '@/pages/ModelsPage'
import KeysPage from '@/pages/KeysPage'
import LogsPage from '@/pages/LogsPage'
import SettingsPage from '@/pages/SettingsPage'
import UsersPage from '@/pages/UsersPage'

export default function App() {
  const { user, loading } = useAuth()

  if (loading) {
    return <div className="flex h-screen items-center justify-center text-sm text-muted-foreground">加载中...</div>
  }
  if (!user) {
    return (
      <Routes>
        <Route path="/login" element={<LoginPage />} />
        <Route path="*" element={<Navigate to="/login" replace />} />
      </Routes>
    )
  }

  const admin = user.role === 'admin'
  return (
    <Routes>
      <Route element={<Layout />}>
        <Route path="/" element={<DashboardPage />} />
        <Route path="/keys" element={<KeysPage />} />
        <Route path="/logs" element={<LogsPage />} />
        {admin && <Route path="/channels" element={<ChannelsPage />} />}
        {admin && <Route path="/models" element={<ModelsPage />} />}
        {admin && <Route path="/settings" element={<SettingsPage />} />}
        {admin && <Route path="/users" element={<UsersPage />} />}
        <Route path="*" element={<Navigate to="/" replace />} />
      </Route>
    </Routes>
  )
}
