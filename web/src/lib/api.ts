const TOKEN_KEY = 'gw_token'

export const getToken = () => localStorage.getItem(TOKEN_KEY)
export const setToken = (t: string) => localStorage.setItem(TOKEN_KEY, t)
export const clearToken = () => localStorage.removeItem(TOKEN_KEY)

export interface User {
  id: number
  username: string
  role: 'admin' | 'user'
  enabled: boolean
  created_at: string
}
export interface ProtocolInfo {
  name: string
  label: string
  vendor: string
  path: string
  default: boolean
}
export interface Channel {
  id: number
  name: string
  protocols: string[]
  base_url: string
  api_key: string
  enabled: boolean
  created_at: string
}
export interface Binding {
  id: number
  model_id: number
  channel_id: number
  upstream_model: string
  weight: number
  enabled: boolean
  channel?: Channel
}
export interface Model {
  id: number
  name: string
  remark: string
  enabled: boolean
  bindings?: Binding[]
}
export interface ApiKey {
  id: number
  user_id: number
  name: string
  key: string
  enabled: boolean
  last_used_at: string | null
  created_at: string
}
export interface LogItem {
  id: string
  protocol: string
  endpoint: string
  username: string
  api_key_name: string
  model_name: string
  channel_name: string
  upstream_model: string
  stream: boolean
  status_code: number
  prompt_tokens: number
  completion_tokens: number
  total_tokens: number
  duration_ms: number
  client_ip: string
  error_message: string
  archive_path: string
  created_at: string
}
export interface CleanerStatus {
  last_run_at: string | null
  next_run_at: string | null
  last_removed_archive_dirs: number
  last_removed_log_rows: number
  last_error: string
  running: boolean
}

class ApiError extends Error {
  status: number
  constructor(status: number, message: string) {
    super(message)
    this.status = status
  }
}

async function request<T>(path: string, init: RequestInit = {}): Promise<T> {
  const headers = new Headers(init.headers)
  if (init.body) headers.set('Content-Type', 'application/json')
  const token = getToken()
  if (token) headers.set('Authorization', `Bearer ${token}`)

  const res = await fetch(`/api${path}`, { ...init, headers })
  const text = await res.text()
  const data = text ? JSON.parse(text) : {}
  if (!res.ok) {
    if (res.status === 401) {
      clearToken()
      if (!location.pathname.startsWith('/login')) location.href = '/login'
    }
    throw new ApiError(res.status, data.error || `请求失败 (${res.status})`)
  }
  return data as T
}

const body = (v: unknown) => JSON.stringify(v)

export const api = {
  meta: () => request<{ allow_register: boolean; protocols: ProtocolInfo[]; strategies: string[] }>('/meta'),
  login: (username: string, password: string) =>
    request<{ token: string; user: User }>('/auth/login', { method: 'POST', body: body({ username, password }) }),
  register: (username: string, password: string) =>
    request<{ token: string; user: User }>('/auth/register', { method: 'POST', body: body({ username, password }) }),
  me: () => request<User>('/auth/me'),
  changePassword: (old_password: string, new_password: string) =>
    request<{ ok: boolean }>('/auth/password', { method: 'POST', body: body({ old_password, new_password }) }),

  stats: () => request<Record<string, number | CleanerStatus>>('/stats'),

  channels: () => request<Channel[]>('/channels'),
  createChannel: (v: Partial<Channel>) => request<Channel>('/channels', { method: 'POST', body: body(v) }),
  updateChannel: (id: number, v: Partial<Channel>) => request<Channel>(`/channels/${id}`, { method: 'PUT', body: body(v) }),
  deleteChannel: (id: number) => request<void>(`/channels/${id}`, { method: 'DELETE' }),

  models: () => request<Model[]>('/models'),
  createModel: (v: Partial<Model>) => request<Model>('/models', { method: 'POST', body: body(v) }),
  updateModel: (id: number, v: Partial<Model>) => request<Model>(`/models/${id}`, { method: 'PUT', body: body(v) }),
  deleteModel: (id: number) => request<void>(`/models/${id}`, { method: 'DELETE' }),

  createBinding: (modelId: number, v: Partial<Binding>) =>
    request<Binding>(`/models/${modelId}/bindings`, { method: 'POST', body: body(v) }),
  updateBinding: (id: number, v: Partial<Binding>) => request<Binding>(`/bindings/${id}`, { method: 'PUT', body: body(v) }),
  deleteBinding: (id: number) => request<void>(`/bindings/${id}`, { method: 'DELETE' }),

  keys: (all = false) => request<ApiKey[]>(`/keys${all ? '?all=1' : ''}`),
  createKey: (name: string) => request<ApiKey>('/keys', { method: 'POST', body: body({ name }) }),
  updateKey: (id: number, v: { name?: string; enabled?: boolean }) =>
    request<ApiKey>(`/keys/${id}`, { method: 'PUT', body: body(v) }),
  deleteKey: (id: number) => request<void>(`/keys/${id}`, { method: 'DELETE' }),

  logs: (params: Record<string, string | number>) => {
    const q = new URLSearchParams(Object.entries(params).map(([k, v]) => [k, String(v)])).toString()
    return request<{ total: number; page: number; page_size: number; items: LogItem[] }>(`/logs?${q}`)
  },
  logArchive: (id: string) => request<{ request: string; response: string }>(`/logs/${id}/archive`),

  settings: () =>
    request<{
      settings: Record<string, string>
      strategies: string[]
      protocols: ProtocolInfo[]
      cleaner: CleanerStatus
    }>('/settings'),
  updateSettings: (v: Record<string, string>) =>
    request<{ settings: Record<string, string> }>('/settings', { method: 'PUT', body: body(v) }),
  runCleanup: () => request<{ ok: boolean; cleaner: CleanerStatus }>('/settings/cleanup', { method: 'POST' }),

  users: () => request<User[]>('/users'),
  updateUser: (id: number, v: { role?: string; enabled?: boolean; password?: string }) =>
    request<User>(`/users/${id}`, { method: 'PUT', body: body(v) }),
  deleteUser: (id: number) => request<void>(`/users/${id}`, { method: 'DELETE' }),
}
