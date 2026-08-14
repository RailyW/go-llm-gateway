import { createContext, useContext, useEffect, useState, type ReactNode } from 'react'
import { api, clearToken, getToken, setToken, type User } from './api'

interface AuthCtx {
  user: User | null
  loading: boolean
  login: (u: string, p: string) => Promise<void>
  register: (u: string, p: string) => Promise<void>
  logout: () => void
}

const Ctx = createContext<AuthCtx>(null as unknown as AuthCtx)

export function AuthProvider({ children }: { children: ReactNode }) {
  const [user, setUser] = useState<User | null>(null)
  const [loading, setLoading] = useState(true)

  useEffect(() => {
    if (!getToken()) {
      setLoading(false)
      return
    }
    api
      .me()
      .then(setUser)
      .catch(() => clearToken())
      .finally(() => setLoading(false))
  }, [])

  const handle = async (p: Promise<{ token: string; user: User }>) => {
    const res = await p
    setToken(res.token)
    setUser(res.user)
  }

  return (
    <Ctx.Provider
      value={{
        user,
        loading,
        login: (u, p) => handle(api.login(u, p)),
        register: (u, p) => handle(api.register(u, p)),
        logout: () => {
          clearToken()
          setUser(null)
        },
      }}
    >
      {children}
    </Ctx.Provider>
  )
}

export const useAuth = () => useContext(Ctx)
